//go:build darwin

package main

import (
	"syscall"
	"time"
)

// dirWatch is one folder's changes, from kqueue: the folder written (an
// entry added, removed or renamed), or the folder itself removed.
type dirWatch struct{ kq, fd int }

func watchDir(path string) (*dirWatch, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	kq, err := syscall.Kqueue()
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	syscall.CloseOnExec(kq)
	var ev syscall.Kevent_t
	syscall.SetKevent(&ev, fd, syscall.EVFILT_VNODE, syscall.EV_ADD|syscall.EV_CLEAR)
	ev.Fflags = syscall.NOTE_WRITE | syscall.NOTE_DELETE | syscall.NOTE_RENAME | syscall.NOTE_REVOKE
	if _, err := syscall.Kevent(kq, []syscall.Kevent_t{ev}, nil, nil); err != nil {
		syscall.Close(kq)
		syscall.Close(fd)
		return nil, err
	}
	return &dirWatch{kq, fd}, nil
}

// wait blocks until the folder changes (true) or d passes (false).
func (w *dirWatch) wait(d time.Duration) bool {
	evs := make([]syscall.Kevent_t, 8)
	end := time.Now().Add(d)
	for {
		left := time.Until(end)
		if left <= 0 {
			return false
		}
		ts := syscall.NsecToTimespec(int64(left))
		n, err := syscall.Kevent(w.kq, nil, evs, &ts)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			time.Sleep(left) // never a loop that spins on a broken watch
			return false
		}
		if n == 0 {
			return false
		}
		break
	}
	// One letter is a few events (its temp file, the rename): one change.
	time.Sleep(dirSettle)
	var zero syscall.Timespec
	for {
		if n, err := syscall.Kevent(w.kq, nil, evs, &zero); err != nil || n <= 0 {
			break
		}
	}
	return true
}

func (w *dirWatch) close() {
	syscall.Close(w.kq)
	syscall.Close(w.fd)
}
