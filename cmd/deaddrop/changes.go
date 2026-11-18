package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// `deaddrop changes`: on the store's machine, a line each time a pigeonhole
// changes, for as long as whoever asked stays. It's what `wait` listens to
// when the store is elsewhere: one ssh session that stays open, instead of
// a new one every --every.
//
// It says only that something changed, never what: no id, no sender, no
// subject, no text. The wait that hears it looks at the store the usual way
// (inbox, as stored) and checks what it gets on the reader's machine, so a
// signature is never judged here, and the reader's own state of what's been
// reported stays the reader's. It reads, and writes nothing: no store, box
// or folder is made, no lock taken, no state kept.
//
// Its lines:
//
//	ready    the watch is armed: a look from now on misses nothing
//	change   something came, went or moved in the pigeonhole
//	alive    --every passed with no change: still here
//
// Lines it doesn't know a reader ignores, so more may come.

// dirSettle is how long a change is let settle before it's said: a letter
// arriving is a temp file and a rename, said once.
const dirSettle = 20 * time.Millisecond

// watchSlot watches a pigeonhole's new/ folder, or, until there is one, the
// nearest folder above it, up to the one the store is in, so the folders
// being made is a change too (and the next watch goes deeper). nil: nothing
// to watch (not even the store's folder, or no folder events on this
// system); a wait then looks every --every.
func watchSlot(dir, slot string) *dirWatch {
	dir = filepath.Clean(dir)
	p := filepath.Join(slot, "new")
	for {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			w, err := watchDir(p)
			if err != nil {
				return nil
			}
			return w
		}
		// Up to the folder the store is in: a store being made is a change.
		if p == filepath.Dir(dir) || !strings.HasPrefix(p, dir) {
			return nil
		}
		p = filepath.Dir(p)
	}
}

func cmdChanges(args []string) error {
	const u = "changes [WHO] [--for HELPER] [--every 1m]"
	f, err := parseFlags(args, map[string]bool{"for": true, "every": true})
	if err != nil {
		return bad(u, err)
	}
	// The box is checked as every read's is: behind the gate, only the
	// key's own.
	box, err := boxArg(u, f.pos)
	if err != nil {
		return err
	}
	helper, all, err := pigeonholeOf(f)
	if err != nil {
		return bad(u, err)
	}
	if all {
		return badf(u, "--for: one helper")
	}
	every := time.Minute
	if f.has("every") {
		var ok bool
		if every, ok = parseSpan(f.get("every")); !ok || every < minEvery {
			return badf(u, "--every %s: a span, not more often than every %s", f.get("every"), minEvery)
		}
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	// Someone else's box that isn't there is a typo, as for inbox.
	if me, _ := whoAmI(); box != me {
		if _, err := os.Stat(filepath.Join(dir, box)); os.IsNotExist(err) {
			return noBox(box)
		}
	}
	slot := slotDir(dir, box, helper)
	say := func(line string) bool {
		_, err := os.Stdout.WriteString(line + "\n")
		return err == nil
	}
	w := watchSlot(dir, slot)
	if !say("ready") {
		return nil
	}
	for {
		changed := true // with nothing to watch, each --every is a look
		if w != nil {
			changed = w.wait(every)
		} else {
			time.Sleep(every)
		}
		// The next watch is armed before the change is said, so whatever
		// lands after the reader's look is a change too.
		next := watchSlot(dir, slot)
		if w != nil {
			w.close()
		}
		w = next
		line := "alive"
		if changed {
			line = "change"
		}
		if !say(line) {
			return nil // the reader is gone
		}
	}
}

// changeFeed is a wait's open session to the store's `deaddrop changes`.
type changeFeed struct {
	cmd   *exec.Cmd
	lines chan string // closed when the session ends; err is set by then
	err   error
	errb  bytes.Buffer
	began time.Time
	quiet time.Duration // how long without a line before it's taken for gone
	gone  error         // set when it went quiet (err is the session's own)
}

// startFeed opens the session and waits for "ready". An error that's
// exitError with exitUnreachable is a store it can't reach (retry); any
// other means the store can't do this (look every --every instead).
func startFeed(host, box, helper string, every time.Duration) (*changeFeed, error) {
	me, err := explicitMe()
	if err != nil {
		return nil, err
	}
	if me == "" {
		return nil, errors.New("no name to ask as")
	}
	args := []string{box, "--every", fmt.Sprintf("%ds", int(every/time.Second))}
	if helper != "" {
		args = append(args, "--for", helper)
	}
	fd := &changeFeed{cmd: remoteCommand(host, me, "changes", args, nil), lines: make(chan string, 64), began: time.Now(),
		quiet: 2*every + 15*time.Second}
	out, err := fd.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	fd.cmd.Stderr = &fd.errb
	if err := fd.cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not run ssh: %v", err)
	}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			fd.lines <- sc.Text()
		}
		fd.err = fd.cmd.Wait()
		close(fd.lines)
	}()
	t := time.NewTimer(fd.quiet)
	defer t.Stop()
	for {
		select {
		case l, ok := <-fd.lines:
			if !ok {
				var ee *exec.ExitError
				if errors.As(fd.err, &ee) && ee.ExitCode() == exitUnreachable {
					return nil, exitError{exitUnreachable, "could not reach the store on " + host + " over ssh"}
				}
				return nil, fmt.Errorf("the store can't say when mail comes: %s", strings.TrimSpace(fd.errb.String()))
			}
			if l == "ready" {
				return fd, nil
			}
		case <-t.C:
			fd.stop()
			return nil, exitError{exitUnreachable, "no answer from the store on " + host + " over ssh"}
		}
	}
}

// await blocks until the store says something changed (true), or the
// session ends or goes quiet (false). Each "alive" calls alive.
func (fd *changeFeed) await(every time.Duration, alive func()) bool {
	t := time.NewTimer(fd.quiet)
	defer t.Stop()
	for {
		select {
		case l, ok := <-fd.lines:
			if !ok {
				return false
			}
			switch l {
			case "change":
				return true
			case "alive":
				alive()
				if !t.Stop() {
					<-t.C
				}
				t.Reset(fd.quiet)
			}
		case <-t.C:
			fd.stop()
			fd.gone = fmt.Errorf("no word from the store in %s", fd.quiet)
			return false
		}
	}
}

// dropped says why the session ended.
func (fd *changeFeed) dropped(host string) error {
	if fd.gone != nil {
		return fmt.Errorf("lost the store on %s (%v): reconnecting", host, fd.gone)
	}
	var ee *exec.ExitError
	if errors.As(fd.err, &ee) && ee.ExitCode() == exitUnreachable {
		return exitError{exitUnreachable, "lost the store on " + host + " over ssh: reconnecting"}
	}
	why := "it ended"
	if fd.err != nil {
		why = fd.err.Error()
	}
	return fmt.Errorf("lost the store on %s (%s): reconnecting", host, why)
}

func (fd *changeFeed) stop() {
	if fd.cmd.Process != nil {
		fd.cmd.Process.Kill()
	}
	go func() {
		for range fd.lines {
		}
	}()
}
