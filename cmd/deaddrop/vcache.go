package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
)

// The verification cache: ssh-keygen once per letter, every time, is most of
// what reading costs (seconds, for a few hundred letters). A check's answer
// depends only on the list of signers, the signer named, the seal and the
// exact text signed, so it's kept under a hash of all four, on the reader's
// machine beside the seal ledger. A changed list, a changed letter, or a
// different signer is a different key, and is checked afresh. Like the
// ledger, it's the reader's: an account that can write the reader's state
// could poison it (the shared-login limit in design.md).

type verifyCache struct {
	Checks map[string]bool     `json:"checks"` // list, signer, seal, text -> it verified
	Owners map[string][]string `json:"owners"` // list, seal -> whose key it is
	dirty  bool
}

var vcache *verifyCache

func vcachePath() string { return filepath.Join(stateDir(), "verified.json") }

// signersHash is the list's identity: its path and every byte of it.
var signersHashMemo = map[string]string{}

func signersHash(path string) string {
	if h, ok := signersHashMemo[path]; ok {
		return h
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(append([]byte(path+"\x00"), b...))
	h := hex.EncodeToString(sum[:])
	signersHashMemo[path] = h
	return h
}

func loadVCache() *verifyCache {
	if vcache == nil {
		vcache = &verifyCache{Checks: map[string]bool{}, Owners: map[string][]string{}}
		if os.Getenv("DEADDROP_NO_VCACHE") == "" {
			if b, err := os.ReadFile(vcachePath()); err == nil {
				json.Unmarshal(b, vcache)
			}
		}
		if vcache.Checks == nil {
			vcache.Checks = map[string]bool{}
		}
		if vcache.Owners == nil {
			vcache.Owners = map[string][]string{}
		}
	}
	return vcache
}

func vkey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cachedCheck runs check unless the same list, signer, seal and text were
// checked before.
func cachedCheck(signers, signer, sig string, payload []byte, check func() bool) bool {
	sh := signersHash(signers)
	if sh == "" {
		return check()
	}
	c := loadVCache()
	k := vkey(sh, signer, sig, string(payload))
	if ok, seen := c.Checks[k]; seen {
		return ok
	}
	ok := check()
	c.Checks[k], c.dirty = ok, true
	return ok
}

// cachedOwners is find-principals, cached the same way.
func cachedOwners(signers, sig string, find func() []string) []string {
	sh := signersHash(signers)
	if sh == "" {
		return find()
	}
	c := loadVCache()
	k := vkey(sh, sig)
	if o, seen := c.Owners[k]; seen {
		return o
	}
	o := find()
	if o == nil {
		o = []string{}
	}
	c.Owners[k], c.dirty = o, true
	return o
}

// saveVCache writes what this run learned, merged with what's on disk.
func saveVCache() {
	if vcache == nil || !vcache.dirty || os.Getenv("DEADDROP_NO_VCACHE") != "" {
		return
	}
	if os.MkdirAll(stateDir(), 0o700) != nil {
		return
	}
	lock, err := os.OpenFile(filepath.Join(stateDir(), ".verified.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX) != nil {
		return
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	var onDisk verifyCache
	if b, err := os.ReadFile(vcachePath()); err == nil {
		json.Unmarshal(b, &onDisk)
	}
	for k, v := range onDisk.Checks {
		if _, mine := vcache.Checks[k]; !mine {
			vcache.Checks[k] = v
		}
	}
	for k, v := range onDisk.Owners {
		if _, mine := vcache.Owners[k]; !mine {
			vcache.Owners[k] = v
		}
	}
	// Bounded: a cache that only grows would be the slow part one day.
	if len(vcache.Checks) > 20000 {
		vcache.Checks = map[string]bool{}
		vcache.Owners = map[string][]string{}
	}
	writeJSONFile(vcachePath(), vcache)
	vcache.dirty = false
}
