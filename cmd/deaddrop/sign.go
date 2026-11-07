package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Signed messages. Everyone may reach the store as the same login, so the
// store can't tell who sent what, and anyone on it could write a message in
// someone else's name. So the sender's own machine signs each message with
// their ssh key (ssh-keygen -Y sign, the key or the agent, never on the
// store), and the reader's own machine checks it against their own list of
// who holds which key (an allowed_signers file). Neither the store nor its
// login can make a signature, and a reader trusts only their own list.
//
// What's signed is a canonical text of everything the sender chose: who
// from, the recipients, subject, tags, urgency, thread, the attachments'
// names and sha256s, when it was signed, and the body. The id and the sent
// time are the store's, so they aren't signed; a copy delivered again later,
// or to another box, fails the recipients or the time check.

const signNamespace = "deaddrop"

// signedSkew is how far apart signed_at and sent may be: the clocks of two
// machines, and the time a send takes.
const signedSkew = time.Hour

// Signature states, as readers see them.
const (
	sigVerified = "verified" // signed by the sender's own key, as the reader's list says
	sigUnsigned = "unsigned" // no signature: the sender is only a claim
	sigUnknown  = "unknown"  // signed, but the reader's list has no key for the signer
	sigBad      = "bad"      // signed, and it doesn't hold: changed, moved, or someone else's
)

type attachSum struct{ name, sum string }

// signedPayload is the text a signature covers.
//
// v1 is a message with no persona, exactly as 0.3 signed it, so those still
// verify; v2 adds the persona. Taking a persona off, or putting one on,
// changes which is checked, and the signature fails.
//
// v3 (0.6) adds the helpers it's addressed to, v4 (0.6.4) a reply box, and
// v5 (0.6.4) a follow-up time. Each is used only when the letter has that
// field, so a letter verifies as the version it was signed as.
func signedPayload(from, persona string, helpers []string, replyBox, followUp string, recipients []string, subject string, tags []string, urgency, thread string,
	sums []attachSum, signedAt string, body string) []byte {
	var b bytes.Buffer
	q, _ := json.Marshal(subject)
	p, _ := json.Marshal(persona)
	switch {
	case followUp != "":
		// v5 (0.6.4) seals the follow-up time: only a letter its sender gave
		// a follow-up can be raised to urgent and still verify.
		fmt.Fprintf(&b, "deaddrop signed message v5\n")
		fmt.Fprintf(&b, "from: %s\n", from)
		fmt.Fprintf(&b, "persona: %s\n", p)
		fmt.Fprintf(&b, "for: %s\n", strings.Join(helpers, ","))
		fmt.Fprintf(&b, "reply_box: %s\n", replyBox)
		fmt.Fprintf(&b, "follow_up: %s\n", followUp)
	case replyBox != "":
		// v4 (0.6.4) adds the reply box; only for letters that name one.
		fmt.Fprintf(&b, "deaddrop signed message v4\n")
		fmt.Fprintf(&b, "from: %s\n", from)
		fmt.Fprintf(&b, "persona: %s\n", p)
		fmt.Fprintf(&b, "for: %s\n", strings.Join(helpers, ","))
		fmt.Fprintf(&b, "reply_box: %s\n", replyBox)
	case len(helpers) > 0:
		fmt.Fprintf(&b, "deaddrop signed message v3\n")
		fmt.Fprintf(&b, "from: %s\n", from)
		fmt.Fprintf(&b, "persona: %s\n", p)
		fmt.Fprintf(&b, "for: %s\n", strings.Join(helpers, ","))
	case persona != "":
		fmt.Fprintf(&b, "deaddrop signed message v2\n")
		fmt.Fprintf(&b, "from: %s\n", from)
		fmt.Fprintf(&b, "persona: %s\n", p)
	default:
		fmt.Fprintf(&b, "deaddrop signed message v1\n")
		fmt.Fprintf(&b, "from: %s\n", from)
	}
	fmt.Fprintf(&b, "recipients: %s\n", strings.Join(recipients, ","))
	fmt.Fprintf(&b, "subject: %s\n", q)
	fmt.Fprintf(&b, "tags: %s\n", strings.Join(tags, ","))
	fmt.Fprintf(&b, "urgency: %s\n", urgency)
	fmt.Fprintf(&b, "thread: %s\n", thread)
	fmt.Fprintf(&b, "signed_at: %s\n", signedAt)
	for _, a := range sums {
		fmt.Fprintf(&b, "attachment: %s %s\n", a.sum, a.name)
	}
	b.WriteString("body:\n")
	b.WriteString(canonicalBody(body))
	return b.Bytes()
}

// canonicalBody is the body as the file holds it: ending in a newline.
func canonicalBody(body string) string {
	if body != "" && !strings.HasSuffix(body, "\n") {
		return body + "\n"
	}
	return body
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sshKeygen() string {
	if p := os.Getenv("DEADDROP_SSH_KEYGEN"); p != "" {
		return p
	}
	return "ssh-keygen"
}

// signKey is the key this machine signs with: $DEADDROP_SIGN_KEY, else
// sign_key in the config. A .pub path signs through the ssh agent.
func signKey() (string, error) {
	if k := os.Getenv("DEADDROP_SIGN_KEY"); k != "" {
		return expandHome(k), nil
	}
	c, err := loadConfig()
	if err != nil {
		return "", err
	}
	return expandHome(c.SignKey), nil
}

// systemSigners is a machine's pinned list of who holds which key, put
// there by its administrator. When it's there it's the only list checked:
// on a machine many people (or agents) share one login, that login can
// change its own config and environment, but not a root-owned file.
var systemSigners = "/etc/deaddrop/allowed_signers"

// pinnedSigners says the machine's administrator has pinned the list.
func pinnedSigners() bool {
	info, err := os.Stat(systemSigners)
	return err == nil && info.Mode().IsRegular()
}

// signersPath is this machine's list of who holds which key: the pinned
// one if there is one, else $DEADDROP_SIGNERS, signers in the config, or
// allowed_signers beside the config.
func signersPath() string {
	if pinnedSigners() {
		return systemSigners
	}
	if p := os.Getenv("DEADDROP_SIGNERS"); p != "" {
		return expandHome(p)
	}
	if c, err := loadConfig(); err == nil && c.Signers != "" {
		return expandHome(c.Signers)
	}
	return filepath.Join(filepath.Dir(configPath()), "allowed_signers")
}

// sign signs payload with key; the signature comes back as one line.
func sign(key string, payload []byte) (string, error) {
	c := exec.Command(sshKeygen(), "-Y", "sign", "-n", signNamespace, "-f", key)
	c.Stdin = bytes.NewReader(payload)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("signing with %s: %s", key, strings.TrimSpace(errb.String()+" "+err.Error()))
	}
	return base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

func sigFile(sig string) (string, func(), error) {
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return "", nil, fmt.Errorf("the signature isn't base64")
	}
	f, err := os.CreateTemp("", "deaddrop-sig-*")
	if err != nil {
		return "", nil, err
	}
	f.Write(raw)
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// signatureHolds checks only that sig is a good signature of payload, by
// whatever key: what the store checks, so a send that would never verify is
// refused at once.
func signatureHolds(sig string, payload []byte) error {
	path, done, err := sigFile(sig)
	if err != nil {
		return err
	}
	defer done()
	c := exec.Command(sshKeygen(), "-Y", "check-novalidate", "-n", signNamespace, "-s", path)
	c.Stdin = bytes.NewReader(payload)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// sumsOf pairs a message's attachments with their recorded sums.
func sumsOf(m *Message) []attachSum {
	out := make([]attachSum, len(m.Attachments))
	for i, a := range m.Attachments {
		out[i].name = a
		if i < len(m.AttachmentSums) {
			out[i].sum = m.AttachmentSums[i]
		}
	}
	return out
}

// verify works out a message's signature state, on this machine, with this
// machine's list of signers. It needs the body.
func verify(m *Message) {
	m.Signature, m.SignatureNote = verifyState(m)
	if m.Signature == sigVerified {
		checkLedger(m)
	}
}

// wellFormed checks every header field a signature covers, before anything
// from them is shown or signed over: names are names, tags are tags, files
// are plain file names, sums are sums. A list item with a comma or a newline
// in it would otherwise sign the same as two items.
func wellFormed(m *Message) bool {
	name := func(s string) bool { return checkBox("", s) == nil }
	if !(m.From == automation || name(m.From)) || !name(m.SignedBy) {
		return false
	}
	if _, err := time.Parse(time.RFC3339, m.SignedAt); err != nil {
		return false
	}
	for _, r := range append(append([]string{}, m.Recipients...), m.For...) {
		if !name(r) {
			return false
		}
	}
	for _, t := range m.Tags {
		if c, err := checkTag(t); err != nil || c != t {
			return false
		}
	}
	for _, a := range m.Attachments {
		if !fileRE.MatchString(a) || filepath.Base(a) != a {
			return false
		}
	}
	if len(m.AttachmentSums) != 0 && len(m.AttachmentSums) != len(m.Attachments) {
		return false
	}
	for _, s := range m.AttachmentSums {
		if len(s) != 64 || strings.Trim(s, "0123456789abcdef") != "" {
			return false
		}
	}
	if m.ReplyBox != "" && !name(m.ReplyBox) {
		return false
	}
	if m.FollowUp != "" {
		if _, err := time.Parse(time.RFC3339, m.FollowUp); err != nil {
			return false
		}
	}
	return m.Urgency == normal || m.Urgency == fyi || m.Urgency == urgent
}

const tamperedHeader = "its header has been tampered with: a name, tag, file or time no sender could have signed"

func verifyState(m *Message) (string, string) {
	if m.Sig == "" {
		return sigUnsigned, "not signed: the sender is only a claim"
	}
	if m.SignedBy == "" {
		return sigBad, "a signature with no signer"
	}
	if !wellFormed(m) {
		return sigBad, tamperedHeader
	}
	if m.From != automation && m.SignedBy != m.From {
		return sigBad, fmt.Sprintf("signed by %s, but it says it's from %s", m.SignedBy, m.From)
	}
	box := m.Box
	if box == "" {
		box = m.To
	}
	if !contains(m.Recipients, box) {
		return sigBad, fmt.Sprintf("signed for %s, but it's in %s's box", strings.Join(m.Recipients, ", "), box)
	}
	switch {
	case len(m.For) > 0 && m.Pigeonhole != "" && !contains(m.For, m.Pigeonhole):
		return sigBad, fmt.Sprintf("signed for %s, but it's in %s's pigeonhole", strings.Join(m.For, ", "), m.Pigeonhole)
	case len(m.For) > 0 && m.Pigeonhole == "" && m.Box != "":
		return sigBad, fmt.Sprintf("signed for %s, but it's in %s's own pigeonhole", strings.Join(m.For, ", "), box)
	case len(m.For) == 0 && m.Pigeonhole != "":
		return sigBad, fmt.Sprintf("signed for %s, but it's in %s's pigeonhole", box, m.Pigeonhole)
	}
	at, err := time.Parse(time.RFC3339, m.SignedAt)
	if err != nil {
		return sigBad, "no time of signing"
	}
	if d := m.Sent.Sub(at); d > signedSkew || d < -signedSkew {
		return sigBad, fmt.Sprintf("signed %s but delivered %s: sent again?", at.Format(time.RFC3339), m.Sent.Format(time.RFC3339))
	}
	signers := signersPath()
	if _, err := os.Stat(signers); err != nil {
		return sigUnknown, "signed by " + m.SignedBy + ", but this machine has no list of signers (" + signers + ")"
	}
	path, done, err := sigFile(m.Sig)
	if err != nil {
		return sigBad, err.Error()
	}
	defer done()
	// A follow-up raises a message to urgent after it was signed, and says
	// what it was. Only ever to urgent, and the note says so.
	// What the letter could have been signed as. A follow-up time is sealed
	// from 0.6.4 (v5); a letter signed earlier carries one unsealed, and is
	// checked as it was signed. Only a sealed follow-up can have raised it.
	type attempt struct {
		urgency, followUp string
		raise             bool
	}
	var tries []attempt
	if m.Raised != "" {
		fu, err1 := time.Parse(time.RFC3339, m.FollowUp)
		at, err2 := time.Parse(time.RFC3339, m.Raised)
		switch {
		case m.Urgency != urgent:
			return sigBad, "marked raised by a follow-up, but it isn't urgent"
		case m.FollowUp == "" || err1 != nil || err2 != nil || at.Before(fu):
			return sigBad, "marked raised by a follow-up its sender never asked for"
		case now().Before(fu):
			// The shared login can at most do what the sender's own
			// follow-up would have done, and not before its time.
			return sigBad, "marked raised by its follow-up before the follow-up's time"
		}
		if m.UrgencyWas != normal && m.UrgencyWas != fyi {
			return sigBad, tamperedHeader
		}
		tries = append(tries, attempt{m.UrgencyWas, m.FollowUp, true})
	} else {
		if m.FollowUp != "" {
			tries = append(tries, attempt{m.Urgency, m.FollowUp, false})
		}
		tries = append(tries, attempt{m.Urgency, "", false})
	}
	for _, t := range tries {
		payload := signedPayload(m.From, m.Persona, m.For, m.ReplyBox, t.followUp, m.Recipients, m.Subject, m.Tags, t.urgency, m.Thread, sumsOf(m), m.SignedAt, m.Body)
		c := exec.Command(sshKeygen(), "-Y", "verify", "-f", signers, "-I", m.SignedBy, "-n", signNamespace, "-s", path)
		c.Stdin = bytes.NewReader(payload)
		if c.Run() == nil {
			if t.followUp == "" && m.FollowUp != "" {
				// Not in what was signed (a letter from before 0.6.4, or one
				// written onto it since): not shown as the sender's.
				m.FollowUp = ""
			}
			raised := ""
			if t.raise {
				raised = "; raised to urgent by its follow-up, not by the sender"
			}
			// When it was signed, too: a letter delivered again shows its age.
			when := ""
			if at, err := time.Parse(time.RFC3339, m.SignedAt); err == nil {
				when = ", " + at.Local().Format("2006-01-02 15:04")
			}
			if m.Persona != "" {
				return sigVerified, "signed by " + m.SignedBy + ", as " + m.Persona + when + raised
			}
			return sigVerified, "signed by " + m.SignedBy + when + raised
		}
	}
	// Whose key it is, by this machine's list: someone else's is a forgery;
	// nobody's is a key this machine can't vouch for.
	c := exec.Command(sshKeygen(), "-Y", "find-principals", "-f", signers, "-s", path)
	out, _ := c.Output()
	owners := strings.Fields(string(out))
	switch {
	case len(owners) == 0 && listsPrincipal(signers, m.SignedBy):
		return sigBad, fmt.Sprintf("it says %s signed it, but that isn't %s's key", m.SignedBy, m.SignedBy)
	case len(owners) == 0:
		return sigUnknown, "signed by " + m.SignedBy + ", whom " + signers + " has no key for"
	case !contains(owners, m.SignedBy):
		return sigBad, fmt.Sprintf("it says %s signed it, but the key is %s's", m.SignedBy, strings.Join(owners, ", "))
	}
	return sigBad, "the signature doesn't match: the message was changed after it was signed"
}

// listsPrincipal says whether the list has any key for who.
func listsPrincipal(signers, who string) bool {
	b, err := os.ReadFile(signers)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, p := range strings.Split(strings.Fields(line)[0], ",") {
			if p == who {
				return true
			}
		}
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// ---- the sending side ----

// withPersona adds $DEADDROP_AS to a send or reply that doesn't name a
// persona, so a robot's session can set it once. It's added here, on the
// sender's machine, so it goes to the store (and into the signature) as a
// flag.
func withPersona(cmd string, args []string) []string {
	if rb := os.Getenv("DEADDROP_REPLY_BOX"); rb != "" && hops() == 0 && (cmd == "send" || cmd == "reply") && !hasFlag(args, "reply-box") {
		args = append(append([]string{}, args...), "--reply-box", rb)
	}
	p := os.Getenv("DEADDROP_AS")
	if p == "" || hops() > 0 || (cmd != "send" && cmd != "reply") {
		return args
	}
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--as" || strings.HasPrefix(a, "--as=") {
			return args
		}
	}
	return append(append([]string{}, args...), "--as", p)
}

// readsFor are the commands $DEADDROP_FOR points at a helper's pigeonhole.
var readsFor = map[string]bool{"inbox": true, "ls": true, "pull": true, "show": true, "file": true,
	"mark": true, "watch": true, "bar": true, "thread": true, "with": true, "threads": true, "open": true}

// withFor adds $DEADDROP_FOR to a reading command that doesn't name a
// pigeonhole, so a helper's session can set it once. It's added here, on the
// reader's machine, so it reaches the store as a flag.
func withFor(cmd string, args []string) []string {
	h := os.Getenv("DEADDROP_FOR")
	if h == "" || hops() > 0 || !readsFor[cmd] {
		return args
	}
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--for" || strings.HasPrefix(a, "--for=") {
			return args
		}
	}
	return append(append([]string{}, args...), "--for", h)
}

// hasFlag says args name --flag (before any --).
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--"+flag || strings.HasPrefix(a, "--"+flag+"=") {
			return true
		}
	}
	return false
}

// presetBody, when set, is the body: the signing step has read stdin already.
var presetBody *string

// signOutgoing prepares a send (or a reply, which it turns into a send) on
// the sender's own machine: it works the message out exactly as the store
// will, signs it, and returns the send's arguments with the signature added.
// With no signing key, the command goes as it is.
func signOutgoing(cmd string, args []string) (string, []string, error) {
	if hops() > 0 || (cmd != "send" && cmd != "reply") {
		return cmd, args, nil
	}
	// Whatever can't be signed goes on as the person typed it, so its
	// errors name their own command (a reply's, not the send it became).
	origCmd, origArgs := cmd, args
	key, err := signKey()
	if err != nil || key == "" {
		return cmd, args, err
	}
	if cmd == "reply" {
		if cmd, args, err = replyAsSend(args); err != nil {
			return cmd, args, err
		}
	}
	f, err := parseFlags(args, sendFlags)
	if err != nil || f.has("signature") {
		return origCmd, origArgs, nil // the command says what's wrong
	}
	body, err := readBody()
	if err != nil {
		return cmd, args, err
	}
	presetBody = &body
	d, err := draftOf(f)
	if err != nil {
		return origCmd, origArgs, nil // the command says what's wrong
	}
	d.body = body
	if err := d.settle(); err != nil {
		return cmd, args, err
	}
	signer := d.from
	c, err := loadConfig()
	if err != nil {
		return cmd, args, err
	}
	if c.SharedLogin {
		// Everyone here is the same login, so this machine's key is nobody's
		// own: it never signs as a person (a front end sending for someone
		// goes unsigned, as it should), only a job's automation mail, and
		// only under a name the config gives it.
		if d.from != automation || c.SignAs == "" {
			return origCmd, origArgs, nil
		}
		signer = c.SignAs
	} else if signer == automation {
		if signer, err = signAs(); err != nil || signer == "" {
			return cmd, args, err
		}
	}
	var sums []attachSum
	for _, p := range d.attach {
		s, err := fileSum(p)
		if err != nil {
			return cmd, args, fmt.Errorf("--attach %s: %v", p, err)
		}
		sums = append(sums, attachSum{filepath.Base(p), s})
	}
	at := now().UTC().Format(time.RFC3339)
	sig, err := sign(key, signedPayload(d.from, d.persona, d.helpers, d.replyBox, followUpText(d.followUp), d.to, d.subject, d.tags, d.urgency, d.thread, sums, at, d.body))
	if err != nil {
		return cmd, args, err
	}
	out := append([]string{}, args...)
	if fu := followUpText(d.followUp); fu != "" {
		// Worked out once, here, and sealed: the store must not work out
		// "2d" again a moment later and get another time.
		out = append(out, "--follow-up", fu)
	}
	return "send", append(out, "--subject", d.subject, "--signed-by", signer, "--signed-at", at, "--signature", sig), nil
}

// signAs is who signs a job's automation mail: sign_as in the config, else
// the person whose machine it is.
func signAs() (string, error) {
	c, err := loadConfig()
	if err != nil {
		return "", err
	}
	if c.SignAs != "" {
		if err := checkBox("sign_as", c.SignAs); err != nil {
			return "", fmt.Errorf("%s: %v", c.path, err)
		}
		return c.SignAs, nil
	}
	return explicitMe()
}

// replyAsSend turns `reply ID` into the send the store would make of it, so
// it can be signed here: the original comes from the store first.
func replyAsSend(args []string) (string, []string, error) {
	const u = "reply ID [--urgent|--fyi] [--tag T]... [--untag T]... [--as NAME] [--for HELPER]... [--reply-box BOX] [--follow-up 2d|TIME] < body.md"
	f, err := parseFlags(args, map[string]bool{"urgent": false, "fyi": false, "tag": true, "untag": true, "follow-up": true, "as": true, "for": true, "reply-box": true})
	if err != nil {
		return "", nil, bad(u, err)
	}
	if len(f.pos) != 1 {
		return "", nil, badf(u, "reply to one message: its id")
	}
	raw, err := self("show", f.pos[0], "--json")
	if err != nil {
		return "", nil, err
	}
	var orig Message
	if err := json.Unmarshal(raw, &orig); err != nil {
		return "", nil, err
	}
	if orig.From == automation {
		return "", nil, fmt.Errorf("%s came from automation, which has no box to reply to", orig.ID)
	}
	subject := orig.Subject
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	kept, err := replyTags(orig.Tags, f.all("untag"))
	if err != nil {
		return "", nil, bad(u, err)
	}
	out := []string{"--to", replyTo(&orig), "--reply-to", orig.ID, "--subject", subject}
	for _, t := range kept {
		out = append(out, "--tag", t)
	}
	for _, t := range f.all("tag") {
		out = append(out, "--tag", t)
	}
	for _, k := range []string{"urgent", "fyi"} {
		if f.has(k) {
			out = append(out, "--"+k)
		}
	}
	if f.has("follow-up") {
		out = append(out, "--follow-up", f.get("follow-up"))
	}
	if f.has("as") {
		out = append(out, "--as", f.get("as"))
	}
	if f.has("reply-box") {
		out = append(out, "--reply-box", f.get("reply-box"))
	}
	hs, err := replyHelpers(&orig, f)
	if err != nil {
		return "", nil, bad(u, err)
	}
	for _, h := range hs {
		out = append(out, "--for", h)
	}
	return "send", out, nil
}

// ---- deaddrop key ----

// cmdKey prints this machine's line for everyone's allowed_signers.
func cmdKey(args []string) error {
	const u = "key"
	if len(args) > 0 {
		return badf(u, "key takes nothing")
	}
	key, err := signKey()
	if err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("no signing key: set sign_key in %s (your ssh key, or its .pub to sign through the agent)", configPath())
	}
	who, err := explicitMe()
	if err != nil {
		return err
	}
	if who == "" {
		if who, err = whoAmI(); err != nil {
			return err
		}
	}
	pub := key
	if !strings.HasSuffix(pub, ".pub") {
		pub += ".pub"
	}
	b, err := os.ReadFile(pub)
	if err != nil {
		return fmt.Errorf("reading %s: %v", pub, err)
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return fmt.Errorf("%s isn't an ssh public key", pub)
	}
	fmt.Printf("%s namespaces=\"%s\" %s %s\n", who, signNamespace, fields[0], fields[1])
	if pinnedSigners() {
		fmt.Fprintf(os.Stderr, "deaddrop: this machine checks signatures only against %s (pinned by its administrator)\n", systemSigners)
	}
	return nil
}
