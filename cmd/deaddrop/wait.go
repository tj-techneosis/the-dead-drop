package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// `deaddrop wait`: block until a box has unread mail this machine's waits
// haven't reported yet, then print a line per letter (never its text) and
// exit. It's for a script or a session that should do nothing until mail
// comes: started in the background, it ends when there's something to read.
//
// What's been reported is kept per box and pigeonhole
// (waited@<box>[@<helper>].json in the state folder, named by the box even
// for your own, so two names sharing a state folder never share a file),
// under a lock of its own, never seeded from what's unread at the start: a letter that lands
// between one wait ending and the next starting is still reported, and one
// already reported and left unread isn't reported again (--again reports
// everything unread). A letter that's read and later marked unread again is
// new again.
//
// It isn't a timer. With the store here it waits on the pigeonhole's folder
// (watchSlot), armed before each look so nothing lands between the look and
// the wait unseen, and looks again every --every in case an event was
// missed. With the store elsewhere it holds one ssh session to the store's
// `deaddrop changes` (changes.go), which says "change" and nothing about
// what; the look is the usual inbox, checked here. --every is then the
// store's heartbeat (which rewrites --alive) and the pause before
// reconnecting a session that dropped soon after it opened; a reconnect
// looks again once the session is ready. A store that can't say changes
// (an older deaddrop) is looked at every --every, said once.
//
// --alive FILE is rewritten with "BOX" (or "BOX HELPER") after every look at
// the store that worked, and on every "alive" from the store's changes
// session, so something else (a hook) can tell a wait is
// running, and on which box, by the file's age. A look that fails leaves it
// alone: a wait that can't reach the store goes stale, as it should.
//
// It retries only what can clear by itself: a store it can't reach (ssh's
// 255, which is also what a key the store's machine turns away looks like)
// and an answer it can't read, saying so once per new error; a hook
// watching --alive sees a wait that retries for good go stale. Anything else
// ends it with the error and its code (4 for no such box, 5 for a refusal,
// 1 for the rest, an older store's refusals among them): waiting on what
// won't change would never end.

type waitedFile struct {
	Box      string   `json:"box"`
	For      string   `json:"for,omitempty"`
	Reported []string `json:"reported"`
	// SavedAt is when a wait last saved it (this machine's clock): a look
	// whose fetch began before then mustn't drop anything from it, since
	// another wait saw a newer store.
	SavedAt time.Time `json:"saved_at"`
}

func waitedPath(box string) string {
	name := "waited@" + box
	if watchFor != "" {
		name += "@" + watchFor
	}
	return filepath.Join(stateDir(), name+".json")
}

// lockWaited holds the box's wait file for one look's read, print and save,
// so two waits on one box never both report a letter.
func lockWaited(box string) (func(), error) {
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(waitedPath(box)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func cmdWait(args []string) error {
	const u = "wait [--box WHO] [--for HELPER] [--every 1m] [--alive FILE] [--again] [--json]"
	f, err := parseFlags(args, map[string]bool{"box": true, "for": true, "every": true, "alive": true, "again": false, "json": false})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) > 0 {
		return badf(u, "wait takes no words of its own")
	}
	if err := setWatchFor(f); err != nil {
		return bad(u, err)
	}
	box := watchBox
	if box == "" {
		if box, err = whoAmI(); err != nil {
			return err
		}
	}
	every := time.Minute
	if f.has("every") {
		var ok bool
		if every, ok = parseSpan(f.get("every")); !ok {
			return badf(u, "--every %s: a span, like 30s, 1m or 5m", f.get("every"))
		}
		if every < minEvery {
			return badf(u, "--every %s: not more often than every %s", f.get("every"), minEvery)
		}
	}
	// --alive's folder is checked now, not after the first look, so a path
	// that can't be written fails at the start rather than waking whoever
	// waits as if mail had come.
	alive := f.get("alive")
	if alive != "" {
		// The nearest folder that's there: writeAtomic makes the rest.
		dir := filepath.Dir(alive)
		for {
			if _, err := os.Stat(dir); err == nil || filepath.Dir(dir) == dir {
				break
			}
			dir = filepath.Dir(dir)
		}
		t, err := os.CreateTemp(dir, ".deaddrop-alive-*")
		if err != nil {
			return badf(u, "--alive %s: can't write there (%v)", alive, err)
		}
		t.Close()
		os.Remove(t.Name())
	}
	// It looks at the store itself, never a kept copy.
	os.Unsetenv("DEADDROP_CACHED")
	fetch := []string{"inbox", box, "--json"}
	if watchFor != "" {
		fetch = append(fetch, "--for", watchFor)
	}
	host, err := remoteHost()
	if err != nil {
		return err
	}
	dir := ""
	if host == "" {
		if dir, err = storeDir(); err != nil {
			return err
		}
	}
	// The inner inbox's codes that mean something here come back as they are.
	keeps := map[int]bool{exitNoBox: true, exitRefused: true, exitUnreachable: true, 2: true}
	lastErr, aliveErr := "", ""
	say := func(err error) {
		if err.Error() != lastErr {
			fmt.Fprintf(os.Stderr, "%s deaddrop: %v (still waiting)\n", time.Now().Format("15:04:05"), err)
			lastErr = err.Error()
		}
	}
	touchAlive := func() {
		if alive == "" {
			return
		}
		line := box
		if watchFor != "" {
			line += " " + watchFor
		}
		// A write that fails is said once, and the file goes stale:
		// that's what it's for, and it isn't mail.
		if err := writeAtomic(alive, []byte(line+"\n"), 0o600); err != nil && err.Error() != aliveErr {
			fmt.Fprintf(os.Stderr, "%s deaddrop: --alive %s: %v (still waiting)\n", time.Now().Format("15:04:05"), alive, err)
			aliveErr = err.Error()
		} else if err == nil {
			aliveErr = ""
		}
	}
	// With the store elsewhere, one ssh session stays open to it and says
	// when the pigeonhole changes (feed); a store that can't (an older
	// deaddrop, or one whose gate doesn't know changes) is looked at every
	// --every instead (polling, said once after a look that worked).
	var feed *changeFeed
	polling, pollSaid := false, false
	defer func() {
		if feed != nil {
			feed.stop()
		}
	}()
	for {
		var w *dirWatch // armed before the look, so nothing lands unseen
		switch {
		case host == "":
			w = watchSlot(dir, slotDir(dir, box, watchFor))
		case feed == nil && !polling:
			f, err := startFeed(host, box, watchFor, every)
			if ee, ok := err.(exitError); ok && ee.code == exitUnreachable {
				say(err)
				time.Sleep(every)
				continue
			}
			if err != nil {
				polling = true
			} else {
				feed = f
			}
		}
		pause := func() {
			switch {
			case w != nil:
				w.wait(every)
				w.close()
			case feed != nil:
				if !feed.await(every, touchAlive) {
					// Dropped: the next look starts a new session (after
					// --every, if this one didn't last that long), and
					// looks again once it's open, so nothing is lost.
					say(feed.dropped(host))
					short := time.Since(feed.began) < every
					feed.stop()
					feed = nil
					if short {
						time.Sleep(every)
					}
				}
			default:
				time.Sleep(every)
			}
		}
		started := time.Now()
		out, err := selfKeeping(keeps, fetch...)
		if ee, ok := err.(exitError); err != nil && (!ok || ee.code != exitUnreachable) {
			if w != nil {
				w.close()
			}
			return err // waiting won't change it
		}
		var ms []*Message
		if err == nil {
			if jerr := json.Unmarshal(out, &ms); jerr != nil {
				err = fmt.Errorf("the store's answer wasn't a list of messages: %v", jerr)
			}
		}
		if err != nil {
			say(err)
			if w != nil {
				w.close()
			}
			time.Sleep(every)
			continue
		}
		lastErr = ""
		touchAlive()
		if polling && !pollSaid {
			fmt.Fprintf(os.Stderr, "%s deaddrop: the store can't say when mail comes (an older deaddrop there?): looking every %s\n", time.Now().Format("15:04:05"), every)
			pollSaid = true
		}
		unlock, err := lockWaited(box)
		if err != nil {
			if w != nil {
				w.close()
			}
			return fmt.Errorf("the wait's state: %v", err)
		}
		reported := map[string]bool{}
		newer := false // saved by a wait that looked after this one's fetch began
		if prev := readWaited(box); prev != nil && prev.Box == box && prev.For == watchFor && !f.has("again") {
			for _, id := range prev.Reported {
				reported[id] = true
			}
			newer = prev.SavedAt.After(started) && prev.SavedAt.Before(time.Now().Add(time.Minute))
		}
		var fresh []*Message
		unread := map[string]bool{}
		for _, m := range ms {
			if m.State != "unread" {
				continue
			}
			unread[m.ID] = true
			if !reported[m.ID] {
				fresh = append(fresh, m)
			}
		}
		// Kept: only what's still unread, so a letter read and then marked
		// unread again is new again; but never pruned by a fetch older than
		// the state, which can't know of letters that came since.
		keep := []string{}
		for id := range reported {
			if unread[id] || newer {
				keep = append(keep, id)
			}
		}
		if len(fresh) == 0 {
			writeWaited(box, keep) // only pruning: nothing lost if it fails
			unlock()
			pause()
			continue
		}
		if w != nil {
			w.close()
		}
		// Said first, kept after: a wait killed in between reports them
		// again, never loses them.
		if f.has("json") {
			for _, m := range fresh {
				m.Body = ""
			}
			if err := printJSON(fresh); err != nil {
				unlock()
				return err
			}
		} else {
			for _, m := range fresh {
				fmt.Println(waitLine(m))
			}
		}
		for _, m := range fresh {
			keep = append(keep, m.ID)
		}
		err = writeWaited(box, keep)
		unlock()
		if err != nil {
			fmt.Fprintf(os.Stderr, "deaddrop: couldn't keep what this wait reported (%v): the next wait may report these again\n", err)
		}
		return nil
	}
}

// waitLine is one letter on one line: who, how it's signed, its tags and
// subject. Never its text.
func waitLine(m *Message) string {
	tags := strings.Join(m.Tags, ",")
	if tags == "" {
		tags = "-"
	}
	sig := m.Signature
	if sig == "" {
		sig = "unsigned"
	}
	where := ""
	if m.Pigeonhole != "" {
		where = " (for " + m.Pigeonhole + ")"
	}
	return fmt.Sprintf("%s%s  %s  from %s  [%s]  [%s]  %s", m.ID, where, m.Urgency, cleanLine(m.sender(), 64), sig, cleanLine(tags, 120), cleanLine(m.Subject, 160))
}

func readWaited(box string) *waitedFile {
	b, err := os.ReadFile(waitedPath(box))
	if err != nil {
		return nil
	}
	var w waitedFile
	if json.Unmarshal(b, &w) != nil {
		return nil
	}
	return &w
}

func writeWaited(box string, ids []string) error {
	sort.Strings(ids)
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	return writeJSONFile(waitedPath(box), &waitedFile{Box: box, For: watchFor, Reported: ids, SavedAt: time.Now().UTC()})
}
