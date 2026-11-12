package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
// Every lookout is tagged mail and owner-<box>: the box's owner is who acts.
func cmdFiretower(args []string) error {
	const u = "firetower [--next 6m|TIME|none] [--box WHO]... [--smoke-after 30m] [--fire-after 3h] [--ring] [--pigeonholes]"
	f, err := parseFlags(args, map[string]bool{"next": true, "box": true, "smoke-after": true, "fire-after": true, "ring": false, "pigeonholes": false})
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
			if !contains(list, b) {
				list = append(list, b)
			}
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
	// --ring: a box that's just gone to fire rings this machine's notify
	// hooks once (Crontinental), tagged with the box so they can route it.
	wasOnFire := map[string]bool{}
	onFire := map[string]bool{}
	firstRing := false
	ringUrgent := false
	if f.has("ring") {
		// One run at a time decides who rings: the state lock is held from
		// reading the fire list to writing it.
		unlock, err := lockState()
		if err != nil {
			return err
		}
		defer unlock()
		if wasOnFire, firstRing, err = readFireList(); err != nil {
			return err
		}
		if c, err := loadConfig(); err == nil {
			ringUrgent = c.RingUrgent
		}
	}
	// Each box's own mail, and with --pigeonholes each helper's pigeonhole
	// in it too, as a lookout of its own: mail/<box>/inbox, mail/<box>/for/<h>.
	type slot struct{ box, helper string }
	var slots []slot
	for _, box := range list {
		slots = append(slots, slot{box, ""})
		if f.has("pigeonholes") {
			for _, h := range helpers(dir, box) {
				slots = append(slots, slot{box, h})
			}
		}
	}
	visited := map[string]bool{}
	for _, sl := range slots {
		box := sl.box
		name, label := "mail/"+box+"/inbox", "Mail: "+box
		mark := box // the fire list's key
		if sl.helper != "" {
			name, label, mark = "mail/"+box+"/for/"+sl.helper, "Mail: "+box+" (for "+sl.helper+")", box+"/for/"+sl.helper
		}
		visited[mark] = true
		s := slotStatus(dir, box, sl.helper)
		status, msg, value := "green", "No mail.", "none"
		redByAge := false
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
			unheard, waited := unheardWait(dir, box, sl.helper, false)
			switch {
			case s.Urgent > 0:
				// Urgent is fire either way; the line still says how
				// long the rest has waited.
			case unheard == 0, smokeAfter > 0 && waited < smokeAfter:
				status = "green"
			case fireAfter > 0 && waited >= fireAfter:
				status = "red"
			}
			// Red by age from the letters that aren't urgent: an old urgent
			// letter is ring_urgent's, already rung.
			if fireAfter > 0 {
				if n, w := unheardWait(dir, box, sl.helper, true); n > 0 && w >= fireAfter {
					redByAge = true
				}
			}
			if unheard > 0 {
				msg += fmt.Sprintf(", %d unheard, the oldest %s", unheard, spanWords(waited))
			}
		}
		if s.Newest != nil {
			value = boardValue(s.Newest.ID)
			if detailed(box) && sl.helper == "" {
				msg = whatsWaiting(msg, dir, box, s)
			}
		}
		a := []string{"set", name, status, msg, "--value", value, "--label", label, "--tag", "mail", "--tag", "owner-" + box}
		if next != "" {
			a = append(a, "--next", next)
		}
		out, err := exec.Command(tower, a...).CombinedOutput()
		if err != nil {
			failed = append(failed, mark+": "+strings.TrimSpace(string(out)))
			if wasOnFire[mark] || (firstRing && status == "red") {
				// Not reported, so not known to be out; and on the first run a
				// fire is noted whether or not the board took it.
				onFire[mark] = true
			}
			continue
		}
		if status == "red" {
			switch {
			case !f.has("ring"), wasOnFire[mark]:
				onFire[mark] = true
			case firstRing:
				// The first --ring run on this machine: what's on fire now is
				// noted, not rung (it may have been burning for days).
				onFire[mark] = true
			case ringUrgent && !redByAge:
				// Red only because of urgent mail, which ring_urgent has
				// already rung as it arrived: not again.
				onFire[mark] = true
			default:
				if err := ringFire(box, sl.helper, msg); err != nil {
					failed = append(failed, mark+": could not ring: "+err.Error())
				} else {
					onFire[mark] = true // marked only once it's really rung
				}
			}
		}
		fmt.Printf("%s  %s  %s\n", name, status, msg)
	}
	if f.has("ring") {
		// Boxes not reported this run because --box named others keep their
		// mark. Without --box every box was reported, so a mark for a box
		// that's gone goes with it (a new box of that name rings afresh).
		// A mark for a slot this run didn't look at (a box --box left out,
		// a pigeonhole when this run had no --pigeonholes) stays, as long as
		// that box or pigeonhole is still there; one that's gone goes, so a
		// new one of that name rings afresh.
		for b := range wasOnFire {
			if visited[b] {
				continue
			}
			owner, helper, _ := strings.Cut(b, "/for/")
			if _, err := os.Stat(slotDir(dir, owner, helper)); err == nil {
				onFire[b] = true
			}
		}
		if err := writeFireList(onFire); err != nil {
			return fmt.Errorf("could not keep the list of boxes on fire (%s): %v", fireListPath(), err)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not report %s", strings.Join(failed, "; "))
	}
	if len(list) == 0 {
		fmt.Println("No boxes yet: nothing to report.")
	}
	return nil
}

// slotStatus is a box's own counts (statusOf), or one pigeonhole's.
func slotStatus(dir, box, helper string) boxStatus {
	if helper == "" {
		return statusOf(dir, box)
	}
	s := boxStatus{Box: box}
	unread := loadSlot(dir, box, helper, []string{"unread"}, false)
	s.Unread = len(unread)
	for _, m := range unread {
		if m.Urgency == urgent {
			s.Urgent++
		}
	}
	s.Newest = newest(unread)
	return s
}

// The boxes on fire at the last --ring run, so each rings once per fire.
func fireListPath() string { return filepath.Join(stateDir(), "firetower-fire.json") }

// readFireList is the boxes on fire at the last --ring run. first: there's
// no list yet, so this run notes what's on fire and rings nothing. A list
// that can't be read is an error, not an empty list (that would ring every
// fire again).
func readFireList() (map[string]bool, bool, error) {
	out := map[string]bool{}
	b, err := os.ReadFile(fireListPath())
	if os.IsNotExist(err) {
		return out, true, nil
	}
	var boxes []string
	if err == nil {
		err = json.Unmarshal(b, &boxes)
	}
	if err != nil {
		return nil, false, fmt.Errorf("the list of boxes on fire (%s) can't be read: %v; remove it to start again (the next run notes what's on fire and rings nothing)", fireListPath(), err)
	}
	for _, b := range boxes {
		out[b] = true
	}
	return out, false, nil
}

func writeFireList(on map[string]bool) error {
	boxes := []string{}
	for b := range on {
		boxes = append(boxes, b)
	}
	sort.Strings(boxes)
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	return writeJSONFile(fireListPath(), boxes)
}

// ringFire rings for a box that's gone to fire: the box and the board's own
// line (counts, unless the box is in board_detail), tagged deaddrop_box=<box>.
func ringFire(box, helper, line string) error {
	cron, err := crontinental()
	if err != nil {
		return err
	}
	a := []string{"notify", "--urgent", "--tag", "deaddrop_box=" + box, "--title", "Dead Drop: " + box}
	if helper != "" {
		a = []string{"notify", "--urgent", "--tag", "deaddrop_box=" + box, "--tag", "deaddrop_for=" + helper, "--title", "Dead Drop: " + box + " (for " + helper + ")"}
	}
	if out, err := exec.Command(cron, append(a, line)...).CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return err
	}
	return nil
}

// unheardWait counts the box's unread letters that arrived after it was last
// read (heardAt), and how long the oldest has waited. A letter arrived at
// the earlier of the store's time for its file and the date it gives
// itself: a letter can make itself look older (louder on the board), never
// fresher (hidden).
func unheardWait(dir, box, helper string, skipUrgent bool) (int, time.Duration) {
	at, heard := now(), heardAt(dir, box, helper)
	n := 0
	var most time.Duration
	for _, m := range loadSlot(dir, box, helper, []string{"unread"}, false) {
		if skipUrgent && m.Urgency == urgent {
			continue
		}
		arrived := m.Sent
		if info, err := os.Stat(m.path); err == nil {
			mt := info.ModTime()
			// No date, a date later than the file, or one from before this
			// tool existed (1970 would read as a wait of centuries): the
			// store's own time for the file.
			if arrived.IsZero() || mt.Before(arrived) || arrived.Year() < 2000 {
				arrived = mt
			}
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
