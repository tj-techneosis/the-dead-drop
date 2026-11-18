package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A box exists once something's been sent to it, so a mistyped name (a
// short form of someone's box, a word left off) used to start a box nobody
// reads, and the mail sat there. Now a send to a box that has never existed
// is refused, naming the boxes it most likely meant; --new-box starts one on
// purpose. The sender's own box needs no --new-box ("me" can't be
// mistyped), and nor does a reply (its box comes from the letter).
//
// Behind the gate the store says nothing about boxes the key can't read,
// not even whether they exist, so a gated key's send to such a box goes
// through as before, unless the store's config says box names aren't
// private (box_names_public = true): then it's refused with hints like
// anyone's.

// checkBoxesExist refuses a delivery to any box that has never existed,
// before anything is written: the whole send. Exit 4, a box nothing was
// ever sent to.
func checkBoxesExist(dir string, d draft) error {
	if d.newBox {
		return nil
	}
	public := false
	if gateRead != nil {
		if c, err := loadConfig(); err == nil {
			public = c.BoxNamesPublic
		}
	}
	for _, to := range d.to {
		if to == d.from || boxExists(dir, to) {
			continue
		}
		if !gateReadable(to) && !public {
			continue // nothing is said about a box this key can't read
		}
		msg := fmt.Sprintf("no box %q", to)
		known := allBoxes(dir)
		if gateRead != nil && !public {
			known = readableOnly(known)
		}
		if near := nearBoxes(to, known); len(near) > 0 {
			msg += "; did you mean " + orList(near) + "?"
			msg += fmt.Sprintf(" (--new-box starts %s)", to)
		} else {
			msg += ": nothing's ever been sent to it (a typo? --new-box starts it)"
		}
		return exitError{exitNoBox, msg}
	}
	return nil
}

func boxExists(dir, box string) bool {
	info, err := os.Stat(filepath.Join(dir, box))
	return err == nil && info.IsDir()
}

// allBoxes is every box folder in the store, whatever the gate reads, and
// none of the retired ones (mail can't go there, so they're never a hint).
func allBoxes(dir string) []string {
	retired, _ := loadRetired(dir)
	var out []string
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if _, gone := retired[e.Name()]; e.IsDir() && boxRE.MatchString(e.Name()) && e.Name() != automation && !gone {
			out = append(out, e.Name())
		}
	}
	return out
}

func readableOnly(bs []string) []string {
	var out []string
	for _, b := range bs {
		if gateReadable(b) {
			out = append(out, b)
		}
	}
	return out
}

// nearBoxes are the boxes a mistyped name most likely meant, best first, at
// most three: one that has the name as a whole part (keeper in team-keeper,
// or keeper-desk), then one that starts or ends with it (or the other way
// round: alice for alice-helper), then those a few edits away.
func nearBoxes(name string, known []string) []string {
	type cand struct {
		box   string
		score int
	}
	var cs []cand
	for _, b := range known {
		if b == name {
			continue
		}
		parts := strings.FieldsFunc(b, func(r rune) bool { return r == '-' || r == '_' })
		whole := false
		for _, p := range parts {
			if p == name {
				whole = true
			}
		}
		switch {
		case whole:
			cs = append(cs, cand{b, 0})
		case strings.HasSuffix(b, name) || strings.HasPrefix(b, name),
			strings.HasPrefix(name, b+"-") || strings.HasSuffix(name, "-"+b):
			cs = append(cs, cand{b, 1})
		default:
			d := editDistance(name, b)
			if limit := max(1, min(len(name), len(b))/4); d <= limit {
				cs = append(cs, cand{b, 1 + d})
			}
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].score != cs[j].score {
			return cs[i].score < cs[j].score
		}
		return cs[i].box < cs[j].box
	})
	var out []string
	for i := 0; i < len(cs) && i < 3; i++ {
		out = append(out, cs[i].box)
	}
	return out
}

// editDistance is Levenshtein's: inserts, deletes and changes, one each.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func orList(xs []string) string {
	if len(xs) == 1 {
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " or " + xs[len(xs)-1]
}
