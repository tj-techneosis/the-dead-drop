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
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(m.Sig), ""))
	if err != nil {
		raw = []byte(m.Sig)
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

// checkLedger looks a verified letter up in the reader's ledger.
func checkLedger(m *Message) {
	if m.Sig == "" || m.ID == "" {
		return
	}
	l := loadLedger()
	k := sealKey(m)
	if first, ok := l.Seen[k]; ok && first != m.ID {
		if _, mine := l.fresh[k]; !mine {
			m.Signature, m.SignatureNote = sigBad, "the same signed letter as "+first+", first seen here earlier: delivered again"
			return
		}
	}
	if _, ok := l.Seen[k]; !ok {
		l.Seen[k], l.dirty = m.ID, true
	}
	for _, other := range l.fresh[k] {
		if other.ID == m.ID {
			return
		}
	}
	l.fresh[k] = append(l.fresh[k], m)
	if len(l.fresh[k]) > 1 {
		// Two copies, both new to this reader: neither can be told to be the
		// original, so both say so.
		for _, x := range l.fresh[k] {
			if !strings.Contains(x.SignatureNote, "one is a copy") {
				x.SignatureNote += "; the same letter is here more than once: one is a copy"
			}
		}
	}
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
	for k, id := range onDisk.Seen {
		ledger.Seen[k] = id // the earlier record wins
	}
	writeJSONFile(ledgerPath(), ledger)
	ledger.dirty = false
}
