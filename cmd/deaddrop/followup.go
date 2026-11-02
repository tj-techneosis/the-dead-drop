package main

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The Dead Drop keeps no clock. A follow-up is a one-off Crontinental job,
// booked at send time on the store's machine, that runs `deaddrop follow-up`:
// if the message is still unread it is raised to urgent, and if it was read
// in the meantime nothing happens. Marking the message read or archived
// cancels the job by its name.

func crontinental() (string, error) {
	if p := os.Getenv("DEADDROP_CRONTINENTAL"); p != "" {
		return p, nil
	}
	p, err := exec.LookPath("crontinental")
	if err != nil {
		return "", fmt.Errorf("crontinental isn't installed on this machine")
	}
	return p, nil
}

// followUpTime reads --follow-up: a span from now (2d, 90m) or a time.
func followUpTime(s string) (time.Time, error) {
	at := now()
	if d, ok := parseSpan(s); ok {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("--follow-up %q: give it some time", s)
		}
		return at.Add(d), nil
	}
	if t, ok := parseWhen(s); ok {
		if !t.After(at) {
			return time.Time{}, fmt.Errorf("--follow-up %q is already past", s)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--follow-up %q: a span (2d, 4h, 90m) or a time (\"2026-11-04 09:00\", 2026-11-04T09:00:00Z)", s)
}

// followUpJob names a message's job: deaddrop-<when>-<its random part>.
func followUpJob(id string) string {
	return "deaddrop-" + strings.ToLower(id[:17]) + "-" + id[len(id)-4:]
}

func bookFollowUp(dir string, m *Message) error {
	cron, err := crontinental()
	if err != nil {
		return err
	}
	at, err := time.Parse(time.RFC3339, m.FollowUp)
	if err != nil {
		return err
	}
	exe, err := stableBin()
	if err != nil {
		return err
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	secs := int64(math.Ceil(at.Sub(now()).Seconds()))
	if secs < 1 {
		secs = 1
	}
	args := []string{"in", fmt.Sprintf("%ds", secs), "--name", followUpJob(m.ID),
		"--notify", "never", "--catchup", "run", "--by", "deaddrop",
		"--note", "Dead Drop follow-up: raise " + m.ID + " to urgent if " + m.To + " hasn't read it",
		"--", "env", "DEADDROP_DIR=" + dir, "DEADDROP_LOCAL=1", exe, "follow-up", m.To, m.ID}
	out, err := exec.Command(cron, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("crontinental: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// stableBin is the deaddrop a job should run: the installed one, whose path
// stays put across upgrades, not whichever binary booked it (a release's
// folder is replaced on upgrade, and a checkout build moves).
func stableBin() (string, error) {
	dir := os.Getenv("DEADDROP_BIN_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "bin")
	}
	p := filepath.Join(dir, "deaddrop")
	if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		return p, nil
	}
	if p, err := exec.LookPath("deaddrop"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs, nil
		}
	}
	return os.Executable()
}

// cancelFollowUp drops a message's job once it's been read. A job that has
// already run, or is gone, is no matter.
func cancelFollowUp(m *Message) {
	if m.FollowUp == "" {
		return
	}
	if at, err := time.Parse(time.RFC3339, m.FollowUp); err != nil || !at.After(now()) {
		return
	}
	cron, err := crontinental()
	if err != nil {
		return
	}
	exec.Command(cron, "rm", followUpJob(m.ID), "--force").Run()
}

// cmdFollowUp is what the job runs: deaddrop follow-up BOX ID.
func cmdFollowUp(args []string) error {
	const u = "follow-up BOX ID (what a --follow-up job runs)"
	f, err := parseFlags(args, map[string]bool{})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) != 2 {
		return badf(u, "a box and a message id")
	}
	box, err := resolveBox("box", f.pos[0])
	if err != nil {
		return err
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	unlock, err := lockBox(dir, box)
	if err != nil {
		return err
	}
	defer unlock()
	m, err := findMessage(dir, box, f.pos[1], true)
	if err != nil {
		fmt.Println(err.Error() + "; nothing to follow up")
		return nil
	}
	switch {
	case m.State != "unread":
		fmt.Printf("%s has been %s; nothing to do\n", m.ID, m.State)
		return nil
	case m.Urgency == urgent:
		fmt.Printf("%s is unread and already urgent\n", m.ID)
		return nil
	}
	m.Urgency, m.Raised = urgent, now().Format(time.RFC3339)
	if err := writeAtomic(m.path, m.render(), 0o600); err != nil {
		return err
	}
	fmt.Printf("%s is still unread: raised to urgent\n", m.ID)
	ring(box)
	return nil
}

// ring tells the store machine's notify hooks that urgent mail is waiting,
// when the config asks for it. It never says the subject or the sender.
func ring(box string) {
	c, err := loadConfig()
	if err != nil || !c.RingUrgent {
		return
	}
	cron, err := crontinental()
	if err != nil {
		fmt.Fprintln(os.Stderr, "deaddrop: ring_urgent is on, but "+err.Error())
		return
	}
	if out, err := exec.Command(cron, "notify", "--urgent", "--title", "Dead Drop", box+": urgent mail waiting").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "deaddrop: could not ring: %s\n", strings.TrimSpace(string(out)))
	}
}
