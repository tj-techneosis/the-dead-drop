package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The files are the state: one Markdown file per message, in its box's new,
// read or archive folder. Moving a file between them is the only change of
// state; there is no database and no index.
//
//	<dir>/<who>/new/<id>.md       unread
//	<dir>/<who>/read/<id>.md      read
//	<dir>/<who>/archive/<id>.md   filed
//	<dir>/<who>/files/<id>/...    attachments that came with a message
//	<dir>/<who>/for/<helper>/...  a helper's own pigeonhole: the same again,
//	                              for mail addressed --for that helper

var stateDirs = []struct{ state, dir string }{
	{"unread", "new"},
	{"read", "read"},
	{"archived", "archive"},
}

func dirFor(state string) string {
	for _, s := range stateDirs {
		if s.state == state {
			return s.dir
		}
	}
	panic("no state " + state)
}

const storeReadme = `# The Dead Drop

This folder is a Dead Drop store: each box is a folder, each message a
Markdown file in its new/, read/ or archive/. The deaddrop command is the
only writer. Read what you like; change things through deaddrop.

**Never put a secret in a message.** Everyone who can read this folder can
read every box. A report that needs a secret names where it lives instead.
`

// openStore makes sure the store exists, with its README.
func openStore() (string, error) {
	dir, err := storeDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	readme := filepath.Join(dir, "README.md")
	if _, err := os.Stat(readme); errors.Is(err, os.ErrNotExist) {
		writeAtomic(readme, []byte(storeReadme), 0o600)
	}
	return dir, nil
}

// lockBox takes a box's lock: every change of state holds it, so a
// follow-up raising a message can't race a mark moving it.
func lockBox(dir, box string) (func(), error) {
	if err := os.MkdirAll(filepath.Join(dir, box), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, box, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// writeAtomic writes a temp file beside path, syncs it and renames it over
// path, so a reader never sees half a message.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Chmod(mode)
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	return werr
}

// copyAtomic copies a stream into place the same way.
func copyAtomic(path string, r io.Reader) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return 0, err
	}
	n, werr := io.Copy(tmp, r)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	return n, werr
}

// boxes lists every box in the store.
func boxes(dir string) []string {
	var out []string
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.IsDir() && boxRE.MatchString(e.Name()) && e.Name() != automation {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// slotDir is a pigeonhole: a person's own (helper ""), or one of their
// helpers'.
func slotDir(dir, box, helper string) string {
	if helper == "" {
		return filepath.Join(dir, box)
	}
	return filepath.Join(dir, box, "for", helper)
}

// filesDir is where a message's attachments are, beside it.
func filesDir(dir string, m *Message) string {
	return filepath.Join(slotDir(dir, m.Box, m.Pigeonhole), "files", m.ID)
}

// helpers are the helpers a box has pigeonholes for.
func helpers(dir, box string) []string {
	var out []string
	ents, _ := os.ReadDir(filepath.Join(dir, box, "for"))
	for _, e := range ents {
		if e.IsDir() && boxRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// loadBox reads a person's own pigeonhole in the given states.
func loadBox(dir, box string, states []string, withBody bool) []*Message {
	return loadSlot(dir, box, "", states, withBody)
}

// loadSlot reads one pigeonhole's messages in the given states. A file that
// can't be read is reported on stderr and skipped, never dropped silently.
func loadSlot(dir, box, helper string, states []string, withBody bool) []*Message {
	var out []*Message
	for _, state := range states {
		folder := filepath.Join(slotDir(dir, box, helper), dirFor(state))
		ents, _ := os.ReadDir(folder)
		for _, e := range ents {
			n := e.Name()
			if !strings.HasSuffix(n, ".md") || !e.Type().IsRegular() || checkID(strings.TrimSuffix(n, ".md")) != nil {
				continue
			}
			path := filepath.Join(folder, n)
			m, err := readMessage(path, withBody)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					fmt.Fprintf(os.Stderr, "deaddrop: skipping %s: %v\n", path, err)
				}
				continue
			}
			m.Box, m.Pigeonhole, m.State = box, helper, state
			out = append(out, m)
		}
	}
	return out
}

// loadAll reads a person's pigeonhole and every helper's.
func loadAll(dir, box string, states []string, withBody bool) []*Message {
	out := loadSlot(dir, box, "", states, withBody)
	for _, h := range helpers(dir, box) {
		out = append(out, loadSlot(dir, box, h, states, withBody)...)
	}
	return out
}

func readMessage(path string, withBody bool) (*Message, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := parseMessage(b, withBody)
	if err != nil {
		return nil, err
	}
	// The path is the truth: a file whose id disagrees with its name is read
	// as its name.
	m.ID = strings.TrimSuffix(filepath.Base(path), ".md")
	m.path = path
	return m, nil
}

// findMessage finds one message in a person's own pigeonhole, in any state.
func findMessage(dir, box, id string, withBody bool) (*Message, error) {
	return findIn(dir, box, "", id, withBody)
}

// findAnywhere looks in the person's pigeonhole, then each helper's.
func findAnywhere(dir, box, id string, withBody bool) (*Message, error) {
	m, err := findIn(dir, box, "", id, withBody)
	if err == nil {
		return m, nil
	}
	for _, h := range helpers(dir, box) {
		if m, err2 := findIn(dir, box, h, id, withBody); err2 == nil {
			return m, nil
		}
	}
	if other := boxHolding(dir, box, id); other != "" && checkID(id) == nil {
		return nil, fmt.Errorf("%v; it's in %s's box (--box %s)", err, other, other)
	}
	return nil, err
}

// boxHolding is another box with a copy of id, for a hint: "" if none.
func boxHolding(dir, not, id string) string {
	if checkID(id) != nil {
		return ""
	}
	for _, b := range boxes(dir) {
		if b == not {
			continue
		}
		for _, h := range append([]string{""}, helpers(dir, b)...) {
			for _, s := range stateDirs {
				if _, err := os.Stat(filepath.Join(slotDir(dir, b, h), s.dir, id+".md")); err == nil {
					return b
				}
			}
		}
	}
	return ""
}

// heardFile stamps when a pigeonhole was last read (pull, show, mark): its
// time is all it holds. A board counts mail waiting since then as unheard.
const heardFile = ".heard"

// hear stamps a pigeonhole as read now, if it's there; never makes one.
func hear(dir, box, helper string) { hearAt(dir, box, helper, now()) }

// hearAt stamps a pigeonhole heard as of t (never later than now), only
// moving the stamp forward: a reader who was shown the box as of t has heard
// what had arrived by then, not what came after.
func hearAt(dir, box, helper string, t time.Time) {
	slot := slotDir(dir, box, helper)
	if _, err := os.Stat(slot); err != nil {
		return
	}
	if at := now(); t.After(at) {
		t = at
	}
	if !t.After(heardAt(dir, box, helper)) {
		return
	}
	path := filepath.Join(slot, heardFile)
	if writeAtomic(path, nil, 0o600) == nil {
		os.Chtimes(path, t, t)
	}
}

// heardAt is when a pigeonhole was last read: zero if never stamped.
func heardAt(dir, box, helper string) time.Time {
	info, err := os.Stat(filepath.Join(slotDir(dir, box, helper), heardFile))
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// findIn finds one message in one pigeonhole, in any state.
func findIn(dir, box, helper, id string, withBody bool) (*Message, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	for _, s := range stateDirs {
		path := filepath.Join(slotDir(dir, box, helper), s.dir, id+".md")
		m, err := readMessage(path, withBody)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %v", path, err)
		}
		m.Box, m.Pigeonhole, m.State = box, helper, s.state
		return m, nil
	}
	if helper != "" {
		return nil, fmt.Errorf("no message %s for %s in %s's box", id, helper, box)
	}
	return nil, fmt.Errorf("no message %s in %s's box", id, box)
}

// move puts a message in another state. Archiving stamps the file's time, so
// gc counts from when it was filed.
func move(dir string, m *Message, state string) error {
	if m.State == state {
		return nil
	}
	to := filepath.Join(slotDir(dir, m.Box, m.Pigeonhole), dirFor(state), m.ID+".md")
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	if err := os.Rename(m.path, to); err != nil {
		return err
	}
	if state == "archived" {
		t := now()
		os.Chtimes(to, t, t)
	}
	m.path, m.State = to, state
	return nil
}

// sortForPull: unread first, then oldest first.
func sortForPull(ms []*Message) {
	rank := map[string]int{"unread": 0, "read": 1, "archived": 2}
	sort.SliceStable(ms, func(i, j int) bool {
		if rank[ms[i].State] != rank[ms[j].State] {
			return rank[ms[i].State] < rank[ms[j].State]
		}
		if !ms[i].Sent.Equal(ms[j].Sent) {
			return ms[i].Sent.Before(ms[j].Sent)
		}
		// Sent the same second: the file's own time knows better.
		if a, b := modTime(ms[i].path), modTime(ms[j].path); !a.Equal(b) {
			return a.Before(b)
		}
		return ms[i].ID < ms[j].ID
	})
}

// newest is the most recently sent of ms, or nil.
func newest(ms []*Message) *Message {
	var n *Message
	for _, m := range ms {
		if n == nil || m.Sent.After(n.Sent) || (m.Sent.Equal(n.Sent) && m.ID > n.ID) {
			n = m
		}
	}
	return n
}

func modTime(path string) time.Time {
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}
