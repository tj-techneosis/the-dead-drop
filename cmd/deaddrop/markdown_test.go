package main

import (
	"strings"
	"testing"
)

func TestRenderMarkdownEdges(t *testing.T) {
	cases := []struct{ in, want, never string }{
		// Emphasis never reaches into a URL.
		{"see https://example.com/_a_/b and _this_", `<a href="https://example.com/_a_/b" rel="noopener noreferrer">https://example.com/_a_/b</a> and <em>this</em>`, "<em>a</em>"},
		// A NUL can't fake a set-aside URL.
		{"x \x000\x00 y", "<p>x 0 y</p>", "\x00"},
		// Nested items fold into the one above.
		{"- one\n  - inner\n- two", "<li>one<br>\n– inner</li>", ""},
		// An unclosed fence runs to the end, escaped.
		{"```\n<x>", "<pre><code>&lt;x&gt;</code></pre>", "<x>"},
		// Link text is escaped; a quote can't leave the attribute.
		{`[<b>](https://e.com/"x)`, `<a href="https://e.com/&#34;x" rel="noopener noreferrer">&lt;b&gt;</a>`, "<b>"},
		{"[x](data:text/html,hi)", "x (data:text/html,hi)", "href"},
	}
	for _, c := range cases {
		got := renderMarkdown(c.in)
		if !strings.Contains(got, c.want) {
			t.Errorf("renderMarkdown(%q) = %q, want it to hold %q", c.in, got, c.want)
		}
		if c.never != "" && strings.Contains(got, c.never) {
			t.Errorf("renderMarkdown(%q) = %q, holds %q", c.in, got, c.never)
		}
	}
}
