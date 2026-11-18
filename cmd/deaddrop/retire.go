package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A retired box is one nobody reads any more: its owner moved to another box,
// or it's gone. Mail sent to it would land where no one looks, so the store
// refuses it and says where to send instead. What's already in it stays, and
// reads as ever.
//
// The list is the store's own, beside the boxes: <dir>/retired.json, one
// entry per box. Only the store changes it (retire, unretire), so it's
// administered by whoever runs commands on the store's machine: a gated key
// can't (they aren't in gateCommands).

const retireUsage = `retire BOX [--moved-to NEW] [--note "…"] | unretire BOX`

// retiredBox is one entry: where its mail goes now (if anywhere), a line
// about it, and who retired it when.
type retiredBox struct {
	MovedTo string `json:"moved_to,omitempty"`
	Note    string `json:"note,omitempty"`
	Retired string `json:"retired"`
	By      string `json:"by,omitempty"`
}

func retiredPath(dir string) string { return filepath.Join(dir, "retired.json") }

// loadRetired reads the store's list. No file is no retired boxes; a file
// that won't read is an error, so a damaged list never lets mail through
// to a box that was retired.
func loadRetired(dir string) (map[string]retiredBox, error) {
	b, err := os.ReadFile(retiredPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]retiredBox{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]retiredBox{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("%s won't read (%v): fix or remove it", retiredPath(dir), err)
	}
	return out, nil
}

// movedTo follows a retired box to the live box its mail goes to now, through
// any box that was itself retired since; "" if nowhere.
func movedTo(list map[string]retiredBox, box string) string {
	seen := map[string]bool{}
	for {
		r, ok := list[box]
		if !ok {
			return box
		}
		if r.MovedTo == "" || seen[box] {
			return ""
		}
		seen[box] = true
		box = r.MovedTo
	}
}

// retiredRefusal is what a send to a retired box is told: exit 5, since
// trying again won't change it.
func retiredRefusal(list map[string]retiredBox, box, what string) error {
	r := list[box]
	msg := box + " is retired"
	if what != "" {
		msg = what + " " + msg
	}
	if r.Note != "" {
		msg += "; " + r.Note
	}
	if to := movedTo(list, box); to != "" {
		msg += " (send to " + to + ")"
	} else if r.Note == "" {
		msg += ", and nothing takes its mail"
	}
	return exitError{exitRefused, msg}
}

// checkNotRetired refuses a delivery with any retired box in it, before
// anything is written: the whole send, naming the box.
func checkNotRetired(dir string, d draft) error {
	list, err := loadRetired(dir)
	if err != nil || len(list) == 0 {
		return err
	}
	for _, to := range d.to {
		if _, ok := list[to]; ok {
			return retiredRefusal(list, to, "")
		}
	}
	// Answers asked for at a retired box would land where nobody reads them.
	if _, ok := list[d.replyBox]; ok && d.replyBox != "" {
		return retiredRefusal(list, d.replyBox, "--reply-box:")
	}
	return nil
}

func cmdRetire(args []string) error {
	const u = retireUsage
	f, err := parseFlags(args, map[string]bool{"moved-to": true, "note": true})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) != 1 {
		return badf(u, "retire one box: its name")
	}
	box, err := retireName("retire", f.pos[0])
	if err != nil {
		return bad(u, err)
	}
	r := retiredBox{Retired: now().UTC().Format("2006-01-02T15:04:05Z")}
	if f.has("moved-to") {
		if r.MovedTo, err = retireName("--moved-to", f.get("moved-to")); err != nil {
			return bad(u, err)
		}
		if r.MovedTo == box {
			return badf(u, "--moved-to: %s can't move to itself", box)
		}
	}
	if f.has("note") {
		r.Note = cleanLine(f.get("note"), 200)
		if looksSecret(r.Note) {
			return fmt.Errorf("that note looks like it holds a secret (a key or a token): never put one in the store")
		}
	}
	if me, err := whoAmI(); err == nil {
		r.By = me
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(dir, ".retired.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	list, err := loadRetired(dir)
	if err != nil {
		return err
	}
	if r.MovedTo != "" {
		if _, ok := list[r.MovedTo]; ok {
			to := movedTo(list, r.MovedTo)
			if to == "" || to == box {
				return fmt.Errorf("%s is retired too: move %s to a box that's read", r.MovedTo, box)
			}
			return fmt.Errorf("%s is retired too: its mail goes to %s, so say --moved-to %s", r.MovedTo, to, to)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, box)); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "deaddrop: there's no box %s yet; it's retired anyway, so nothing can start one\n", box)
	}
	_, was := list[box]
	list[box] = r
	if err := writeJSONFile(retiredPath(dir), list); err != nil {
		return err
	}
	verb := "retired"
	if was {
		verb = "still retired, now"
	}
	switch {
	case r.MovedTo != "":
		fmt.Printf("%s %s: mail to it is refused, and pointed at %s\n", verb, box, r.MovedTo)
	default:
		fmt.Printf("%s %s: mail to it is refused\n", verb, box)
	}
	return nil
}

func cmdUnretire(args []string) error {
	const u = retireUsage
	f, err := parseFlags(args, map[string]bool{})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) != 1 {
		return badf(u, "unretire one box: its name")
	}
	box, err := retireName("unretire", f.pos[0])
	if err != nil {
		return bad(u, err)
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(dir, ".retired.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	list, err := loadRetired(dir)
	if err != nil {
		return err
	}
	if _, ok := list[box]; !ok {
		return fmt.Errorf("%s isn't retired", box)
	}
	delete(list, box)
	if len(list) == 0 {
		if err := os.Remove(retiredPath(dir)); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := writeJSONFile(retiredPath(dir), list); err != nil {
		return err
	}
	fmt.Printf("unretired %s: it takes mail again\n", box)
	return nil
}

// retireName is a box named to retire or move to: a real name, never "me"
// (retiring is about a box, said in full) and never automation.
func retireName(what, s string) (string, error) {
	b := strings.ToLower(strings.TrimSpace(s))
	if b == "me" {
		return "", fmt.Errorf("%s: name the box in full, not me", what)
	}
	if err := checkBox(what, b); err != nil {
		return "", err
	}
	if b == automation {
		return "", fmt.Errorf("%s: automation is a sender, not a box", what)
	}
	return b, nil
}

// retiredLines are status's footer: each retired box it shows, and where its
// mail goes now.
func retiredLines(list map[string]retiredBox, only []string) []string {
	var names []string
	for b := range list {
		if only == nil || contains(only, b) {
			names = append(names, b)
		}
	}
	sort.Strings(names)
	var out []string
	for _, b := range names {
		r := list[b]
		line := b + " is retired"
		if to := movedTo(list, b); to != "" {
			line += ", moved to " + to
		}
		if r.Note != "" {
			line += ": " + r.Note
		}
		out = append(out, line)
	}
	return out
}
