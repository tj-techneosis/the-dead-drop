package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStderr runs f and returns what it wrote to stderr.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	f()
	w.Close()
	os.Stderr = old
	b, _ := io.ReadAll(r)
	return string(b)
}

// Behind the gate, why a ring failed is the store owner's business: the key
// gets a word, gate.log gets notify's output. (The gate runs the account's
// own crontinental, so a black-box test can't stand one in.)
func TestRingTroubleBehindTheGate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEADDROP_DIR", dir)
	detail := "could not ring: hook https://hooks.example/secret-token failed"

	gateRead = map[string]bool{"alice": true}
	got := captureStderr(t, func() { ringTrouble(detail) })
	gateRead = nil
	if strings.Contains(got, "secret-token") || !strings.Contains(got, "couldn't ring") {
		t.Errorf("a gated key was told %q", got)
	}
	logged, _ := os.ReadFile(filepath.Join(dir, "gate.log"))
	if !strings.Contains(string(logged), "secret-token") {
		t.Errorf("gate.log is missing the detail: %q", logged)
	}

	// Ungated (the owner at their own terminal): the detail itself.
	if got := captureStderr(t, func() { ringTrouble(detail) }); !strings.Contains(got, "secret-token") {
		t.Errorf("the owner wasn't told why: %q", got)
	}
}
