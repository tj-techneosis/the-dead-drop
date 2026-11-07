package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// The verification cache: ssh-keygen once per letter, every time, is most of
// what reading costs (seconds, for a few hundred letters). A check that
// passed is kept under a hash of everything its answer depends on: the list
// of signers (path and bytes), the ssh-keygen that checked it, the signer
// named, the seal and the exact text signed. Only passes are kept, never
// failures, so a passing check can't be talked out of, nor a failure into.
//
// A kept "verified" is only as trustworthy as whoever can write the cache.
// So it's used only where that's already the person who controls the list:
// the list is their own file, the state folder is theirs and private, and
// neither is writable by anyone else. Under a pinned list (a shared login,
// where the login can write the state folder but not the list) there is no
// cache. Nor when the list has keys that expire (valid-before, valid-after)
// or a certificate authority, whose answers change with the clock.

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

// vcacheOK says the cache may be used with this list (see above).
var vcacheOKMemo = map[string]bool{}

func vcacheOK(signers string) bool {
	if ok, seen := vcacheOKMemo[signers]; seen {
		return ok
	}
	ok := vcacheAllowed(signers)
	vcacheOKMemo[signers] = ok
	return ok
}

func vcacheAllowed(signers string) bool {
	if os.Getenv("DEADDROP_NO_VCACHE") != "" || pinnedSigners() {
		return false
	}
	mine := func(path string) bool {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0o022 != 0 {
			return false
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		return ok && int(st.Uid) == os.Getuid()
	}
	os.MkdirAll(stateDir(), 0o700)
	if !mine(signers) || !mine(filepath.Dir(signers)) || !mine(stateDir()) {
		return false
	}
	// The cache file itself, if it's there, and every folder above the state
	// folder up to home: the person's (or root's), writable by nobody else.
	if _, err := os.Lstat(vcachePath()); err == nil && !mine(vcachePath()) {
		return false
	}
	home, _ := os.UserHomeDir()
	for d := filepath.Dir(stateDir()); d != "/" && d != "." && strings.HasPrefix(d, home) && d != home; d = filepath.Dir(d) {
		info, err := os.Stat(d)
		if err != nil || info.Mode().Perm()&0o022 != 0 {
			return false
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || (int(st.Uid) != os.Getuid() && st.Uid != 0) {
			return false
		}
	}
	b, err := os.ReadFile(signers)
	if err != nil {
		return false
	}
	low := strings.ToLower(string(b))
	return !strings.Contains(low, "valid-before") && !strings.Contains(low, "valid-after") && !strings.Contains(low, "cert-authority")
}

// keygenID is the ssh-keygen that checks: its path and its file's size and
// time, so a different one is a different key.
var keygenIDMemo string

func keygenID() string {
	if keygenIDMemo == "" {
		p := sshKeygen()
		if lp, err := exec.LookPath(p); err == nil {
			p = lp
		}
		keygenIDMemo = p
		if info, err := os.Stat(p); err == nil {
			keygenIDMemo = fmt.Sprintf("%s\x00%d\x00%d", p, info.Size(), info.ModTime().UnixNano())
		}
	}
	return keygenIDMemo
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
	if sh == "" || !vcacheOK(signers) {
		return check()
	}
	c := loadVCache()
	k := vkey(sh, keygenID(), signer, sig, string(payload))
	if c.Checks[k] {
		return true
	}
	ok := check()
	if ok {
		c.Checks[k], c.dirty = true, true
	}
	return ok
}

// cachedOwners is find-principals, cached the same way.
func cachedOwners(signers, sig string, find func() []string) []string {
	sh := signersHash(signers)
	if sh == "" || !vcacheOK(signers) {
		return find()
	}
	c := loadVCache()
	k := vkey(sh, keygenID(), sig)
	if o := c.Owners[k]; len(o) > 0 {
		return o
	}
	o := find()
	if len(o) > 0 {
		c.Owners[k], c.dirty = o, true
	}
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
	// Only passes are ever kept, so merging is a union.
	for k, v := range onDisk.Checks {
		if v {
			vcache.Checks[k] = true
		}
	}
	for k, v := range onDisk.Owners {
		if _, mine := vcache.Owners[k]; !mine && len(v) > 0 {
			vcache.Owners[k] = v
		}
	}
	// Bounded: a cache that only grows would be the slow part one day.
	if len(vcache.Checks)+len(vcache.Owners) > 20000 {
		vcache.Checks = map[string]bool{}
		vcache.Owners = map[string][]string{}
	}
	writeJSONFile(vcachePath(), vcache)
	vcache.dirty = false
}
