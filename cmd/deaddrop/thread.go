package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// Threads and conversations: both sides of the mail, in time order. Your own
// box holds what others sent you; what you sent lives in theirs, and comes
// from `sent`. These run on your own machine and build on pull and sent, so
// every letter is checked here, as anywhere else.

// Letter is one message in a thread or a conversation.
type Letter struct {
	*Message
	Mine   bool       `json:"mine"`             // you sent it
	Copies []SentCopy `json:"copies,omitempty"` // yours: where each copy is
	Gist   string     `json:"gist"`             // its first real paragraph
}

// Correspondence is what thread and with hand out.
type Correspondence struct {
	Box     string    `json:"box"`
	With    string    `json:"with,omitempty"`   // with WHO
	Thread  string    `json:"thread,omitempty"` // the letter a thread was asked for
	Letters []*Letter `json:"letters"`
	// SentMissing says why your side isn't there, when it isn't (a store
	// from before 0.4 has no sent).
	SentMissing string `json:"sent_missing,omitempty"`
}

// gist is a letter's first real paragraph: past any heading, one line, short.
func gist(body string) string {
	var para []string
	inFence := false
	for _, l := range strings.Split(body, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			if len(para) > 0 {
				break
			}
			continue
		}
		if inFence {
			continue
		}
		switch {
		case t == "" && len(para) > 0:
			return cleanLine(strings.Join(para, " "), 280)
		case t == "" || strings.HasPrefix(t, "#") || mdRule.MatchString(t):
			continue
		}
		para = append(para, strings.TrimLeft(t, "-*+> "))
	}
	return cleanLine(strings.Join(para, " "), 280)
}

// lettersFor is the pigeonhole thread and with read: --for, or the person's.
var lettersFor []string

// letters gathers your mail, both ways: everything in your box (archived
// too) and everything you sent.
func letters() (string, []*Letter, string, error) {
	me, err := whoAmI()
	if err != nil {
		return "", nil, "", err
	}
	var notes []string
	out, err := self(append([]string{"pull", "--all", "--json"}, lettersFor...)...)
	if err != nil && strings.Contains(err.Error(), "unknown flag --for") {
		return "", nil, "", fmt.Errorf("the store's deaddrop is from before 0.6, which has no pigeonholes: upgrade it there")
	}
	if err != nil && strings.Contains(err.Error(), "unknown flag --all") {
		// A store from before 0.4: no archived mail to be had.
		notes = append(notes, "filed (archived) letters aren't shown")
		out, err = self(append([]string{"pull", "--json"}, lettersFor...)...)
	}
	if err != nil {
		return "", nil, "", err
	}
	var got []*Message
	if err := json.Unmarshal(out, &got); err != nil {
		return "", nil, "", fmt.Errorf("pull's answer wasn't a list of messages: %v", err)
	}
	var ls []*Letter
	for _, m := range got {
		ls = append(ls, &Letter{Message: m, Gist: gist(m.Body)})
	}
	out, err = self("sent", "--json")
	switch {
	case err != nil && strings.Contains(err.Error(), `no command "sent"`):
		notes = append([]string{"your side isn't shown"}, notes...)
	case err != nil:
		return "", nil, "", err
	default:
		var ss []*SentMessage
		if err := json.Unmarshal(out, &ss); err != nil {
			return "", nil, "", fmt.Errorf("sent's answer wasn't a list of messages: %v", err)
		}
		helper := ""
		if len(lettersFor) == 2 && lettersFor[1] != "*" {
			helper = lettersFor[1]
		}
		// What's in this view already, so the person's answers to it count.
		inView := map[string]bool{}
		for _, l := range ls {
			inView[l.ID] = true
			if l.Letter != "" {
				inView["letter:"+l.Letter] = true
			}
		}
		for _, s := range ss {
			if len(s.Copies) == 0 {
				continue
			}
			// A helper's view: what that helper wrote (as its persona), notes
			// the person wrote to that helper, and the person's answers to
			// letters in the helper's pigeonhole.
			if helper != "" && helperSlug(s.Persona) != helper {
				toIt := inView[s.Thread] || (s.ThreadLetter != "" && inView["letter:"+s.ThreadLetter])
				for _, c := range s.Copies {
					toIt = toIt || (c.Box == me && c.For == helper)
				}
				if !toIt {
					continue
				}
			}
			// A note to yourself that went only to your helpers' pigeonholes
			// isn't part of the plain view.
			if len(lettersFor) == 0 {
				outside := false
				for _, c := range s.Copies {
					outside = outside || !(c.Box == me && c.For != "")
				}
				if !outside {
					continue
				}
			}
			s.ID = s.Copies[0].ID
			ls = append(ls, &Letter{Message: s.Message, Mine: true, Copies: s.Copies, Gist: gist(s.Body)})
		}
	}
	// A letter you sent yourself is in your box and in what you sent: show
	// it once, as yours (it knows every copy).
	// (Ids are unique only within a box, so it's the box and the id.)
	yours := map[string]bool{}
	for _, l := range ls {
		if l.Mine {
			for _, c := range l.Copies {
				yours[c.Box+"/"+c.ID] = true
			}
		}
	}
	kept := ls[:0]
	for _, l := range ls {
		if l.Mine || !yours[me+"/"+l.ID] {
			kept = append(kept, l)
		}
	}
	ls = kept
	sortLetters(ls)
	missing := ""
	if len(notes) > 0 {
		missing = strings.Join(notes, ", and ") + ": the store's deaddrop is from before 0.4 (upgrade it there)"
	}
	return me, ls, missing, nil
}

// sortLetters puts letters in time order; within the same second, a reply
// after the letter it answers.
func sortLetters(ls []*Letter) {
	byID := map[string]*Letter{}
	for _, l := range ls {
		for _, id := range l.ids() {
			byID[id] = l
		}
	}
	depth := map[*Letter]int{}
	var depthOf func(*Letter, int) int
	depthOf = func(l *Letter, guard int) int {
		if d, ok := depth[l]; ok {
			return d
		}
		d := 0
		if p := byID[l.Thread]; p != nil && p != l && guard < 1000 {
			d = depthOf(p, guard+1) + 1
		}
		depth[l] = d
		return d
	}
	sort.SliceStable(ls, func(i, j int) bool {
		if !ls[i].Sent.Equal(ls[j].Sent) {
			return ls[i].Sent.Before(ls[j].Sent)
		}
		if di, dj := depthOf(ls[i], 0), depthOf(ls[j], 0); di != dj {
			return di < dj
		}
		return ls[i].ID < ls[j].ID
	})
}

// fingerprintOf asks the store for the letter an id is, in whatever box.
func fingerprintOf(id string) string {
	out, err := self("show", id, "--fingerprint")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// joinLetters ties a letter into its thread: by the id it answers, and by
// the letters (every copy's fingerprint) it is and answers, so copies with
// different ids in different boxes still meet.
func joinLetters(l *Letter, id string, union func(a, b string)) {
	if l.Thread != "" {
		union(id, l.Thread)
	}
	if l.Letter != "" {
		union(id, "letter:"+l.Letter)
	}
	if l.ThreadLetter != "" {
		union(id, "letter:"+l.ThreadLetter)
	}
}

// ids are every id a letter goes by: one per copy, for what you sent.
func (l *Letter) ids() []string {
	if !l.Mine {
		return []string{l.ID}
	}
	var out []string
	for _, c := range l.Copies {
		out = append(out, c.ID)
	}
	return out
}

// threadOf is every letter in the same thread as id: joined by what each
// one answers, through any copy's id.
func threadOf(ls []*Letter, id string) ([]*Letter, error) {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		for parent[x] != "" && parent[x] != x {
			x = parent[x]
		}
		return x
	}
	union := func(a, b string) {
		if ra, rb := find(a), find(b); ra != rb {
			parent[ra] = rb
		}
	}
	found := false
	for _, l := range ls {
		ids := l.ids()
		for _, x := range ids {
			found = found || x == id
			union(x, ids[0])
		}
		joinLetters(l, ids[0], union)
	}
	if !found {
		// An id from someone else's box: the same letter, by its fingerprint.
		if fp := fingerprintOf(id); fp != "" {
			for _, l := range ls {
				if l.Letter == fp || l.ThreadLetter == fp {
					id, found = l.ids()[0], true
					break
				}
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("no letter %s in your box or among what you sent", id)
	}
	root := find(id)
	var out []*Letter
	for _, l := range ls {
		if find(l.ids()[0]) == root {
			out = append(out, l)
		}
	}
	return out, nil
}

func cmdThread(args []string) error {
	const u = "thread ID [--for HELPER|'*'] [--json|--html]"
	f, err := parseFlags(args, map[string]bool{"json": false, "html": false, "for": true})
	if err != nil {
		return bad(u, err)
	}
	lettersFor = forArgs(f)
	if len(f.pos) != 1 {
		return badf(u, "which letter's thread?")
	}
	if err := checkID(f.pos[0]); err != nil {
		return bad(u, err)
	}
	me, ls, missing, err := letters()
	if err != nil {
		return err
	}
	th, err := threadOf(ls, f.pos[0])
	if err != nil {
		return err
	}
	return showCorrespondence(f, &Correspondence{Box: me, Thread: f.pos[0], Letters: th, SentMissing: missing})
}

func cmdWith(args []string) error {
	const u = "with WHO [--tag T]... [--for HELPER|'*'] [--json|--html]"
	f, err := parseFlags(args, map[string]bool{"json": false, "html": false, "tag": true, "for": true})
	if err != nil {
		return bad(u, err)
	}
	lettersFor = forArgs(f)
	if len(f.pos) != 1 {
		return badf(u, "with whom?")
	}
	who, err := resolveBox("WHO", f.pos[0])
	if err != nil {
		return bad(u, err)
	}
	tags, err := tagsOf(f)
	if err != nil {
		return bad(u, err)
	}
	me, ls, missing, err := letters()
	if err != nil {
		return err
	}
	var out []*Letter
	for _, l := range ls {
		between := (!l.Mine && l.From == who) || (l.Mine && contains(l.Recipients, who))
		if who == me {
			between = l.Mine && contains(l.Recipients, me) // notes to yourself
		}
		if !between {
			continue
		}
		keep := true
		for _, t := range tags {
			keep = keep && l.hasTag(t)
		}
		if keep {
			out = append(out, l)
		}
	}
	return showCorrespondence(f, &Correspondence{Box: me, With: who, Letters: out, SentMissing: missing})
}

func showCorrespondence(f flags, c *Correspondence) error {
	if c.Letters == nil {
		c.Letters = []*Letter{}
	}
	switch {
	case f.has("json") && f.has("html"):
		return fmt.Errorf("--json or --html, not both")
	case f.has("json"):
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(c)
	case f.has("html"):
		_, err := os.Stdout.Write(correspondencePage(c))
		return err
	}
	if c.SentMissing != "" {
		fmt.Println("(" + c.SentMissing + ")")
	}
	if len(c.Letters) == 0 {
		fmt.Println("Nothing yet.")
		return nil
	}
	at := now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, l := range c.Letters {
		who := "← " + cleanLine(l.sender(), 40)
		state := l.State
		if l.Mine {
			who = "→ " + strings.Join(l.Recipients, ", ")
			state = seen(l)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ago(l.Sent, at), sigMark(l.Message), who, state, cleanLine(l.Subject, 120))
		if l.Gist != "" && l.Gist != l.Subject {
			fmt.Fprintf(tw, "\t\t\t\t  %s\n", cleanLine(l.Gist, 120))
		}
	}
	return tw.Flush()
}

// seen says whether the people you sent a letter to have read it.
func seen(l *Letter) string {
	var read, unread []string
	for _, c := range l.Copies {
		who := c.Box
		if c.For != "" {
			who += "/" + c.For
		}
		if c.State == "unread" {
			if !contains(unread, who) {
				unread = append(unread, who)
			}
		} else if !contains(read, who) {
			read = append(read, who)
		}
	}
	switch {
	case len(unread) == 0:
		return "seen"
	case len(read) == 0:
		return "not yet read"
	}
	return "seen by " + strings.Join(read, ", ")
}

// ---- threads: the topics ----

// Topic is one thread, as a list of topics shows it.
type Topic struct {
	Root         string        `json:"root"` // its first letter's id
	Subject      string        `json:"subject"`
	Started      time.Time     `json:"started"`
	Last         time.Time     `json:"last"`
	Count        int           `json:"count"`
	Unread       int           `json:"unread"` // letters to you, not yet read
	Tags         []string      `json:"tags"`   // every tag its letters carry
	Participants []Participant `json:"participants"`
	Letters      []*Letter     `json:"letters,omitempty"` // with --letters
}

// Participant is one seal in a thread: who wrote, as whom, for whom.
type Participant struct {
	Box       string `json:"box"`
	Persona   string `json:"persona,omitempty"`
	For       string `json:"for,omitempty"`
	Signature string `json:"signature"` // the least trusted of their letters
}

// Topics is what threads hands out.
type Topics struct {
	Box         string   `json:"box"`
	For         string   `json:"for,omitempty"`
	Topics      []*Topic `json:"topics"`
	SentMissing string   `json:"sent_missing,omitempty"`
}

// groups splits letters into threads, each in time order.
func groups(ls []*Letter) [][]*Letter {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		for parent[x] != "" && parent[x] != x {
			x = parent[x]
		}
		return x
	}
	union := func(a, b string) {
		if ra, rb := find(a), find(b); ra != rb {
			parent[ra] = rb
		}
	}
	for _, l := range ls {
		ids := l.ids()
		for _, x := range ids {
			union(x, ids[0])
		}
		joinLetters(l, ids[0], union)
	}
	byRoot := map[string][]*Letter{}
	var order []string
	for _, l := range ls {
		r := find(l.ids()[0])
		if _, ok := byRoot[r]; !ok {
			order = append(order, r)
		}
		byRoot[r] = append(byRoot[r], l)
	}
	out := make([][]*Letter, 0, len(order))
	for _, r := range order {
		out = append(out, byRoot[r])
	}
	return out
}

// viewHas says a pigeonhole is part of what thread, with and threads were
// asked to read.
func viewHas(pigeonhole string) bool {
	if len(lettersFor) == 2 {
		return lettersFor[1] == "*" || lettersFor[1] == pigeonhole
	}
	return pigeonhole == ""
}

var trustRank = map[string]int{sigBad: 0, sigUnknown: 1, sigUnsigned: 2, sigVerified: 3}

func topicOf(me string, g []*Letter) *Topic {
	t := &Topic{Root: g[0].ID, Subject: g[0].Subject, Started: g[0].Sent, Last: g[0].Sent, Count: len(g), Tags: []string{}}
	for len(t.Subject) > 4 && strings.EqualFold(t.Subject[:4], "re: ") {
		t.Subject = t.Subject[4:]
	}
	seen := map[string]int{}
	for _, l := range g {
		if l.Sent.After(t.Last) {
			t.Last = l.Sent
		}
		if !l.Mine && l.State == "unread" {
			t.Unread++
		}
		if l.Mine {
			// A note to yourself you haven't read yet is unread too.
			for _, c := range l.Copies {
				if c.Box == me && c.State == "unread" && viewHas(c.For) {
					t.Unread++
					break
				}
			}
		}
		for _, tag := range l.Tags {
			if !contains(t.Tags, tag) {
				t.Tags = append(t.Tags, tag)
			}
		}
		box := l.From
		if l.Mine {
			box = me
		}
		p := Participant{Box: box, Persona: l.Persona, For: strings.Join(l.For, ","), Signature: l.Signature}
		k := p.Box + "\x00" + p.Persona + "\x00" + p.For
		if i, ok := seen[k]; ok {
			if trustRank[p.Signature] < trustRank[t.Participants[i].Signature] {
				t.Participants[i].Signature = p.Signature
			}
			continue
		}
		seen[k] = len(t.Participants)
		t.Participants = append(t.Participants, p)
	}
	return t
}

func cmdThreads(args []string) error {
	const u = "threads [--for HELPER|'*'] [--since 7d|DATE] [--limit N] [--letters [--bodies]] [--json|--html]"
	f, err := parseFlags(args, map[string]bool{"for": true, "since": true, "limit": true, "letters": false, "bodies": false, "json": false, "html": false})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) > 0 {
		return badf(u, "threads takes no words of its own")
	}
	if f.has("bodies") && !f.has("letters") {
		return badf(u, "--bodies goes with --letters")
	}
	if f.has("json") && f.has("html") {
		return badf(u, "--json or --html, not both")
	}
	if f.has("html") {
		// The page shows every letter, whole: it needs them all.
		f.vals["letters"], f.vals["bodies"] = []string{""}, []string{""}
	}
	since := flags{vals: map[string][]string{}}
	if f.has("since") {
		since.vals["since"] = f.all("since")
	}
	fl, err := filterOf(since)
	if err != nil {
		return bad(u, err)
	}
	limit := 0
	if f.has("limit") {
		if limit, err = strconv.Atoi(f.get("limit")); err != nil || limit < 1 {
			return badf(u, "--limit is a whole number, at least 1")
		}
	}
	lettersFor = forArgs(f)
	me, ls, missing, err := letters()
	if err != nil {
		return err
	}
	out := &Topics{Box: me, For: strings.ToLower(strings.TrimSpace(f.get("for"))), Topics: []*Topic{}, SentMissing: missing}
	for _, g := range groups(ls) {
		t := topicOf(me, g)
		if !fl.since.IsZero() && t.Last.Before(fl.since) {
			continue
		}
		if f.has("letters") {
			t.Letters = g
			if !f.has("bodies") {
				for _, l := range g {
					l.Body = ""
				}
			}
		}
		out.Topics = append(out.Topics, t)
	}
	sort.SliceStable(out.Topics, func(i, j int) bool { return out.Topics[i].Last.After(out.Topics[j].Last) })
	if limit > 0 && len(out.Topics) > limit {
		out.Topics = out.Topics[:limit]
	}
	if f.has("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if f.has("html") {
		_, err := os.Stdout.Write(threadsPage(out))
		return err
	}
	if missing != "" {
		fmt.Println("(" + missing + ")")
	}
	if len(out.Topics) == 0 {
		fmt.Println("No threads yet.")
		return nil
	}
	at := now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LAST\tLETTERS\tUNREAD\tWHO\tSUBJECT\tROOT")
	for _, t := range out.Topics {
		var who []string
		for _, p := range t.Participants {
			w := p.Box
			if p.Persona != "" {
				w += " as " + p.Persona
				if p.Signature != sigVerified {
					w += " (unverified)"
				}
			}
			if p.Signature == sigBad {
				w += " ✗"
			}
			if !contains(who, w) {
				who = append(who, w)
			}
		}
		unread := "-"
		if t.Unread > 0 {
			unread = strconv.Itoa(t.Unread)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\n", ago(t.Last, at), t.Count, unread, cleanLine(strings.Join(who, ", "), 60), cleanLine(t.Subject, 70), t.Root)
	}
	return tw.Flush()
}

// ---- asks: letters that need their recipient's decision ----

const askTag = "ask"

// Ask is one open ask: what's asked, by whom, how long it has waited.
type Ask struct {
	*Letter
	Question string `json:"question"`
}

// isAsk says a letter asks its recipient for a decision: --ask, or an
// ask-<box> tag naming them.
func isAsk(l *Letter, box string) bool {
	return l.hasTag(askTag) || l.hasTag("ask-"+box)
}

// answered says the recipient has replied in the ask's thread. Someone who
// signs is taken to have answered only by a letter that verifies, so a copy
// planted in their name can't close an ask.
func answered(ask *Letter, ls []*Letter) bool {
	signs := false
	for _, l := range ls {
		signs = signs || (l.Mine && l.Signature == sigVerified)
	}
	for _, l := range ls {
		if !l.Mine || (signs && l.Signature != sigVerified) {
			continue
		}
		if l.Thread == ask.ID || (ask.Letter != "" && l.ThreadLetter == ask.Letter) {
			return true
		}
	}
	return false
}

func cmdAsks(args []string) error {
	const u = "asks [--for HELPER] [--json]"
	f, err := parseFlags(args, map[string]bool{"json": false, "for": true})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) > 0 {
		return badf(u, "asks takes no words of its own: it lists your own")
	}
	lettersFor = forArgs(f)
	me, ls, missing, err := letters()
	if err != nil {
		return err
	}
	out := []*Ask{}
	for _, l := range ls {
		// Open: to you, not filed, and not answered by you in its thread.
		if l.Mine || l.State == "archived" || !isAsk(l, me) || answered(l, ls) {
			continue
		}
		out = append(out, &Ask{Letter: l, Question: l.Gist})
	}
	if f.has("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if missing != "" {
		fmt.Println("(" + missing + ")")
	}
	if len(out) == 0 {
		fmt.Println("Nothing waiting on your decision.")
		return nil
	}
	at := now()
	for _, a := range out {
		fmt.Printf("%s %s  %s, %s:\n  %s\n  (%s; answer with deaddrop reply %s)\n\n", sigMark(a.Message), cleanLine(a.Subject, 80),
			cleanLine(a.sender(), 60), ago(a.Sent, at), cleanLine(a.Question, 300), a.ID, a.ID)
	}
	return nil
}
