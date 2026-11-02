package main

import (
	"reflect"
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
	got := followUpJob("2026-11-02T153012-a-very-long-sender-name-here-3f9a")
	if got != "deaddrop-2026-11-02t153012-3f9a" || len(got) > 49 {
		t.Fatal(got)
	}
}
