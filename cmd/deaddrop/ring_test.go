package main

import (
	"io"
	"os"
	"os/exec"
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
	got := captureStderr(t, func() { ringTrouble("2026-11-02T153000-alice-0001", "bob", detail) })
	gateRead = nil
	if strings.Contains(got, "secret-token") || !strings.Contains(got, "couldn't ring") || !strings.Contains(got, "to bob") {
		t.Errorf("a gated key was told %q", got)
	}
	logged, _ := os.ReadFile(filepath.Join(dir, "gate.log"))
	if !strings.Contains(string(logged), "secret-token") {
		t.Errorf("gate.log is missing the detail: %q", logged)
	}

	// Ungated (the owner at their own terminal): the detail itself.
	if got := captureStderr(t, func() { ringTrouble("x", "bob", detail) }); !strings.Contains(got, "secret-token") {
		t.Errorf("the owner wasn't told why: %q", got)
	}
}

// A gated key past its cap isn't shown the count: its follow-ups are in it,
// and they ring only while a letter's unread.
func TestHeldRingHidesTheCountBehindTheGate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEADDROP_DIR", dir)
	cfg := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(cfg, []byte("ring_urgent = true\n"), 0o600)
	t.Setenv("DEADDROP_CONFIG", cfg)
	yes, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true on PATH")
	}
	t.Setenv("DEADDROP_CRONTINENTAL", yes)
	cachedConfig = nil
	defer func() { cachedConfig = nil }()
	m := &Message{ID: "2026-11-02T153000-bob-0001", From: "bob"}
	for i := 0; i < ringsPerBox; i++ {
		ring("carol", m, false)
	}
	gateRead = map[string]bool{"bob": true}
	got := captureStderr(t, func() { ring("carol", m, false) })
	gateRead = nil
	if !strings.Contains(got, "not rung") || strings.Contains(got, "times") {
		t.Errorf("a gated key was told %q", got)
	}
	if got := captureStderr(t, func() { ring("carol", m, false) }); !strings.Contains(got, "4 times") {
		t.Errorf("the owner wasn't told the count: %q", got)
	}
}

// gate.log never grows past its bound: it rolls over, one old one kept.
func TestGateLogIsBounded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEADDROP_DIR", dir)
	path := filepath.Join(dir, "gate.log")
	os.WriteFile(path, make([]byte, gateLogMax), 0o600)
	gateLog("after the bound")
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() != gateLogMax {
		t.Fatalf("no rolled-over log: %v", err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "after the bound") || len(b) > 100 {
		t.Errorf("the new log is %q", b)
	}
}

// An early flat rings.json with a sender named rung is still read flat.
func TestRingBookReadsAFlatFileWithASenderNamedRung(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rings.json")
	os.WriteFile(path, []byte(`{"rung":["bob 2026-11-07T10:00:00Z"],"bob":["carol 2026-11-07T10:00:00Z"]}`), 0o600)
	b := readRingBook(path)
	if len(b.Rung["bob"]) != 1 || len(b.Rung["rung"]) != 1 {
		t.Errorf("read as %+v", b)
	}
}

// A line like one written this hour isn't written again; the next one after
// the hour says how many it stood for.
func TestGateLogWritesARepeatOnceAnHour(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEADDROP_DIR", dir)
	t.Setenv("DEADDROP_NOW", "2026-11-02T15:30:00Z")
	gateLog("could not ring: %s to bob", "2026-11-02T153000-bob-0001")
	gateLog("could not ring: %s to bob", "2026-11-02T153001-bob-0002")
	gateLog("could not ring: %s to bob", "2026-11-02T153002-bob-0003")
	gateLog("something else")
	b, _ := os.ReadFile(filepath.Join(dir, "gate.log"))
	if n := strings.Count(string(b), "could not ring"); n != 1 || !strings.Contains(string(b), "something else") {
		t.Fatalf("this hour: %q", b)
	}
	t.Setenv("DEADDROP_NOW", "2026-11-02T16:31:00Z")
	gateLog("could not ring: %s to bob", "2026-11-02T163100-bob-0004")
	b, _ = os.ReadFile(filepath.Join(dir, "gate.log"))
	if !strings.Contains(string(b), "and 2 more like it") {
		t.Errorf("the hour after: %q", b)
	}
}

// A config that can't be read is still noted, in the usual place.
func TestGateLogNotesABrokenConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DEADDROP_DIR", "")
	cfg := filepath.Join(home, "broken.toml")
	os.WriteFile(cfg, []byte("this is = = not toml"), 0o600)
	t.Setenv("DEADDROP_CONFIG", cfg)
	cachedConfig = nil
	defer func() { cachedConfig = nil }()
	gateLog("the config can't be read: x")
	b, err := os.ReadFile(filepath.Join(home, ".local", "share", "deaddrop", "gate.log"))
	if err != nil || !strings.Contains(string(b), "the config can't be read") {
		t.Errorf("not noted: %v %q", err, b)
	}
}
