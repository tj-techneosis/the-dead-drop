package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

func gateRefusal(box string) error {
	var names []string
	for b := range gateRead {
		names = append(names, b)
	}
	sort.Strings(names)
	return fmt.Errorf("this key reads only %s, not %s", strings.Join(names, ", "), box)
}

func cmdGate(args []string) error {
	const u = "gate --as NAME --read BOX[,BOX]...   (an ssh forced command on the store's machine)"
	f, err := parseFlags(args, map[string]bool{"as": true, "read": true})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) > 0 || len(f.all("as")) != 1 || !f.has("read") {
		return badf(u, "one --as and at least one --read, nothing else")
	}
	as := strings.ToLower(f.get("as"))
	if err := checkBox("--as", as); err != nil || as == automation {
		return badf(u, "--as %s: a name, like bob", f.get("as"))
	}
	read := map[string]bool{}
	for _, raw := range f.all("read") {
		for _, w := range strings.Split(raw, ",") {
			b := strings.ToLower(strings.TrimSpace(w))
			if err := checkBox("--read", b); err != nil || b == automation {
				return badf(u, "--read %s: a box, like bob", w)
			}
			read[b] = true
		}
	}
	// Started by its full path, as authorized_keys names it: never found
	// through a PATH that someone else's setup decides.
	if !filepath.IsAbs(os.Args[0]) {
		return fmt.Errorf("the gate runs only by its full path (command=\"/home/…/.local/bin/deaddrop gate …\")")
	}
	line, ok := os.LookupEnv("SSH_ORIGINAL_COMMAND")
	if !ok {
		return fmt.Errorf("this key is for the Dead Drop's remote mode only: set host on your machine and run deaddrop there")
	}
	words, err := gateWords(line)
	if err != nil {
		return fmt.Errorf("this key is for the Dead Drop's remote mode only (%v)", err)
	}
	if len(words) == 0 || !gateCommands[words[0]] {
		what := "nothing"
		if len(words) > 0 {
			what = cleanLine(words[0], 40)
		}
		return fmt.Errorf("this key can't run %s: only send, reply, inbox, pull, show, file, mark, status and sent", what)
	}
	cmd, rest := words[0], words[1:]
	if err := gateCheck(cmd, rest, as); err != nil {
		return err
	}
	// Nothing the caller's side or this login's own setup says about who's
	// asking survives: the gate says it.
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "DEADDROP_") && k != "DEADDROP_DIR" && k != "DEADDROP_CONFIG" {
			os.Unsetenv(k)
		}
	}
	os.Setenv("DEADDROP_FROM", as)
	os.Setenv("DEADDROP_HOPS", "1")
	gateAs, gateRead = as, read
	return dispatch(cmd, rest)
}

// gateCheck is what a gated command line may not say, beyond the boxes
// (which resolveBox checks): who it's from, and status of every box.
func gateCheck(cmd string, args []string, as string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		name, val, hasVal := strings.Cut(a, "=")
		if !hasVal && i+1 < len(args) {
			val = args[i+1]
		}
		switch name {
		case "--from":
			if cmd == "send" && strings.ToLower(val) != as {
				return fmt.Errorf("this key sends only as %s", as)
			}
		case "--signed-by":
			if strings.ToLower(val) != as {
				return fmt.Errorf("this key signs only as %s", as)
			}
		}
	}
	if cmd == "status" && !hasFlag(args, "box") {
		return fmt.Errorf("this key's status names its boxes: status --box %s", as)
	}
	return nil
}

// gateQuoted is one single-quoted word as remote mode writes it ('it'\”s').
var gateQuoted = regexp.MustCompile(`^'([^']*)'((?:\\''[^']*')*)`)

// gateBare is a word a person might type unquoted: nothing a shell reads.
var gateBare = regexp.MustCompile(`^[A-Za-z0-9_.,:@/=+-]+`)

// The front of remote mode's line: which deaddrop to run, then who's asking
// (ignored: the gate says who), then exec.
var gatePrefix = regexp.MustCompile(`^(?:d="\$HOME/\.local/bin/deaddrop"; \[ -x "\$d" \] \|\| d=deaddrop; |d='[^']*'; )DEADDROP_HOPS=[0-9]+ DEADDROP_FROM='[^']*' exec "\$d" `)

// gateWords reads the command line remote mode sends, or a plain
// "deaddrop <command> <words>" a person types, and nothing else.
func gateWords(line string) ([]string, error) {
	if len(line) > 64*1024 {
		return nil, fmt.Errorf("a command line that long isn't one")
	}
	rest := line
	if m := gatePrefix.FindString(rest); m != "" {
		rest = rest[len(m):]
	} else if strings.HasPrefix(rest, "deaddrop ") {
		rest = strings.TrimPrefix(rest, "deaddrop ")
	} else {
		return nil, fmt.Errorf("not a deaddrop command")
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
			return nil, fmt.Errorf("characters a shell would read")
		}
		if rest == "" {
			break
		}
		if rest[0] != ' ' {
			return nil, fmt.Errorf("characters a shell would read")
		}
		rest = rest[1:]
	}
	for _, w := range words {
		if strings.ContainsAny(w, "\x00\n\r") {
			return nil, fmt.Errorf("a control character in a word")
		}
	}
	return words, nil
}
