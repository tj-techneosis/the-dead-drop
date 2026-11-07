package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// cmdFiretower reports one lookout per box, mail/<who>/inbox, to the
// Firetower on this machine: clear when empty, smoke when there's unread
// mail, fire when some of it is urgent. The value stands for the newest
// unread message, so the board says "moved" when mail comes in. By default
// the message is only a count: boards can be on screens anyone sees, so no
// subject and no sender (and so the value is a hash of the id, which names its
// sender). A box named in board_detail also gets what's waiting (whatsWaiting).
func cmdFiretower(args []string) error {
	const u = "firetower [--next 6m|TIME|none] [--box WHO]... [--smoke-after 30m] [--fire-after 3h]"
	f, err := parseFlags(args, map[string]bool{"next": true, "box": true, "smoke-after": true, "fire-after": true})
	if err != nil {
		return bad(u, err)
	}
	// Optional ages: unread mail is clear until its oldest letter has waited
	// --smoke-after, and fire once it has waited --fire-after. Without them,
	// any unread is smoke, as before. Urgent mail is fire either way.
	var smokeAfter, fireAfter time.Duration
	for _, a := range []struct {
		flag string
		to   *time.Duration
	}{{"smoke-after", &smokeAfter}, {"fire-after", &fireAfter}} {
		if f.has(a.flag) {
			d, ok := parseSpan(f.get(a.flag))
			if !ok || d <= 0 {
				return badf(u, "--%s %s: a span, like 30m or 3h", a.flag, f.get(a.flag))
			}
			*a.to = d
		}
	}
	if smokeAfter > 0 && fireAfter > 0 && fireAfter <= smokeAfter {
		return badf(u, "--fire-after must be longer than --smoke-after")
	}
	if len(f.pos) > 0 {
		return badf(u, "firetower takes no words of its own")
	}
	tower := os.Getenv("DEADDROP_FIRETOWER")
	if tower == "" {
		if tower, err = exec.LookPath("firetower"); err != nil {
			return fmt.Errorf("firetower isn't installed on this machine")
		}
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	list := boxes(dir)
	if f.has("box") {
		list = nil
		for _, b := range f.all("box") {
			b, err := resolveBox("--box", b)
			if err != nil {
				return bad(u, err)
			}
			list = append(list, b)
		}
	}
	next := f.get("next")
	if next == "" {
		// Under Crontinental, the next run's time plus a little for the run.
		if t, err := time.Parse(time.RFC3339, os.Getenv("CRONTINENTAL_NEXT")); err == nil {
			next = t.Add(5 * time.Minute).UTC().Format(time.RFC3339)
		}
	}
	var failed []string
	for _, box := range list {
		s := statusOf(dir, box)
		status, msg, value := "green", "No mail.", "none"
		switch {
		case s.Urgent > 0:
			status, msg = "red", fmt.Sprintf("%d urgent", s.Urgent)
			if s.Unread > s.Urgent {
				msg += fmt.Sprintf(", %d unread", s.Unread)
			}
		case s.Unread > 0:
			status, msg = "yellow", fmt.Sprintf("%d unread", s.Unread)
		}
		if s.Unread > 0 && (smokeAfter > 0 || fireAfter > 0) {
			// By what's unheard: mail that's arrived since anyone last read
			// the box. Readers who pull and don't mark stay clear; a box
			// nobody reads goes to fire on its own.
			unheard, waited := unheardWait(dir, box)
			switch {
			case s.Urgent > 0:
				// Urgent is fire either way; the line still says how
				// long the rest has waited.
			case unheard == 0, smokeAfter > 0 && waited < smokeAfter:
				status = "green"
			case fireAfter > 0 && waited >= fireAfter:
				status = "red"
			}
			if unheard > 0 {
				msg += fmt.Sprintf(", %d unheard, the oldest %s", unheard, spanWords(waited))
			}
		}
		if s.Newest != nil {
			value = boardValue(s.Newest.ID)
			if detailed(box) {
				msg = whatsWaiting(msg, dir, box, s)
			}
		}
		a := []string{"set", "mail/" + box + "/inbox", status, msg, "--value", value, "--label", "Mail: " + box, "--tag", "mail"}
		if next != "" {
			a = append(a, "--next", next)
		}
		out, err := exec.Command(tower, a...).CombinedOutput()
		if err != nil {
			failed = append(failed, box+": "+strings.TrimSpace(string(out)))
			continue
		}
		fmt.Printf("mail/%s/inbox  %s  %s\n", box, status, msg)
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not report %s", strings.Join(failed, "; "))
	}
	if len(list) == 0 {
		fmt.Println("No boxes yet: nothing to report.")
	}
	return nil
}

// unheardWait counts the box's unread letters that arrived after it was last
// read (heardAt), and how long the oldest has waited. A letter arrived at
// the earlier of the store's time for its file and the date it gives
// itself: a letter can make itself look older (louder on the board), never
// fresher (hidden).
func unheardWait(dir, box string) (int, time.Duration) {
	at, heard := now(), heardAt(dir, box, "")
	n := 0
	var most time.Duration
	for _, m := range loadBox(dir, box, []string{"unread"}, false) {
		arrived := m.Sent
		if info, err := os.Stat(m.path); err == nil && (arrived.IsZero() || info.ModTime().Before(arrived)) {
			arrived = info.ModTime()
		}
		if !arrived.After(heard) {
			continue
		}
		n++
		if w := at.Sub(arrived); w > most {
			most = w
		}
	}
	return n, most
}

// spanWords is a wait as a board says it: 12m, 3h, 2d.
func spanWords(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// lineMax keeps a board line inside the Firetower's cut (200 characters), so
// it's shortened here, cleanly, and not cut off there.
const lineMax = 200

// whatsWaiting adds the message that matters most to the counts: the newest
// urgent one when there is any, else the newest unread. One line:
//
//	2 unread · newest from bob, 5m ago: Requests for tool X
//	1 urgent, 3 unread · urgent from bob, 2h ago: The build is down
func whatsWaiting(counts, dir, box string, s boxStatus) string {
	m, which := s.Newest, "newest from"
	if s.Urgent > 0 {
		var urgentOnes []*Message
		for _, u := range loadBox(dir, box, []string{"unread"}, false) {
			if u.Urgency == urgent {
				urgentOnes = append(urgentOnes, u)
			}
		}
		if u := newest(urgentOnes); u != nil {
			m, which = u, "urgent from"
		}
	} else if s.Unread == 1 {
		which = "from"
	}
	return summaryLine(counts+" · "+which, m)
}

// summaryLine is "<lead> <sender>, <age>: <subject>", the subject shortened
// to keep the whole line within lineMax.
func summaryLine(lead string, m *Message) string {
	head := cleanLine(fmt.Sprintf("%s %s, %s: ", lead, m.From, ago(m.Sent, now())), lineMax) + " "
	room := lineMax - len([]rune(head))
	if room < 2 {
		return strings.TrimSpace(head)
	}
	subject := cleanLine(m.Subject, room)
	if full := cleanLine(m.Subject, len([]rune(m.Subject))+1); subject != full {
		// Shortened: end on a whole word when there is one to end on.
		cut := strings.TrimSuffix(subject, "…")
		if i := strings.LastIndex(cut, " "); i > 0 && !strings.HasPrefix(full[len(cut):], " ") {
			cut = cut[:i]
		}
		subject = strings.TrimRight(cut, " ,;:.-") + "…"
	}
	if subject == "" {
		subject = "(no subject)"
	}
	return head + subject
}

// boardValue stands for a message on the board without saying who sent it.
func boardValue(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:6])
}
