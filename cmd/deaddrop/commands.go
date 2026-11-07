package main

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	to, tags                       []string
	body                           string
	attach                         []string // paths on this machine
	followUp                       time.Time
	// A signature made on the sender's machine (sign.go), if any.
	signedBy, signedAt, sig string
}

const sendUsage = `send --to WHO [--subject "…"] [--tag T]... [--urgent|--fyi] [--reply-to ID] [--attach FILE]... [--follow-up 2d|TIME] [--from WHO] < body.md`

var sendFlags = map[string]bool{
	"to": true, "subject": true, "tag": true, "urgent": false, "fyi": false,
	"reply-to": true, "attach": true, "follow-up": true, "from": true, "bundle": false,
	"signed-by": true, "signed-at": true, "signature": true, "as": true,
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
	const u = "reply ID [--urgent|--fyi] [--tag T]... [--as NAME] [--follow-up 2d|TIME] < body.md"
	f, err := parseFlags(args, map[string]bool{"urgent": false, "fyi": false, "tag": true, "follow-up": true, "as": true})
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
	orig, err := findMessage(dir, me, f.pos[0], false)
	if err != nil {
		return err
	}
	if orig.From == automation {
		return fmt.Errorf("%s came from automation, which has no box to reply to", orig.ID)
	}
	d := draft{from: me, to: []string{orig.From}, thread: orig.ID, tags: append([]string{}, orig.Tags...)}
	if d.urgency, err = urgencyOf(f); err != nil {
		return bad(u, err)
	}
	if d.persona, err = personaOf(f); err != nil {
		return bad(u, err)
	}
	extra, err := tagsOf(f)
	if err != nil {
		return bad(u, err)
	}
	for _, t := range extra {
		if !orig.hasTag(t) {
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
		payload := signedPayload(d.from, d.persona, d.to, d.subject, d.tags, d.urgency, d.thread, as, d.signedAt, d.body)
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
	for _, to := range d.to {
		m := &Message{From: d.from, Persona: d.persona, To: to, Subject: d.subject, Tags: d.tags, Urgency: d.urgency,
			Thread: d.thread, Attachments: attached, AttachmentSums: sums, Sent: sent, Body: d.body}
		// Who it went to, in every copy (deaddrop sent puts them together).
		m.Recipients = d.to
		if d.sig != "" {
			m.SignedBy, m.SignedAt, m.Sig = d.signedBy, d.signedAt, d.sig
		}
		if m.Tags == nil {
			m.Tags = []string{}
		}
		if !d.followUp.IsZero() {
			m.FollowUp = d.followUp.Format(time.RFC3339)
		}
		var path string
		for {
			m.ID = newID(d.from, sent)
			path = filepath.Join(dir, to, "new", m.ID+".md")
			if _, err := findMessage(dir, to, m.ID, false); err != nil {
				break
			}
		}
		for i, p := range d.attach {
			src, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = copyAtomic(filepath.Join(dir, to, "files", m.ID, attached[i]), src)
			src.Close()
			if err != nil {
				return err
			}
		}
		if err := writeAtomic(path, m.render(), 0o600); err != nil {
			return err
		}
		fmt.Printf("sent %s to %s: %q\n", m.ID, to, m.Subject)
		if !d.followUp.IsZero() {
			if err := bookFollowUp(dir, m); err != nil {
				// The mail is in the box; sending again would only make two.
				fmt.Fprintf(os.Stderr, "deaddrop: %s is delivered, but its follow-up isn't booked: %v\n", m.ID, err)
			}
		}
		if m.Urgency == urgent {
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
	const u = "inbox [WHO] [--tag T]... [--from WHO]... [--read|--all] [--json]"
	f, err := parseFlags(args, map[string]bool{"tag": true, "from": true, "since": true, "read": false, "all": false, "json": false, "raw": false})
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
		ms = fl.apply(loadBox(dir, box, states, true))
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
		fmt.Printf("%s in %s's box.\n", what, box)
		if !f.has("all") && !f.has("read") && viaHost == "" {
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.State, m.ID, m.Urgency, cleanLine(m.sender(), 48), sigMark(m), ago(m.Sent, at), tags, subj)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, m := range ms {
		if m.Signature == sigBad {
			fmt.Printf("\n✗ %s: %s. Don't trust who it says it's from.\n", m.ID, m.SignatureNote)
		}
	}
	return nil
}

func cmdPull(args []string) error {
	const u = "pull [WHO] [--tag T]... [--from WHO]... [--since 7d|DATE] [--unread] [--json]"
	f, err := parseFlags(args, map[string]bool{"tag": true, "from": true, "since": true, "unread": false, "json": false, "raw": false})
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
	if f.has("unread") {
		states = states[:1]
	}
	var ms []*Message
	if viaHost != "" {
		if ms, err = fetchRemote("pull", args); err != nil {
			return err
		}
		prepare(ms, false, "")
	} else {
		ms = fl.apply(loadBox(dir, box, states, true))
		sortForPull(ms)
		prepare(ms, f.has("raw"), dir)
	}
	if f.has("json") {
		return printJSON(ms)
	}
	what := "mail"
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
			} else if info, err := os.Stat(filepath.Join(dir, box, "files", m.ID, a)); err == nil {
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
	const u = "file ID [NAME] [--box WHO]"
	f, err := parseFlags(args, map[string]bool{"box": true})
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
	m, err := findMessage(dir, box, f.pos[0], len(f.pos) == 2)
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
	found := false
	for _, a := range m.Attachments {
		found = found || a == name
	}
	if !found {
		return fmt.Errorf("%s has no attachment %q (deaddrop file %s lists them)", m.ID, name, m.ID)
	}
	data, err := os.ReadFile(filepath.Join(dir, box, "files", m.ID, name))
	if err != nil {
		return err
	}
	return handOver(m, name, data)
}

// handOver writes an attachment out once it's checked: never a file that
// isn't what was sent, and a word when nobody vouches for who sent it.
func handOver(m *Message, name string, data []byte) error {
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
	if f.has("box") {
		return []string{"--box", f.get("box")}
	}
	return nil
}

// ---- marking ----

func cmdMark(args []string) error {
	const u = "mark read|unread|archive ID... | --tag T... [--from WHO]... [--box WHO]"
	f, err := parseFlags(args, map[string]bool{"tag": true, "from": true, "since": true, "box": true})
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
	unlock, err := lockBox(dir, box)
	if err != nil {
		return err
	}
	defer unlock()
	var ms []*Message
	if len(ids) > 0 {
		// All or nothing: every id has to be there before anything moves.
		for _, id := range ids {
			m, err := findMessage(dir, box, id, false)
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
		ms = fl.apply(loadBox(dir, box, from, false))
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
	Box      string   `json:"box"`
	Unread   int      `json:"unread"`
	Urgent   int      `json:"urgent"`
	Read     int      `json:"read"`
	Archived int      `json:"archived"`
	Newest   *Message `json:"newest_unread,omitempty"`
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
	const u = "status [--json]"
	f, err := parseFlags(args, map[string]bool{"json": false})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) > 0 {
		return badf(u, "status takes nothing but --json")
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	out := []boxStatus{}
	for _, b := range boxes(dir) {
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
	fmt.Fprintln(tw, "BOX\tUNREAD\tURGENT\tREAD\tARCHIVED\tNEWEST UNREAD")
	for _, s := range out {
		newest := "-"
		if s.Newest != nil {
			newest = s.Newest.Subject + " (from " + s.Newest.From + ")"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\n", s.Box, s.Unread, s.Urgent, s.Read, s.Archived, newest)
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
		for _, m := range loadBox(dir, box, []string{"archived"}, false) {
			if modTime(m.path).Before(cut) {
				if err := os.Remove(m.path); err == nil {
					os.RemoveAll(filepath.Join(dir, box, "files", m.ID))
					removed++
				}
			}
		}
		// Attachments whose message is gone.
		ents, _ := os.ReadDir(filepath.Join(dir, box, "files"))
		for _, e := range ents {
			if _, err := findMessage(dir, box, e.Name(), false); err != nil && checkID(e.Name()) == nil {
				os.RemoveAll(filepath.Join(dir, box, "files", e.Name()))
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
