package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
)

// deaddrop sent: the messages a person sent. A message is stored only in its
// recipients' boxes, a copy (with its own id) in each, so this looks through
// every box for mail from that person, and puts the copies of one message
// back together: one entry, with its recipients and each copy's state.

// SentMessage is one message sent: the fields pull gives, from its first
// copy, with state "sent", plus where each copy is.
type SentMessage struct {
	*Message
	Copies []SentCopy `json:"copies"`
}

// SentCopy is one recipient's copy, and whether they've read it.
type SentCopy struct {
	Box   string `json:"box"`
	For   string `json:"for,omitempty"` // the helper whose pigeonhole it's in
	ID    string `json:"id"`
	State string `json:"state"` // unread, read or archived
}

// sentKey is what the copies of one message share. A signed message's
// signature is the same in every copy; an unsigned one is known by its
// sender, time, subject, thread and text.
func sentKey(m *Message) string {
	if m.Sig != "" {
		return "sig:" + m.Sig
	}
	h := sha256.Sum256([]byte(canonicalBody(m.Body)))
	return strings.Join([]string{m.From, m.Sent.UTC().Format("2006-01-02T15:04:05Z"), m.Subject, m.Thread, hex.EncodeToString(h[:])}, "\x00")
}

// gatherSent is everything from who, across every box, one entry per message.
func gatherSent(dir, who string) []*SentMessage {
	type found struct {
		c SentCopy
		m *Message
	}
	groups := map[string][]found{}
	var order []string
	for _, box := range boxes(dir) {
		for _, m := range loadAll(dir, box, []string{"unread", "read", "archived"}, true) {
			if m.From != who {
				continue
			}
			k := sentKey(m)
			// Two identical unsigned sends in one second are two letters, not
			// one: a letter has at most one copy in each pigeonhole.
			for n := 0; ; n++ {
				kn := fmt.Sprintf("%s\x00%d", k, n)
				clash := false
				for _, f := range groups[kn] {
					clash = clash || (f.c.Box == box && f.c.For == m.Pigeonhole)
				}
				if !clash || m.Sig != "" {
					k = kn
					break
				}
			}
			if _, ok := groups[k]; !ok {
				order = append(order, k)
			}
			groups[k] = append(groups[k], found{SentCopy{Box: box, For: m.Pigeonhole, ID: m.ID, State: m.State}, m})
		}
	}
	var out []*SentMessage
	for _, k := range order {
		fs := groups[k]
		// The copies of one letter say the same thing. Where they don't (a copy
		// planted with the same seal and a changed body or header), the ones
		// that agree are the letter, and the odd ones out are left out: they
		// can't stand in for it, nor make it read as forged. A tie goes to a
		// copy sitting where it says it was sent.
		count := map[string]int{}
		for _, f := range fs {
			if placedRight(f.m) {
				count[contentKey(f.m)]++
			}
		}
		// Copies that disagree: one that verifies is the letter, whatever
		// the others say (planting more copies wins nothing). Failing that,
		// the most copies sitting where they say they were sent.
		verified := map[string]bool{}
		if len(count) > 1 {
			for _, f := range fs {
				k := contentKey(f.m)
				if _, done := verified[k]; !done && placedRight(f.m) {
					c := *f.m
					verify(&c)
					verified[k] = c.Signature == sigVerified
				}
			}
		}
		best := ""
		bestRank := -1
		for _, f := range fs {
			k := contentKey(f.m)
			rank := count[k] * 2
			if verified[k] {
				rank += 1000
			}
			if placedRight(f.m) {
				rank++
			}
			if rank > bestRank {
				best, bestRank = k, rank
			}
		}
		s := &SentMessage{}
		for _, f := range fs {
			if contentKey(f.m) != best {
				continue
			}
			if s.Message == nil || (!placedRight(s.Message) && placedRight(f.m)) {
				s.Message = f.m
			}
			s.Copies = append(s.Copies, f.c)
		}
		out = append(out, s)
	}
	for _, s := range out {
		sort.Slice(s.Copies, func(i, j int) bool {
			if s.Copies[i].Box != s.Copies[j].Box {
				return s.Copies[i].Box < s.Copies[j].Box
			}
			return s.Copies[i].For < s.Copies[j].For
		})
		if s.Sig == "" {
			// Unsigned: it went wherever its copies are (and wherever it says,
			// from 0.4 on).
			for _, c := range s.Copies {
				if !contains(s.Recipients, c.Box) {
					s.Recipients = append(s.Recipients, c.Box)
				}
			}
		}
		s.Box = verifyAs(s)
	}
	sortSent(out)
	return out
}

// contentKey is everything a letter's copies have in common: what was
// written, never where a copy is.
func contentKey(m *Message) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00",
		m.From, m.Persona+"\x00"+m.ReplyBox, strings.Join(m.For, ","), strings.Join(m.Recipients, ","), m.Subject,
		strings.Join(m.Tags, ","), m.FollowUp, m.Thread, strings.Join(m.Attachments, ","),
		strings.Join(m.AttachmentSums, ","), m.SignedBy, m.SignedAt)
	h.Write([]byte(canonicalBody(m.Body)))
	return hex.EncodeToString(h.Sum(nil))
}

// placedRight says a copy sits where the letter says it was sent.
func placedRight(m *Message) bool {
	if len(m.Recipients) > 0 && !contains(m.Recipients, m.Box) {
		return false
	}
	if len(m.For) > 0 {
		return contains(m.For, m.Pigeonhole)
	}
	return m.Pigeonhole == ""
}

// verifyAs is the copy a sent message is checked as: one its signature names
// as a recipient, so a stray copy someone put in another box can't make the
// real message look forged.
func verifyAs(s *SentMessage) string {
	pick := s.Copies[0]
	for _, c := range s.Copies {
		if contains(s.Recipients, c.Box) && (len(s.For) == 0 && c.For == "" || contains(s.For, c.For)) {
			pick = c
			break
		}
	}
	s.Pigeonhole = pick.For
	return pick.Box
}

func sortSent(ss []*SentMessage) {
	sort.SliceStable(ss, func(i, j int) bool {
		if !ss[i].Sent.Equal(ss[j].Sent) {
			return ss[i].Sent.Before(ss[j].Sent)
		}
		// The same second: the order the store wrote them, as pull does.
		if a, b := modTime(ss[i].path), modTime(ss[j].path); !a.Equal(b) {
			return a.Before(b)
		}
		return ss[i].ID < ss[j].ID
	})
}

func cmdSent(args []string) error {
	const u = "sent [--to WHO]... [--tag T]... [--since 7d|DATE] [--json] [--box WHO]"
	f, err := parseFlags(args, map[string]bool{"to": true, "tag": true, "since": true, "json": false, "box": true, "raw": false})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) > 0 {
		return badf(u, "sent takes no words of its own (%q)", f.pos[0])
	}
	who, err := resolveBox("--box", f.get("box"))
	if err != nil {
		return err
	}
	ledgerOff = true
	fl, err := filterOf(f)
	if err != nil {
		return bad(u, err)
	}
	var to []string
	for _, raw := range f.all("to") {
		for _, w := range strings.Split(raw, ",") {
			b, err := resolveBox("--to", strings.TrimSpace(w))
			if err != nil {
				return bad(u, err)
			}
			to = append(to, b)
		}
	}

	var ss []*SentMessage
	if viaHost != "" {
		out, err := mirroredBytes(viaHost, "sent", append(append([]string{}, args...), "--json", "--raw"))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(out, &ss); err != nil {
			return fmt.Errorf("the store's answer wasn't a list of messages: %v", err)
		}
		for _, s := range ss {
			if s.Message == nil {
				return fmt.Errorf("the store's answer had an empty message")
			}
			if len(s.Copies) == 0 {
				return fmt.Errorf("the store's answer had a message with no copies")
			}
			// Checked here, against this machine's list.
			s.Box = verifyAs(s)
			prepare([]*Message{s.Message}, false, "")
		}
	} else {
		dir, err := storeDir()
		if err != nil {
			return err
		}
		for _, s := range gatherSent(dir, who) {
			if !fl.match(s.Message) {
				continue
			}
			if len(to) > 0 && !anyOf(s.Recipients, to) {
				continue
			}
			ss = append(ss, s)
		}
		for _, s := range ss {
			prepare([]*Message{s.Message}, f.has("raw"), dir)
		}
	}
	for _, s := range ss {
		s.State, s.To = "sent", strings.Join(s.Recipients, ",")
		if !f.has("raw") {
			s.Box = ""
		}
	}
	if f.has("json") {
		if ss == nil {
			ss = []*SentMessage{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(ss)
	}
	if len(ss) == 0 {
		what := "Nothing sent by " + who
		if len(to) > 0 {
			what += " to " + strings.Join(to, " or ")
		}
		if d := fl.describe(); d != "" {
			what += ", " + d
		}
		fmt.Println(what + ".")
		return nil
	}
	at := now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SENT\tSIG\tTO\tTAGS\tSUBJECT")
	for _, s := range ss {
		var to []string
		for _, c := range s.Copies {
			if c.For != "" {
				to = append(to, c.Box+"/"+c.For+" ("+c.State+")")
			} else {
				to = append(to, c.Box+" ("+c.State+")")
			}
		}
		tags := strings.Join(s.Tags, ",")
		if tags == "" {
			tags = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ago(s.Sent, at), sigMark(s.Message), strings.Join(to, ", "), cleanLine(tags, 60), cleanLine(s.Subject, 120))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, s := range ss {
		if s.Signature == sigBad {
			fmt.Printf("\n✗ %s: %s. Someone else may have written it in %s's name.\n", s.Copies[0].ID, s.SignatureNote, who)
		}
	}
	return nil
}

func anyOf(have, want []string) bool {
	for _, w := range want {
		if contains(have, w) {
			return true
		}
	}
	return false
}
