package main

import (
	"os"
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
