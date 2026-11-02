package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Remote mode: on a machine without the store, every command runs on the
// store's machine over ssh. Bodies and attachments go over stdin, output
// comes back on stdout, and nothing is written on this side.
//
// Everyone may log in to the store's machine as the same user, so its login
// name says nothing about who is asking: the caller's own name always goes
// across, and there is no remote mode without one.

func remoteHost() (string, error) {
	if os.Getenv("DEADDROP_LOCAL") == "1" {
		return "", nil
	}
	if h := os.Getenv("DEADDROP_HOST"); h != "" {
		return h, nil
	}
	c, err := loadConfig()
	if err != nil {
		return "", err
	}
	return c.Host, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func runRemote(host, cmd string, args []string) error {
	me, err := explicitMe()
	if err != nil {
		return err
	}
	if me == "" {
		return fmt.Errorf("the store is on %s, where everyone may be the same login, so say who you are: set DEADDROP_FROM, or me in %s", host, configPath())
	}
	if strings.HasPrefix(host, "-") {
		return fmt.Errorf("host %q is not a host", host)
	}

	var stdin io.Reader
	// send's attachments are files here: they go across with the body as one
	// stream, and the far side gets --bundle instead of their paths.
	if cmd == "send" {
		var attach, rest []string
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "--":
				rest = append(rest, args[i:]...)
				i = len(args)
			case a == "--attach" && i+1 < len(args):
				attach = append(attach, args[i+1])
				i++
			case strings.HasPrefix(a, "--attach="):
				attach = append(attach, strings.TrimPrefix(a, "--attach="))
			case a == "--bundle":
				return fmt.Errorf("unknown flag --bundle")
			default:
				rest = append(rest, a)
			}
		}
		if len(attach) > 0 {
			body, err := readBody()
			if err != nil {
				return err
			}
			for _, p := range attach {
				if info, err := os.Stat(p); err != nil {
					return fmt.Errorf("--attach %s: %v", p, err)
				} else if !info.Mode().IsRegular() {
					return fmt.Errorf("--attach %s: not a file", p)
				}
			}
			pr, pw := io.Pipe()
			go func() { pw.CloseWithError(bundle(pw, body, attach)) }()
			stdin = pr
			args = append(rest, "--bundle")
		}
	}
	if stdin == nil && (cmd == "send" || cmd == "reply") {
		stdin = os.Stdin
	}

	bin := os.Getenv("DEADDROP_REMOTE_BIN")
	if bin == "" {
		c, _ := loadConfig()
		if c != nil {
			bin = c.RemoteBin
		}
	}
	line := `d="$HOME/.local/bin/deaddrop"; [ -x "$d" ] || d=deaddrop; `
	if bin != "" {
		line = "d=" + shellQuote(bin) + "; "
	}
	line += "DEADDROP_LOCAL=1 DEADDROP_FROM=" + shellQuote(me) + ` exec "$d" ` + shellQuote(cmd)
	for _, a := range args {
		line += " " + shellQuote(a)
	}

	ssh := os.Getenv("DEADDROP_SSH")
	if ssh == "" {
		ssh = "ssh"
	}
	sshArgs := []string{"-T", "-o", "BatchMode=yes"}
	if stdin == nil {
		sshArgs = append(sshArgs, "-n")
	}
	sshArgs = append(sshArgs, host, line)
	c := exec.Command(ssh, sshArgs...)
	c.Stdin, c.Stdout, c.Stderr = stdin, os.Stdout, os.Stderr
	err = c.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ee) && ee.ExitCode() == 255:
		return exitError{255, "could not reach the store on " + host + " over ssh"}
	case errors.As(err, &ee) && ee.ExitCode() > 0:
		// The far side said what was wrong.
		return exitError{ee.ExitCode(), ""}
	}
	return fmt.Errorf("could not run ssh: %v", err)
}
