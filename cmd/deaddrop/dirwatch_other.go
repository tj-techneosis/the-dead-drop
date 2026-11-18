//go:build !linux && !darwin

package main

import (
	"errors"
	"time"
)

// dirWatch: no folder events here, so a wait looks every --every.
type dirWatch struct{}

func watchDir(string) (*dirWatch, error) {
	return nil, errors.New("no folder events on this system")
}

func (w *dirWatch) wait(d time.Duration) bool { time.Sleep(d); return false }
func (w *dirWatch) close()                    {}
