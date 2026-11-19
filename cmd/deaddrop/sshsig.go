package main

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"
)

// Checking a signature here, without ssh-keygen. Running ssh-keygen once per
// letter is most of what reading costs where the cache of passed checks is
// off (a pinned list: a shared login), so the common answers are worked out
// in this process instead, exactly as ssh-keygen -Y verify works them out.
//
// It's a fast path, never a second opinion. It answers only when it's sure:
//
//   - "it holds": the signature is good by the key inside it, and a line of
//     the list names that key for the signer, in our namespace, valid now;
//   - "it doesn't, and nobody's key it is": the list doesn't have the key at
//     all, so ssh-keygen could neither verify it nor find who holds it.
//
// Everything else goes to ssh-keygen as before: a signature that doesn't
// hold (a forgery, a changed letter, someone else's key), and anything this
// doesn't read in full: a list with a line it doesn't understand (a
// certificate authority, an option it doesn't know, a quoted principal, a
// key type it doesn't take), a key inside the signature that isn't a plain
// ed25519, ecdsa or rsa key, an armour or a layout ssh-keygen might read
// differently, a time within a day of a key's valid-after or valid-before.
// So it never says "verified" where ssh-keygen wouldn't, and every failure
// still gets ssh-keygen's own verdict.

// signerLine is one line of an allowed_signers file, read in full.
type signerLine struct {
	principals string // the pattern list, as written
	namespaces string // "" when the line doesn't limit them
	key        []byte // the key, as its wire bytes
	after      int64  // valid-after, as Unix seconds; 0 when none
	before     int64  // valid-before; 0 when none
	hasNS      bool
}

// signerList is a whole list, or nil when any line of it isn't one this
// reads in full (then every check of it goes to ssh-keygen).
type signerList struct{ lines []signerLine }

var (
	signerListMu   sync.Mutex
	signerListMemo = map[string]*signerList{}
)

// inprocOff turns the fast path off: every check goes to ssh-keygen.
func inprocOff() bool { return os.Getenv("DEADDROP_NO_INPROC") != "" }

// loadSignerList reads the list at path as it is now (a watch that runs for
// days sees a changed list at once), parsing each text of it once.
func loadSignerList(path string) *signerList {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	signerListMu.Lock()
	defer signerListMu.Unlock()
	if l, ok := signerListMemo[string(b)]; ok {
		return l
	}
	l := parseSignerList(string(b))
	if len(signerListMemo) > 16 {
		clear(signerListMemo)
	}
	signerListMemo[string(b)] = l
	return l
}

func parseSignerList(text string) *signerList {
	l := &signerList{}
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimLeft(line, " \t")
		if t == "" || t[0] == '#' {
			continue
		}
		sl, ok := parseSignerLine(t)
		if !ok {
			return nil
		}
		l.lines = append(l.lines, sl)
	}
	return l
}

// parseSignerLine reads `principals [options] keytype base64 [comment]`.
func parseSignerLine(t string) (signerLine, bool) {
	var sl signerLine
	// Only spaces and tabs between fields, as ssh-keygen splits them; a
	// carriage return or any other space isn't read here.
	if strings.IndexFunc(t, func(r rune) bool { return unicode.IsSpace(r) && r != ' ' && r != '\t' }) >= 0 {
		return sl, false
	}
	fields := strings.Fields(t)
	if len(fields) < 3 {
		return sl, false
	}
	sl.principals = fields[0]
	if strings.ContainsAny(sl.principals, "\"\\") || len(sl.principals) >= 1024 {
		return sl, false
	}
	rest := fields[1:]
	if !plainKeyType(rest[0]) {
		// Options, then the key. Options with a space in a quoted value
		// would have been split by Fields: such a line isn't read here.
		if !parseSignerOptions(rest[0], &sl) || len(rest) < 3 {
			return sl, false
		}
		rest = rest[1:]
	}
	if !plainKeyType(rest[0]) {
		return sl, false
	}
	blob, err := base64.StdEncoding.DecodeString(rest[1])
	if err != nil {
		return sl, false
	}
	pk, err := ssh.ParsePublicKey(blob)
	if err != nil || pk.Type() != rest[0] || !keyUsable(pk) {
		return sl, false
	}
	sl.key = pk.Marshal()
	return sl, true
}

func plainKeyType(t string) bool {
	switch t {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoRSA, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
		return true
	}
	return false
}

// keyUsable leaves out what OpenSSH refuses to verify with: rsa under 1024
// bits.
func keyUsable(pk ssh.PublicKey) bool {
	if !plainKeyType(pk.Type()) {
		return false
	}
	if cpk, ok := pk.(ssh.CryptoPublicKey); ok {
		if r, ok := cpk.CryptoPublicKey().(*rsa.PublicKey); ok && r.N.BitLen() < 1024 {
			return false
		}
	}
	return true
}

// parseSignerOptions reads the options ssh-keygen knows for a list, each
// once: namespaces, valid-after and valid-before. cert-authority, or
// anything else, isn't read here.
func parseSignerOptions(opts string, sl *signerLine) bool {
	seen := map[string]bool{}
	for opts != "" {
		eq := strings.IndexByte(opts, '=')
		if eq < 0 {
			return false
		}
		name := strings.ToLower(opts[:eq])
		v := opts[eq+1:]
		if len(v) < 2 || v[0] != '"' {
			return false
		}
		end := strings.IndexByte(v[1:], '"')
		if end < 0 {
			return false
		}
		val := v[1 : 1+end]
		opts = v[2+end:]
		if opts != "" {
			if opts[0] != ',' || len(opts) == 1 {
				return false
			}
			opts = opts[1:]
		}
		if seen[name] || val == "" || len(val) >= 1024 || strings.ContainsAny(val, "\\ \t") {
			return false
		}
		seen[name] = true
		switch name {
		case "namespaces":
			sl.namespaces, sl.hasNS = val, true
		case "valid-after", "valid-before":
			at, ok := signerTime(val)
			if !ok {
				return false
			}
			if name == "valid-after" {
				sl.after = at
			} else {
				sl.before = at
			}
		default:
			return false
		}
	}
	return true
}

// signerTime reads YYYYMMDD[HHMM[SS]], with Z or UTC for UTC, else local
// time, as ssh-keygen does. A date that isn't one isn't read.
func signerTime(s string) (int64, bool) {
	loc := time.Local
	switch u := strings.ToUpper(s); {
	case strings.HasSuffix(u, "UTC") && len(s) > 3:
		s, loc = s[:len(s)-3], time.UTC
	case strings.HasSuffix(u, "Z") && len(s) > 1:
		s, loc = s[:len(s)-1], time.UTC
	}
	if strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	var layout string
	switch len(s) {
	case 8:
		layout = "20060102"
	case 12:
		layout = "200601021504"
	case 14:
		layout = "20060102150405"
	default:
		return 0, false
	}
	t, err := time.ParseInLocation(layout, s, loc)
	if err != nil || t.Unix() <= 0 {
		return 0, false
	}
	return t.Unix(), true
}

// matchPatternList is OpenSSH's match_pattern_list: 1 if s matches a
// pattern of the comma-separated list and no negated one, -1 if a negated
// one matches, 0 if none does. Case counts.
func matchPatternList(s, list string) int {
	got := 0
	for _, p := range strings.Split(list, ",") {
		neg := strings.HasPrefix(p, "!")
		if neg {
			p = p[1:]
		}
		if globMatch(s, p) {
			if neg {
				return -1
			}
			got = 1
		}
	}
	return got
}

// globMatch is OpenSSH's match_pattern: * any run, ? any one character.
func globMatch(s, p string) bool {
	for {
		if p == "" {
			return s == ""
		}
		switch p[0] {
		case '*':
			for p != "" && p[0] == '*' {
				p = p[1:]
			}
			if p == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if globMatch(s[i:], p) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
		default:
			if s == "" || s[0] != p[0] {
				return false
			}
		}
		s, p = s[1:], p[1:]
	}
}

// parsedSig is an SSHSIG read in full.
type parsedSig struct {
	key     ssh.PublicKey
	keyWire []byte
	hashAlg string
	sig     *ssh.Signature
}

// parseSSHSig reads a letter's signature (the armoured SSHSIG, base64'd
// once more), or nil if anything about it isn't exactly what ssh-keygen
// makes for our namespace.
func parseSSHSig(sig string) *parsedSig {
	armored, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return nil
	}
	const begin, end = "-----BEGIN SSH SIGNATURE-----\n", "\n-----END SSH SIGNATURE-----"
	a := string(armored)
	if !strings.HasPrefix(a, begin) {
		return nil
	}
	a = a[len(begin):]
	e := strings.Index(a, end)
	if e < 0 {
		return nil
	}
	body := a[:e]
	if strings.Trim(body, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=\n") != "" {
		return nil
	}
	blob, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body, "\n", ""))
	if err != nil || len(blob) < 10 || string(blob[:6]) != "SSHSIG" || binary.BigEndian.Uint32(blob[6:10]) != 1 {
		return nil
	}
	rest := blob[10:]
	var parts [5][]byte
	for i := range parts {
		if len(rest) < 4 {
			return nil
		}
		n := uint64(binary.BigEndian.Uint32(rest[:4]))
		if 4+n > uint64(len(rest)) {
			return nil
		}
		parts[i], rest = rest[4:4+n], rest[4+n:]
	}
	if len(rest) != 0 || string(parts[1]) != signNamespace {
		return nil
	}
	hashAlg := string(parts[3])
	if hashAlg != "sha256" && hashAlg != "sha512" {
		return nil
	}
	pk, err := ssh.ParsePublicKey(parts[0])
	if err != nil || !keyUsable(pk) {
		return nil
	}
	// The signature itself: its format and blob, and nothing after.
	s := parts[4]
	var fields [2][]byte
	for i := range fields {
		if len(s) < 4 {
			return nil
		}
		n := uint64(binary.BigEndian.Uint32(s[:4]))
		if 4+n > uint64(len(s)) {
			return nil
		}
		fields[i], s = s[4:4+n], s[4+n:]
	}
	if len(s) != 0 {
		return nil
	}
	format := string(fields[0])
	switch pk.Type() {
	case ssh.KeyAlgoRSA:
		// ssh-keygen refuses SHA-1 rsa signatures here.
		if format != ssh.KeyAlgoRSASHA256 && format != ssh.KeyAlgoRSASHA512 {
			return nil
		}
	case ssh.KeyAlgoED25519:
		if format != pk.Type() || len(fields[1]) != 64 {
			return nil
		}
	default: // ecdsa: two positive mpints, and nothing after them
		if format != pk.Type() || !twoMPInts(fields[1]) {
			return nil
		}
	}
	return &parsedSig{key: pk, keyWire: pk.Marshal(), hashAlg: hashAlg, sig: &ssh.Signature{Format: format, Blob: fields[1]}}
}

// holds says whether p is a good signature of payload by its own key.
func (p *parsedSig) holds(payload []byte) bool {
	var h []byte
	if p.hashAlg == "sha256" {
		s := sha256.Sum256(payload)
		h = s[:]
	} else {
		s := sha512.Sum512(payload)
		h = s[:]
	}
	str := func(b *bytes.Buffer, x []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(x)))
		b.Write(n[:])
		b.Write(x)
	}
	var signed bytes.Buffer
	signed.WriteString("SSHSIG")
	str(&signed, []byte(signNamespace))
	str(&signed, nil)
	str(&signed, []byte(p.hashAlg))
	str(&signed, h)
	return p.key.Verify(signed.Bytes(), p.sig) == nil
}

// keyListed says whether any line of the list has p's key.
func (l *signerList) keyListed(p *parsedSig) bool {
	for _, sl := range l.lines {
		if bytes.Equal(sl.key, p.keyWire) {
			return true
		}
	}
	return false
}

// vouches says whether a line names p's key for principal, in our
// namespace, and valid now by a margin of a day: a time closer than that is
// ssh-keygen's to call.
func (l *signerList) vouches(p *parsedSig, principal string) bool {
	t := time.Now().Unix()
	const margin = 24 * 60 * 60
	for _, sl := range l.lines {
		if !bytes.Equal(sl.key, p.keyWire) || matchPatternList(principal, sl.principals) != 1 {
			continue
		}
		if sl.hasNS && matchPatternList(signNamespace, sl.namespaces) != 1 {
			continue
		}
		if sl.after != 0 && t-sl.after < margin || sl.before != 0 && sl.before-t < margin {
			continue
		}
		return true
	}
	return false
}

// quickVerify is ssh-keygen -Y verify -f signers -I principal -n deaddrop,
// when it can be sure: (the answer, true), else (false, false).
func quickVerify(signers, principal, sig string, payload []byte) (ok, sure bool) {
	if inprocOff() {
		return false, false
	}
	l, p := loadSignerList(signers), parsedSigOf(sig)
	if l == nil || p == nil {
		return false, false
	}
	if !l.keyListed(p) {
		return false, true // nobody's key: nothing could vouch for it
	}
	if !l.vouches(p, principal) {
		return false, false // ssh-keygen gives the failure its own verdict
	}
	if !p.holds(payload) {
		return false, false
	}
	return true, true
}

// quickOwners is ssh-keygen -Y find-principals, when it can be sure: only
// that a key on no line of the list is nobody's.
func quickOwners(signers, sig string) (owners []string, sure bool) {
	if inprocOff() {
		return nil, false
	}
	l, p := loadSignerList(signers), parsedSigOf(sig)
	if l == nil || p == nil || l.keyListed(p) {
		return nil, false
	}
	return nil, true
}

var parsedSigMemo = map[string]*parsedSig{}

func parsedSigOf(sig string) *parsedSig {
	signerListMu.Lock()
	defer signerListMu.Unlock()
	if p, ok := parsedSigMemo[sig]; ok {
		return p
	}
	p := parseSSHSig(sig)
	if len(parsedSigMemo) > 4096 {
		clear(parsedSigMemo)
	}
	parsedSigMemo[sig] = p
	return p
}

// twoMPInts says b is exactly two ssh mpints, each positive and minimal.
func twoMPInts(b []byte) bool {
	for i := 0; i < 2; i++ {
		if len(b) < 4 {
			return false
		}
		n := uint64(binary.BigEndian.Uint32(b[:4]))
		if n == 0 || 4+n > uint64(len(b)) {
			return false
		}
		x := b[4 : 4+n]
		if x[0]&0x80 != 0 || x[0] == 0 && (len(x) == 1 || x[1]&0x80 == 0) {
			return false
		}
		b = b[4+n:]
	}
	return len(b) == 0
}
