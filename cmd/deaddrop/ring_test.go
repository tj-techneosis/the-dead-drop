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
	if !strings.Contains(string(b), "2 more like this in the hour before") {
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

// One key adds at most gateLogPerKey lines an hour, however its words vary;
// the rest are counted and said the hour after. Another key isn't held.
func TestGateLogBudgetsEachKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEADDROP_DIR", dir)
	t.Setenv("DEADDROP_NOW", "2026-11-02T15:30:00Z")
	gateAs = "bob"
	for i := 0; i < gateLogPerKey+30; i++ {
		gateLog("could not ring to box%d", i)
	}
	gateAs = "carol"
	gateLog("carol's own trouble")
	gateAs = ""
	defer func() { gateAs = "" }()
	b, _ := os.ReadFile(filepath.Join(dir, "gate.log"))
	if n := strings.Count(string(b), "could not ring"); n != gateLogPerKey || !strings.Contains(string(b), "carol's own trouble") {
		t.Fatalf("%d lines from bob: %q", n, b)
	}
	t.Setenv("DEADDROP_NOW", "2026-11-02T16:31:00Z")
	gateAs = "bob"
	gateLog("later")
	b, _ = os.ReadFile(filepath.Join(dir, "gate.log"))
	if !strings.Contains(string(b), "30 more lines from bob held in the hour before") {
		t.Errorf("the hour after: %q", b)
	}
	if bk, _ := os.ReadFile(filepath.Join(dir, "gate.seen.json")); len(bk) > 2000 {
		t.Errorf("the book kept the hour before: %d bytes", len(bk))
	}
}

// A line a key can make long is cut, so its 20 can't roll the log over.
func TestGateLogCutsALongLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEADDROP_DIR", dir)
	gateLog("--box: %q is not a name", strings.Repeat("x", 60000))
	b, _ := os.ReadFile(filepath.Join(dir, "gate.log"))
	if len(b) > gateLogLine+100 || !strings.Contains(string(b), "(cut)") {
		t.Errorf("%d bytes", len(b))
	}
}

// A write that fails forgets nothing: the hour before's counts stay to be
// said, and the line is counted as held.
func TestGateLogFailedWriteKeepsCounts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEADDROP_DIR", dir)
	t.Setenv("DEADDROP_NOW", "2026-11-02T15:30:00Z")
	gateLog("trouble %s", "2026-11-02T153000-bob-0001")
	gateLog("trouble %s", "2026-11-02T153001-bob-0002") // held: 1 more
	log := filepath.Join(dir, "gate.log")
	os.Chmod(log, 0o400)
	t.Setenv("DEADDROP_NOW", "2026-11-02T16:31:00Z")
	gateLog("other")
	os.Chmod(log, 0o600)
	gateLog("again")
	b, _ := os.ReadFile(log)
	if strings.Count(string(b), "1 more like this") != 1 || strings.Contains(string(b), "other") {
		t.Errorf("%q", b)
	}
	// "other" never went in: counted as held, to be said the hour after.
	t.Setenv("DEADDROP_NOW", "2026-11-02T17:32:00Z")
	gateLog("later")
	b, _ = os.ReadFile(log)
	if !strings.Contains(string(b), "1 more lines from - held") {
		t.Errorf("%q", b)
	}
}

// An early 0.7.10 build's flat book: its counts carry over.
func TestGateLogReadsTheEarlyFlatBook(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEADDROP_DIR", dir)
	os.WriteFile(filepath.Join(dir, "gate.seen.json"), []byte(`{"old kind":{"first":"2026-11-02T15:00:00Z","more":3}}`), 0o600)
	t.Setenv("DEADDROP_NOW", "2026-11-02T16:31:00Z")
	gateLog("now")
	b, _ := os.ReadFile(filepath.Join(dir, "gate.log"))
	if !strings.Contains(string(b), "3 more like this in the hour before: old kind") {
		t.Errorf("%q", b)
	}
}

// A newline in an error can't make a line of its own in gate.log.
func TestGateLogKeepsOneLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEADDROP_DIR", dir)
	gateLog("could not ring: %s", "hook failed\n2026-11-02T15:30:00Z forged line\r\x1b[2J")
	b, _ := os.ReadFile(filepath.Join(dir, "gate.log"))
	if strings.Count(string(b), "\n") != 1 || strings.ContainsAny(string(b), "\r\x1b") {
		t.Errorf("%q", b)
	}
}

// A gated key's follow-ups are capped an hour, and the hour turns; ones a
// send couldn't book are given back.
func TestGateFollowUpCapTurnsWithTheHour(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEADDROP_DIR", dir)
	t.Setenv("DEADDROP_NOW", "2026-11-02T15:30:00Z")
	book := func(who string, n int) (func(int), error) { return gateBookFollowUps(who, n) }
	giveBack, err := book("bob", gateFollowUpsPerHour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := book("bob", 1); err == nil {
		t.Fatal("past the cap")
	}
	giveBack(2) // two of them weren't booked in the end
	if _, err := book("bob", 2); err != nil {
		t.Fatalf("after giving back two: %v", err)
	}
	if _, err := book("carol", 1); err != nil {
		t.Fatalf("another key: %v", err)
	}
	t.Setenv("DEADDROP_NOW", "2026-11-02T16:31:00Z")
	if _, err := book("bob", 1); err != nil {
		t.Fatalf("an hour on: %v", err)
	}
}

// A seal the gate has carried isn't carried again, while it could still
// pass the freshness check.
func TestGateCarriesASealOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEADDROP_DIR", dir)
	t.Setenv("DEADDROP_NOW", "2026-11-02T15:30:00Z")
	if err := gateSealOnce("c2lnbmF0dXJl"); err != nil {
		t.Fatal(err)
	}
	if err := gateSealOnce("c2lnbmF0dXJl"); err == nil || !strings.Contains(err.Error(), "delivered already") {
		t.Fatalf("a replay: %v", err)
	}
	if err := gateSealOnce("b3RoZXI="); err != nil {
		t.Fatalf("another seal: %v", err)
	}
	t.Setenv("DEADDROP_NOW", "2026-11-02T15:51:00Z") // past 2 × gateSignedFresh
	if err := gateSealOnce("c2lnbmF0dXJl"); err != nil {
		t.Fatalf("forgotten after the window (freshness refuses it anyway): %v", err)
	}
}

// A signature from outside the window is refused before it's checked.
func TestGateRefusesAStaleSignature(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEADDROP_DIR", dir)
	t.Setenv("DEADDROP_NOW", "2026-11-02T15:30:00Z")
	signers := filepath.Join(dir, "allowed_signers")
	os.WriteFile(signers, []byte("bob ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"), 0o600)
	t.Setenv("DEADDROP_SIGNERS", signers)
	err := gateSignatureIs("bob", "eA==", "2026-11-02T15:00:00Z", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "sign it again") {
		t.Errorf("%v", err)
	}
}
