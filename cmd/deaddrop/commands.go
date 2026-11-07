package main

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	maxBody       = 8 << 20  // a document, not a dump
	maxAttachment = 64 << 20 // per file
)

// ---- send and reply ----

// draft is a message on its way in, before it has an id or a box.
type draft struct {
	from, subject, urgency, thread string
	persona                        string
	helpers                        []string // --for: pigeonholes in each recipient's box
	replyBox                       string   // --reply-box: where answers go
	to, tags                       []string
	body                           string
	attach                         []string // paths on this machine
	followUp                       time.Time
	// A signature made on the sender's machine (sign.go), if any.
	signedBy, signedAt, sig string
}

const sendUsage = `send --to WHO [--subject "…"] [--tag T]... [--urgent|--fyi] [--reply-to ID] [--attach FILE]... [--follow-up 2d|TIME] [--from WHO] [--as NAME] [--for HELPER]... [--reply-box BOX] < body.md`

var sendFlags = map[string]bool{
	"to": true, "subject": true, "tag": true, "urgent": false, "fyi": false,
	"reply-to": true, "attach": true, "follow-up": true, "from": true, "bundle": false,
	"signed-by": true, "signed-at": true, "signature": true, "as": true, "for": true, "reply-box": true,
	"ask": false,
}

// helpersOf is a send's --for: the helpers it's addressed to.
func helpersOf(f flags) ([]string, error) {
	var out []string
	for _, raw := range f.all("for") {
		for _, h := range strings.Split(raw, ",") {
			h = strings.ToLower(strings.TrimSpace(h))
			if err := checkBox("--for", h); err != nil {
				return nil, err
			}
			if h == automation {
				return nil, fmt.Errorf("--for: automation is a sender, not a helper")
			}
			if !contains(out, h) {
				out = append(out, h)
			}
		}
	}
	return out, nil
}

// pigeonholeOf is a reading command's --for: one helper's pigeonhole, every
// pigeonhole in the box ("*"), or the person's own (""). $DEADDROP_FOR has
// been turned into the flag already (withFor).
func pigeonholeOf(f flags) (string, bool, error) {
	h := strings.ToLower(strings.TrimSpace(f.get("for")))
	switch {
	case !f.has("for"), h == "":
		return "", false, nil
	case h == "*":
		return "", true, nil
	}
	if err := checkBox("--for", h); err != nil {
		return "", false, err
	}
	return h, false, nil
}

// loadFor reads the pigeonhole a reading command asked for.
func loadFor(dir, box, helper string, all bool, states []string, withBody bool) []*Message {
	if all {
		return loadAll(dir, box, states, withBody)
	}
	return loadSlot(dir, box, helper, states, withBody)
}

// forArgs passes a reading command's --for on, to the store's machine.
func forArgs(f flags) []string {
	if f.has("for") {
		return []string{"--for", strings.ToLower(strings.TrimSpace(f.get("for")))}
	}
	return nil
}

// followUpText is a follow-up time as the header and the signature hold it.
func followUpText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// helperSlug is the pigeonhole a persona answers at: "Release manager" is
// release-manager. "" if it makes no name.
func helperSlug(persona string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(persona) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	if checkBox("", s) != nil || s == automation {
		return ""
	}
	return s
}

// personaOf is a send's --as, cleaned: one short line.
func personaOf(f flags) (string, error) {
	if !f.has("as") {
		return "", nil
	}
	p := cleanLine(f.get("as"), 60)
	if p == "" {
		return "", fmt.Errorf("--as: a name for who is speaking, like \"Release manager\"")
	}
	return p, nil
}

// draftOf works a send's flags out into a draft: the same on the sender's
// machine, which signs it, and on the store, which delivers it.
func draftOf(f flags) (draft, error) {
	const u = sendUsage
	var err error
	if len(f.pos) > 0 {
		return draft{}, badf(u, "send takes no words of its own (%q): the document comes on stdin, a subject goes in --subject", f.pos[0])
	}
	if !f.has("to") {
		return draft{}, badf(u, "who is it for? --to WHO (--to me for your own box)")
	}
	d := draft{subject: f.get("subject"), thread: f.get("reply-to"), attach: f.all("attach"),
		signedBy: f.get("signed-by"), signedAt: f.get("signed-at"), sig: f.get("signature")}
	if d.urgency, err = urgencyOf(f); err != nil {
		return d, bad(u, err)
	}
	if d.persona, err = personaOf(f); err != nil {
		return d, bad(u, err)
	}
	if d.helpers, err = helpersOf(f); err != nil {
		return d, bad(u, err)
	}
	if f.has("reply-box") {
		d.replyBox = strings.ToLower(strings.TrimSpace(f.get("reply-box")))
		if err := checkBox("--reply-box", d.replyBox); err != nil || d.replyBox == automation {
			if env := os.Getenv("DEADDROP_REPLY_BOX"); env != "" && strings.EqualFold(strings.TrimSpace(env), d.replyBox) {
				return d, badf(u, "$DEADDROP_REPLY_BOX (%q) isn't a box name, like alice-helper", env)
			}
			return d, badf(u, "--reply-box: a box, like alice-helper")
		}
	}
	if d.from, err = senderOf(f); err != nil {
		return d, bad(u, err)
	}
	for _, w := range f.all("to") {
		for _, w := range strings.Split(w, ",") {
			who, err := resolveBox("--to", strings.TrimSpace(w))
			if err != nil {
				return d, bad(u, err)
			}
			if !contains(d.to, who) {
				d.to = append(d.to, who)
			}
		}
	}
	if d.tags, err = tagsOf(f); err != nil {
		return d, bad(u, err)
	}
	if d.tags == nil {
		d.tags = []string{}
	}
	// An ask needs its recipient's decision: a reserved tag, sealed like any.
	if f.has("ask") && !contains(d.tags, askTag) {
		d.tags = append(d.tags, askTag)
	}
	if d.thread != "" {
		if err := checkID(d.thread); err != nil {
			return d, bad(u, fmt.Errorf("--reply-to: %v", err))
		}
	}
	if f.has("follow-up") {
		if d.followUp, err = followUpTime(f.get("follow-up")); err != nil {
			return d, bad(u, err)
		}
	}
	return d, nil
}

// settle finishes a draft once its body is known: the subject, and the
// check for secrets.
func (d *draft) settle() error {
	if contains(d.tags, askTag) && gist(d.body) == "" {
		return fmt.Errorf("an ask starts with the question: the document's first paragraph")
	}
	if d.subject == "" {
		d.subject = subjectFrom(d.body)
	}
	d.subject = cleanLine(d.subject, 200)
	if d.subject == "" {
		return fmt.Errorf("no subject: give --subject, or start the document with a heading")
	}
	if looksSecret(d.body) || looksSecret(d.subject) {
		return fmt.Errorf("that looks like it holds a secret (a key or a token). Never put one in a message: say where it lives instead")
	}
	return nil
}

func cmdSend(args []string) error {
	const u = sendUsage
	f, err := parseFlags(args, sendFlags)
	if err != nil {
		return bad(u, err)
	}
	d, err := draftOf(f)
	if err != nil {
		return err
	}
	if (d.sig != "") != (d.signedBy != "") || (d.sig != "") != (d.signedAt != "") {
		return badf(u, "--signature, --signed-by and --signed-at go together")
	}
	dir, err := openStore()
	if err != nil {
		return err
	}
	if f.has("bundle") {
		// A remote send: the body and the attachments came as one tar.
		incoming, err := os.MkdirTemp(dir, ".incoming-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(incoming)
		if d.body, d.attach, err = unbundle(os.Stdin, incoming); err != nil {
			return err
		}
	} else if d.body, err = readBody(); err != nil {
		return err
	}
	return deliver(dir, d)
}

func cmdReply(args []string) error {
	const u = "reply ID [--urgent|--fyi] [--tag T]... [--untag T]... [--as NAME] [--for HELPER]... [--reply-box BOX] [--follow-up 2d|TIME] < body.md"
	f, err := parseFlags(args, map[string]bool{"urgent": false, "fyi": false, "tag": true, "untag": true, "follow-up": true, "as": true, "for": true, "reply-box": true})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) != 1 {
		return badf(u, "reply to one message: its id")
	}
	me, err := whoAmI()
	if err != nil {
		return err
	}
	dir, err := openStore()
	if err != nil {
		return err
	}
	orig, err := findAnywhere(dir, me, f.pos[0], true)
	if err != nil {
		return err
	}
	verify(orig) // a reply follows only a persona that's really theirs
	if orig.From == automation {
		return fmt.Errorf("%s came from automation, which has no box to reply to", orig.ID)
	}
	kept, err := replyTags(orig.Tags, f.all("untag"))
	if err != nil {
		return bad(u, err)
	}
	d := draft{from: me, to: []string{replyTo(orig)}, thread: orig.ID, tags: kept}
	if d.urgency, err = urgencyOf(f); err != nil {
		return bad(u, err)
	}
	if d.persona, err = personaOf(f); err != nil {
		return bad(u, err)
	}
	if d.helpers, err = replyHelpers(orig, f); err != nil {
		return bad(u, err)
	}
	if f.has("reply-box") {
		d.replyBox = strings.ToLower(strings.TrimSpace(f.get("reply-box")))
		if err := checkBox("--reply-box", d.replyBox); err != nil || d.replyBox == automation {
			return badf(u, "--reply-box: a box, like alice-helper")
		}
	}
	extra, err := tagsOf(f)
	if err != nil {
		return bad(u, err)
	}
	for _, t := range extra {
		if !contains(d.tags, t) {
			d.tags = append(d.tags, t)
		}
	}
	d.subject = orig.Subject
	if !strings.HasPrefix(strings.ToLower(d.subject), "re:") {
		d.subject = "Re: " + d.subject
	}
	if f.has("follow-up") {
		if d.followUp, err = followUpTime(f.get("follow-up")); err != nil {
			return bad(u, err)
		}
	}
	if d.body, err = readBody(); err != nil {
		return err
	}
	return deliver(dir, d)
}

var warnedReplyBox bool

// replyTo is the box a reply goes to: the letter's reply box, when it has
// one and verifies (anyone could write one onto an unsigned letter), else its
// sender's.
func replyTo(orig *Message) string {
	if orig.ReplyBox == "" {
		return orig.From
	}
	if orig.Signature != sigVerified {
		if !warnedReplyBox {
			warnedReplyBox = true
			fmt.Fprintf(os.Stderr, "deaddrop: %s asks for answers at %s, but isn't verified, so this goes to %s\n", orig.ID, orig.ReplyBox, orig.From)
		}
		return orig.From
	}
	return orig.ReplyBox
}

// replyHelpers is where a reply goes in the sender's box: --for if given,
// else the pigeonhole of the persona that wrote it (an answer to "bob, as
// Planner" goes to bob's planner), else bob's own. What the original was for is
// never carried over: that was the replier's own pigeonhole.
func replyHelpers(orig *Message, f flags) ([]string, error) {
	if f.has("for") {
		return helpersOf(f)
	}
	if replyTo(orig) != orig.From {
		return nil, nil // its own box is the address: no pigeonhole on top
	}
	// Only a sealed persona that verifies says where a reply goes: anyone can
	// write a letter claiming one.
	if orig.Signature != sigVerified {
		return nil, nil
	}
	if s := helperSlug(orig.Persona); s != "" {
		return []string{s}, nil
	}
	return nil, nil
}

// replyTags are the tags a reply carries over from what it answers: what it's
// about, not who said it or what kind of letter it was. Tags starting
// persona- name a speaker, so they stay behind (the replier names their own),
// and so does each --untag.
func replyTags(orig, untag []string) ([]string, error) {
	drop := map[string]bool{}
	for _, raw := range untag {
		for _, t := range strings.Split(raw, ",") {
			t, err := checkTag(t)
			if err != nil {
				return nil, fmt.Errorf("--untag: %v", err)
			}
			drop[t] = true
		}
	}
	out := []string{}
	for _, t := range orig {
		// Who spoke, and "needs your decision", stay with the original.
		if !drop[t] && !strings.HasPrefix(t, "persona-") && t != askTag {
			out = append(out, t)
		}
	}
	return out, nil
}

func urgencyOf(f flags) (string, error) {
	switch {
	case f.has("urgent") && f.has("fyi"):
		return "", fmt.Errorf("--urgent or --fyi, not both")
	case f.has("urgent"):
		return urgent, nil
	case f.has("fyi"):
		return fyi, nil
	}
	return normal, nil
}

func senderOf(f flags) (string, error) {
	if !f.has("from") {
		return whoAmI()
	}
	// You send as yourself, or a job of yours sends as automation. Nobody
	// signs someone else's name.
	from := strings.ToLower(f.get("from"))
	if from == automation {
		return from, nil
	}
	me, err := whoAmI()
	if err != nil {
		return "", err
	}
	if from == "me" || from == me {
		return me, nil
	}
	return "", fmt.Errorf("--from %s: you send as yourself (%s) or as automation", from, me)
}

func tagsOf(f flags) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range f.all("tag") {
		for _, t := range strings.Split(raw, ",") {
			t, err := checkTag(t)
			if err != nil {
				return nil, err
			}
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out, nil
}

func readBody() (string, error) {
	if presetBody != nil {
		return *presetBody, nil
	}
	if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		return "", fmt.Errorf("the document comes on stdin: deaddrop send ... < report.md (or a pipe, or a heredoc)")
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, maxBody+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxBody {
		return "", fmt.Errorf("the document is over %d MB; attach big things with --attach", maxBody>>20)
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", fmt.Errorf("the document is empty (it comes on stdin)")
	}
	return string(b), nil
}

// deliver puts a copy of the draft in each recipient's box: attachments
// first, then the message, renamed into new/ last, so nobody sees a message
// whose files aren't there yet.
func deliver(dir string, d draft) error {
	if err := d.settle(); err != nil {
		return err
	}
	// Check every attachment before anything is written.
	names := map[string]bool{}
	var attached, sums []string
	for _, p := range d.attach {
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("--attach %s: %v", p, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("--attach %s: not a file", p)
		}
		if info.Size() > maxAttachment {
			return fmt.Errorf("--attach %s: over %d MB", p, maxAttachment>>20)
		}
		name := filepath.Base(p)
		if !fileRE.MatchString(name) {
			return fmt.Errorf("--attach %s: rename it to plain letters, digits, ., _, - and spaces first", p)
		}
		if names[name] {
			return fmt.Errorf("--attach: two files named %s", name)
		}
		names[name] = true
		attached = append(attached, name)
		sum, err := fileSum(p)
		if err != nil {
			return fmt.Errorf("--attach %s: %v", p, err)
		}
		sums = append(sums, sum)
	}
	if d.sig != "" {
		// The signature is checked by readers, against their own list of
		// keys. Here, only that it's a signature of this very message, so a
		// send that could never verify is refused rather than delivered.
		if d.from != automation && d.signedBy != d.from {
			return fmt.Errorf("signed by %s, but sent as %s", d.signedBy, d.from)
		}
		as := make([]attachSum, len(attached))
		for i := range attached {
			as[i] = attachSum{attached[i], sums[i]}
		}
		payload := signedPayload(d.from, d.persona, d.helpers, d.replyBox, followUpText(d.followUp), d.to, d.subject, d.tags, d.urgency, d.thread, as, d.signedAt, d.body)
		if err := signatureHolds(d.sig, payload); err != nil {
			return fmt.Errorf("the signature isn't of this message (%v): are both ends the same deaddrop?", err)
		}
	}
	if !d.followUp.IsZero() {
		if _, err := crontinental(); err != nil {
			return fmt.Errorf("--follow-up books a Crontinental job, and %v", err)
		}
	}
	sent := now()
	type copyAt struct{ box, helper string }
	var copies []copyAt
	for _, to := range d.to {
		if len(d.helpers) == 0 {
			copies = append(copies, copyAt{to, ""})
		}
		for _, h := range d.helpers {
			copies = append(copies, copyAt{to, h})
		}
	}
	rung := map[string]bool{}
	if d.replyBox != "" {
		if _, err := os.Stat(filepath.Join(dir, d.replyBox)); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "deaddrop: there's no box %s yet, so answers to this will start one (a typo?)\n", d.replyBox)
		}
	}
	for _, h := range d.helpers {
		for _, to := range d.to {
			if _, err := os.Stat(slotDir(dir, to, h)); os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "deaddrop: %s has no %s pigeonhole yet; this starts one (a typo?)\n", to, h)
			}
		}
	}
	// Hold every box while its copies go in, so gc never takes a new
	// letter's attachments for leftovers. All of them, in name order, before
	// anything is written: two sends to the same boxes can't wait on each
	// other.
	order := append([]string{}, d.to...)
	sort.Strings(order)
	var unlocks []func()
	defer func() {
		for _, u := range unlocks {
			u()
		}
	}()
	for _, to := range order {
		u, err := lockBox(dir, to)
		if err != nil {
			return err
		}
		unlocks = append(unlocks, u)
	}
	for _, at := range copies {
		to := at.box
		m := &Message{From: d.from, Persona: d.persona, For: d.helpers, ReplyBox: d.replyBox, Box: to, Pigeonhole: at.helper, To: to, Subject: d.subject, Tags: d.tags, Urgency: d.urgency,
			Thread: d.thread, Attachments: attached, AttachmentSums: sums, Sent: sent, Body: d.body}
		// Who it went to, in every copy (deaddrop sent puts them together).
		m.Recipients = d.to
		if d.sig != "" {
			m.SignedBy, m.SignedAt, m.Sig = d.signedBy, d.signedAt, d.sig
		}
		if m.Tags == nil {
			m.Tags = []string{}
		}
		m.FollowUp = followUpText(d.followUp)
		var path string
		for {
			m.ID = newID(d.from, sent)
			path = filepath.Join(slotDir(dir, to, at.helper), "new", m.ID+".md")
			if _, err := findAnywhere(dir, to, m.ID, false); err != nil {
				break
			}
		}
		for i, p := range d.attach {
			src, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = copyAtomic(filepath.Join(filesDir(dir, m), attached[i]), src)
			src.Close()
			if err != nil {
				return err
			}
		}
		if err := writeAtomic(path, m.render(), 0o600); err != nil {
			return err
		}
		where := to
		if at.helper != "" {
			where = to + " (for " + at.helper + ")"
		}
		fmt.Printf("sent %s to %s: %q\n", m.ID, where, m.Subject)
		if !d.followUp.IsZero() {
			if err := bookFollowUp(dir, m); err != nil {
				// The mail is in the box; sending again would only make two.
				fmt.Fprintf(os.Stderr, "deaddrop: %s is delivered, but its follow-up isn't booked: %v\n", m.ID, err)
			}
		}
		if m.Urgency == urgent && !rung[to] {
			rung[to] = true
			ring(to, m)
		}
	}
	return nil
}

// bundle and unbundle carry a send's body and attachments over ssh as one
// stream: body.md, then files/<name>.
func bundle(w io.Writer, body string, attach []string) error {
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{Name: "body.md", Mode: 0o600, Size: int64(len(body))}); err != nil {
		return err
	}
	if _, err := io.WriteString(tw, body); err != nil {
		return err
	}
	for _, p := range attach {
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("--attach %s: %v", p, err)
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			return fmt.Errorf("--attach %s: not a file", p)
		}
		if info.Size() > maxAttachment {
			f.Close()
			return fmt.Errorf("--attach %s: over %d MB", p, maxAttachment>>20)
		}
		err = tw.WriteHeader(&tar.Header{Name: "files/" + filepath.Base(p), Mode: 0o600, Size: info.Size()})
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

func unbundle(r io.Reader, into string) (string, []string, error) {
	tr := tar.NewReader(r)
	body, gotBody := "", false
	var files []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("reading the send: %v", err)
		}
		switch {
		case h.Name == "body.md" && !gotBody:
			b, err := io.ReadAll(io.LimitReader(tr, maxBody+1))
			if err != nil {
				return "", nil, err
			}
			if len(b) > maxBody {
				return "", nil, fmt.Errorf("the document is over %d MB", maxBody>>20)
			}
			body, gotBody = string(b), true
		case strings.HasPrefix(h.Name, "files/") && h.Typeflag == tar.TypeReg:
			name := strings.TrimPrefix(h.Name, "files/")
			if !fileRE.MatchString(name) {
				return "", nil, fmt.Errorf("--attach %s: rename it to plain letters, digits, ., _, - and spaces first", name)
			}
			if h.Size > maxAttachment {
				return "", nil, fmt.Errorf("--attach %s: over %d MB", name, maxAttachment>>20)
			}
			p := filepath.Join(into, name)
			if _, err := copyAtomic(p, io.LimitReader(tr, maxAttachment)); err != nil {
				return "", nil, err
			}
			files = append(files, p)
		default:
			return "", nil, fmt.Errorf("reading the send: unexpected %s", h.Name)
		}
	}
	if strings.TrimSpace(body) == "" {
		return "", nil, fmt.Errorf("the document is empty (it comes on stdin)")
	}
	return body, files, nil
}

// ---- reading ----

// filter is what inbox, pull and mark select by: every tag, any sender.
type filter struct {
	tags  []string
	from  []string
	since time.Time
}

func filterOf(f flags) (filter, error) {
	var fl filter
	var err error
	if fl.tags, err = tagsOf(f); err != nil {
		return fl, err
	}
	for _, raw := range f.all("from") {
		for _, w := range strings.Split(raw, ",") {
			w = strings.ToLower(strings.TrimSpace(w))
			if w == "me" {
				if w, err = whoAmI(); err != nil {
					return fl, err
				}
			}
			if err := checkBox("--from", w); err != nil {
				return fl, err
			}
			fl.from = append(fl.from, w)
		}
	}
	if f.has("since") {
		s := f.get("since")
		if d, ok := parseSpan(s); ok {
			fl.since = now().Add(-d)
		} else if t, ok := parseWhen(s); ok {
			fl.since = t
		} else {
			return fl, fmt.Errorf("--since %q: a span back from now (7d, 12h) or a date (2026-11-01, \"2026-11-01 09:00\")", s)
		}
	}
	return fl, nil
}

func (fl filter) empty() bool { return len(fl.tags) == 0 && len(fl.from) == 0 && fl.since.IsZero() }

func (fl filter) match(m *Message) bool {
	for _, t := range fl.tags {
		if !m.hasTag(t) {
			return false
		}
	}
	if len(fl.from) > 0 {
		ok := false
		for _, w := range fl.from {
			ok = ok || m.From == w
		}
		if !ok {
			return false
		}
	}
	return fl.since.IsZero() || !m.Sent.Before(fl.since)
}

func (fl filter) apply(ms []*Message) []*Message {
	var out []*Message
	for _, m := range ms {
		if fl.match(m) {
			out = append(out, m)
		}
	}
	return out
}

func (fl filter) describe() string {
	var parts []string
	if len(fl.tags) > 0 {
		parts = append(parts, "tagged "+strings.Join(fl.tags, " and "))
	}
	if len(fl.from) > 0 {
		parts = append(parts, "from "+strings.Join(fl.from, " or "))
	}
	if !fl.since.IsZero() {
		parts = append(parts, "since "+fl.since.Local().Format("2006-01-02 15:04"))
	}
	return strings.Join(parts, ", ")
}

// boxArg is a command's [WHO]: the caller's box unless named.
func boxArg(u string, pos []string) (string, error) {
	if len(pos) > 1 {
		return "", badf(u, "one box at a time (got %s)", strings.Join(pos, " "))
	}
	who := ""
	if len(pos) == 1 {
		who = pos[0]
	}
	return resolveBox("box", who)
}

func cmdInbox(args []string) error {
	const u = "inbox [WHO] [--tag T]... [--from WHO]... [--since D] [--read|--all] [--for HELPER|'*'] [--json]"
	f, err := parseFlags(args, map[string]bool{"tag": true, "from": true, "since": true, "read": false, "all": false, "json": false, "raw": false, "for": true})
	if err != nil {
		return bad(u, err)
	}
	box, err := boxArg(u, f.pos)
	if err != nil {
		return err
	}
	fl, err := filterOf(f)
	if err != nil {
		return bad(u, err)
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	states := []string{"unread"}
	switch {
	case f.has("all"):
		states = []string{"unread", "read", "archived"}
	case f.has("read"):
		states = []string{"unread", "read"}
	}
	var ms []*Message
	if viaHost != "" {
		if ms, err = fetchRemote("inbox", args); err != nil {
			return err
		}
		prepare(ms, false, "")
	} else {
		helper, all, err := pigeonholeOf(f)
		if err != nil {
			return bad(u, err)
		}
		ms = fl.apply(loadFor(dir, box, helper, all, states, true))
		sortForPull(ms)
		prepare(ms, f.has("raw"), dir)
	}
	if !f.has("raw") {
		// A list: the text comes with pull and show.
		for _, m := range ms {
			m.Body = ""
		}
	}
	if f.has("json") {
		return printJSON(ms)
	}
	if len(ms) == 0 {
		what := "No unread mail"
		if f.has("all") {
			what = "No mail"
		}
		if d := fl.describe(); d != "" {
			what += " " + d
		}
		where := box + "'s box"
		if h := f.get("for"); h != "" && h != "*" {
			where = h + "'s pigeonhole in " + where
		}
		fmt.Printf("%s in %s.\n", what, where)
		helperHint(dir, box, f)
		if !f.has("all") && !f.has("read") && viaHost == "" && !f.has("for") {
			if n := len(fl.apply(loadBox(dir, box, []string{"read"}, false))); n > 0 {
				fmt.Printf("(%d read: --all shows them.)\n", n)
			}
		}
		return nil
	}
	at := now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATE\tID\tURGENCY\tFROM\tSIG\tSENT\tTAGS\tSUBJECT")
	for _, m := range ms {
		tags := strings.Join(m.Tags, ",")
		if tags == "" {
			tags = "-"
		}
		subj := m.Subject
		if len(m.Attachments) > 0 {
			subj += fmt.Sprintf(" [%d attached]", len(m.Attachments))
		}
		state := m.State
		if m.Pigeonhole != "" && f.get("for") == "*" {
			state += " (for " + m.Pigeonhole + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", state, m.ID, m.Urgency, cleanLine(m.sender(), 48), sigMark(m), ago(m.Sent, at), tags, subj)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, m := range ms {
		if m.Signature == sigBad {
			fmt.Printf("\n✗ %s: %s. Don't trust who it says it's from.\n", m.ID, m.SignatureNote)
		}
	}
	helperHint(dir, box, f)
	return nil
}

func cmdPull(args []string) error {
	const u = "pull [WHO] [--tag T]... [--from WHO]... [--since 7d|DATE] [--unread|--all] [--for HELPER|'*'] [--json]"
	f, err := parseFlags(args, map[string]bool{"tag": true, "from": true, "since": true, "unread": false, "all": false, "json": false, "raw": false, "for": true})
	if err != nil {
		return bad(u, err)
	}
	box, err := boxArg(u, f.pos)
	if err != nil {
		return err
	}
	fl, err := filterOf(f)
	if err != nil {
		return bad(u, err)
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	states := []string{"unread", "read"}
	switch {
	case f.has("unread") && f.has("all"):
		return badf(u, "--unread or --all, not both")
	case f.has("unread"):
		states = states[:1]
	case f.has("all"):
		states = []string{"unread", "read", "archived"}
	}
	var ms []*Message
	if viaHost != "" {
		if ms, err = fetchRemote("pull", args); err != nil {
			return err
		}
		prepare(ms, false, "")
	} else {
		helper, all, err := pigeonholeOf(f)
		if err != nil {
			return bad(u, err)
		}
		ms = fl.apply(loadFor(dir, box, helper, all, states, true))
		sortForPull(ms)
		prepare(ms, f.has("raw"), dir)
	}
	if f.has("json") {
		return printJSON(ms)
	}
	defer helperHint(dir, box, f)
	what := "mail"
	if h := f.get("for"); h == "*" {
		what = "mail, every pigeonhole,"
	} else if h != "" {
		what = "mail for " + h
	}
	if d := fl.describe(); d != "" {
		what += " " + d
	}
	if len(ms) == 0 {
		fmt.Printf("No %s in %s's box.\n", what, box)
		return nil
	}
	// A boundary of its own, so a document that quotes a separator can't
	// end its message early.
	mark := newID("x", now())
	mark = mark[len(mark)-4:]
	unread := 0
	for _, m := range ms {
		if m.State == "unread" {
			unread++
		}
	}
	fmt.Printf("%s (%d unread) of %s in %s's box, unread first, oldest first within that.\n\n", plural(len(ms), "message"), unread, what, box)
	for i, m := range ms {
		fmt.Printf("===== deaddrop %s: message %d of %d (%s) =====\n", mark, i+1, len(ms), m.State)
		fmt.Println(sigLine(m))
		os.Stdout.Write(m.display())
		for j, a := range m.Attachments {
			size := "missing"
			if viaHost != "" {
				if j < len(m.AttachmentSizes) && m.AttachmentSizes[j] >= 0 {
					size = humanSize(m.AttachmentSizes[j])
				}
			} else if info, err := os.Stat(filepath.Join(filesDir(dir, m), a)); err == nil {
				size = humanSize(info.Size())
			}
			fmt.Printf("[attached: %s (%s); deaddrop file %s %q > %q]\n", a, size, m.ID, a, a)
		}
		fmt.Println()
	}
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	fmt.Printf("===== deaddrop %s: end of %s =====\n", mark, plural(len(ms), "message"))
	fmt.Printf("Nothing is marked yet. When done with them:\n  deaddrop mark read %s\n(or mark archive, to file them)\n", strings.Join(ids, " "))
	return nil
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

func printJSON(v any) error {
	if ms, ok := v.([]*Message); ok && ms == nil {
		v = []*Message{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func cmdFile(args []string) error {
	const u = "file ID [NAME] [--box WHO] [--for HELPER]"
	f, err := parseFlags(args, map[string]bool{"box": true, "for": true})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) < 1 || len(f.pos) > 2 {
		return badf(u, "which message, and which of its files?")
	}
	if viaHost != "" {
		return fileVia(f)
	}
	box, err := resolveBox("--box", f.get("box"))
	if err != nil {
		return err
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	m, err := findFor(dir, box, f, f.pos[0], len(f.pos) == 2)
	if err != nil {
		return err
	}
	if len(f.pos) == 1 {
		if len(m.Attachments) == 0 {
			fmt.Printf("%s has no attachments.\n", m.ID)
		}
		for _, a := range m.Attachments {
			fmt.Println(a)
		}
		return nil
	}
	name := f.pos[1]
	if !fileRE.MatchString(name) || filepath.Base(name) != name {
		return fmt.Errorf("%q isn't a plain file name, so it's not handed out", name)
	}
	found := false
	for _, a := range m.Attachments {
		found = found || a == name
	}
	if !found {
		return fmt.Errorf("%s has no attachment %q (deaddrop file %s lists them)", m.ID, name, m.ID)
	}
	data, err := os.ReadFile(filepath.Join(filesDir(dir, m), name))
	if err != nil {
		return err
	}
	return handOver(m, name, data)
}

// handOver writes an attachment out once it's checked: never a file that
// isn't what was sent, and a word when nobody vouches for who sent it.
func handOver(m *Message, name string, data []byte) error {
	if !fileRE.MatchString(name) || filepath.Base(name) != name {
		return fmt.Errorf("%q isn't a plain file name, so it's not handed out", name)
	}
	if !contains(m.Attachments, name) {
		return fmt.Errorf("%s has no attachment %q", m.ID, name)
	}
	verify(m)
	if err := checkAttachment(m, name, data); err != nil {
		return err
	}
	switch m.Signature {
	case sigVerified:
	case sigBad:
		return fmt.Errorf("%s: %s, so its attachments aren't handed out", m.ID, m.SignatureNote)
	default:
		fmt.Fprintf(os.Stderr, "deaddrop: %s isn't signed by a key this machine knows: the file is the one stored, but who sent it is only a claim\n", m.ID)
	}
	_, err := os.Stdout.Write(data)
	return err
}

// fileVia is file with the store elsewhere: the bytes and the message come
// from there, and are checked here.
func fileVia(f flags) error {
	if len(f.pos) == 1 {
		return runRemote(viaHost, "file", append([]string{f.pos[0]}, boxFlag(f)...))
	}
	ms, err := fetchRemote("show", append([]string{f.pos[0]}, boxFlag(f)...))
	if err != nil {
		return err
	}
	data, err := remoteBytes(viaHost, "file", append([]string{f.pos[0], f.pos[1]}, boxFlag(f)...))
	if err != nil {
		return err
	}
	return handOver(ms[0], f.pos[1], data)
}

func boxFlag(f flags) []string {
	out := forArgs(f)
	if f.has("box") {
		out = append(out, "--box", f.get("box"))
	}
	return out
}

// letterFileThere says a letter's file is in a pigeonhole, readable or not:
// gc never takes a damaged letter's attachments for leftovers.
func letterFileThere(dir, box, helper, id string) bool {
	for _, s := range stateDirs {
		if _, err := os.Stat(filepath.Join(slotDir(dir, box, helper), s.dir, id+".md")); err == nil {
			return true
		}
	}
	return false
}

// slotEmpty says a pigeonhole holds nothing at all.
func slotEmpty(dir, box, helper string) bool {
	for _, sub := range []string{"new", "read", "archive", "files"} {
		if ents, err := os.ReadDir(filepath.Join(slotDir(dir, box, helper), sub)); err == nil && len(ents) > 0 {
			return false
		}
	}
	return true
}

// helperHint says, under a person's own inbox, that their helpers have
// unread mail of their own (with the store here; status says it anywhere).
func helperHint(dir, box string, f flags) {
	if viaHost != "" || f.has("for") || dir == "" {
		return
	}
	var hs []string
	for _, h := range helpers(dir, box) {
		if n := len(loadSlot(dir, box, h, []string{"unread"}, false)); n > 0 {
			hs = append(hs, fmt.Sprintf("%s %d", h, n))
		}
	}
	if len(hs) > 0 {
		fmt.Printf("(unread for helpers: %s. deaddrop inbox --for '*')\n", strings.Join(hs, ", "))
	}
}

// findFor finds one letter by id: in the pigeonhole --for names, else in any
// of the box's (ids are unique across them).
func findFor(dir, box string, f flags, id string, withBody bool) (*Message, error) {
	helper, all, err := pigeonholeOf(f)
	if err != nil {
		return nil, err
	}
	if helper != "" && !all {
		m, err := findIn(dir, box, helper, id, withBody)
		if err != nil {
			if other, err2 := findAnywhere(dir, box, id, false); err2 == nil {
				where := "the person's own pigeonhole"
				if other.Pigeonhole != "" {
					where = other.Pigeonhole + "'s pigeonhole"
				}
				return nil, fmt.Errorf("%s isn't in %s's pigeonhole; it's in %s", id, helper, where)
			}
		}
		return m, err
	}
	return findAnywhere(dir, box, id, withBody)
}

// ---- marking ----

func cmdMark(args []string) error {
	const u = "mark read|unread|archive ID... | --tag T... [--from WHO]... [--since D] [--box WHO] [--for HELPER|'*']"
	f, err := parseFlags(args, map[string]bool{"tag": true, "from": true, "since": true, "box": true, "for": true})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) == 0 {
		return badf(u, "mark them read, unread or archive?")
	}
	target := map[string]string{"read": "read", "unread": "unread", "new": "unread", "archive": "archived", "archived": "archived", "file": "archived"}[f.pos[0]]
	if target == "" {
		return badf(u, "%q: mark read, unread or archive", f.pos[0])
	}
	ids := f.pos[1:]
	fl, err := filterOf(f)
	if err != nil {
		return bad(u, err)
	}
	var uniq []string
	for _, id := range ids {
		if !contains(uniq, id) {
			uniq = append(uniq, id)
		}
	}
	ids = uniq
	if len(ids) > 0 && !fl.empty() {
		return badf(u, "ids or --tag/--from, not both")
	}
	if len(ids) == 0 && fl.empty() {
		return badf(u, "which ones? ids, or --tag/--from to pick by those")
	}
	box, err := resolveBox("--box", f.get("box"))
	if err != nil {
		return err
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	// A box that isn't there has nothing to mark: never make one by asking.
	if _, err := os.Stat(filepath.Join(dir, box)); os.IsNotExist(err) {
		return fmt.Errorf("there's no box %s", box)
	}
	unlock, err := lockBox(dir, box)
	if err != nil {
		return err
	}
	defer unlock()
	var ms []*Message
	if len(ids) > 0 {
		// All or nothing: every id has to be there before anything moves.
		for _, id := range ids {
			m, err := findFor(dir, box, f, id, false)
			if err != nil {
				return err
			}
			ms = append(ms, m)
		}
	} else {
		var from []string
		for _, s := range stateDirs {
			if s.state != target {
				from = append(from, s.state)
			}
		}
		helper, all, err := pigeonholeOf(f)
		if err != nil {
			return bad(u, err)
		}
		ms = fl.apply(loadFor(dir, box, helper, all, from, false))
		sortForPull(ms)
	}
	moved := 0
	for _, m := range ms {
		was := m.State
		if err := move(dir, m, target); err != nil {
			return err
		}
		if was != target {
			moved++
			if was == "unread" {
				cancelFollowUp(m)
			}
		}
	}
	word := map[string]string{"read": "read", "unread": "unread", "archived": "archived"}[target]
	if len(ids) == 0 {
		fmt.Printf("%d marked %s (%s) in %s's box\n", moved, word, fl.describe(), box)
	} else {
		fmt.Printf("%d marked %s in %s's box\n", moved, word, box)
	}
	return nil
}

// ---- the whole store ----

type boxStatus struct {
	Box    string `json:"box"`
	Unread int    `json:"unread"`
	// HelpersUnread: unread mail in each helper's pigeonhole (0.6.4).
	HelpersUnread map[string]int `json:"helpers_unread,omitempty"`
	Urgent        int            `json:"urgent"`
	Read          int            `json:"read"`
	Archived      int            `json:"archived"`
	Newest        *Message       `json:"newest_unread,omitempty"`
}

func statusOf(dir, box string) boxStatus {
	s := boxStatus{Box: box}
	unread := loadBox(dir, box, []string{"unread"}, false)
	s.Unread = len(unread)
	for _, m := range unread {
		if m.Urgency == urgent {
			s.Urgent++
		}
	}
	s.Read = countFiles(filepath.Join(dir, box, "read"))
	s.Archived = countFiles(filepath.Join(dir, box, "archive"))
	s.Newest = newest(unread)
	for _, h := range helpers(dir, box) {
		if n := len(loadSlot(dir, box, h, []string{"unread"}, false)); n > 0 {
			if s.HelpersUnread == nil {
				s.HelpersUnread = map[string]int{}
			}
			s.HelpersUnread[h] = n
		}
	}
	return s
}

func countFiles(dir string) int {
	n := 0
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".md") && !strings.HasPrefix(e.Name(), ".") {
			n++
		}
	}
	return n
}

func cmdStatus(args []string) error {
	const u = "status [--box WHO]... [--json]"
	f, err := parseFlags(args, map[string]bool{"json": false, "box": true})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) > 0 {
		return badf(u, "status takes --box and --json, nothing else")
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	// Every box, or only those named: a front end asks for the viewer's own
	// and never holds anyone else's counts.
	list := boxes(dir)
	if f.has("box") {
		list = nil
		for _, raw := range f.all("box") {
			for _, w := range strings.Split(raw, ",") {
				b, err := resolveBox("--box", strings.TrimSpace(w))
				if err != nil {
					return bad(u, err)
				}
				if !contains(list, b) {
					list = append(list, b)
				}
			}
		}
	}
	out := []boxStatus{}
	for _, b := range list {
		out = append(out, statusOf(dir, b))
	}
	if f.has("json") {
		return printJSON(out)
	}
	if len(out) == 0 {
		fmt.Printf("No boxes yet in %s: a box exists once something's been sent to it.\n", dir)
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BOX\tUNREAD\tURGENT\tREAD\tARCHIVED\tHELPERS\tNEWEST UNREAD")
	for _, s := range out {
		newest := "-"
		if s.Newest != nil {
			newest = cleanLine(s.Newest.Subject+" (from "+s.Newest.From+")", 120)
		}
		hs := []string{}
		for h, n := range s.HelpersUnread {
			hs = append(hs, fmt.Sprintf("%s %d", h, n))
		}
		sort.Strings(hs)
		helpersCol := "-"
		if len(hs) > 0 {
			helpersCol = strings.Join(hs, ", ")
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%s\n", s.Box, s.Unread, s.Urgent, s.Read, s.Archived, helpersCol, newest)
	}
	return tw.Flush()
}

func cmdGc(args []string) error {
	const u = "gc [--days N]"
	f, err := parseFlags(args, map[string]bool{"days": true})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) > 0 {
		return badf(u, "gc takes nothing but --days")
	}
	days := 30
	if f.has("days") {
		if days, err = strconv.Atoi(f.get("days")); err != nil || days < 1 {
			return badf(u, "--days is a whole number of days, at least 1")
		}
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	cut := now().Add(-time.Duration(days) * 24 * time.Hour)
	removed := 0
	for _, box := range boxes(dir) {
		unlock, err := lockBox(dir, box)
		if err != nil {
			return err
		}
		for _, m := range loadAll(dir, box, []string{"archived"}, false) {
			if modTime(m.path).Before(cut) {
				if err := os.Remove(m.path); err == nil {
					os.RemoveAll(filesDir(dir, m))
					removed++
				}
			}
		}
		// Attachments whose message is gone, in every pigeonhole.
		for _, h := range append([]string{""}, helpers(dir, box)...) {
			files := filepath.Join(slotDir(dir, box, h), "files")
			ents, _ := os.ReadDir(files)
			for _, e := range ents {
				if checkID(e.Name()) == nil && !letterFileThere(dir, box, h, e.Name()) {
					os.RemoveAll(filepath.Join(files, e.Name()))
				}
			}
			// A helper's pigeonhole with nothing at all left in it goes too:
			// not one file, readable or not.
			if h != "" && slotEmpty(dir, box, h) {
				os.RemoveAll(slotDir(dir, box, h))
			}
		}
		unlock()
	}
	// A remote send that died half way.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".incoming-") {
			if info, err := e.Info(); err == nil && info.ModTime().Before(now().Add(-24*time.Hour)) {
				os.RemoveAll(filepath.Join(dir, e.Name()))
			}
		}
	}
	fmt.Printf("removed %s filed over %d days ago\n", plural(removed, "archived message"), days)
	return nil
}
