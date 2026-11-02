// The Dead Drop: a shared, tagged inbox of Markdown documents. People leave
// documents in each other's boxes; jobs file reports in their owner's box; a
// working session pulls in what matters and files it away. See README.md and
// docs/design.md.
package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const usage = `deaddrop: a shared, tagged inbox of documents

Leave something:
  deaddrop send --to WHO [--subject "…"] [--tag T]... [--urgent|--fyi]
                [--reply-to ID] [--attach FILE]... [--follow-up 2d|TIME]
                [--from automation] < body.md
  deaddrop reply ID [--urgent|--fyi] [--tag T]... < body.md

Collect it:
  deaddrop inbox [WHO] [--tag T]... [--from WHO]... [--all] [--json]
  deaddrop pull  [WHO] [--tag T]... [--from WHO]... [--since 7d|DATE] [--unread] [--json]
  deaddrop file ID [NAME]          an attachment, on stdout (no NAME: list them)
  deaddrop mark read|unread|archive ID... [--box WHO]
  deaddrop mark read|unread|archive --tag T... [--from WHO]... [--box WHO]

Look after it:
  deaddrop status [--json]         per box: unread, urgent, newest subject
  deaddrop firetower [--next 6m|TIME|none]   one lookout per box, mail/<who>/inbox
  deaddrop gc [--days 30]          archived mail older than that goes

  deaddrop upgrade [--check] | deaddrop version | deaddrop help

WHO is a lowercase name; "me" is you: $DEADDROP_FROM, else "me" in the
config, else your login name. It is the default sender and the default box
for inbox, pull and mark. You send as yourself, or a job sends --from
automation; "automation" is a sender, never a box. With shared_login in the
config, the login name is never used.

The body is Markdown on stdin. With no --subject, the subject is the
document's first heading (or its first line). Several --tag on inbox, pull
and mark all have to match; several --from match any of them. pull prints
every matching message in full (unread first, oldest first within that) and
marks nothing: file them afterwards with mark. --follow-up books a
Crontinental job that raises the message to urgent if it's still unread then.

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
	err := dispatch(cmd, args)
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
	run, ok := commands[cmd]
	if !ok {
		return badf("<command> ...", "no command %q (deaddrop help lists them)", cmd)
	}
	// Everything that touches the store runs where the store is.
	if host, err := remoteHost(); err != nil {
		return err
	} else if host != "" {
		return runRemote(host, cmd, args)
	}
	return run(args)
}

var commands = map[string]func([]string) error{
	"send":      cmdSend,
	"reply":     cmdReply,
	"inbox":     cmdInbox,
	"ls":        cmdInbox,
	"pull":      cmdPull,
	"file":      cmdFile,
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
