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
func signedPayload(from, persona string, recipients []string, subject string, tags []string, urgency, thread string,
	sums []attachSum, signedAt string, body string) []byte {
	var b bytes.Buffer
	q, _ := json.Marshal(subject)
	if persona == "" {
		fmt.Fprintf(&b, "deaddrop signed message v1\n")
		fmt.Fprintf(&b, "from: %s\n", from)
	} else {
		p, _ := json.Marshal(persona)
		fmt.Fprintf(&b, "deaddrop signed message v2\n")
		fmt.Fprintf(&b, "from: %s\n", from)
		fmt.Fprintf(&b, "persona: %s\n", p)
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

// signersPath is this machine's list of who holds which key.
func signersPath() string {
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
}

func verifyState(m *Message) (string, string) {
	if m.Sig == "" {
		return sigUnsigned, "not signed: the sender is only a claim"
	}
	if m.SignedBy == "" {
		return sigBad, "a signature with no signer"
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
	// A follow-up raises a message to urgent after it was signed.
	urgencies := []string{m.Urgency}
	if m.Raised != "" {
		urgencies = []string{normal, fyi, urgent}
	}
	for _, u := range urgencies {
		payload := signedPayload(m.From, m.Persona, m.Recipients, m.Subject, m.Tags, u, m.Thread, sumsOf(m), m.SignedAt, m.Body)
		c := exec.Command(sshKeygen(), "-Y", "verify", "-f", signers, "-I", m.SignedBy, "-n", signNamespace, "-s", path)
		c.Stdin = bytes.NewReader(payload)
		if c.Run() == nil {
			if m.Persona != "" {
				return sigVerified, "signed by " + m.SignedBy + ", as " + m.Persona
			}
			return sigVerified, "signed by " + m.SignedBy
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
		return cmd, args, nil // send says what's wrong
	}
	body, err := readBody()
	if err != nil {
		return cmd, args, err
	}
	presetBody = &body
	d, err := draftOf(f)
	if err != nil {
		return cmd, args, nil // send says what's wrong
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
			return cmd, args, nil
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
	sig, err := sign(key, signedPayload(d.from, d.persona, d.to, d.subject, d.tags, d.urgency, d.thread, sums, at, d.body))
	if err != nil {
		return cmd, args, err
	}
	out := append([]string{}, args...)
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
		return c.SignAs, nil
	}
	return explicitMe()
}

// replyAsSend turns `reply ID` into the send the store would make of it, so
// it can be signed here: the original comes from the store first.
func replyAsSend(args []string) (string, []string, error) {
	const u = "reply ID [--urgent|--fyi] [--tag T]... [--follow-up 2d|TIME] < body.md"
	f, err := parseFlags(args, map[string]bool{"urgent": false, "fyi": false, "tag": true, "follow-up": true, "as": true})
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
	out := []string{"--to", orig.From, "--reply-to", orig.ID, "--subject", subject}
	for _, t := range orig.Tags {
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
	return nil
}
