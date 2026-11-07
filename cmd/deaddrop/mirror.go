package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The mirror: with the store on another machine, a reader that asks for it
// (--cached, or $DEADDROP_CACHED=1) keeps the store's answers to its reading
// commands on its own machine and reads them from there, with no ssh. What's
// kept is what the store handed over (--raw): every read still checks the
// signatures here, against this machine's list, and the seal ledger still
// applies. Only a reader that asks for it ever writes one, since it puts
// letter text on this machine's disk (0600, in a 0700 folder).
//
// An answer is used while it's younger than mirrorFresh; after that, or once
// this machine has changed anything on the store (mark, send, reply, …), the
// next read asks the store again. `watch --mirror` asks again for every
// answer read in the last day, so a panel reading --cached sees what the
// watch saw, at most a pass old.

// mirrorFresh is how long a kept answer is used without asking the store.
const mirrorFresh = 10 * time.Minute

// mirrorKeep is how long an answer nobody reads is kept, and mirrorMax how
// many are kept at most (the least lately read go first).
const (
	mirrorKeep = 24 * time.Hour
	mirrorMax  = 64
)

// mirrored are the reading commands whose answers may be kept: never `file`
// (attachments' bytes), never anything that changes the store.
var mirrored = map[string]bool{"inbox": true, "pull": true, "show": true, "sent": true}

type mirrorEntry struct {
	Host      string    `json:"host"`
	Cmd       string    `json:"cmd"`
	Args      []string  `json:"args"`
	Me        string    `json:"me"`
	FetchedAt time.Time `json:"fetched_at"`
	Answer    []byte    `json:"answer"`
}

func mirrorDir() string { return filepath.Join(stateDir(), "mirror") }

// saidNoMirror: the --cached warning is said once a run, not per read.
var saidNoMirror bool

func cachedMode() bool { return os.Getenv("DEADDROP_CACHED") == "1" }

// readsCached are the commands --cached is a flag of: those that read.
var readsCached = map[string]bool{"inbox": true, "ls": true, "pull": true, "show": true, "sent": true,
	"thread": true, "threads": true, "with": true, "asks": true, "people": true}

// takeCached strips --cached from a reading command's flags (before any
// "--", and never a flag's value) and turns the mirror on for this command
// and everything it runs.
func takeCached(cmd string, args []string) []string {
	if !readsCached[cmd] {
		return args
	}
	out := make([]string, 0, len(args))
	for i, a := range args {
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		if a == "--cached" && (i == 0 || !takesValue(args[i-1])) {
			os.Setenv("DEADDROP_CACHED", "1")
			continue
		}
		out = append(out, a)
	}
	return out
}

// takesValue says a reading command's flag is followed by its value.
func takesValue(flag string) bool {
	switch flag {
	case "--tag", "--from", "--since", "--for", "--box", "--to", "--limit", "--thread", "--with":
		return true
	}
	return false
}

func mirrorKey(host, cmd string, args []string, me string) string {
	h := sha256.Sum256([]byte(strings.Join(append([]string{host, cmd, me}, args...), "\x00")))
	return hex.EncodeToString(h[:16])
}

// staleBefore is when this machine last changed something on the store:
// answers fetched before then aren't used.
func staleBefore() time.Time {
	info, err := os.Stat(filepath.Join(mirrorDir(), ".changed"))
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// mirrorChanged says this machine has changed the store, if it keeps a mirror.
func mirrorChanged() {
	if _, err := os.Stat(mirrorDir()); err != nil {
		return
	}
	path := filepath.Join(mirrorDir(), ".changed")
	if writeAtomic(path, nil, 0o600) == nil {
		t := now()
		os.Chtimes(path, t, t)
	}
}

// mirroredBytes is remoteBytes through the mirror: a fresh kept answer if
// there is one, else the store's, kept for next time.
func mirroredBytes(host, cmd string, args []string) ([]byte, error) {
	if !cachedMode() || !mirrored[cmd] || hops() > 0 {
		return remoteBytes(host, cmd, args)
	}
	me, err := explicitMe()
	if err != nil || me == "" {
		if !saidNoMirror {
			saidNoMirror = true
			if err != nil {
				fmt.Fprintf(os.Stderr, "deaddrop: --cached not used: %v\n", err)
			} else {
				fmt.Fprintln(os.Stderr, "deaddrop: --cached not used: it needs your name (DEADDROP_FROM, or me in the config)")
			}
		}
		return remoteBytes(host, cmd, args)
	}
	path := filepath.Join(mirrorDir(), mirrorKey(host, cmd, args, me)+".json")
	if b, err := os.ReadFile(path); err == nil {
		var e mirrorEntry
		at := now()
		// Fresh: fetched in the last mirrorFresh, not "in the future", and
		// after this machine last changed the store.
		if json.Unmarshal(b, &e) == nil && e.Host == host && e.Cmd == cmd && e.Me == me && equalArgs(e.Args, args) &&
			!e.FetchedAt.After(at) && at.Sub(e.FetchedAt) < mirrorFresh && e.FetchedAt.After(staleBefore()) {
			os.Chtimes(path, at, at) // read lately: the watch keeps it fresh
			return e.Answer, nil
		}
	}
	return fetchIntoMirror(path, host, cmd, args, me, false, false, time.Time{})
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fetchIntoMirror asks the store and keeps the answer. unheard: only
// keeping a copy, so the store doesn't stamp the box heard. fromRefresh: a
// watch's pass, which prunes once itself.
func fetchIntoMirror(path, host, cmd string, args []string, me string, unheard, fromRefresh bool, heardAt time.Time) ([]byte, error) {
	// Stamped when the fetch starts: a write from here while it's in flight
	// is later than this, so the answer reads as stale. The folder is made
	// first, so even the very first fetch has somewhere for that write's
	// stamp to land.
	started := now()
	if os.MkdirAll(mirrorDir(), 0o700) == nil {
		os.Chmod(mirrorDir(), 0o700)
	}
	var out []byte
	var err error
	if !heardAt.IsZero() && (cmd == "pull" || cmd == "show") {
		// Read from the mirror: heard as of the answer the reader was shown.
		out, err = remoteBytes(host, cmd, append(append([]string{}, args...), "--heard-at", heardAt.UTC().Format(time.RFC3339Nano)))
		if err != nil && strings.Contains(err.Error(), "--heard-at") {
			out, err = remoteBytes(host, cmd, append(append([]string{}, args...), "--unheard")) // an older store
		}
	} else if (unheard || !heardAt.IsZero()) && (cmd == "pull" || cmd == "show") {
		// Keeping a copy isn't reading it: the box isn't stamped heard.
		out, err = remoteBytes(host, cmd, append(append([]string{}, args...), "--unheard"))
		if err != nil && strings.Contains(err.Error(), "--unheard") {
			out, err = remoteBytes(host, cmd, args) // an older store
		}
	} else {
		out, err = remoteBytes(host, cmd, args)
	}
	if err != nil {
		return nil, err
	}
	if os.MkdirAll(mirrorDir(), 0o700) == nil {
		os.Chmod(mirrorDir(), 0o700)
		e := mirrorEntry{Host: host, Cmd: cmd, Args: args, Me: me, FetchedAt: started, Answer: out}
		if b, err := json.Marshal(e); err == nil && writeAtomic(path, b, 0o600) == nil {
			// Last read when fetched: a later read from the mirror is
			// what tells a refresh someone has read it since.
			os.Chtimes(path, started, started)
		}
		if !fromRefresh {
			pruneMirror()
		}
	}
	return out, nil
}

// pruneMirror lets go of what's past mirrorKeep, the least lately read past
// mirrorMax, and any half-written file a crash left (letter text included).
// It returns what's kept, most lately read first.
func pruneMirror() []string {
	at := now()
	if tmps, _ := filepath.Glob(filepath.Join(mirrorDir(), ".tmp-*")); len(tmps) > 0 {
		for _, t := range tmps {
			if info, err := os.Stat(t); err == nil && at.Sub(info.ModTime()) > time.Minute {
				os.Remove(t)
			}
		}
	}
	paths, _ := filepath.Glob(filepath.Join(mirrorDir(), "*.json"))
	type kept struct {
		path string
		used time.Time
	}
	var ks []kept
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if at.Sub(info.ModTime()) > mirrorKeep {
			os.Remove(p)
			continue
		}
		ks = append(ks, kept{p, info.ModTime()})
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i].used.After(ks[j].used) })
	var out []string
	for i, k := range ks {
		if i >= mirrorMax {
			os.Remove(k.path)
			continue
		}
		out = append(out, k.path)
	}
	return out
}

// refreshMirror asks the store again for every answer read in the last day,
// as the person this watch runs as, and lets go of the rest. It says how many
// it refreshed and the first error.
func refreshMirror() (int, error) {
	me, err := explicitMe()
	if err != nil || me == "" {
		return 0, err
	}
	// Only answers from the store this machine reaches now: an entry naming
	// another host (planted, or from an old config) is never asked again.
	host, err := remoteHost()
	if err != nil {
		return 0, err
	}
	if host == "" {
		os.RemoveAll(mirrorDir()) // the store is here: nothing to keep a copy of
		return 0, nil
	}
	n := 0
	for _, path := range pruneMirror() {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		used := info.ModTime()
		b, err := os.ReadFile(path)
		var e mirrorEntry
		if err != nil || json.Unmarshal(b, &e) != nil || e.Host != host || !mirrored[e.Cmd] ||
			filepath.Base(path) != mirrorKey(e.Host, e.Cmd, e.Args, e.Me)+".json" {
			os.Remove(path)
			continue
		}
		if e.Me != me {
			continue // someone else's on this machine
		}
		// Read from the mirror since it was last fetched: someone read it,
		// so this fetch tells the store the box was heard as of the answer
		// they were shown. Otherwise it's only keeping a copy (--unheard).
		var shown time.Time
		if used.After(e.FetchedAt) {
			shown = e.FetchedAt // what the reader saw was as of this
		}
		if _, err := fetchIntoMirror(path, e.Host, e.Cmd, e.Args, e.Me, true, true, shown); err != nil {
			return n, err // the store isn't answering: one try a pass, not 64
		}
		os.Chtimes(path, used, used) // a refresh isn't a read
		n++
	}
	return n, nil
}
