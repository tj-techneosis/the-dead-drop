//go:build linux

package main

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// dirWatch is one folder's changes, from inotify: a letter arriving or
// leaving, a folder made or removed in it.
type dirWatch struct{ f *os.File }

const dirWatchMask = syscall.IN_CREATE | syscall.IN_MOVED_TO | syscall.IN_MOVED_FROM | syscall.IN_DELETE |
	syscall.IN_DELETE_SELF | syscall.IN_MOVE_SELF | syscall.IN_ONLYDIR

func watchDir(path string) (*dirWatch, error) {
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		return nil, err
	}
	if _, err := syscall.InotifyAddWatch(fd, path, dirWatchMask); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	// Non-blocking, so Go's poller holds it and a deadline ends a read.
	return &dirWatch{os.NewFile(uintptr(fd), "inotify")}, nil
}

// wait blocks until the folder changes (true) or d passes (false).
func (w *dirWatch) wait(d time.Duration) bool {
	buf := make([]byte, 64<<10)
	w.f.SetReadDeadline(time.Now().Add(d))
	n, err := w.f.Read(buf)
	if err != nil || n == 0 {
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			time.Sleep(d) // never a loop that spins on a broken watch
		}
		return false
	}
	// One letter is a few events (its temp file, the rename): one change.
	time.Sleep(dirSettle)
	w.f.SetReadDeadline(time.Now())
	for {
		if n, err := w.f.Read(buf); err != nil || n == 0 {
			break
		}
	}
	return true
}

func (w *dirWatch) close() { w.f.Close() }
