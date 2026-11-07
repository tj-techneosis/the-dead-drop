package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
var mirrored = map[string]bool{"inbox": true, "ls": true, "pull": true, "show": true, "sent": true, "status": true}

type mirrorEntry struct {
	Host      string    `json:"host"`
	Cmd       string    `json:"cmd"`
	Args      []string  `json:"args"`
	Me        string    `json:"me"`
	FetchedAt time.Time `json:"fetched_at"`
	Answer    []byte    `json:"answer"`
}

func mirrorDir() string { return filepath.Join(stateDir(), "mirror") }

func cachedMode() bool { return os.Getenv("DEADDROP_CACHED") == "1" }

// takeCached strips --cached from a command's flags (before any "--") and
// turns the mirror on for this command and everything it runs.
func takeCached(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		if a == "--cached" {
			os.Setenv("DEADDROP_CACHED", "1")
			continue
		}
		out = append(out, a)
	}
	return out
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
	writeAtomic(filepath.Join(mirrorDir(), ".changed"), nil, 0o600)
}

// mirroredBytes is remoteBytes through the mirror: a fresh kept answer if
// there is one, else the store's, kept for next time.
func mirroredBytes(host, cmd string, args []string) ([]byte, error) {
	if !cachedMode() || !mirrored[cmd] || hops() > 0 {
		return remoteBytes(host, cmd, args)
	}
	me, err := explicitMe()
	if err != nil || me == "" {
		return remoteBytes(host, cmd, args)
	}
	path := filepath.Join(mirrorDir(), mirrorKey(host, cmd, args, me)+".json")
	if b, err := os.ReadFile(path); err == nil {
		var e mirrorEntry
		at := time.Now()
		if json.Unmarshal(b, &e) == nil && e.Host == host && e.Cmd == cmd && e.Me == me &&
			at.Sub(e.FetchedAt) < mirrorFresh && e.FetchedAt.After(staleBefore()) {
			os.Chtimes(path, at, at) // read lately: the watch keeps it fresh
			return e.Answer, nil
		}
	}
	return fetchIntoMirror(path, host, cmd, args, me, false)
}

func fetchIntoMirror(path, host, cmd string, args []string, me string, refresh bool) ([]byte, error) {
	var out []byte
	var err error
	if refresh && (cmd == "pull" || cmd == "show") {
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
		e := mirrorEntry{Host: host, Cmd: cmd, Args: args, Me: me, FetchedAt: time.Now(), Answer: out}
		if b, err := json.Marshal(e); err == nil {
			writeAtomic(path, b, 0o600)
		}
	}
	return out, nil
}

// refreshMirror asks the store again for every answer read in the last day,
// as the person this watch runs as, and lets go of the rest. It says how many
// it refreshed and the first error.
func refreshMirror() (int, error) {
	me, err := explicitMe()
	if err != nil || me == "" {
		return 0, err
	}
	paths, _ := filepath.Glob(filepath.Join(mirrorDir(), "*.json"))
	type kept struct {
		path string
		used time.Time
	}
	var ks []kept
	at := time.Now()
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
	for _, k := range ks[min(len(ks), mirrorMax):] {
		os.Remove(k.path)
	}
	ks = ks[:min(len(ks), mirrorMax)]
	n := 0
	var first error
	for _, k := range ks {
		b, err := os.ReadFile(k.path)
		var e mirrorEntry
		if err != nil || json.Unmarshal(b, &e) != nil {
			os.Remove(k.path)
			continue
		}
		if e.Me != me || !mirrored[e.Cmd] {
			continue // someone else's on this machine, or not ours to ask
		}
		if _, err := fetchIntoMirror(k.path, e.Host, e.Cmd, e.Args, e.Me, true); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		os.Chtimes(k.path, k.used, k.used) // a refresh isn't a read
		n++
	}
	return n, first
}
