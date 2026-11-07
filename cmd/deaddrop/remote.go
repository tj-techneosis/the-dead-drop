package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Remote mode: on a machine without the store, every command runs on the
// store's machine over ssh. Bodies and attachments go over stdin, output
// comes back on stdout, and nothing is written on this side (but the inbox
// file, if the person runs watch: see watch.go).
//
// The far side is told DEADDROP_HOPS (how many machines the command has come
// through) and runs the command itself, whatever its own host says, unless
// its config says relay = true: then it passes the command on to its host.
// That's for a machine you can only reach through another.
//
// Everyone may log in to the store's machine as the same user, so its login
// name says nothing about who is asking: the caller's own name always goes
// across, and there is no remote mode without one.

// maxHops is how many machines a command may pass through on its way to the
// store: enough for a relay or two, and an end to a loop.
const maxHops = 3

// hops is how many machines this command has come through over ssh: 0 when
// a person or a job ran it here.
func hops() int {
	n, _ := strconv.Atoi(os.Getenv("DEADDROP_HOPS"))
	return n
}

func remoteHost() (string, error) {
	if os.Getenv("DEADDROP_LOCAL") == "1" {
		return "", nil
	}
	c, err := loadConfig()
	if err != nil {
		return "", err
	}
	if hops() > 0 {
		// It came over ssh. It runs here, unless this machine is a relay:
		// then it goes on to this machine's own host, as the same person.
		if !c.Relay || c.Host == "" {
			return "", nil
		}
		if hops() >= maxHops {
			return "", fmt.Errorf("this command has come through %d machines already; is a relay pointing back at itself? (relay in %s)", hops(), c.path)
		}
		return c.Host, nil
	}
	if h := os.Getenv("DEADDROP_HOST"); h != "" {
		return h, nil
	}
	return c.Host, nil
}

// reuseArgs keep one ssh connection to the store open for a minute and run
// each command over it, so a click costs a round trip, not a handshake. It
// steps aside whenever the person's own ssh setup speaks for the host (their
// own ControlMaster or ControlPath, which our options would otherwise
// override), on a relay, when $DEADDROP_SSH names a command of its own, with
// ssh_reuse = false, when ~/.ssh isn't a private folder of the person's own,
// and when the socket's path would be too long for one.
func reuseArgs(host string) []string {
	if os.Getenv("DEADDROP_SSH") != "" || hops() > 0 {
		return nil
	}
	if c, err := loadConfig(); err == nil && c.SSHReuse != nil && !*c.SSHReuse {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	dir := filepath.Join(home, ".ssh")
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return nil
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return nil
	}
	path := filepath.Join(dir, "dd-%C") // %C is a 40-character hash
	// ssh first binds the master at the path plus "." and 16 random
	// characters; 104 bytes is the smallest a unix socket's path may be.
	if len(dir)+len("/dd-")+40+17 >= 104 {
		return nil
	}
	if strings.ContainsAny(dir, "% \t\"'\\") {
		return nil // ssh would read these in ControlPath as its own
	}
	if host != "" && sshSpeaksFor(host) {
		return nil
	}
	return []string{"-o", "ControlMaster=auto", "-o", "ControlPath=" + path, "-o", "ControlPersist=60",
		"-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2"}
}

// sshSpeaksFor says the person's own ssh config already sets connection
// sharing for host: ssh -G prints what it would use, without connecting.
func sshSpeaksFor(host string) bool {
	out, err := exec.Command("ssh", "-G", host).Output()
	if err != nil {
		return true // can't tell: leave their setup alone
	}
	return ownSharing(string(out))
}

// ownSharing reads ssh -G's output for a ControlMaster or ControlPath of
// the person's own.
func ownSharing(g string) bool {
	for _, line := range strings.Split(g, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch strings.ToLower(k) {
		case "controlmaster":
			if v != "false" && v != "no" {
				return true
			}
		case "controlpath":
			if v != "none" && v != "" {
				return true
			}
		}
	}
	return false
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// tooOld is what a store from before 0.3 is told when it's handed a
// signed message or asked for one as it is.
const tooOld = "the store's machine has a deaddrop from before 0.3, which can't take or hand out signed messages: upgrade it there first (deaddrop upgrade)"

func runRemote(host, cmd string, args []string) error {
	if cmd != "send" || !hasFlag(args, "signed-by") {
		return runRemoteTo(host, cmd, args, os.Stdout, os.Stderr)
	}
	// A signed send: an older store only says it doesn't know the flags.
	var errb bytes.Buffer
	err := runRemoteTo(host, cmd, args, os.Stdout, &errb)
	if err != nil && strings.Contains(errb.String(), "unknown flag --signed-by") {
		return errors.New(tooOld)
	}
	os.Stderr.Write(errb.Bytes())
	return err
}

// remoteBytes runs a command on the store's machine and returns what it
// printed; what it said on stderr becomes the error.
func remoteBytes(host, cmd string, args []string) ([]byte, error) {
	var out, errb bytes.Buffer
	err := runRemoteTo(host, cmd, args, &out, &errb)
	if err != nil {
		msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(errb.String()), "deaddrop: "))
		var ee exitError
		if errors.As(err, &ee) && ee.msg != "" {
			msg = strings.TrimSpace(ee.msg + "\n" + msg)
		}
		if strings.Contains(msg, "unknown flag --raw") {
			msg = tooOld
		}
		if msg == "" {
			msg = err.Error()
		}
		if errors.As(err, &ee) && (ee.code == exitNoBox || ee.code == exitRefused || ee.code == exitUnreachable) {
			return nil, exitError{ee.code, msg} // a job tells these apart by their code
		}
		return nil, errors.New(msg)
	}
	return out.Bytes(), nil
}

func runRemoteTo(host, cmd string, args []string, stdout, stderr io.Writer) error {
	me, err := explicitMe()
	if err != nil {
		return err
	}
	if me == "" {
		return exitError{exitRefused, fmt.Sprintf("the store is on %s, where everyone may be the same login, so say who you are: set DEADDROP_FROM, or me in %s", host, configPath())}
	}
	if strings.HasPrefix(host, "-") {
		return fmt.Errorf("host %q is not a host", host)
	}

	var stdin io.Reader
	relaying := hops() > 0
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
			case a == "--bundle" && relaying:
				// Already one stream, from the machine before: pass it on.
				rest = append(rest, a)
				stdin = os.Stdin
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
		if presetBody != nil {
			stdin = strings.NewReader(*presetBody)
		}
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
	line += fmt.Sprintf("DEADDROP_HOPS=%d DEADDROP_FROM=%s exec \"$d\" %s", hops()+1, shellQuote(me), shellQuote(cmd))
	for _, a := range args {
		line += " " + shellQuote(a)
	}

	ssh := os.Getenv("DEADDROP_SSH")
	if ssh == "" {
		ssh = "ssh"
	}
	sshArgs := []string{"-T", "-o", "BatchMode=yes"}
	sshArgs = append(sshArgs, reuseArgs(host)...)
	if stdin == nil {
		sshArgs = append(sshArgs, "-n")
	}
	sshArgs = append(sshArgs, host, line)
	c := exec.Command(ssh, sshArgs...)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	err = c.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ee) && ee.ExitCode() == exitUnreachable:
		return exitError{exitUnreachable, "could not reach the store on " + host + " over ssh"}
	case errors.As(err, &ee) && ee.ExitCode() > 0:
		// The far side said what was wrong.
		return exitError{ee.ExitCode(), ""}
	}
	return fmt.Errorf("could not run ssh: %v", err)
}
