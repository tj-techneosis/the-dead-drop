package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The gate: a key that can only do Dead Drop things, as one name, reading
// only the boxes it's given. On the store's machine it's an ssh forced
// command, one line per key in that login's authorized_keys:
//
//	restrict,command="/home/store/.local/bin/deaddrop gate --as bob --read bob,bob-helper" ssh-ed25519 AAAA…
//
// Whoever holds the key runs ordinary remote mode (host = the store) and
// nothing else: sshd hands the gate the command line remote mode sent
// ($SSH_ORIGINAL_COMMAND). The gate reads it with a strict parser (only the
// shape remote mode writes: single-quoted words), runs it only if it's one of
// the store's commands, as --as whatever the line says, with every box a
// reading command picks checked against --read. A shell, scp, sftp, a port
// forward or anything else is refused: there's nothing for them to run.

// gateLogMax bounds gate.log: past it, the log becomes gate.log.1 (one old
// one kept) and starts again, so nothing a key does can fill the disk with it.
const gateLogMax = 1 << 20

// gateIDs are message ids in a line: a repeat is the same line with other ids.
var gateIDs = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d{6}-[a-z][a-z0-9_-]*-[0-9a-f]{4}`)

// gateLogPerKey is how many lines one key may add to gate.log in its hour
// (from its first line; so at most twice that in any rolling hour). Past it
// they're counted, and the next line after the hour says how many were held.
// Each line is cut at gateLogLine. So a key can't wash the owner's history
// out, whatever words it makes vary: at most ~40 KB an hour.
const (
	gateLogPerKey = 20
	gateLogLine   = 1000
)

// gateLog notes, for the store's owner, what went wrong that a key holder
// isn't told: one line in gate.log at the store's top (the owner's folder).
// Under .gate.lock, kept in gate.seen.json:
//   - a line like one written this hour (the same words, ids aside) isn't
//     written again; the next line after the hour says how many it stood
//     for (the count waits in gate.seen.json until something is logged);
//   - each key adds at most gateLogPerKey lines in its hour, the rest
//     counted the same way;
//   - a line is cut at gateLogLine characters;
//   - the log rolls over to gate.log.1 at 1 MB.
//
// So the book holds at most twice gateLogPerKey kinds per key, and
// everything in it goes an hour after it came.
func gateLog(format string, a ...any) {
	dir, err := storeDir()
	if err != nil {
		// A config that can't be read is a thing to log: the usual place.
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "share", "deaddrop")
	}
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	unlock, err := lockFile(filepath.Join(dir, ".gate.lock"))
	if err != nil {
		return
	}
	defer unlock()
	who := gateAs
	if who == "" {
		who = "-"
	}
	line := fmt.Sprintf(format, a...)
	if len(line) > gateLogLine {
		line = line[:gateLogLine] + "…(cut)"
	}
	kind := who + " " + gateIDs.ReplaceAllString(line, "<id>")
	t := now().UTC()
	bookPath := filepath.Join(dir, "gate.seen.json")
	var book gateBook
	if b, err := os.ReadFile(bookPath); err == nil {
		json.Unmarshal(b, &book)
	}
	if book.Kinds == nil {
		// An early 0.7.10 build's book: a flat map of kinds. Its counts
		// carry over.
		book.Kinds = map[string]gateSeen{}
		if b, err := os.ReadFile(bookPath); err == nil {
			var flat map[string]gateSeen
			if json.Unmarshal(b, &flat) == nil {
				for k, v := range flat {
					if k != "kinds" && k != "keys" {
						book.Kinds[k] = v
					}
				}
			}
		}
	}
	if book.Keys == nil {
		book.Keys = map[string]gateSeen{}
	}
	stale := func(v gateSeen) bool { return t.Sub(v.First) > time.Hour || v.First.After(t.Add(time.Minute)) }
	// What the hour before held back, said now, before it's forgotten.
	// Kept aside, so a write that fails forgets nothing.
	var out []string
	oldKinds, oldKeys := map[string]gateSeen{}, map[string]gateSeen{}
	for k, v := range book.Kinds {
		if stale(v) {
			if v.More > 0 {
				out = append(out, fmt.Sprintf("(%d more like this in the hour before: %s)", v.More, k))
			}
			oldKinds[k] = v
			delete(book.Kinds, k)
		}
	}
	for k, v := range book.Keys {
		if stale(v) {
			if v.More > 0 {
				out = append(out, fmt.Sprintf("(%d more lines from %s held in the hour before)", v.More, k))
			}
			oldKeys[k] = v
			delete(book.Keys, k)
		}
	}
	sort.Strings(out)
	budget, have := book.Keys[who]
	if !have {
		budget = gateSeen{First: t}
	}
	written := false
	switch v, ok := book.Kinds[kind]; {
	case ok:
		v.More++
		book.Kinds[kind] = v
	case budget.Lines >= gateLogPerKey:
		budget.More++
	default:
		out = append(out, line)
		written = true
	}
	if len(out) > 0 && !gateAppend(dir, t, out) {
		// Nothing went in (one write): the hour before's counts stay to be
		// said, and this line, if it was to be written, is counted as held.
		for k, v := range oldKinds {
			book.Kinds[k] = v
		}
		for k, v := range oldKeys {
			if k != who {
				book.Keys[k] = v
			} else {
				budget.More += v.More // said with this hour's, later
			}
		}
		if written {
			budget.More++
		}
	} else if written {
		budget.Lines++
		book.Kinds[kind] = gateSeen{First: t}
	}
	book.Keys[who] = budget
	writeJSONFile(bookPath, &book)
}

// gateAppend adds lines to gate.log, rolling it over at 1 MB (under the
// caller's .gate.lock), and says whether they went in.
func gateAppend(dir string, t time.Time, lines []string) bool {
	path := filepath.Join(dir, "gate.log")
	if st, err := os.Stat(path); err == nil && st.Size() >= gateLogMax {
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	// One write, so it goes in whole or not at all (as near as a file
	// allows).
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "%s %s\n", t.Format(time.RFC3339), l)
	}
	_, err = f.WriteString(b.String())
	return err == nil
}

// gateSeen is a kind of line, or a key's budget: since when, how many were
// written (a budget's), and how many held back since.
type gateSeen struct {
	First time.Time `json:"first"`
	Lines int       `json:"lines,omitempty"`
	More  int       `json:"more,omitempty"`
}

// gateBook is gate.seen.json.
type gateBook struct {
	Kinds map[string]gateSeen `json:"kinds"`
	Keys  map[string]gateSeen `json:"keys"`
}

// gateRead is the boxes a gated caller may read: nil when not gated.
var gateRead map[string]bool

// gateAs is the gated caller's name.
var gateAs string

// gateCommands are what a gated key may run: the store's own commands, as
// remote mode sends them. Not gc, firetower or follow-up (the store's jobs),
// and not the commands that run on a caller's machine.
var gateCommands = map[string]bool{
	"send": true, "reply": true, "inbox": true, "ls": true, "pull": true, "show": true,
	"file": true, "mark": true, "status": true, "sent": true, "version": true,
}

// gateReadable says a gated caller may read box (always true ungated).
func gateReadable(box string) bool { return gateRead == nil || gateRead[box] }

// refused is a gate's refusal: exit 5, the same for every one, since none
// changes by asking again.
func refused(format string, a ...any) error {
	return exitError{exitRefused, fmt.Sprintf(format, a...)}
}

func gateRefusal(box string) error {
	var names []string
	for b := range gateRead {
		names = append(names, b)
	}
	sort.Strings(names)
	return exitError{exitRefused, fmt.Sprintf("this key reads only %s, not %s", strings.Join(names, ", "), box)}
}

func cmdGate(args []string) error {
	const u = "gate --as NAME --read BOX[,BOX]... [--dir STORE] [--config FILE]   (an ssh forced command on the store's machine)"
	f, err := parseFlags(args, map[string]bool{"as": true, "read": true, "dir": true, "config": true})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) > 0 || len(f.all("as")) != 1 || !f.has("read") {
		return badf(u, "one --as and at least one --read, nothing else")
	}
	as := strings.ToLower(f.get("as"))
	if err := checkBox("--as", as); err != nil || as == automation || as == "me" {
		return badf(u, "--as %s: a name, like bob", f.get("as"))
	}
	read := map[string]bool{}
	for _, raw := range f.all("read") {
		for _, w := range strings.Split(raw, ",") {
			b := strings.ToLower(strings.TrimSpace(w))
			if err := checkBox("--read", b); err != nil || b == automation || b == "me" {
				return badf(u, "--read %s: a box, like bob", w)
			}
			read[b] = true
		}
	}
	for _, k := range []string{"dir", "config"} {
		if f.has(k) && (len(f.all(k)) != 1 || !filepath.IsAbs(f.get(k)) || filepath.Clean(f.get(k)) != f.get(k)) {
			return badf(u, "--%s: one full path, written plainly (no //, /./, .. or trailing /)", k)
		}
	}
	// Nothing from the environment sshd built (AcceptEnv, environment= in
	// authorized_keys, a profile) decides anything: no DEADDROP_ setting and
	// no XDG_ folder, PATH a fixed one (it picks the programs that run as the
	// store's login), HOME the account's own. Where the store and its config
	// are comes from this line, written by the store's owner (--dir,
	// --config), or else their usual place in the account's home.
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "DEADDROP_") || strings.HasPrefix(k, "XDG_") {
			os.Unsetenv(k)
		}
	}
	if acct, err := user.Current(); err == nil && acct.HomeDir != "" {
		os.Setenv("HOME", acct.HomeDir)
	}
	home, _ := os.UserHomeDir()
	os.Setenv("PATH", filepath.Join(home, ".local", "bin")+":/usr/local/bin:/usr/bin:/bin")
	if f.has("dir") {
		os.Setenv("DEADDROP_DIR", f.get("dir"))
	}
	if f.has("config") {
		os.Setenv("DEADDROP_CONFIG", f.get("config"))
	}
	cachedConfig = nil
	// Started by its full path, as authorized_keys names it: never found
	// through a PATH that someone else's setup decides.
	if !filepath.IsAbs(os.Args[0]) {
		return refused("the gate runs only by its full path (command=\"/home/…/.local/bin/deaddrop gate …\")")
	}
	// A store that passes commands on would hand the line to another machine
	// before any check here ran, and one with a host of its own isn't the
	// store: the gate runs only where the store is. (Which host is nobody's
	// business who holds a key.)
	if c, err := loadConfig(); err != nil {
		gateLog("the config can't be read: %v", err) // the owner's to see, not the caller's
		return refused("the store's config can't be read")
	} else if c.Host != "" {
		return refused("this machine isn't the store (its config sends commands to another machine): the gate won't run here")
	}
	// The store by its full path from here on, so every path an error
	// carries is spelled the way gateClean looks for it (a config's dir may
	// be relative).
	if d, err := storeDir(); err == nil {
		if abs, err := filepath.Abs(d); err == nil {
			os.Setenv("DEADDROP_DIR", abs)
		}
	}
	line, ok := os.LookupEnv("SSH_ORIGINAL_COMMAND")
	if !ok {
		return refused("this key is for the Dead Drop's remote mode only: set host on your machine and run deaddrop there")
	}
	words, hops, err := gateWords(line)
	if err != nil {
		return refused("this key is for the Dead Drop's remote mode only (%v)", err)
	}
	if len(words) == 0 {
		return refused("this key runs one deaddrop command: say which (send, inbox, pull, …)")
	}
	if words[0] == "" {
		return refused("this key runs one deaddrop command, and '' isn't one: say which (send, inbox, pull, …)")
	}
	if !gateCommands[words[0]] {
		what := cleanLine(words[0], 40)
		return refused("this key can't run %s: only send, reply, inbox, ls, pull, show, file, mark, status, sent and version", what)
	}
	cmd, rest := words[0], words[1:]
	if err := gateCheck(cmd, rest, as); err != nil {
		return err
	}
	if hops > maxHops {
		return refused("this command has come through %d machines; %d is the limit", hops, maxHops)
	}
	os.Setenv("DEADDROP_FROM", as)
	os.Setenv("DEADDROP_HOPS", fmt.Sprint(max(hops, 1)))
	gateAs, gateRead = as, read
	return gateClean(dispatch(cmd, rest))
}

// gateClean keeps where the store lives from a key holder: an error naming
// the store's folder or the account's home goes to gate.log whole, and to
// the key with those parts as <store> and ~. Its exit code stays.
func gateClean(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	clean := msg
	// As errors spell it: filepath.Join cleans, so the pattern is clean
	// and absolute too (a relative one would match any word).
	if dir, derr := storeDir(); derr == nil && dir != "" {
		if abs, aerr := filepath.Abs(dir); aerr == nil && abs != "/" {
			clean = strings.ReplaceAll(clean, abs, "<store>")
		}
	}
	if home, herr := os.UserHomeDir(); herr == nil && len(home) > 1 {
		clean = strings.ReplaceAll(clean, home, "~")
	}
	if clean == msg {
		return err
	}
	gateLog("%s", msg)
	var ee exitError
	if errors.As(err, &ee) {
		return exitError{ee.code, clean}
	}
	var ue usageError
	if errors.As(err, &ue) {
		return err // usage text names no paths
	}
	return errors.New(clean)
}

// gateCheck is what a gated command line may not say, beyond the boxes
// (which resolveBox checks): who it's from, and status of every box.
func gateCheck(cmd string, args []string, as string) error {
	// Who a letter is from, who signs it and its attachments are checked by
	// send itself, on its own parse of the flags (draftOf), so a "--" or a
	// flag's value can't read differently here and there. Here: status names
	// its boxes, and reads come as stored.
	switch cmd {
	case "status":
		if !hasFlag(args, "box") {
			return refused("this key's status names its boxes: status --box %s", as)
		}
	case "inbox", "ls", "pull", "sent":
		// Remote mode reads as stored and checks on the reader's machine; the
		// store mustn't check, or keep a ledger, on a gated key's behalf.
		if !hasFlag(args, "raw") {
			return refused("this key reads as remote mode does (--raw)")
		}
	case "show":
		if !hasFlag(args, "raw") && !hasFlag(args, "fingerprint") {
			return refused("this key reads as remote mode does (--raw)")
		}
	}
	return nil
}

// gateQuoted is one single-quoted word as remote mode writes it ('it'\”s').
var gateQuoted = regexp.MustCompile(`^'([^']*)'((?:\\''[^']*')*)`)

// gateBare is a word a person might type unquoted: nothing a shell reads.
var gateBare = regexp.MustCompile(`^[A-Za-z0-9_.,:@/=+-]+`)

// The front of remote mode's line: which deaddrop to run, then who's asking
// (ignored: the gate says who), then exec.
var gatePrefix = regexp.MustCompile(`^(?:d="\$HOME/\.local/bin/deaddrop"; \[ -x "\$d" \] \|\| d=deaddrop; |d='(?:[^']|'\\'')*'; )DEADDROP_HOPS=([0-9]{1,3}) DEADDROP_FROM='(?:[^']|'\\'')*' exec "\$d" `)

// gateWords reads the command line remote mode sends, or a plain
// "deaddrop <command> <words>" a person types, and nothing else.
func gateWords(line string) ([]string, int, error) {
	if len(line) > 64*1024 {
		return nil, 0, fmt.Errorf("a command line that long isn't one")
	}
	rest, hops, typed := line, 0, false
	if m := gatePrefix.FindStringSubmatch(rest); m != nil {
		rest = rest[len(m[0]):]
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, 0, fmt.Errorf("a hop count that isn't one")
		}
		hops = n
	} else if strings.TrimRight(rest, " ") == "deaddrop" {
		return nil, 0, nil
	} else if strings.HasPrefix(rest, "deaddrop ") {
		// A line a person typed: spaces between words are spaces, however
		// many (a shell would read them so). Remote mode's own line is
		// held to the one space it writes.
		rest, typed = strings.Trim(strings.TrimPrefix(rest, "deaddrop "), " "), true
	} else {
		return nil, 0, fmt.Errorf("not a deaddrop command")
	}
	var words []string
	for rest != "" {
		if m := gateQuoted.FindStringSubmatch(rest); m != nil {
			w := m[1]
			for _, part := range regexp.MustCompile(`\\''([^']*)'`).FindAllStringSubmatch(m[2], -1) {
				w += "'" + part[1]
			}
			words = append(words, w)
			rest = rest[len(m[0]):]
		} else if m := gateBare.FindString(rest); m != "" {
			words = append(words, m)
			rest = rest[len(m):]
		} else {
			return nil, 0, fmt.Errorf("characters a shell would read")
		}
		if rest == "" {
			break
		}
		if rest[0] != ' ' {
			return nil, 0, fmt.Errorf("characters a shell would read")
		}
		rest = rest[1:]
		if typed {
			rest = strings.TrimLeft(rest, " ")
		}
	}
	for _, w := range words {
		if strings.ContainsRune(w, 0) {
			return nil, 0, fmt.Errorf("a NUL in a word")
		}
	}
	return words, hops, nil
}
