// The Dead Drop: a shared, tagged inbox of Markdown documents. People leave
// documents in each other's boxes; jobs file reports in their owner's box; a
// working session pulls in what matters and files it away. See README.md and
// docs/design.md.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const usage = `deaddrop: a shared, tagged inbox of documents

Leave something:
  deaddrop send --to WHO [--subject "…"] [--tag T]... [--urgent|--fyi]
                [--reply-to ID] [--attach FILE]... [--follow-up 2d|TIME]
                [--from automation] [--as NAME] [--for HELPER]... [--reply-box BOX] [--ask] < body.md
  deaddrop reply ID [--urgent|--fyi] [--tag T]... [--untag T]... [--as NAME]
                [--for HELPER]... [--reply-box BOX] [--follow-up 2d|TIME] < body.md

Collect it ([--for HELPER] reads a helper's pigeonhole; '*' is every one):
  deaddrop inbox [WHO] [--tag T]... [--from WHO]... [--since D] [--read|--all] [--for H] [--json]
  deaddrop pull  [WHO] [--tag T]... [--from WHO]... [--since D] [--unread|--all] [--for H] [--json]
  deaddrop show ID [--json|--html [--fragment]] [--box WHO] [--for H]   one message in full
  deaddrop sent [--to WHO]... [--tag T]... [--since D] [--box WHO] [--json]
                                   what you sent, with each copy's state
  deaddrop file ID [NAME] [--box WHO] [--for H]   an attachment (no NAME: list them)
  deaddrop mark read|unread|archive ID... [--box WHO]
  deaddrop mark read|unread|archive --tag T... [--from WHO]... [--since D] [--box WHO] [--for H]

On your own machine:
  deaddrop watch [--every 1m] [--no-hooks] [--box WHO] [--for H] [--mirror]   keep the inbox file current, run new.d hooks
  deaddrop open ID [--keep-unread] [--for H]   read one in the browser, and mark it read
  deaddrop thread ID | with WHO [--tag T]... [--for H] [--json|--html]   both sides, in time order
  deaddrop asks [--for H] [--json]   letters that need your decision, not yet answered
  deaddrop people [--for H] [--json] everyone in your mail, and their personas
  deaddrop threads [--for H] [--since D] [--limit N] [--letters [--bodies]] [--json|--html]
                                   every thread, newest activity first
  deaddrop open --thread ID | --with WHO | --threads [--for H]   the same, in the browser
  deaddrop bar [--format text|swiftbar] [--for H]   the inbox file as a line, or a menu
  deaddrop key                     your line for everyone's allowed_signers

Look after it:
  deaddrop status [--box WHO]... [--json]   per box: unread, urgent, helpers' unread, newest subject
  deaddrop firetower [--next 6m|TIME|none] [--box WHO]... [--smoke-after 30m] [--fire-after 3h]   one lookout per box
  deaddrop gc [--days 30]          archived mail older than that goes

  deaddrop upgrade [--check] | deaddrop version | deaddrop help

WHO is a lowercase name; "me" is you: $DEADDROP_FROM, else "me" in the
config, else your login name. It is the default sender and the default box
for inbox, pull and mark. You send as yourself, or a job sends --from
automation; "automation" is a sender, never a box. With shared_login in the
config, the login name is never used.

A helper (an assistant working for someone) reads its own pigeonhole with
--for, or $DEADDROP_FOR for a whole session, and signs as a persona with
--as, or $DEADDROP_AS. --reply-box (or $DEADDROP_REPLY_BOX) asks for answers
at another box, such as its own; reply honours it when the letter verifies.

The body is Markdown on stdin. With no --subject, the subject is the
document's first heading (or its first line). Several --tag on inbox, pull
and mark all have to match; several --from match any of them. pull prints
every matching message in full (unread first, oldest first within that) and
marks nothing: file them afterwards with mark. --follow-up books a
Crontinental job that raises the message to urgent if it's still unread then.

Messages are signed with your ssh key (sign_key in the config) and checked
against your own allowed_signers: inbox, pull and show say whether each one
is verified, unsigned, can't be checked, or BAD (forged or changed). On a
machine whose administrator put a list at /etc/deaddrop/allowed_signers,
that's the only one checked.

Never put a secret in a message: name where it lives instead.

The store is ~/.local/share/deaddrop ($DEADDROP_DIR); the config is
~/.config/deaddrop/config.toml ($DEADDROP_CONFIG). With a host set
($DEADDROP_HOST, or host in the config) every command runs on that machine
over ssh, as you.
`

// usageError is a command used wrong: what was wrong first, then the usage.
type usageError struct{ msg, reason string }

func (e usageError) Error() string {
	if e.reason != "" {
		return "deaddrop: " + e.reason + "\nusage: deaddrop " + e.msg
	}
	return "usage: deaddrop " + e.msg
}

func bad(usage string, err error) error { return usageError{usage, err.Error()} }

func badf(usage, format string, a ...any) error {
	return usageError{usage, fmt.Sprintf(format, a...)}
}

// exitError ends the process with a code of its own, after saying msg, if any.
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--help" || a == "-h" {
			fmt.Print(usage)
			return
		}
	}
	err := dispatch(cmd, takeCached(args))
	if err == nil {
		return
	}
	var ee exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(os.Stderr, "deaddrop: "+ee.msg)
		}
		os.Exit(ee.code)
	}
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintln(os.Stderr, ue.Error())
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "deaddrop: "+err.Error())
	os.Exit(1)
}

func dispatch(cmd string, args []string) error {
	defer saveLedger()
	defer saveVCache()
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println(versionString())
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	case "upgrade":
		return cmdUpgrade(args)
	}
	// These run on the caller's machine, and reach the store the way any
	// other command does.
	if run, ok := here[cmd]; ok {
		return run(withFor(cmd, args))
	}
	run, ok := commands[cmd]
	if !ok {
		return badf("<command> ...", "no command %q (deaddrop help lists them)", cmd)
	}
	// Everything that touches the store runs where the store is.
	host, err := remoteHost()
	if err != nil {
		return err
	}
	// A send is signed here, on the sender's own machine, before it goes.
	args = withPersona(cmd, args)
	args = withFor(cmd, args)
	if cmd, args, err = signOutgoing(cmd, args); err != nil {
		return err
	}
	if cmd == "send" {
		run = commands["send"]
	}
	// What's read is checked here, on the reader's own machine: these fetch
	// the messages from the store as they are, and check and show them here.
	if host != "" && hops() == 0 && readsHere[cmd] {
		viaHost = host
		return run(args)
	}
	if host != "" {
		err = runRemote(host, cmd, args)
		mirrorChanged() // whatever it did, kept answers may be out of date now
	} else if err = arrivedHere(cmd); err == nil {
		err = run(args)
	}
	if err == nil && cmd == "mark" {
		refreshInbox(args)
	}
	return err
}

// arrivedHere checks a command that came over ssh and is about to run on this
// machine. A machine that would send its own commands elsewhere isn't the
// store, so it says to make it a relay rather than answer from an empty
// store; and only send and reply may start a store that isn't there.
func arrivedHere(cmd string) error {
	if hops() == 0 {
		return nil
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if c.Host != "" && !c.Relay {
		return fmt.Errorf("this machine's config sends commands to %s, but this one came here over ssh: to pass it on, set relay = true in %s", c.Host, c.path)
	}
	if cmd == "send" || cmd == "reply" {
		return nil
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("there's no Dead Drop store on this machine (%s): is host pointing at the right machine?", dir)
	}
	return nil
}

// refreshInbox brings a watched inbox file up to date after a mark, so a
// viewer shows the change at once. Only for the person whose file it is, and
// never on the store's machine on someone else's behalf.
func refreshInbox(args []string) {
	if os.Getenv("DEADDROP_LOCAL") == "1" || hops() > 0 {
		return
	}
	// Every inbox file watched here, the person's and each helper's: a mark
	// by id may have moved a letter in any of them.
	me, err := whoAmI()
	if err != nil {
		return
	}
	files, _ := filepath.Glob(filepath.Join(stateDir(), "inbox*.json"))
	for _, path := range files {
		// inbox.json, inbox-<helper>.json, inbox@<box>.json, inbox@<box>@<helper>.json
		name := strings.TrimPrefix(strings.TrimSuffix(filepath.Base(path), ".json"), "inbox")
		watchBox, watchFor = "", strings.TrimPrefix(name, "-")
		if strings.HasPrefix(name, "@") {
			watchBox, watchFor, _ = strings.Cut(name[1:], "@")
		}
		in := readInbox()
		if in == nil || in.For != watchFor || (watchBox == "" && in.Box != me) || (watchBox != "" && in.Box != watchBox) {
			continue
		}
		watchPass(false)
	}
	watchBox, watchFor = "", ""
}

// readsHere are the commands whose messages are checked on the reader's
// machine when the store is elsewhere.
var readsHere = map[string]bool{"inbox": true, "ls": true, "pull": true, "show": true, "file": true, "sent": true}

// viaHost is the store's machine while a reading command runs here.
var viaHost string

var here = map[string]func([]string) error{
	"key":     cmdKey,
	"thread":  cmdThread,
	"threads": cmdThreads,
	"asks":    cmdAsks,
	"people":  cmdPeople,
	"with":    cmdWith,
	"watch":   cmdWatch,
	"open":    cmdOpen,
	"bar":     cmdBar,
}

var commands = map[string]func([]string) error{
	"send":      cmdSend,
	"reply":     cmdReply,
	"inbox":     cmdInbox,
	"ls":        cmdInbox,
	"pull":      cmdPull,
	"file":      cmdFile,
	"show":      cmdShow,
	"sent":      cmdSent,
	"mark":      cmdMark,
	"status":    cmdStatus,
	"firetower": cmdFiretower,
	"gc":        cmdGc,
	// Not for people: what a --follow-up job runs.
	"follow-up": cmdFollowUp,
}

// flags is a parsed command line: positionals and flag values. A flag given
// more than once keeps every value, in order.
type flags struct {
	pos  []string
	vals map[string][]string
}

func (f flags) has(k string) bool { _, ok := f.vals[k]; return ok }

func (f flags) get(k string) string {
	v := f.vals[k]
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}

func (f flags) all(k string) []string { return f.vals[k] }

// parseFlags reads flags anywhere among the positionals (--x v or --x=v).
// spec maps each flag to whether it takes a value.
func parseFlags(args []string, spec map[string]bool) (flags, error) {
	f := flags{vals: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			f.pos = append(f.pos, args[i+1:]...)
			return f, nil
		}
		if len(a) > 1 && a[0] == '-' && a[1] != '-' {
			return f, fmt.Errorf("unknown flag %s: flags start with --", a)
		}
		if !strings.HasPrefix(a, "--") {
			f.pos = append(f.pos, a)
			continue
		}
		k, v, hasV := strings.Cut(a[2:], "=")
		takes, ok := spec[k]
		if !ok {
			return f, fmt.Errorf("unknown flag --%s", k)
		}
		if takes && !hasV {
			if i+1 >= len(args) {
				return f, fmt.Errorf("--%s needs a value", k)
			}
			i++
			v = args[i]
			// "--tag $UNSET --urgent": the empty variable is gone and the
			// flag would swallow the next flag as its value.
			if flagLike.MatchString(v) {
				return f, fmt.Errorf("--%s needs a value, and got %s (an empty variable?)", k, v)
			}
		}
		if !takes && hasV {
			return f, fmt.Errorf("--%s takes no value", k)
		}
		if takes && strings.TrimSpace(v) == "" {
			return f, fmt.Errorf("--%s is empty", k)
		}
		f.vals[k] = append(f.vals[k], v)
	}
	return f, nil
}

var flagLike = regexp.MustCompile(`^--[a-z][a-z-]*(=.*)?$`)
