package main

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// A small Markdown renderer for reading one message in a browser. A message
// is someone else's text, so everything is escaped first and only these
// tags are ever made: headings, paragraphs, lists, quotes, code, tables,
// rules, emphasis and links (http, https and mailto only). Images are shown
// as their alt text: nothing is fetched. The page's Content-Security-Policy
// forbids loading anything at all, as a second wall.

// pageStyle is the look both pages share: calm, light and dark.
const pageStyle = `<style>
:root { --bg:#fbfaf8; --fg:#1f2328; --dim:#6b7078; --rule:#e3e1dc; --code:#f1efeb; --accent:#2f6fb3; --urgent:#c4352c; --ok:#2e7d4f; --warn:#9a6a00; }
@media (prefers-color-scheme: dark) { :root { --bg:#17181a; --fg:#e6e4df; --dim:#9a9ea5; --rule:#2c2e31; --code:#222427; --accent:#7fb0e6; --urgent:#ef7a70; --ok:#7cc79a; --warn:#e0b85c; } }
* { box-sizing: border-box; }
body { margin:0; background:var(--bg); color:var(--fg); font:16px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif; }
main { max-width: 46rem; margin: 0 auto; padding: 2.5rem 1rem 4rem; }
header { border-bottom: 1px solid var(--rule); padding-bottom: 1rem; margin-bottom: 1.5rem; }
header h1 { font-size: 1.5rem; line-height: 1.3; margin: 0 0 .5rem; }
.meta { color: var(--dim); font-size: .9rem; display: flex; flex-wrap: wrap; gap: .25rem 1rem; }
.urgent { color: var(--urgent); font-weight: 600; }
.ok { color: var(--ok); }
.warn { color: var(--warn); }
.tag { background: var(--code); border-radius: 4px; padding: 0 .4em; }
.files { margin-top: .75rem; font-size: .9rem; color: var(--dim); }
.files code { color: var(--fg); }
article h1, article h2, article h3, article h4 { line-height: 1.3; margin: 1.6em 0 .5em; }
article h1 { font-size: 1.35rem; } article h2 { font-size: 1.2rem; } article h3 { font-size: 1.05rem; }
article > :first-child { margin-top: 0; }
a { color: var(--accent); }
code { font: .9em ui-monospace, SFMono-Regular, Menlo, monospace; background: var(--code); border-radius: 4px; padding: .1em .3em; }
pre { background: var(--code); border-radius: 6px; padding: .8rem 1rem; overflow-x: auto; }
pre code { background: none; padding: 0; }
blockquote { margin: 1em 0; padding: 0 1em; border-left: 3px solid var(--rule); color: var(--dim); }
table { border-collapse: collapse; display: block; overflow-x: auto; }
th, td { border: 1px solid var(--rule); padding: .3em .6em; text-align: left; }
hr { border: 0; border-top: 1px solid var(--rule); margin: 2em 0; }
</style>`

func messagePage(m *Message) []byte {
	var b strings.Builder
	e := html.EscapeString
	b.WriteString(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>`)
	b.WriteString(e(m.Subject))
	b.WriteString(` · Dead Drop</title>
` + pageStyle + `</head><body><main>
<header><h1>`)
	b.WriteString(e(m.Subject))
	b.WriteString("</h1>\n<div class=\"meta\">")
	if m.Persona != "" {
		fmt.Fprintf(&b, "<span>From <strong>%s</strong>, as %s, to %s</span>", e(m.From), e(m.Persona), e(m.To))
	} else {
		fmt.Fprintf(&b, "<span>From <strong>%s</strong> to %s</span>", e(m.From), e(m.To))
	}
	fmt.Fprintf(&b, "<span>%s</span>", e(m.Sent.Local().Format("Mon 2 Jan 2006, 15:04")))
	switch m.Signature {
	case sigVerified:
		fmt.Fprintf(&b, `<span class="ok">✓ %s</span>`, e(m.SignatureNote))
	case sigBad:
		fmt.Fprintf(&b, `<span class="urgent">✗ forged or changed: %s</span>`, e(m.SignatureNote))
	default:
		fmt.Fprintf(&b, `<span class="warn">%s</span>`, e(m.SignatureNote))
	}
	if m.Urgency == urgent {
		b.WriteString(`<span class="urgent">urgent</span>`)
	} else if m.Urgency == fyi {
		b.WriteString("<span>fyi</span>")
	}
	for _, t := range m.Tags {
		fmt.Fprintf(&b, `<span class="tag">%s</span>`, e(t))
	}
	b.WriteString("</div>\n")
	if len(m.Attachments) > 0 {
		b.WriteString(`<div class="files">Attached: `)
		for i, a := range m.Attachments {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(e(a))
		}
		fmt.Fprintf(&b, ". Fetch one with <code>deaddrop file %s NAME &gt; NAME</code>.</div>\n", e(m.ID))
	}
	b.WriteString("</header>\n<article>\n")
	b.WriteString(renderMarkdown(m.Body))
	fmt.Fprintf(&b, "</article>\n<footer class=\"meta\" style=\"margin-top:3rem\"><span>%s</span><span>opened %s</span></footer>\n",
		e(m.ID), e(now().Local().Format("15:04")))
	b.WriteString("</main></body></html>\n")
	return []byte(b.String())
}

var (
	mdHeading  = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	mdFence    = regexp.MustCompile("^\\s*(```|~~~)")
	mdRule     = regexp.MustCompile(`^\s*([-*_])(\s*[-*_]){2,}\s*$`)
	mdBullet   = regexp.MustCompile(`^\s*[-*+]\s+(.*)$`)
	mdNumber   = regexp.MustCompile(`^\s*\d{1,9}[.)]\s+(.*)$`)
	mdQuote    = regexp.MustCompile(`^\s*>\s?(.*)$`)
	mdTableSep = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
)

// renderMarkdown turns a body into HTML, a block at a time.
func renderMarkdown(src string) string {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var b strings.Builder
	var para []string
	flush := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + inline(strings.Join(para, "\n")) + "</p>\n")
			para = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.TrimSpace(l) == "":
			flush()
		case mdFence.MatchString(l):
			flush()
			fence := mdFence.FindStringSubmatch(l)[1]
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), fence); i++ {
				code = append(code, lines[i])
			}
			b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")
		case mdHeading.MatchString(l):
			flush()
			m := mdHeading.FindStringSubmatch(l)
			n := len(m[1])
			fmt.Fprintf(&b, "<h%d>%s</h%d>\n", n, inline(m[2]), n)
		case mdRule.MatchString(l):
			flush()
			b.WriteString("<hr>\n")
		case mdQuote.MatchString(l):
			flush()
			var q []string
			for ; i < len(lines) && mdQuote.MatchString(lines[i]); i++ {
				q = append(q, mdQuote.FindStringSubmatch(lines[i])[1])
			}
			i--
			b.WriteString("<blockquote>\n" + renderMarkdown(strings.Join(q, "\n")) + "</blockquote>\n")
		case mdBullet.MatchString(l) || mdNumber.MatchString(l):
			flush()
			re, tag := mdBullet, "ul"
			if !mdBullet.MatchString(l) {
				re, tag = mdNumber, "ol"
			}
			b.WriteString("<" + tag + ">\n")
			var item []string
			out := func() {
				if item != nil {
					b.WriteString("<li>" + inline(strings.Join(item, "\n")) + "</li>\n")
				}
			}
			for ; i < len(lines); i++ {
				x := lines[i]
				indented := strings.HasPrefix(x, " ") || strings.HasPrefix(x, "\t")
				if m := re.FindStringSubmatch(x); m != nil && (!indented || item == nil) {
					out()
					item = []string{m[1]}
					continue
				}
				// A wrapped or nested line belongs to the item above.
				if strings.TrimSpace(x) != "" && indented {
					t := strings.TrimSpace(x)
					if m := mdBullet.FindStringSubmatch(x); m != nil {
						t = "– " + m[1]
					} else if m := mdNumber.FindStringSubmatch(x); m != nil {
						t = m[1]
					}
					item = append(item, t)
					continue
				}
				break
			}
			out()
			i--
			b.WriteString("</" + tag + ">\n")
		case strings.Contains(l, "|") && i+1 < len(lines) && mdTableSep.MatchString(lines[i+1]) && strings.Contains(lines[i+1], "-"):
			flush()
			b.WriteString("<table>\n<tr>")
			for _, c := range cells(l) {
				b.WriteString("<th>" + inline(c) + "</th>")
			}
			b.WriteString("</tr>\n")
			for i += 2; i < len(lines) && strings.Contains(lines[i], "|") && strings.TrimSpace(lines[i]) != ""; i++ {
				b.WriteString("<tr>")
				for _, c := range cells(lines[i]) {
					b.WriteString("<td>" + inline(c) + "</td>")
				}
				b.WriteString("</tr>\n")
			}
			i--
			b.WriteString("</table>\n")
		default:
			para = append(para, l)
		}
	}
	flush()
	return b.String()
}

func cells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	parts := strings.Split(row, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

var (
	mdCode   = regexp.MustCompile("`([^`]+)`")
	mdImage  = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]*)[^)]*\)`)
	mdLink   = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	mdBold   = regexp.MustCompile(`(\*\*|__)(\S(?:.*?\S)?)(\*\*|__)`)
	mdItalic = regexp.MustCompile(`(^|[^\w*])[*_](\S(?:[^*_]*?\S)?)[*_]([^\w*]|$)`)
	mdURL    = regexp.MustCompile(`(^|[\s(])(https?://[^\s<]+[^\s<.,;:!?)'"])`)
)

// inline renders one block's text: escaped, then code spans (left as they
// are inside), images as alt text, links, bare URLs, bold and italics.
func inline(s string) string {
	var b strings.Builder
	for {
		loc := mdCode.FindStringIndex(s)
		if loc == nil {
			b.WriteString(spans(s))
			break
		}
		b.WriteString(spans(s[:loc[0]]))
		b.WriteString("<code>" + html.EscapeString(s[loc[0]+1:loc[1]-1]) + "</code>")
		s = s[loc[1]:]
	}
	return strings.ReplaceAll(b.String(), "\n", "<br>\n")
}

func safeHref(u string) (string, bool) {
	l := strings.ToLower(u)
	if strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "mailto:") {
		return u, true
	}
	return "", false
}

func spans(s string) string {
	// Images and links are found in the raw text, and every piece of them is
	// escaped on its way out; the rest is escaped before emphasis is added.
	var out strings.Builder
	rest := s
	for {
		im := mdImage.FindStringSubmatchIndex(rest)
		ln := mdLink.FindStringSubmatchIndex(rest)
		if im == nil && ln == nil {
			out.WriteString(emphasis(rest))
			break
		}
		if im != nil && (ln == nil || im[0] <= ln[0]) {
			out.WriteString(emphasis(rest[:im[0]]))
			alt := rest[im[2]:im[3]]
			if alt == "" {
				alt = "image"
			}
			out.WriteString("[" + html.EscapeString(alt) + "]")
			rest = rest[im[1]:]
			continue
		}
		out.WriteString(emphasis(rest[:ln[0]]))
		text, href := rest[ln[2]:ln[3]], rest[ln[4]:ln[5]]
		if u, ok := safeHref(href); ok {
			out.WriteString(`<a href="` + html.EscapeString(u) + `" rel="noopener noreferrer">` + emphasis(text) + "</a>")
		} else {
			out.WriteString(emphasis(text) + " (" + html.EscapeString(href) + ")")
		}
		rest = rest[ln[1]:]
	}
	return out.String()
}

func emphasis(s string) string {
	s = html.EscapeString(strings.ReplaceAll(s, "\x00", ""))
	// Bare URLs are set aside first, so emphasis never reaches into one.
	var urls []string
	s = mdURL.ReplaceAllStringFunc(s, func(m string) string {
		sub := mdURL.FindStringSubmatch(m)
		urls = append(urls, sub[2])
		return sub[1] + fmt.Sprintf("\x00%d\x00", len(urls)-1)
	})
	s = mdBold.ReplaceAllString(s, "<strong>$2</strong>")
	s = mdItalic.ReplaceAllString(s, "$1<em>$2</em>$3")
	for i, u := range urls {
		// Already escaped, so it goes into the attribute as it is.
		s = strings.Replace(s, fmt.Sprintf("\x00%d\x00", i), `<a href="`+u+`" rel="noopener noreferrer">`+u+"</a>", 1)
	}
	return s
}

// correspondencePage is a thread or a conversation, read like letters on a
// desk: theirs on the left, yours on the right, oldest at the top. Each shows
// its gist; the whole letter opens beneath it (no script needed).
func correspondencePage(c *Correspondence) []byte {
	var b strings.Builder
	e := html.EscapeString
	title := "Thread"
	if c.With != "" {
		title = "With " + c.With
	} else if len(c.Letters) > 0 {
		title = c.Letters[0].Subject
	}
	writePageHead(&b, title)
	b.WriteString(e(title))
	b.WriteString("</h1>\n<div class=\"meta\">")
	fmt.Fprintf(&b, "<span>%s's mail</span><span>%d letters</span>", e(c.Box), len(c.Letters))
	if c.SentMissing != "" {
		fmt.Fprintf(&b, `<span class="note">%s</span>`, e(c.SentMissing))
	}
	b.WriteString("</div></header>\n<div class=\"letters\">\n")
	if len(c.Letters) == 0 {
		b.WriteString("<p class=\"meta\">Nothing yet.</p>\n")
	}
	for _, l := range c.Letters {
		writeLetter(&b, l)
	}
	b.WriteString("</div>\n</main></body></html>\n")
	return []byte(b.String())
}

// writePageHead starts a page of letters: the safe head, the shared look,
// and the open <h1>, which the caller fills and closes.
func writePageHead(b *strings.Builder, title string) {
	e := html.EscapeString
	b.WriteString(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>`)
	b.WriteString(e(title))
	b.WriteString(" · Dead Drop</title>\n")
	b.WriteString(pageStyle)
	b.WriteString(`<style>
.letters { display: flex; flex-direction: column; gap: 1rem; }
.letter { max-width: 85%; border: 1px solid var(--rule); border-radius: 10px; padding: .75rem 1rem; background: var(--bg); }
.letter.mine { align-self: flex-end; background: var(--code); }
.letter.theirs { align-self: flex-start; }
.letter.unread { border-color: var(--accent); }
.letter.forged { border-color: var(--urgent); }
.letter h2 { font-size: 1rem; margin: .25rem 0; }
.letter .gist { margin: .25rem 0 .5rem; }
.letter details summary { cursor: pointer; color: var(--dim); font-size: .9rem; }
.letter details article { margin-top: .75rem; border-top: 1px solid var(--rule); padding-top: .75rem; }
.note { color: var(--warn); }
@media (max-width: 40rem) { .letter { max-width: 100%; } }
</style></head><body><main>
<header><h1>`)
}

// writeLetter is one letter on a page: who, when, its seal, its gist, and
// the whole of it opening beneath.
func writeLetter(b *strings.Builder, l *Letter) {
	e := html.EscapeString
	class := "letter theirs"
	if l.Mine {
		class = "letter mine"
	} else if l.State == "unread" {
		class += " unread"
	}
	if l.Signature == sigBad {
		class += " forged"
	}
	fmt.Fprintf(b, "<section class=\"%s\">\n<div class=\"meta\">", class)
	if l.Mine {
		fmt.Fprintf(b, "<span>You, to <strong>%s</strong></span>", e(strings.Join(l.Recipients, ", ")))
		if l.Persona != "" {
			fmt.Fprintf(b, "<span>as %s</span>", e(l.Persona))
		}
		fmt.Fprintf(b, "<span>%s</span>", e(seen(l)))
	} else {
		fmt.Fprintf(b, "<span>From <strong>%s</strong></span>", e(l.sender()))
		if l.State == "unread" {
			b.WriteString(`<span class="ok">new</span>`)
		}
	}
	fmt.Fprintf(b, "<span>%s</span>", e(l.Sent.Local().Format("Mon 2 Jan, 15:04")))
	switch l.Signature {
	case sigVerified:
		b.WriteString(`<span class="ok">✓</span>`)
	case sigBad:
		fmt.Fprintf(b, `<span class="urgent">✗ forged or changed: %s</span>`, e(l.SignatureNote))
	default:
		fmt.Fprintf(b, `<span class="warn">%s</span>`, e(map[string]string{sigUnsigned: "unsigned", sigUnknown: "can't check"}[l.Signature]))
	}
	if l.Urgency == urgent {
		b.WriteString(`<span class="urgent">urgent</span>`)
	}
	for _, t := range l.Tags {
		fmt.Fprintf(b, `<span class="tag">%s</span>`, e(t))
	}
	b.WriteString("</div>\n")
	fmt.Fprintf(b, "<h2>%s</h2>\n", e(l.Subject))
	if l.Gist != "" {
		fmt.Fprintf(b, "<p class=\"gist\">%s</p>\n", e(l.Gist))
	}
	b.WriteString("<details><summary>The whole letter</summary><article>\n")
	b.WriteString(renderMarkdown(l.Body))
	if len(l.Attachments) > 0 {
		// A letter you sent is in its recipient's box: say which.
		where := ""
		if l.Mine && len(l.Copies) > 0 {
			where = " --box " + l.Copies[0].Box
			if l.Copies[0].For != "" {
				where += " --for " + l.Copies[0].For
			}
			l.ID = l.Copies[0].ID
		}
		fmt.Fprintf(b, "<p class=\"meta\">Attached: %s. <code>deaddrop file %s NAME%s</code></p>\n", e(strings.Join(l.Attachments, ", ")), e(l.ID), e(where))
	}
	b.WriteString("</article></details>\n</section>\n")
}

// threadsPage is every thread on one page, newest activity first: each a
// line (subject, who, how many, how many new) that opens in place into its
// letters, so the browser is the whole reading view. No script needed.
func threadsPage(t *Topics) []byte {
	var b strings.Builder
	e := html.EscapeString
	writePageHead(&b, "Threads")
	b.WriteString("Threads</h1>\n<div class=\"meta\">")
	fmt.Fprintf(&b, "<span>%s's mail</span><span>%d threads</span>", e(t.Box), len(t.Topics))
	if t.For != "" {
		fmt.Fprintf(&b, "<span>for %s</span>", e(t.For))
	}
	if t.SentMissing != "" {
		fmt.Fprintf(&b, `<span class="note">%s</span>`, e(t.SentMissing))
	}
	b.WriteString("</div></header>\n")
	b.WriteString(`<style>
details.topic { border-top: 1px solid var(--rule); padding: .6rem 0; }
details.topic > summary { cursor: pointer; list-style: none; }
details.topic > summary::-webkit-details-marker { display: none; }
details.topic > summary .subject { font-weight: 600; }
details.topic.new > summary .subject::before { content: "● "; color: var(--accent); }
details.topic .letters { margin-top: .9rem; }
</style>
`)
	if len(t.Topics) == 0 {
		b.WriteString("<p class=\"meta\">No threads yet.</p>\n")
	}
	now0 := now()
	for _, tp := range t.Topics {
		class := "topic"
		if tp.Unread > 0 {
			class += " new"
		}
		var who []string
		for _, p := range tp.Participants {
			w := p.Box
			if p.Persona != "" {
				w += ", as " + p.Persona
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
		fmt.Fprintf(&b, "<details class=\"%s\"><summary><span class=\"subject\">%s</span><div class=\"meta\">", class, e(tp.Subject))
		fmt.Fprintf(&b, "<span>%s</span><span>%d letters</span>", e(strings.Join(who, " · ")), tp.Count)
		if tp.Unread > 0 {
			fmt.Fprintf(&b, `<span class="ok">%d new</span>`, tp.Unread)
		}
		fmt.Fprintf(&b, "<span>%s</span>", e(ago(tp.Last, now0)))
		for _, tag := range tp.Tags {
			fmt.Fprintf(&b, `<span class="tag">%s</span>`, e(tag))
		}
		b.WriteString("</div></summary>\n<div class=\"letters\">\n")
		for _, l := range tp.Letters {
			writeLetter(&b, l)
		}
		b.WriteString("</div></details>\n")
	}
	b.WriteString("</main></body></html>\n")
	return []byte(b.String())
}
