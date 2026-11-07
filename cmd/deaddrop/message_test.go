package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRenderParseRoundTrip(t *testing.T) {
	m := &Message{ID: "2026-11-02T153012-alice-3f9a", From: "alice", To: "bob",
		Subject: `a "quoted": [thing] # not a comment`, Tags: []string{"tool-x", "request"},
		Urgency: normal, Thread: "2026-10-30T091102-bob-77c2", FollowUp: "2026-11-04T09:00:00Z",
		Attachments: []string{"run log.txt", "a,b.csv"}, Sent: time.Date(2026, 11, 2, 15, 30, 12, 0, time.UTC),
		Body: "# Title\n\n---\nnot the header\n"}
	got, err := parseMessage(m.render(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, m)
	}
}

func TestSubjectFrom(t *testing.T) {
	for body, want := range map[string]string{
		"# Requests for v9 #\n\nbody":      "Requests for v9",
		"\n\nplain first line\n## later":   "## later"[3:],
		"---\ntitle: x\n---\nintro\n":      "intro",
		"no heading at all\nsecond line\n": "no heading at all",
	} {
		if got := subjectFrom(body); got != want {
			t.Errorf("subjectFrom(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestFollowUpJobName(t *testing.T) {
	m := &Message{ID: "2026-11-02T153012-a-very-long-sender-name-here-3f9a", Box: "bob"}
	got := followUpJob(m)
	if !strings.HasPrefix(got, "deaddrop-2026-11-02t153012-3f9a-") || len(got) > 49 {
		t.Fatal(got)
	}
	m.Pigeonhole = "planner"
	if followUpJob(m) == got {
		t.Fatal("two copies in one box, one job name")
	}
}

func TestUnknownHeaderLinesSurviveARewrite(t *testing.T) {
	in := "---\nid: 2026-11-02T153000-bob-0000\nfrom: bob\nto: alice\nsubject: \"x\"\ntags: []\nurgency: normal\nfrom_the_future: [1, 2]\nsent: 2026-11-02T15:30:00Z\n---\nbody\n"
	m, err := parseMessage([]byte(in), true)
	if err != nil {
		t.Fatal(err)
	}
	m.Urgency = "urgent"
	if out := string(m.render()); !strings.Contains(out, "from_the_future: [1, 2]\n") {
		t.Errorf("the unknown line was dropped:\n%s", out)
	}
}

func TestAPinnedListWins(t *testing.T) {
	dir := t.TempDir()
	pinned := dir + "/allowed_signers"
	was := systemSigners
	defer func() { systemSigners = was }()
	t.Setenv("DEADDROP_SIGNERS", dir+"/mine")
	systemSigners = pinned
	if signersPath() != dir+"/mine" {
		t.Fatal("no pinned list: the user's own is used")
	}
	if err := os.WriteFile(pinned, []byte("# pinned\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if signersPath() != pinned {
		t.Fatal("a pinned list must win over DEADDROP_SIGNERS and the config")
	}
}

func TestSSHReuse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DEADDROP_SSH", "")
	t.Setenv("DEADDROP_CONFIG", home+"/none.toml")
	cachedConfig = nil
	defer func() { cachedConfig = nil }()
	if got := reuseArgs(""); got != nil {
		t.Fatalf("no ~/.ssh, no reuse: %v", got)
	}
	os.Mkdir(home+"/.ssh", 0o700)
	got := strings.Join(reuseArgs(""), " ")
	if len(home)+66 < 104 && (!strings.Contains(got, "ControlMaster=auto") || !strings.Contains(got, "ConnectTimeout=10")) {
		t.Fatalf("reuse: %q", got)
	}
	t.Setenv("HOME", home+"/a%b")
	os.MkdirAll(home+"/a%b/.ssh", 0o700)
	if got := reuseArgs(""); got != nil {
		t.Fatalf("a %% in HOME: %v", got)
	}
	t.Setenv("HOME", home+"/"+strings.Repeat("x", 60))
	os.MkdirAll(home+"/"+strings.Repeat("x", 60)+"/.ssh", 0o700)
	if got := reuseArgs(""); got != nil {
		t.Fatalf("a socket path too long once ssh adds its suffix: %v", got)
	}
	t.Setenv("HOME", home)
	os.Chmod(home+"/.ssh", 0o777)
	if got := reuseArgs(""); got != nil {
		t.Fatalf("a ~/.ssh others can write: %v", got)
	}
	os.Chmod(home+"/.ssh", 0o700)
	os.WriteFile(home+"/none.toml", []byte("ssh_reuse = false\n"), 0o600)
	cachedConfig = nil
	if got := reuseArgs(""); got != nil {
		t.Fatalf("ssh_reuse = false: %v", got)
	}
}

func TestOwnSharing(t *testing.T) {
	if ownSharing("user tec\ncontrolmaster false\ncontrolpath none\n") {
		t.Fatal("ssh's defaults are nobody's own")
	}
	if !ownSharing("controlmaster auto\ncontrolpath none\n") || !ownSharing("controlmaster false\ncontrolpath ~/.ssh/cm-%C\n") {
		t.Fatal("the person's own ControlMaster or ControlPath wins")
	}
}

func TestVCacheOnlyWhereTheListIsYourOwn(t *testing.T) {
	d := t.TempDir()
	lists := t.TempDir() // the list lives outside home, so only the home chain is under test
	t.Setenv("HOME", d)
	state := d + "/.local/state/deaddrop"
	t.Setenv("DEADDROP_STATE", state)
	t.Setenv("DEADDROP_NO_VCACHE", "")
	was := systemSigners
	defer func() { systemSigners = was }()
	systemSigners = d + "/no-pin"
	list := lists + "/allowed_signers"
	os.WriteFile(list, []byte("alice ssh-ed25519 AAAA\n"), 0o644)
	if !vcacheAllowed(list) {
		t.Fatal("your own private list: the cache is yours")
	}
	// Every folder from the state folder up to home, home included: any one
	// others can write means no cache (they could swap the state folder).
	for _, dir := range []string{d + "/.local/state", d + "/.local", d} {
		os.Chmod(dir, 0o777)
		if vcacheAllowed(list) {
			t.Fatalf("%s writable by others: no cache", dir)
		}
		os.Chmod(dir, 0o755)
	}
	// A cache file that's a link: no cache.
	os.WriteFile(d+"/elsewhere.json", []byte("{}"), 0o600)
	os.Symlink(d+"/elsewhere.json", state+"/verified.json")
	if vcacheAllowed(list) {
		t.Fatal("a linked cache file: no cache")
	}
	os.Remove(state + "/verified.json")
	// A state folder outside home, plainly, by "..", or through a link.
	outside := t.TempDir()
	os.Symlink(outside, d+"/.local/out")
	for _, p := range []string{outside, d + "/../" + filepath.Base(outside), d + "/.local/out"} {
		t.Setenv("DEADDROP_STATE", p)
		if vcacheAllowed(list) {
			t.Fatalf("state folder %s leads outside home: no cache", p)
		}
	}
	// A link from a folder others can write, into the checked state folder:
	// the folder holding the link is checked too, so no cache.
	shared := d + "/shared" // inside home, so it's the folder check that refuses
	os.Mkdir(shared, 0o700)
	os.Chmod(shared, 0o777)
	os.Symlink(state, shared+"/ln")
	t.Setenv("DEADDROP_STATE", shared+"/ln")
	if vcacheAllowed(list) {
		t.Fatal("a state folder reached through a shared folder: no cache")
	}
	// A refused path outside home is never created, plainly or through a
	// link inside home that leads out.
	for _, p := range []string{outside + "/never", d + "/.local/out/never"} {
		t.Setenv("DEADDROP_STATE", p)
		vcacheAllowed(list)
	}
	if _, err := os.Stat(outside + "/never"); err == nil {
		t.Fatal("a refused state folder was created outside home")
	}
	// A save goes nowhere once the state folder no longer resolves to the
	// folder that was checked.
	t.Setenv("DEADDROP_STATE", state)
	vcacheAllowed(list)
	vcache = &verifyCache{Checks: map[string]bool{"k": true}, Owners: map[string][]string{}, dirty: true}
	t.Setenv("DEADDROP_STATE", d+"/.local/out")
	saveVCache()
	if _, err := os.Stat(state + "/verified.json"); err == nil {
		t.Fatal("a save after the state folder moved")
	}
	vcache = nil
	t.Setenv("DEADDROP_STATE", state)
	if !vcacheAllowed(list) || vcacheDir == "" {
		t.Fatal("back home: the cache, in the resolved folder")
	}
	os.WriteFile(list, []byte(`alice valid-before="20300101" ssh-ed25519 AAAA`+"\n"), 0o644)
	if vcacheAllowed(list) {
		t.Fatal("keys that expire: no cache")
	}
	os.WriteFile(list, []byte("alice ssh-ed25519 AAAA\n"), 0o644)
	os.Chmod(list, 0o666)
	if vcacheAllowed(list) {
		t.Fatal("a list others can write: no cache")
	}
	os.Chmod(list, 0o644)
	os.WriteFile(d+"/pin", []byte("alice ssh-ed25519 AAAA\n"), 0o644)
	systemSigners = d + "/pin"
	if vcacheAllowed(list) {
		t.Fatal("a pinned list (a shared login): no cache")
	}
}

func TestAFutureHeardStampHidesNothing(t *testing.T) {
	d := t.TempDir()
	t.Setenv("DEADDROP_NOW", "2026-11-02T15:30:00Z")
	os.MkdirAll(d+"/bob/new", 0o700)
	os.WriteFile(d+"/bob/.heard", nil, 0o600)
	future := now().Add(48 * time.Hour)
	os.Chtimes(d+"/bob/.heard", future, future)
	if !heardAt(d, "bob", "").IsZero() {
		t.Fatal("a stamp in the future counts as never heard")
	}
	hear(d, "bob", "")
	if got := heardAt(d, "bob", ""); !got.Equal(now().Add(-time.Second)) {
		t.Fatalf("the next read writes a true stamp, a second back: %v", got)
	}
}

func TestAPlantedVerdictIsNeverTrusted(t *testing.T) {
	d := t.TempDir()
	t.Setenv("DEADDROP_STATE", d+"/state")
	t.Setenv("DEADDROP_NO_VCACHE", "")
	t.Setenv("HOME", d)
	os.MkdirAll(d+"/state", 0o700)
	was := systemSigners
	defer func() { systemSigners = was }()
	systemSigners = d + "/no-pin"
	list := d + "/allowed_signers"
	os.WriteFile(list, []byte("alice ssh-ed25519 AAAA\n"), 0o644)
	plant := func(mode os.FileMode) {
		vcache, vcacheOKMemo, signersHashMemo = nil, map[string]bool{}, map[string]string{}
		k := vkey(signersHash(list), keygenID(), "alice", "SIG", "TEXT")
		b, _ := json.Marshal(map[string]any{"checks": map[string]bool{k: true}})
		os.MkdirAll(d+"/state", 0o700)
		os.WriteFile(d+"/state/verified.json", b, 0o600)
		os.Chmod(d+"/state/verified.json", mode)
	}
	checked := false
	check := func() bool { checked = true; return false }
	plant(0o600)
	if !cachedCheck(list, "alice", "SIG", []byte("TEXT"), check) || checked {
		t.Fatal("the person's own private cache is used")
	}
	plant(0o666)
	checked = false
	if cachedCheck(list, "alice", "SIG", []byte("TEXT"), check) || !checked {
		t.Fatal("a verdict in a cache others can write is checked again, never trusted")
	}
}
