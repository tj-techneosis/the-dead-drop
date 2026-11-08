package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The Dead Drop keeps no clock. A follow-up is a one-off Crontinental job,
// booked at send time on the store's machine, that runs `deaddrop follow-up`:
// if the message is still unread it is raised to urgent, and if it was read
// in the meantime nothing happens. Marking the message read or archived
// cancels the job by its name.

func crontinental() (string, error) {
	if p := os.Getenv("DEADDROP_CRONTINENTAL"); p != "" {
		return p, nil
	}
	p, err := exec.LookPath("crontinental")
	if err != nil {
		return "", fmt.Errorf("crontinental isn't installed on this machine")
	}
	return p, nil
}

// followUpTime reads --follow-up: a span from now (2d, 90m) or a time.
func followUpTime(s string) (time.Time, error) {
	at := now()
	if d, ok := parseSpan(s); ok {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("--follow-up %q: give it some time", s)
		}
		return at.Add(d), nil
	}
	if t, ok := parseWhen(s); ok {
		// A time a moment past is a short follow-up worked out on the
		// sender's machine and sealed: it comes due now, not refused.
		if !t.After(at) && at.Sub(t) > signedSkew {
			return time.Time{}, fmt.Errorf("--follow-up %q is already past", s)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--follow-up %q: a span (2d, 4h, 90m) or a time (\"2026-11-04 09:00\", 2026-11-04T09:00:00Z)", s)
}

// followUpJob names a message's job: deaddrop-<when>-<its random part>.
// followUpJob names a copy's job: its id, and a short mark of the box and
// pigeonhole it's in, since ids are unique only within a box.
func followUpJob(m *Message) string {
	id := m.ID
	h := sha256.Sum256([]byte(m.Box + "/" + m.Pigeonhole))
	return "deaddrop-" + strings.ToLower(id[:17]) + "-" + id[len(id)-4:] + "-" + hex.EncodeToString(h[:2])
}

func bookFollowUp(dir string, m *Message) error {
	cron, err := crontinental()
	if err != nil {
		return err
	}
	at, err := time.Parse(time.RFC3339, m.FollowUp)
	if err != nil {
		return err
	}
	exe, err := stableBin()
	if err != nil {
		return err
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	secs := int64(math.Ceil(at.Sub(now()).Seconds()))
	if secs < 1 {
		secs = 1
	}
	args := []string{"in", fmt.Sprintf("%ds", secs), "--name", followUpJob(m),
		"--notify", "never", "--catchup", "run", "--by", "deaddrop",
		"--note", "Dead Drop follow-up: raise " + m.ID + " to urgent if " + m.To + " hasn't read it",
		"--", "env", "DEADDROP_DIR=" + dir, "DEADDROP_LOCAL=1", exe, "follow-up", m.To, m.ID}
	out, err := exec.Command(cron, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("crontinental: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// stableBin is the deaddrop a job should run: the installed one, whose path
// stays put across upgrades, not whichever binary booked it (a release's
// folder is replaced on upgrade, and a checkout build moves).
func stableBin() (string, error) {
	dir := os.Getenv("DEADDROP_BIN_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "bin")
	}
	p := filepath.Join(dir, "deaddrop")
	if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		return p, nil
	}
	if p, err := exec.LookPath("deaddrop"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs, nil
		}
	}
	return os.Executable()
}

// cancelFollowUp drops a message's job once it's been read. A job that has
// already run, or is gone, is no matter.
func cancelFollowUp(m *Message) {
	if m.FollowUp == "" {
		return
	}
	if at, err := time.Parse(time.RFC3339, m.FollowUp); err != nil || !at.After(now()) {
		return
	}
	cron, err := crontinental()
	if err != nil {
		return
	}
	exec.Command(cron, "rm", followUpJob(m), "--force").Run()
	// Booked by 0.6.3 or earlier, under the name it had then.
	if len(m.ID) > 17 {
		exec.Command(cron, "rm", "deaddrop-"+strings.ToLower(m.ID[:17])+"-"+m.ID[len(m.ID)-4:], "--force").Run()
	}
}

// cmdFollowUp is what the job runs: deaddrop follow-up BOX ID.
func cmdFollowUp(args []string) error {
	const u = "follow-up BOX ID (what a --follow-up job runs)"
	f, err := parseFlags(args, map[string]bool{})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) != 2 {
		return badf(u, "a box and a message id")
	}
	box, err := resolveBox("box", f.pos[0])
	if err != nil {
		return err
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, box)); os.IsNotExist(err) {
		fmt.Printf("there's no box %s; nothing to follow up\n", box)
		return nil
	}
	unlock, err := lockBox(dir, box)
	if err != nil {
		return err
	}
	defer unlock()
	m, err := findAnywhere(dir, box, f.pos[1], true)
	if err != nil {
		fmt.Println(err.Error() + "; nothing to follow up")
		return nil
	}
	switch {
	case m.State != "unread":
		fmt.Printf("%s has been %s; nothing to do\n", m.ID, m.State)
		return nil
	case m.Urgency == urgent:
		fmt.Printf("%s is unread and already urgent\n", m.ID)
		return nil
	}
	m.UrgencyWas, m.Urgency, m.Raised = m.Urgency, urgent, now().Format(time.RFC3339)
	if err := writeAtomic(m.path, m.render(), 0o600); err != nil {
		return err
	}
	fmt.Printf("%s is still unread: raised to urgent\n", m.ID)
	// One ring per letter: a letter for several helpers has a copy (and a
	// follow-up) each, and rings for whichever is raised first.
	fp, already := letterOf(m), false
	for _, other := range loadAll(dir, box, []string{"unread", "read", "archived"}, true) {
		if other.ID != m.ID && other.Raised != "" && letterOf(other) == fp {
			already = true
		}
	}
	if !already {
		ring(box, m, true)
	}
	return nil
}

// ring tells the store machine's notify hooks that urgent mail is waiting,
// when the config asks for it. It says only which box, unless the box is in
// board_detail: then who sent the message and its subject too.
func ring(box string, m *Message, raised bool) {
	c, err := loadConfig()
	if err != nil || !c.RingUrgent {
		return
	}
	cron, err := crontinental()
	if err != nil {
		ringTrouble(m.ID, box, "ring_urgent is on, but "+err.Error())
		return
	}
	// Counted only once there's something to ring with, and given back if
	// the ring fails: a ring that never went out doesn't spend the cap.
	// A raised follow-up is the store's own job ringing, not the sender:
	// it counts in a bucket of its own (still per sender, so one sender's
	// follow-ups never hold another's), and nothing a sender is told
	// depends on it. Else whether a send was held would say whether a
	// follow-up rang, so whether its letter was read.
	bucket := m.From
	if raised {
		bucket = strings.ToLower(m.From) + "+follow-up"
	}
	ok, why, first, refund := ringAllowed(bucket, box)
	if !ok {
		// The letter is still urgent and on the board: only the ring is held.
		// A gated key isn't shown the count: it's the store's book, and
		// what a key is told stays to what it did (held, or not). Its own
		// sends are all a send's count holds; follow-ups have a bucket
		// of their own, so whether it's held never says a box's read state.
		if gateRead != nil {
			fmt.Fprintf(os.Stderr, "deaddrop: %s is urgent and waiting, but not rung: past this sender's ring cap\n", m.ID)
		} else {
			fmt.Fprintf(os.Stderr, "deaddrop: %s is urgent and waiting, but not rung: %s\n", m.ID, why)
		}
		if first {
			gateLog("rings held: %s to %s: %s (the first this hour; more aren't noted)", m.ID, box, why)
		}
		return
	}
	text := box + ": urgent mail waiting"
	if detailed(box) {
		text = summaryLine(box+": urgent from", m)
	}
	// Tagged with the message, so a viewer can fold the ring into it.
	a := []string{"notify", "--urgent", "--tag", "drop=" + m.ID, "--tag", "deaddrop_box=" + box}
	if m.Pigeonhole != "" {
		a = append(a, "--tag", "deaddrop_for="+m.Pigeonhole) // routed like the pigeonhole's own ring
	}
	if out, err := exec.Command(cron, append(a, "--title", "Dead Drop", text)...).CombinedOutput(); err != nil {
		// Given back: nothing went out. That rests on Crontinental's notify
		// (0.11) running every hook and exiting 0 whatever they did, and
		// failing only before any hook runs. A Crontinental that reports a
		// hook's failure in its exit code would make this refund a ring that
		// went out: keep the count then.
		refund()
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		ringTrouble(m.ID, box, "could not ring: "+detail)
	}
}

// ringTrouble says why a ring didn't go: to whoever's there, unless that's a
// gated key, which gets a word and no more (notify's output is the store
// machine's own business), the detail going to gate.log for the owner.
func ringTrouble(id, box, detail string) {
	if gateRead != nil {
		gateLog("%s to %s: %s", id, box, detail)
		fmt.Fprintf(os.Stderr, "deaddrop: %s to %s: the store couldn't ring (its owner can see why)\n", id, box)
		return
	}
	fmt.Fprintln(os.Stderr, "deaddrop: "+detail)
}

// Rings are capped per sender, so no one sender (a gated key's above all)
// can page whoever reads the store's notify hooks without end: so many an
// hour to one box, and so many in all. The in-all cap is a send's most
// copies, so one urgent letter to everyone still rings everyone. Not
// automation: only the store login's own jobs send as it (a gated key
// can't), and one job's urgent mail mustn't hold another's ring.
const (
	ringsPerBox   = 4
	ringsInAll    = maxCopies
	ringsInterval = time.Hour
)

// ringBook is rings.json: per sender, "box time" for each ring in the last
// interval, and for each box a held ring was noted for, when.
type ringBook struct {
	Rung map[string][]string `json:"rung"`
	Held map[string][]string `json:"held,omitempty"`
}

// ringAllowed counts a ring against the sender's caps and says whether it
// may ring; refund takes the count back. first: a held ring is the first
// held to that box this interval (so gate.log gets one line, not one a
// letter). Kept in rings.json at the store's top, under its own lock. A
// count that can't be kept rings anyway: the cap is a guard, and urgent
// mail failing to ring is worse than one ring too many.
func ringAllowed(from, box string) (ok bool, why string, first bool, refund func()) {
	refund = func() {}
	from = strings.ToLower(from)
	if from == automation || from == automation+"+follow-up" {
		return true, "", false, refund
	}
	dir, err := storeDir()
	if err != nil {
		return true, "", false, refund
	}
	unlock, err := lockFile(filepath.Join(dir, ".rings.lock"))
	if err != nil {
		return true, "", false, refund
	}
	defer unlock()
	path := filepath.Join(dir, "rings.json")
	book := readRingBook(path)
	t := now()
	live := func(m map[string][]string) map[string][]string {
		out := map[string][]string{}
		for who, rs := range m {
			for _, r := range rs {
				_, at, ok := strings.Cut(r, " ")
				when, err := time.Parse(time.RFC3339, at)
				if ok && err == nil && when.After(t.Add(-ringsInterval)) && !when.After(t.Add(time.Minute)) {
					out[who] = append(out[who], r)
				}
			}
		}
		return out
	}
	book.Rung, book.Held = live(book.Rung), live(book.Held)
	all, toBox := len(book.Rung[from]), 0
	for _, r := range book.Rung[from] {
		if b, _, _ := strings.Cut(r, " "); b == box {
			toBox++
		}
	}
	stamp := box + " " + t.UTC().Format(time.RFC3339)
	if toBox >= ringsPerBox || all >= ringsInAll {
		// Noted once an hour per box held by its own cap, and once in all
		// for the in-all cap (whatever boxes it's sent to after), so the
		// held book stays as small as the rung one.
		why, held := fmt.Sprintf("%s has rung %s %d times in the last hour", from, box, toBox), box
		if toBox < ringsPerBox {
			why, held = fmt.Sprintf("%s has rung %d times in the last hour", from, all), "*"
		}
		first = true
		for _, r := range book.Held[from] {
			if b, _, _ := strings.Cut(r, " "); b == held {
				first = false
			}
		}
		if first {
			book.Held[from] = append(book.Held[from], held+" "+t.UTC().Format(time.RFC3339))
			// A note that can't be kept isn't noted: else every held ring
			// would be "the first".
			first = writeJSONFile(path, &book) == nil
		}
		return false, why, first, refund
	}
	book.Rung[from] = append(book.Rung[from], stamp)
	if writeJSONFile(path, &book) != nil {
		return true, "", false, refund
	}
	refund = func() {
		u, err := lockFile(filepath.Join(dir, ".rings.lock"))
		if err != nil {
			return
		}
		defer u()
		b := readRingBook(path)
		rs := b.Rung[from]
		for i := len(rs) - 1; i >= 0; i-- {
			if rs[i] == stamp {
				b.Rung[from] = append(rs[:i:i], rs[i+1:]...)
				writeJSONFile(path, &b)
				return
			}
		}
	}
	return true, "", false, refund
}

// readRingBook reads rings.json, in either shape: {"rung": …, "held": …}, or
// an early 0.7.10 build's flat sender map, read as rung, so the counts
// survive the change. Unreadable is empty: the cap is a guard.
func readRingBook(path string) ringBook {
	book := ringBook{Rung: map[string][]string{}, Held: map[string][]string{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return book
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) != nil {
		return book
	}
	// The new shape's "rung" is an object; the flat one's values are lists
	// (a sender named rung included).
	if r, ok := top["rung"]; ok && len(bytes.TrimSpace(r)) > 0 && bytes.TrimSpace(r)[0] == '{' {
		json.Unmarshal(b, &book)
	} else {
		json.Unmarshal(b, &book.Rung)
	}
	if book.Rung == nil {
		book.Rung = map[string][]string{}
	}
	if book.Held == nil {
		book.Held = map[string][]string{}
	}
	return book
}
