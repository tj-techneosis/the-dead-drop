package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The seal ledger: the reader's own record of every signed letter it has
// seen, by where it was (box and pigeonhole) and its seal. Someone who can
// write to the store can copy a signed letter under a new id, with its own
// sent line, and it would verify again as new: a replay. The store can't be
// trusted to remember, since that same someone could edit what it remembers.
// The reader can: its ledger lives on its own machine, with the inbox file.
//
// The first copy a reader sees stands. A copy with the same seal in the same
// place under another id is "delivered again". Two copies seen for the first
// time together both stay verified, with a note that one is a copy: which
// one is the original can't be told from the store, and a planted copy must
// not be able to make the real one read bad.

type sealLedger struct {
	Seen  map[string]string `json:"seen"` // place and seal -> the id first seen
	dirty bool
	fresh map[string][]*Message // first seen in this run, per key
}

var ledger *sealLedger

func ledgerPath() string { return filepath.Join(stateDir(), "seals.json") }

// sealKey is where a letter is and what seals it. The seal is read as bytes,
// so writing the same signature with other whitespace changes nothing.
func sealKey(m *Message) string {
	// The header holds the armored signature, base64'd onto one line. The
	// seal is what's inside the armor: line breaks, line endings and the
	// wrapping of either layer change nothing.
	raw := []byte(m.Sig)
	if armor, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(m.Sig), "")); err == nil {
		var body []string
		for _, l := range strings.Split(strings.ReplaceAll(string(armor), "\r", ""), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "-----") {
				body = append(body, l)
			}
		}
		if inner, err := base64.StdEncoding.DecodeString(strings.Join(body, "")); err == nil {
			raw = inner
		} else {
			raw = armor
		}
	}
	h := sha256.Sum256(raw)
	return m.Box + "/" + m.Pigeonhole + "/" + hex.EncodeToString(h[:16])
}

func loadLedger() *sealLedger {
	if ledger == nil {
		ledger = &sealLedger{Seen: map[string]string{}, fresh: map[string][]*Message{}}
		if b, err := os.ReadFile(ledgerPath()); err == nil {
			json.Unmarshal(b, ledger)
			if ledger.Seen == nil {
				ledger.Seen = map[string]string{}
			}
		}
	}
	return ledger
}

// ledgerOff: what you sent isn't mail you received, so the ledger, which
// guards a reader against replays, stays out of it.
var ledgerOff bool

// checkLedger looks a verified letter up in the reader's ledger. A record
// is the id first seen in a place, or, when several copies were first seen
// together, all of them: first-seen order proves nothing, so none of those
// becomes "the original", and only a copy that turns up later reads as
// delivered again.
func checkLedger(m *Message) {
	if ledgerOff || m.Sig == "" || m.ID == "" {
		return
	}
	l := loadLedger()
	k := sealKey(m)
	known := splitIDs(l.Seen[k])
	_, freshNow := l.fresh[k]
	switch {
	case len(known) == 0 || freshNow:
		// New to this reader: the first look at this seal, in this run.
		if !contains(known, m.ID) {
			known = append(known, m.ID)
			l.Seen[k], l.dirty = strings.Join(known, ","), true
		}
		l.fresh[k] = appendOnce(l.fresh[k], m)
	case !contains(known, m.ID):
		m.Signature, m.SignatureNote = sigBad, "the same signed letter as "+known[0]+", seen here earlier: delivered again"
		return
	}
	if len(known) > 1 {
		if !strings.Contains(m.SignatureNote, "one is a copy") {
			m.SignatureNote += "; the same letter is here more than once: one is a copy"
		}
		for _, x := range l.fresh[k] {
			if !strings.Contains(x.SignatureNote, "one is a copy") {
				x.SignatureNote += "; the same letter is here more than once: one is a copy"
			}
		}
	}
}

// ledgerFreshIDs are the ids first seen under k in this run.
func ledgerFreshIDs(k string) string {
	var ids []string
	for _, m := range ledger.fresh[k] {
		ids = append(ids, m.ID)
	}
	return strings.Join(ids, ",")
}

func splitIDs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func appendOnce(ms []*Message, m *Message) []*Message {
	for _, x := range ms {
		if x.ID == m.ID {
			return ms
		}
	}
	return append(ms, m)
}

// saveLedger writes the ledger back, if it learned anything. Merged with
// what's on disk under a lock, so two readers at once both keep theirs.
func saveLedger() {
	if ledger == nil || !ledger.dirty {
		return
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return
	}
	lock, err := os.OpenFile(filepath.Join(stateDir(), ".seals.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX) != nil {
		return
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	var onDisk sealLedger
	if b, err := os.ReadFile(ledgerPath()); err == nil {
		json.Unmarshal(b, &onDisk)
	}
	for k, ids := range onDisk.Seen {
		// What another reader saw first stays first; what this run added
		// alongside it joins it.
		merged := splitIDs(ids)
		for _, id := range splitIDs(ledger.Seen[k]) {
			if !contains(merged, id) && contains(splitIDs(ledgerFreshIDs(k)), id) {
				merged = append(merged, id)
			}
		}
		ledger.Seen[k] = strings.Join(merged, ",")
	}
	writeJSONFile(ledgerPath(), ledger)
	ledger.dirty = false
}
