package main

import (
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

// gateLog notes, for the store's owner, what went wrong that a key holder
// isn't told: one line in gate.log at the store's top (the owner's folder).
func gateLog(format string, a ...any) {
	dir, err := storeDir()
	if err != nil || os.MkdirAll(dir, 0o700) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "gate.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", now().UTC().Format(time.RFC3339), fmt.Sprintf(format, a...))
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
		if f.has(k) && (len(f.all(k)) != 1 || !filepath.IsAbs(f.get(k))) {
			return badf(u, "--%s: one full path", k)
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
	return dispatch(cmd, rest)
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
