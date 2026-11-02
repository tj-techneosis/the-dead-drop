package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// cmdFiretower reports one lookout per box, mail/<who>/inbox, to the
// Firetower on this machine: clear when empty, smoke when there's unread
// mail, fire when some of it is urgent. The value stands for the newest
// unread message, so the board says "moved" when mail comes in. The message
// is only a count: boards can be on screens anyone sees, so no subject and no
// sender (and so the value is a hash of the id, which names its sender).
func cmdFiretower(args []string) error {
	const u = "firetower [--next 6m|TIME|none] [--box WHO]..."
	f, err := parseFlags(args, map[string]bool{"next": true, "box": true})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) > 0 {
		return badf(u, "firetower takes no words of its own")
	}
	tower := os.Getenv("DEADDROP_FIRETOWER")
	if tower == "" {
		if tower, err = exec.LookPath("firetower"); err != nil {
			return fmt.Errorf("firetower isn't installed on this machine")
		}
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	list := boxes(dir)
	if f.has("box") {
		list = nil
		for _, b := range f.all("box") {
			b, err := resolveBox("--box", b)
			if err != nil {
				return bad(u, err)
			}
			list = append(list, b)
		}
	}
	next := f.get("next")
	if next == "" {
		// Under Crontinental, the next run's time plus a little for the run.
		if t, err := time.Parse(time.RFC3339, os.Getenv("CRONTINENTAL_NEXT")); err == nil {
			next = t.Add(5 * time.Minute).UTC().Format(time.RFC3339)
		}
	}
	var failed []string
	for _, box := range list {
		s := statusOf(dir, box)
		status, msg, value := "green", "No mail.", "none"
		switch {
		case s.Urgent > 0:
			status, msg = "red", fmt.Sprintf("%d urgent", s.Urgent)
			if s.Unread > s.Urgent {
				msg += fmt.Sprintf(", %d unread", s.Unread)
			}
		case s.Unread > 0:
			status, msg = "yellow", fmt.Sprintf("%d unread", s.Unread)
		}
		if s.Newest != nil {
			value = boardValue(s.Newest.ID)
		}
		a := []string{"set", "mail/" + box + "/inbox", status, msg, "--value", value, "--label", "Mail: " + box}
		if next != "" {
			a = append(a, "--next", next)
		}
		out, err := exec.Command(tower, a...).CombinedOutput()
		if err != nil {
			failed = append(failed, box+": "+strings.TrimSpace(string(out)))
			continue
		}
		fmt.Printf("mail/%s/inbox  %s  %s\n", box, status, msg)
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not report %s", strings.Join(failed, "; "))
	}
	if len(list) == 0 {
		fmt.Println("No boxes yet: nothing to report.")
	}
	return nil
}

// boardValue stands for a message on the board without saying who sent it.
func boardValue(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:6])
}
