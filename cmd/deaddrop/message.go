package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// A Message is one document in a box: a small header, then Markdown. The
// header is YAML a person can read; the Dead Drop writes it one way and reads
// only what it wrote (strings that need it are JSON-quoted, which YAML reads
// too).
type Message struct {
	ID      string   `json:"id"`
	From    string   `json:"from"`
	To      string   `json:"to"`
	Subject string   `json:"subject"`
	Tags    []string `json:"tags"`
	Urgency string   `json:"urgency"`
	// Persona: who is speaking for the sender, when it's one of their
	// robots ("Release manager"). A label under the sender, never another
	// sender: the box and the signature are still theirs. Added in 0.4.
	Persona string `json:"persona,omitempty"`
	// For: the helpers it's addressed to, in the recipient's box (0.6).
	// Each gets a copy in their own pigeonhole, with its own state.
	For []string `json:"for,omitempty"`
	// ReplyBox: where answers go, when not to the sender's own box (0.6.4),
	// such as an assistant's own box. Sealed, and honoured only when verified.
	ReplyBox string `json:"reply_box,omitempty"`
	Thread   string `json:"thread,omitempty"`
	FollowUp string `json:"follow_up,omitempty"`
	Raised   string `json:"raised,omitempty"`
	// UrgencyWas: what the sender signed, when a follow-up raised it (0.6.4).
	UrgencyWas  string    `json:"urgency_was,omitempty"`
	Attachments []string  `json:"attachments,omitempty"`
	Sent        time.Time `json:"sent"`
	Body        string    `json:"body,omitempty"`

	// The signature (sign.go). Added in 0.3; a message without one is
	// unsigned. AttachmentSums lines up with Attachments.
	AttachmentSums []string `json:"attachment_sha256,omitempty"`
	Recipients     []string `json:"recipients,omitempty"`
	SignedBy       string   `json:"signed_by,omitempty"`
	SignedAt       string   `json:"signed_at,omitempty"`
	Sig            string   `json:"sig,omitempty"` // only in --raw output

	// Worked out by the reader, never stored: verified, unsigned, unknown
	// or bad, and why.
	Signature     string `json:"signature,omitempty"`
	SignatureNote string `json:"signature_note,omitempty"`
	// The letter every copy of this message is, and the letter it answers,
	// as short fingerprints of their seals: worked out by the store, which
	// sees every box, so a reader can join a thread across copies whose ids
	// differ from box to box. Never stored.
	Letter       string `json:"letter,omitempty"`
	ThreadLetter string `json:"thread_letter,omitempty"`
	// Attachment sizes, for a reader on another machine (--raw).
	AttachmentSizes []int64 `json:"attachment_sizes,omitempty"`

	// Where it is, worked out from its path, never stored in it.
	Box   string `json:"box"`
	State string `json:"state"` // unread, read or archived
	// Pigeonhole: whose copy this is, a helper's, or "" for the person's.
	Pigeonhole string `json:"pigeonhole,omitempty"`
	path       string
	// Header lines this version doesn't know, from a later one: kept as
	// they are and written back, so rewriting a message never drops them.
	unknown []string
}

const (
	automation = "automation"
	fyi        = "fyi"
	normal     = "normal"
	urgent     = "urgent"
)

var (
	boxRE  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	tagRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,47}$`)
	idRE   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{6}-[a-z][a-z0-9_-]{0,31}-[0-9a-f]{4}$`)
	fileRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ +-]{0,127}$`)
)

func checkBox(what, s string) error {
	if !boxRE.MatchString(s) {
		if what == "" {
			what = "a name"
		}
		return fmt.Errorf("%s: %q is not a name (lowercase letters, digits, - and _, starting with a letter, at most 32)", what, s)
	}
	return nil
}

func checkTag(s string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if !tagRE.MatchString(t) {
		return "", fmt.Errorf("tag %q: lowercase letters, digits, ., _ and -, at most 48", s)
	}
	return t, nil
}

func checkID(s string) error {
	if !idRE.MatchString(s) {
		return fmt.Errorf("%q is not a message id (they look like 2026-11-02T153012-alice-3f9a)", s)
	}
	return nil
}

// newID says when and from whom: ids sort by time, to the second.
func newID(from string, at time.Time) string {
	b := make([]byte, 2)
	rand.Read(b)
	return at.UTC().Format("2006-01-02T150405") + "-" + from + "-" + hex.EncodeToString(b)
}

// cleanLine makes a one-line header value: no control characters, no
// invisible format characters (a right-to-left override can reorder a line
// on screen), no line or paragraph separators, trimmed, at most n runes.
func cleanLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || (unicode.Is(unicode.Cf, r) && r != '\u200c' && r != '\u200d') || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = strings.TrimSpace(string(r[:n-1])) + "…"
	}
	return s
}

var headingRE = regexp.MustCompile(`^#{1,6}\s+(.+?)\s*#*\s*$`)

// subjectFrom takes a subject from the document: its first heading, else its
// first non-empty line.
func subjectFrom(body string) string {
	first := ""
	inHeader := false
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 0; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		// A document with its own front matter: skip it.
		if n == 0 && line == "---" {
			inHeader = true
			continue
		}
		if inHeader {
			if line == "---" {
				inHeader = false
			}
			continue
		}
		if m := headingRE.FindStringSubmatch(line); m != nil {
			return cleanLine(m[1], 120)
		}
		if first == "" && line != "" {
			first = line
		}
	}
	return cleanLine(first, 120)
}

// Secrets are refused on the way in: the store is plain files many people
// read. These are the shapes that are never anything but a secret.
var secretREs = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{40,}\b`),
	regexp.MustCompile(`\b[sr]k_live_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{30,}\b`),
	regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{20,}\b`),
}

func looksSecret(s string) bool {
	for _, re := range secretREs {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// render writes the file: the header, then the body.
func (m *Message) render() []byte { return m.renderAs(false) }

// display is the file as a person reads it: without the signature's bytes
// (its state is shown instead, by the caller).
func (m *Message) display() []byte { return m.renderAs(true) }

func (m *Message) renderAs(forDisplay bool) []byte {
	if forDisplay {
		// What a person sees on a terminal: every value made safe, so a
		// hand-edited header can't move the cursor or fake a line.
		c := *m
		safe := func(s string) string { return cleanLine(s, 400) }
		c.ID, c.From, c.To, c.Thread, c.FollowUp, c.Raised, c.SignedBy, c.SignedAt, c.UrgencyWas =
			safe(m.ID), safe(m.From), safe(m.To), safe(m.Thread), safe(m.FollowUp), safe(m.Raised), safe(m.SignedBy), safe(m.SignedAt), safe(m.UrgencyWas)
		c.Urgency, c.ReplyBox = safe(m.Urgency), safe(m.ReplyBox)
		c.unknown = nil
		for _, l := range m.unknown {
			c.unknown = append(c.unknown, safe(l))
		}
		m = &c
	}
	var b strings.Builder
	q := func(s string) string { j, _ := json.Marshal(s); return string(j) }
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", m.ID)
	fmt.Fprintf(&b, "from: %s\n", m.From)
	fmt.Fprintf(&b, "to: %s\n", m.To)
	fmt.Fprintf(&b, "subject: %s\n", q(m.Subject))
	fmt.Fprintf(&b, "tags: [%s]\n", strings.Join(m.Tags, ", "))
	fmt.Fprintf(&b, "urgency: %s\n", m.Urgency)
	if m.Persona != "" {
		fmt.Fprintf(&b, "persona: %s\n", q(m.Persona))
	}
	if len(m.For) > 0 {
		fmt.Fprintf(&b, "for: [%s]\n", strings.Join(m.For, ", "))
	}
	if m.ReplyBox != "" {
		fmt.Fprintf(&b, "reply_box: %s\n", m.ReplyBox)
	}
	if m.Thread != "" {
		fmt.Fprintf(&b, "thread: %s\n", m.Thread)
	}
	if m.FollowUp != "" {
		fmt.Fprintf(&b, "follow_up: %s\n", m.FollowUp)
	}
	if m.Raised != "" {
		fmt.Fprintf(&b, "raised: %s\n", m.Raised)
	}
	if m.UrgencyWas != "" {
		fmt.Fprintf(&b, "urgency_was: %s\n", m.UrgencyWas)
	}
	if len(m.Attachments) > 0 {
		a, _ := json.Marshal(m.Attachments)
		fmt.Fprintf(&b, "attachments: %s\n", a)
	}
	if len(m.AttachmentSums) > 0 {
		fmt.Fprintf(&b, "attachment_sha256: [%s]\n", strings.Join(m.AttachmentSums, ", "))
	}
	if len(m.Recipients) > 0 {
		fmt.Fprintf(&b, "recipients: [%s]\n", strings.Join(m.Recipients, ", "))
	}
	if m.SignedBy != "" {
		fmt.Fprintf(&b, "signed_by: %s\n", m.SignedBy)
	}
	if m.SignedAt != "" {
		fmt.Fprintf(&b, "signed_at: %s\n", m.SignedAt)
	}
	if m.Sig != "" && !forDisplay {
		fmt.Fprintf(&b, "signature: %s\n", m.Sig)
	}
	for _, l := range m.unknown {
		b.WriteString(l + "\n")
	}
	fmt.Fprintf(&b, "sent: %s\n", m.Sent.UTC().Format(time.RFC3339))
	b.WriteString("---\n")
	b.WriteString(m.Body)
	if m.Body != "" && !strings.HasSuffix(m.Body, "\n") {
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// parseMessage reads a file. withBody false stops at the header.
func parseMessage(data []byte, withBody bool) (*Message, error) {
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		return nil, fmt.Errorf("no header")
	}
	head, body, ok := strings.Cut(s[4:], "\n---\n")
	if !ok {
		if strings.HasSuffix(s[4:], "\n---") {
			head, body, ok = strings.TrimSuffix(s[4:], "\n---"), "", true
		} else {
			return nil, fmt.Errorf("the header never ends")
		}
	}
	m := &Message{}
	for _, line := range strings.Split(head, "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "id":
			m.ID = v
		case "from":
			m.From = v
		case "to":
			m.To = v
		case "subject":
			m.Subject = unquote(v)
		case "tags":
			m.Tags = list(v)
		case "urgency":
			m.Urgency = v
		case "persona":
			// One clean line, however it was written: it's shown everywhere.
			m.Persona = cleanLine(unquote(v), 60)
		case "for":
			m.For = list(v)
		case "reply_box":
			m.ReplyBox = v
		case "thread":
			m.Thread = v
		case "follow_up":
			m.FollowUp = v
		case "raised":
			m.Raised = v
		case "urgency_was":
			m.UrgencyWas = v
		case "attachments":
			m.Attachments = list(v)
		case "attachment_sha256":
			m.AttachmentSums = list(v)
		case "recipients":
			m.Recipients = list(v)
		case "signed_by":
			m.SignedBy = v
		case "signed_at":
			m.SignedAt = v
		case "signature":
			m.Sig = v
		case "sent":
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return nil, fmt.Errorf("sent: %v", err)
			}
			m.Sent = t.UTC()
		default:
			m.unknown = append(m.unknown, line)
		}
	}
	if m.ID == "" {
		return nil, fmt.Errorf("no id in the header")
	}
	if m.Tags == nil {
		m.Tags = []string{}
	}
	if withBody {
		m.Body = body
	}
	return m, nil
}

func unquote(v string) string {
	if strings.HasPrefix(v, `"`) {
		var s string
		if json.Unmarshal([]byte(v), &s) == nil {
			return s
		}
	}
	return v
}

// list reads [a, b] or ["a", "b"].
func list(v string) []string {
	out := []string{}
	if strings.HasPrefix(v, "[\"") {
		if json.Unmarshal([]byte(v), &out) == nil {
			return out
		}
	}
	v = strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(unquote(strings.TrimSpace(p))); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// sender is who it's from as a person reads it: "alice", or "alice, as Release
// manager" when one of their robots wrote it.
func (m *Message) sender() string {
	switch {
	case m.Persona == "":
		return m.From
	case m.Signature == "" || m.Signature == sigVerified:
		return m.From + ", as " + m.Persona
	}
	// Anyone can write a persona onto a letter that doesn't verify. Said
	// first, so a narrow column can't cut it off.
	return m.From + " (unverified), as " + m.Persona
}

func (m *Message) hasTag(t string) bool {
	for _, x := range m.Tags {
		if x == t {
			return true
		}
	}
	return false
}

// now is the clock. DEADDROP_NOW pins it, for tests.
func now() time.Time {
	if s := os.Getenv("DEADDROP_NOW"); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC().Truncate(time.Second)
}

var spanRE = regexp.MustCompile(`^(\d+)\s*(s|sec|secs|seconds?|m|min|mins|minutes?|h|hr|hrs|hours?|d|days?|w|weeks?)$`)

// parseSpan reads 90m, 2h, 2d, 1w, "3 days".
func parseSpan(s string) (time.Duration, bool) {
	m := spanRE.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	unit := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[m[2][0]]
	// Ten years at most: past that a span is a typo, and it would wrap.
	if err != nil || n < 0 || time.Duration(n) > 10*365*24*time.Hour/unit {
		return 0, false
	}
	return time.Duration(n) * unit, true
}

// parseWhen reads a time: a full one (RFC 3339), or a local "2026-11-04
// 09:00", "2026-11-04T09:00" or "2026-11-04" (its start).
func parseWhen(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	for _, layout := range []string{"2006-01-02T15:04Z07:00", "2006-01-02T15:04Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// ago is a short age for a list.
func ago(t, at time.Time) string {
	d := at.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
