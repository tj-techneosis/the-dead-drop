package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The wiring for a person's own machine: `watch` keeps a local inbox file
// current and runs the new.d hooks once per new message; `show` hands out one
// message in full; `bar` and `open` are what the bundled viewers use. The
// inbox file holds no message text: a viewer fetches that with `show` when
// someone opens a message, and nothing keeps it.

// inboxVersion is the inbox file's format. Fields are added, never renamed.
const inboxVersion = 1

// InboxFile is ~/.local/state/deaddrop/inbox.json: the caller's box, as of
// checked_at. Unread first (urgent on top), then read, newest first in each.
type InboxFile struct {
	Version   int          `json:"version"`
	Box       string       `json:"box"`
	For       string       `json:"for,omitempty"` // the helper whose pigeonhole this is
	CheckedAt time.Time    `json:"checked_at"`
	OK        bool         `json:"ok"`
	Error     string       `json:"error,omitempty"`
	LastOKAt  *time.Time   `json:"last_ok_at,omitempty"`
	Unread    int          `json:"unread"`
	Urgent    int          `json:"urgent"`
	Messages  []InboxEntry `json:"messages"`
}

// InboxEntry is a message without its text. Every field is always there:
// tags and attachments as lists (maybe empty), thread as "" when it's none.
type InboxEntry struct {
	ID          string    `json:"id"`
	From        string    `json:"from"`
	Subject     string    `json:"subject"`
	Tags        []string  `json:"tags"`
	Urgency     string    `json:"urgency"`
	State       string    `json:"state"` // unread or read
	Sent        time.Time `json:"sent"`
	Thread      string    `json:"thread"`
	Attachments []string  `json:"attachments"`
	// Checked on this machine (sign.go): verified, unsigned, unknown or bad.
	Signature     string `json:"signature"`
	SignatureNote string `json:"signature_note"`
	SignedBy      string `json:"signed_by"`
	// Who spoke for the sender, if one of their assistants did ("" if not).
	Persona string `json:"persona"`
}

// stateDir is where this machine keeps the inbox file and what watch has
// announced: $DEADDROP_STATE, else $XDG_STATE_HOME/deaddrop, else
// ~/.local/state/deaddrop.
func stateDir() string {
	if d := os.Getenv("DEADDROP_STATE"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "deaddrop")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "deaddrop")
}

// watchFor is the helper a watch, or a bar, is for: "" is the person.
var watchFor string

// watchBox is the box a watch, or a bar, keeps when it isn't the caller's
// own (--box): "" is the caller's.
var watchBox string

// stateFile names a watch's file: inbox.json, inbox-<helper>.json for a
// helper's pigeonhole, inbox@<box>.json for another box, and
// inbox@<box>@<helper>.json for a pigeonhole there. "@" is never in a name,
// so the two can't be confused.
func stateFile(base string) string {
	name := base
	switch {
	case watchBox != "" && watchFor != "":
		name += "@" + watchBox + "@" + watchFor
	case watchBox != "":
		name += "@" + watchBox
	case watchFor != "":
		name += "-" + watchFor
	}
	return filepath.Join(stateDir(), name+".json")
}

func inboxPath() string     { return stateFile("inbox") }
func announcedPath() string { return stateFile("announced") }
func hooksDir() string      { return filepath.Join(filepath.Dir(configPath()), "new.d") }

// self runs this deaddrop with args and returns what it printed. It goes
// through the usual dispatch, so it reaches the store over ssh when the
// store is elsewhere, exactly as the person's own command would.
func self(args ...string) ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var out, errb bytes.Buffer
	c := exec.Command(exe, args...)
	c.Stdout, c.Stderr = &out, &errb
	// What it reads is said in its flags: a helper's DEADDROP_FOR mustn't
	// quietly point an inner call at another pigeonhole.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DEADDROP_FOR=") {
			c.Env = append(c.Env, kv)
		}
	}
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(errb.String()), "deaddrop: "))
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return out.Bytes(), nil
}

// readInbox reads the inbox file, or nil if there's none (or it's broken).
func readInbox() *InboxFile {
	b, err := os.ReadFile(inboxPath())
	if err != nil {
		return nil
	}
	var f InboxFile
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	return &f
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'), 0o600)
}

func lockState() (func(), error) {
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir(), ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// passResult is what one watch pass found.
type passResult struct {
	inbox     *InboxFile
	announced []InboxEntry // new this pass, hooks run
	baseline  bool         // the first pass: nothing announced
}

// watchPass fetches the caller's box, writes the inbox file and runs the
// hooks for mail it hasn't announced before. When the store can't be
// reached, the inbox file keeps its last list and says ok: false.
func watchPass(hooks bool) (passResult, error) {
	var r passResult
	box := watchBox
	if box == "" {
		var err error
		if box, err = whoAmI(); err != nil {
			return r, err
		}
	}
	unlock, err := lockState()
	if err != nil {
		return r, err
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()

	at := now().UTC()
	prev := readInbox()
	// The box is always named: the caller's own included, so a session's
	// default box (DEADDROP_BOX) never fills the person's own file.
	fetch := []string{"inbox", box, "--read", "--json"}
	if watchFor != "" {
		fetch = append(fetch, "--for", watchFor)
	}
	out, ferr := self(fetch...)
	var ms []Message
	if ferr == nil {
		if err := json.Unmarshal(out, &ms); err != nil {
			ferr = fmt.Errorf("the store's answer wasn't a list of messages: %v", err)
		}
	}
	if ferr != nil {
		f := &InboxFile{Version: inboxVersion, Box: box, For: watchFor, Messages: []InboxEntry{}}
		if prev != nil && prev.Box == box {
			f = prev
			f.Version = inboxVersion
		}
		f.CheckedAt, f.OK, f.Error = at, false, ferr.Error()
		r.inbox = f
		if err := writeJSONFile(inboxPath(), f); err != nil {
			return r, err
		}
		return r, ferr
	}

	f := &InboxFile{Version: inboxVersion, Box: box, For: watchFor, CheckedAt: at, OK: true, LastOKAt: &at, Messages: []InboxEntry{}}
	for _, m := range ms {
		if m.State != "unread" && m.State != "read" {
			continue
		}
		e := InboxEntry{ID: m.ID, From: m.From, Subject: m.Subject, Tags: m.Tags,
			Urgency: m.Urgency, State: m.State, Sent: m.Sent, Thread: m.Thread, Attachments: m.Attachments,
			Signature: m.Signature, SignatureNote: m.SignatureNote, SignedBy: m.SignedBy, Persona: m.Persona}
		if e.Tags == nil {
			e.Tags = []string{}
		}
		if e.Attachments == nil {
			e.Attachments = []string{}
		}
		f.Messages = append(f.Messages, e)
		if m.State == "unread" {
			f.Unread++
			if m.Urgency == urgent {
				f.Urgent++
			}
		}
	}
	sortInbox(f.Messages)
	r.inbox = f
	if err := writeJSONFile(inboxPath(), f); err != nil {
		return r, err
	}
	if !hooks {
		return r, nil
	}

	// What's been announced: every id in the box when it was last looked at.
	// The first pass announces nothing (it would be everything at once); after
	// that, an unread message not seen before is new.
	var seen struct {
		Box string   `json:"box"`
		IDs []string `json:"ids"`
	}
	b, err := os.ReadFile(announcedPath())
	first := err != nil || json.Unmarshal(b, &seen) != nil || seen.Box != box
	known := map[string]bool{}
	for _, id := range seen.IDs {
		known[id] = true
	}
	ids := []string{}
	for _, e := range f.Messages {
		ids = append(ids, e.ID)
		if !first && e.State == "unread" && !known[e.ID] {
			r.announced = append(r.announced, e)
		}
	}
	r.baseline = first
	seen.Box, seen.IDs = box, ids
	// Written before the hooks run: a hook that hangs or a pass that dies
	// halfway must not announce the same mail twice.
	if err := writeJSONFile(announcedPath(), seen); err != nil {
		return r, err
	}
	sort.Slice(r.announced, func(i, j int) bool { return r.announced[i].Sent.Before(r.announced[j].Sent) })
	// The hooks run with the lock let go: one that marks or opens mail
	// refreshes the inbox file, which takes the lock again.
	unlock()
	locked = false
	for _, e := range r.announced {
		runHooks(box, e)
	}
	return r, nil
}

func sortInbox(es []InboxEntry) {
	rank := func(e InboxEntry) int {
		switch {
		case e.State == "unread" && e.Urgency == urgent:
			return 0
		case e.State == "unread":
			return 1
		}
		return 2
	}
	sort.SliceStable(es, func(i, j int) bool {
		if ri, rj := rank(es[i]), rank(es[j]); ri != rj {
			return ri < rj
		}
		if !es[i].Sent.Equal(es[j].Sent) {
			return es[i].Sent.After(es[j].Sent)
		}
		return es[i].ID > es[j].ID
	})
}

// hookTimeout is how long one hook may take before it's stopped.
const hookTimeout = 30 * time.Second

// runHooks runs every executable in new.d for one new message, in name
// order: its fields as DEADDROP_MSG_* variables, and as JSON on stdin. Never
// the text. A hook that fails is reported and doesn't stop the others.
func runHooks(box string, e InboxEntry) {
	ents, err := os.ReadDir(hooksDir())
	if err != nil {
		return
	}
	payload, _ := json.Marshal(e)
	for _, ent := range ents {
		name := ent.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") {
			continue
		}
		path := filepath.Join(hooksDir(), name)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
		c := exec.CommandContext(ctx, path)
		c.Stdin = bytes.NewReader(payload)
		c.Stdout, c.Stderr = os.Stderr, os.Stderr
		c.Env = append(os.Environ(),
			"DEADDROP_MSG_ID="+e.ID,
			"DEADDROP_MSG_BOX="+box,
			"DEADDROP_MSG_SENDER="+e.From,
			"DEADDROP_MSG_SUBJECT="+e.Subject,
			"DEADDROP_MSG_TAGS="+strings.Join(e.Tags, ","),
			"DEADDROP_MSG_URGENCY="+e.Urgency,
			"DEADDROP_MSG_SENT="+e.Sent.UTC().Format(time.RFC3339),
			"DEADDROP_MSG_ATTACHMENTS="+fmt.Sprint(len(e.Attachments)),
			"DEADDROP_MSG_SIGNATURE="+e.Signature,
			"DEADDROP_MSG_SIGNED_BY="+e.SignedBy,
			"DEADDROP_MSG_PERSONA="+e.Persona,
			"DEADDROP_MSG_FOR="+watchFor,
			"DEADDROP_MSG_SIDE_BOX="+watchBox, // set when the watch is on a box not the caller's (--box)
		)
		if err := c.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "deaddrop: hook %s, for %s: %v\n", name, e.ID, err)
		}
		cancel()
	}
}

// setWatchFor takes a watch's or a bar's --for: one helper, never "*"; and
// its --box, one box, where the command takes one.
func setWatchFor(f flags) error {
	helper, all, err := pigeonholeOf(f)
	if err != nil {
		return err
	}
	if all {
		return fmt.Errorf("--for: one helper (a watch keeps one pigeonhole's file)")
	}
	watchFor = helper
	if f.has("box") || defaultBox() != "" {
		if len(f.all("box")) > 1 {
			return fmt.Errorf("--box: one box (a watch keeps one box's file; run one watch per box)")
		}
		b, err := resolveBox("--box", f.get("box"))
		if err != nil {
			return err
		}
		if me, err := whoAmI(); err != nil || me != b {
			watchBox = b // the caller's own box keeps its usual file
		}
	}
	return nil
}

// minEvery keeps a looping watch from hammering the store.
const minEvery = 10 * time.Second

func cmdWatch(args []string) error {
	const u = "watch [--every 1m] [--no-hooks] [--box WHO] [--for HELPER] [--mirror]"
	f, err := parseFlags(args, map[string]bool{"every": true, "no-hooks": false, "box": true, "for": true, "mirror": false})
	if err != nil {
		return bad(u, err)
	}
	if err := setWatchFor(f); err != nil {
		return bad(u, err)
	}
	if len(f.pos) > 0 {
		return badf(u, "watch takes no words of its own")
	}
	hooks := !f.has("no-hooks")
	// A watch looks at the store itself: --cached is for the readers of what
	// it keeps, never for its own look.
	os.Unsetenv("DEADDROP_CACHED")
	mirror := func() {
		if f.has("mirror") {
			if _, err := refreshMirror(); err != nil {
				fmt.Fprintf(os.Stderr, "deaddrop: the mirror: %v\n", err)
			}
		}
	}
	if !f.has("every") {
		mirror()
		r, err := watchPass(hooks)
		cleanOpened()
		if err != nil {
			return err
		}
		say(r)
		return nil
	}
	every, ok := parseSpan(f.get("every"))
	if !ok {
		return badf(u, "--every %s: a span, like 30s, 1m or 5m", f.get("every"))
	}
	if every < minEvery {
		return badf(u, "--every %s: not more often than every %s", f.get("every"), minEvery)
	}
	// The one loop in the Dead Drop, for people with no scheduler: a pass,
	// then sleep. It says something only when mail arrives or the store's
	// reachability changes.
	lastErr := ""
	for {
		mirror()
		r, err := watchPass(hooks)
		cleanOpened()
		switch {
		case err != nil && err.Error() != lastErr:
			fmt.Fprintf(os.Stderr, "%s deaddrop: %v\n", time.Now().Format("15:04:05"), err)
			lastErr = err.Error()
		case err == nil && lastErr != "":
			fmt.Printf("%s reached the store again\n", time.Now().Format("15:04:05"))
			lastErr = ""
		}
		if err == nil && (len(r.announced) > 0 || r.baseline) {
			say(r)
		}
		time.Sleep(every)
	}
}

func say(r passResult) {
	f := r.inbox
	who := f.Box
	if f.For != "" {
		who += " (for " + f.For + ")"
	}
	line := fmt.Sprintf("%s: %d unread", who, f.Unread)
	if f.Urgent > 0 {
		line += fmt.Sprintf(", %d urgent", f.Urgent)
	}
	switch {
	case r.baseline:
		line += " (first look: nothing announced)"
	case len(r.announced) > 0:
		line += fmt.Sprintf(", %d new", len(r.announced))
	}
	fmt.Println(line)
}

// ---- show ----

func cmdShow(args []string) error {
	const u = "show ID [--json|--html [--fragment]] [--box WHO] [--for HELPER]"
	f, err := parseFlags(args, map[string]bool{"json": false, "html": false, "fragment": false, "box": true, "raw": false, "for": true, "fingerprint": false, "unheard": false})
	if err != nil {
		return bad(u, err)
	}
	if f.has("fingerprint") {
		return showFingerprint(f)
	}
	if len(f.pos) != 1 {
		return badf(u, "which message?")
	}
	if f.has("json") && f.has("html") {
		return badf(u, "--json or --html, not both")
	}
	if f.has("fragment") && !f.has("html") {
		return badf(u, "--fragment goes with --html")
	}
	var m *Message
	if viaHost != "" {
		ms, err := fetchRemote("show", append([]string{f.pos[0]}, boxFlag(f)...))
		if err != nil {
			return err
		}
		m = ms[0]
		prepare(ms, false, "")
	} else {
		box, err := resolveBox("--box", f.get("box"))
		if err != nil {
			return err
		}
		dir, err := storeDir()
		if err != nil {
			return err
		}
		if m, err = findFor(dir, box, f, f.pos[0], true); err != nil {
			return err
		}
		prepare([]*Message{m}, f.has("raw"), dir)
		if !f.has("unheard") {
			hear(dir, m.Box, m.Pigeonhole)
		}
	}
	switch {
	case f.has("json"):
		return printJSON(m)
	case f.has("fragment"):
		// Just the rendered text, for a viewer that draws its own header.
		_, err := os.Stdout.WriteString(renderMarkdown(m.Body))
		return err
	case f.has("html"):
		_, err := os.Stdout.Write(messagePage(m))
		return err
	}
	fmt.Println(sigLine(m))
	_, err = os.Stdout.Write(m.display())
	return err
}

// showFingerprint prints the letter an id is, from whichever box holds it:
// only the fingerprint, never the letter, so a thread can be found by the id
// someone else's copy has.
func showFingerprint(f flags) error {
	if len(f.pos) != 1 {
		return fmt.Errorf("which id?")
	}
	if err := checkID(f.pos[0]); err != nil {
		return err
	}
	if viaHost != "" {
		out, err := mirroredBytes(viaHost, "show", []string{f.pos[0], "--fingerprint"})
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(out)
		return err
	}
	dir, err := storeDir()
	if err != nil {
		return err
	}
	for _, box := range boxes(dir) {
		if m, err := findAnywhere(dir, box, f.pos[0], true); err == nil {
			fmt.Println(letterOf(m))
			return nil
		}
	}
	return fmt.Errorf("no message %s anywhere in the store", f.pos[0])
}

// ---- open: read one in the browser ----

func openedDir() string { return filepath.Join(stateDir(), "open") }

// cleanOpened removes pages open made more than an hour ago: they hold a
// message's text, and the browser has long since read them.
func cleanOpened() {
	ents, _ := os.ReadDir(openedDir())
	for _, e := range ents {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
			os.Remove(filepath.Join(openedDir(), e.Name()))
		}
	}
}

func cmdOpen(args []string) error {
	const u = "open ID [--keep-unread] | open --thread ID | open --with WHO | open --threads  [--box WHO] [--for HELPER]"
	f, err := parseFlags(args, map[string]bool{"keep-unread": false, "thread": true, "with": true, "threads": false, "box": true, "for": true})
	if err != nil {
		return bad(u, err)
	}
	if err := setWatchFor(f); err != nil {
		return bad(u, err) // one pigeonhole, never '*': open marks what it shows
	}
	if watchBox != "" {
		// What it shows and marks is in that box: the commands it runs read
		// there by default (defaultBox).
		os.Setenv("DEADDROP_BOX", watchBox)
	}
	var page []byte
	var name, id string
	switch {
	case f.has("threads"):
		// Every thread on one page: the whole reading view.
		if len(f.pos) > 0 || f.has("thread") || f.has("with") {
			return badf(u, "--threads on its own")
		}
		if page, err = self(append([]string{"threads", "--html"}, forArgs(f)...)...); err != nil {
			return err
		}
		name = "threads"
		f.vals["keep-unread"] = []string{""}
	case f.has("thread") || f.has("with"):
		// A thread or a conversation: shown, nothing marked.
		if len(f.pos) > 0 || (f.has("thread") && f.has("with")) {
			return badf(u, "--thread ID or --with WHO, on its own")
		}
		if f.has("thread") {
			if err := checkID(f.get("thread")); err != nil {
				return bad(u, err)
			}
			page, err = self(append([]string{"thread", f.get("thread"), "--html"}, forArgs(f)...)...)
			name = "thread-" + f.get("thread")
		} else {
			who := strings.ToLower(f.get("with"))
			if err := checkBox("--with", who); err != nil {
				return bad(u, err)
			}
			page, err = self(append([]string{"with", who, "--html"}, forArgs(f)...)...)
			name = "with-" + who
		}
		if err != nil {
			return err
		}
		f.vals["keep-unread"] = []string{""}
	default:
		if len(f.pos) != 1 {
			return badf(u, "which message?")
		}
		id = f.pos[0]
		if err := checkID(id); err != nil {
			return bad(u, err)
		}
		if page, err = self(append([]string{"show", id, "--html"}, forArgs(f)...)...); err != nil {
			if strings.Contains(err.Error(), `no command "show"`) {
				return fmt.Errorf("the store's machine has a deaddrop from before show (0.2.0): upgrade it there (deaddrop upgrade)")
			}
			return err
		}
		name = id
	}
	cleanOpened()
	if err := os.MkdirAll(openedDir(), 0o700); err != nil {
		return err
	}
	if watchBox != "" {
		name = watchBox + "-" + name // two boxes' pages never share a name
	}
	path := filepath.Join(openedDir(), name+".html")
	if err := writeAtomic(path, page, 0o600); err != nil {
		return err
	}
	opener := os.Getenv("DEADDROP_OPENER")
	if opener == "" {
		opener = "xdg-open"
		if runtime.GOOS == "darwin" {
			opener = "open"
		}
	}
	if out, err := exec.Command(opener, path).CombinedOutput(); err != nil {
		return fmt.Errorf("could not open %s with %s: %s", path, opener, strings.TrimSpace(string(out)+" "+err.Error()))
	}
	if !f.has("keep-unread") {
		if _, err := self(append([]string{"mark", "read", id}, forArgs(f)...)...); err != nil {
			return err
		}
		// The viewers read the inbox file: bring it up to date now.
		watchPass(false)
	}
	return nil
}

// ---- bar: the inbox file as a menu ----

func cmdBar(args []string) error {
	const u = "bar [--format text|swiftbar] [--box WHO] [--for HELPER]"
	f, err := parseFlags(args, map[string]bool{"format": true, "box": true, "for": true})
	if err != nil {
		return bad(u, err)
	}
	if err := setWatchFor(f); err != nil {
		return bad(u, err)
	}
	if len(f.pos) > 0 {
		return badf(u, "bar takes no words of its own")
	}
	in := readInbox()
	switch f.get("format") {
	case "", "text":
		fmt.Println(barText(in))
		return nil
	case "swiftbar", "xbar":
		bin, _ := stableBin()
		fmt.Print(swiftbar(in, bin))
		return nil
	}
	return badf(u, "--format %s: text or swiftbar", f.get("format"))
}

// watchArgs is the --for a helper's bar passes on to what it runs.
func watchArgs() []string {
	var a []string
	if watchBox != "" {
		a = append(a, "--box", watchBox)
	}
	if watchFor != "" {
		a = append(a, "--for", watchFor)
	}
	return a
}

// staleAfter is how old a check can be before the bar says so.
const staleAfter = 10 * time.Minute

func barText(in *InboxFile) string {
	if in == nil {
		if watchFor != "" {
			return "Not watching " + watchFor + "'s pigeonhole yet (deaddrop watch " + strings.Join(watchArgs(), " ") + ")."
		}
		if watchBox != "" {
			return "Not watching " + watchBox + " yet (deaddrop watch --box " + watchBox + ")."
		}
		return "Not watching yet (deaddrop watch)."
	}
	s := "No mail."
	switch {
	case in.Urgent > 0:
		s = fmt.Sprintf("%d urgent", in.Urgent)
		if in.Unread > in.Urgent {
			s += fmt.Sprintf(", %d unread", in.Unread)
		}
	case in.Unread > 0:
		s = fmt.Sprintf("%d unread", in.Unread)
	}
	if in.For != "" {
		s = "for " + in.For + ": " + s
	}
	if watchBox != "" {
		s = in.Box + ": " + s
	}
	if !in.OK {
		s += " (can't reach the store)"
	} else if now().Sub(in.CheckedAt) > staleAfter {
		s += " (as of " + ago(in.CheckedAt, now()) + ")"
	}
	return s
}

// menuText makes text safe for one SwiftBar line: no newlines, no "|"
// (which starts its parameters), no leading "-" (which makes submenus).
func menuText(s string, n int) string {
	s = strings.ReplaceAll(cleanLine(s, n), "|", "¦")
	return strings.TrimLeft(s, "- ")
}

func swiftbar(in *InboxFile, bin string) string {
	var b strings.Builder
	// SwiftBar reads double-quoted values; ids, words and an installed
	// path never hold a double quote.
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, "") + `"` }
	act := func(prefix, label string, args ...string) {
		fmt.Fprintf(&b, "%s%s | bash=%s", prefix, label, q(bin))
		for i, a := range args {
			fmt.Fprintf(&b, " param%d=%s", i+1, q(a))
		}
		b.WriteString(" terminal=false refresh=true\n")
	}
	plain := " | emojize=false symbolize=false"
	switch {
	case in == nil:
		b.WriteString(" | sfimage=envelope\n---\n")
		b.WriteString(menuText(barText(nil), 120) + plain + "\n")
		act("", "Check now", append([]string{"watch"}, watchArgs()...)...)
		return b.String()
	case in.Urgent > 0:
		fmt.Fprintf(&b, "%d | sfimage=envelope.badge.fill sfcolor=#e5534b color=#e5534b\n", in.Unread)
	case in.Unread > 0:
		fmt.Fprintf(&b, "%d | sfimage=envelope.badge\n", in.Unread)
	default:
		b.WriteString(" | sfimage=envelope\n")
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "Dead Drop: %s%s\n", menuText(barText(in), 120), plain)
	if !in.OK {
		fmt.Fprintf(&b, "%s | color=#e5534b emojize=false symbolize=false\n", menuText(in.Error, 120))
	} else {
		fmt.Fprintf(&b, "Checked %s | size=11%s\n", ago(in.CheckedAt, now()), strings.TrimPrefix(plain, " |"))
	}
	b.WriteString("---\n")
	shownRead := 0
	for _, e := range in.Messages {
		if e.State == "read" {
			if shownRead == 10 {
				continue
			}
			shownRead++
		}
		mark := "●"
		if e.Urgency == urgent && e.State == "unread" {
			mark = "▲"
		} else if e.State == "read" {
			mark = "○"
		}
		from := menuText(e.From, 32)
		if e.Persona != "" {
			from = menuText(e.From+", as "+e.Persona, 60)
		}
		if e.Signature != sigVerified {
			from += " (" + map[string]string{sigUnsigned: "unsigned", sigUnknown: "unchecked", sigBad: "FORGED?"}[e.Signature] + ")"
		}
		fmt.Fprintf(&b, "%s %s · %s%s\n", mark, from, menuText(e.Subject, 70), plain)
		meta := ago(e.Sent, now())
		if len(e.Tags) > 0 {
			meta += " · " + strings.Join(e.Tags, ", ")
		}
		if n := len(e.Attachments); n > 0 {
			meta += fmt.Sprintf(" · %d attached", n)
		}
		fmt.Fprintf(&b, "--%s | disabled=true%s\n", menuText(meta, 120), strings.TrimPrefix(plain, " |"))
		if e.Signature != sigVerified && e.SignatureNote != "" {
			fmt.Fprintf(&b, "--%s | disabled=true%s\n", menuText(e.SignatureNote, 120), strings.TrimPrefix(plain, " |"))
		}
		b.WriteString("-----\n")
		act("--", "Read", append([]string{"open", e.ID}, watchArgs()...)...)
		act("--", "Thread", append([]string{"open", "--thread", e.ID}, watchArgs()...)...)
		if e.State == "unread" {
			act("--", "Mark read", append([]string{"mark", "read", e.ID}, watchArgs()...)...)
		} else {
			act("--", "Mark unread", append([]string{"mark", "unread", e.ID}, watchArgs()...)...)
		}
		act("--", "Archive", append([]string{"mark", "archive", e.ID}, watchArgs()...)...)
	}
	if len(in.Messages) == 0 {
		b.WriteString("No mail" + plain + "\n")
	}
	// Conversations: everyone who has written, both sides in one page.
	var people []string
	for _, e := range in.Messages {
		if e.From != automation && e.From != in.Box && !contains(people, e.From) {
			people = append(people, e.From)
		}
	}
	if len(people) > 0 {
		b.WriteString("---\nConversations" + plain + "\n")
		sort.Strings(people)
		for _, p := range people {
			act("--", "With "+menuText(p, 32), append([]string{"open", "--with", p}, watchArgs()...)...)
		}
	}
	b.WriteString("---\n")
	act("", "Check now", append([]string{"watch"}, watchArgs()...)...)
	return b.String()
}
