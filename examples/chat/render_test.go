// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package main

import (
	"strings"
	"testing"

	bridge "github.com/muster-io/muster-bridge-go"
)

const attrs = ` rel="noopener noreferrer nofollow" target="_blank"`

var alice = []bridge.Mention{{Ref: "m1", ExternalId: "alice", DisplayName: "Alice Smith"}}

func TestRenderBody(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		mentions []bridge.Mention
		native   bool
		want     string
	}{
		{name: "paragraph", in: "a", want: "<p>a</p>"},
		{name: "two paragraphs", in: "a\n\nb", want: "<p>a</p>\n<p>b</p>"},
		{name: "indentation and trailing spaces", in: "  a  ", want: "<p>a</p>"},
		{name: "empty", in: "\n \n", want: ""},
		{name: "soft break", in: "a\nb", want: "<p>a\nb</p>"},
		{name: "soft break after one space", in: "a \nb", want: "<p>a\nb</p>"},
		{name: "hard break by spaces", in: "a  \nb", want: "<p>a<br>b</p>"},
		{name: "hard break by backslash", in: "a\\\nb", want: "<p>a<br>b</p>"},
		{name: "emphasis and strong", in: "*a* _b_ **c** __d__ ***e***",
			want: "<p><em>a</em> <em>b</em> <strong>c</strong> <strong>d</strong> <em><strong>e</strong></em></p>"},
		{name: "intraword underscore", in: "snake_case_name", want: "<p>snake_case_name</p>"},
		{name: "intraword star", in: "a*b*c", want: "<p>a<em>b</em>c</p>"},
		{name: "unmatched delimiters", in: "*a **b _c", want: "<p>*a **b _c</p>"},
		{name: "opener before white space", in: "a * b*", want: "<p>a * b*</p>"},
		{name: "strikethrough", in: "~~gone~~", want: "<p><del>gone</del></p>"},
		{name: "single tilde", in: "~a~", want: "<p>~a~</p>"},
		{name: "code span", in: "`a*b*`", want: "<p><code>a*b*</code></p>"},
		{name: "code span with a backtick", in: "`` a`b ``", want: "<p><code>a`b</code></p>"},
		{name: "code span with HTML", in: "`<b>`", want: "<p><code>&lt;b&gt;</code></p>"},
		{name: "code span ends at a backslash", in: "`C:\\`", want: "<p><code>C:\\</code></p>"},
		{name: "unclosed code span", in: "`a", want: "<p>`a</p>"},
		{name: "link with a query", in: "[Runbook](https://w.example.org/x?a=1&b=2)",
			want: `<p><a href="https://w.example.org/x?a=1&amp;b=2"` + attrs + `>Runbook</a></p>`},
		{name: "link with strong text", in: "[**x**](http://a.example)",
			want: `<p><a href="http://a.example"` + attrs + `><strong>x</strong></a></p>`},
		{name: "link with parentheses", in: "[w](https://w.example.org/A_(b))",
			want: `<p><a href="https://w.example.org/A_(b)"` + attrs + `>w</a></p>`},
		{name: "link in angle brackets", in: "[w](<https://w.example.org/a(b>)",
			want: `<p><a href="https://w.example.org/a(b"` + attrs + `>w</a></p>`},
		{name: "emphasis around a link", in: "*[a](https://a.example)*",
			want: `<p><em><a href="https://a.example"` + attrs + `>a</a></em></p>`},
		{name: "no link in a link", in: "[a [b](https://b.example)](https://a.example)",
			want: `<p>[a <a href="https://b.example"` + attrs + `>b</a>](https://a.example)</p>`},
		{name: "javascript link", in: "[x](JaVaScRiPt:alert(1))", want: "<p>[x](JaVaScRiPt:alert(1))</p>"},
		{name: "javascript link after a space", in: "[x]( javascript:alert(1))",
			want: "<p>[x]( javascript:alert(1))</p>"},
		{name: "data link", in: "[x](data:text/html,hi)", want: "<p>[x](data:text/html,hi)</p>"},
		{name: "vbscript link", in: "[x](vbscript:msgbox)", want: "<p>[x](vbscript:msgbox)</p>"},
		{name: "scheme-relative link", in: "[x](//a.example/x)", want: "<p>[x](//a.example/x)</p>"},
		{name: "relative link", in: "[x](/x)", want: "<p>[x](/x)</p>"},
		{name: "link without a host", in: "[x](https:/x)", want: "<p>[x](https:/x)</p>"},
		{name: "raw HTML", in: "<script>alert(1)</script>", want: "<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>"},
		{name: "raw HTML attribute", in: "<img src=x onerror=alert(1)>",
			want: "<p>&lt;img src=x onerror=alert(1)&gt;</p>"},
		{name: "heading", in: "# T", want: "<p># T</p>"},
		{name: "ordered list", in: "1. a", want: "<p>1. a</p>"},
		{name: "image", in: "![a](https://x.example/y.png)", want: "<p>![a](https://x.example/y.png)</p>"},
		{name: "autolink", in: "<https://a.example>", want: "<p>&lt;https://a.example&gt;</p>"},
		{name: "fence is a code span at most", in: "```\n<b>\n```", want: "<p><code>&lt;b&gt;</code></p>"},
		{name: "bullet list", in: "- a\n- b", want: "<ul>\n<li>a</li>\n<li>b</li>\n</ul>"},
		{name: "bullet list with stars", in: "* a\n* b", want: "<ul>\n<li>a</li>\n<li>b</li>\n</ul>"},
		{name: "bullet list with pluses", in: "+ a", want: "<ul>\n<li>a</li>\n</ul>"},
		{name: "list item continued", in: "- a\n  b\n- c", want: "<ul>\n<li>a\nb</li>\n<li>c</li>\n</ul>"},
		{name: "nested marker is text", in: "- a\n  - b", want: "<ul>\n<li>a\n- b</li>\n</ul>"},
		{name: "list after a paragraph", in: "a\n- b", want: "<p>a</p>\n<ul>\n<li>b</li>\n</ul>"},
		{name: "block quote with strong", in: "> **ns** payments",
			want: "<blockquote>\n<p><strong>ns</strong> payments</p>\n</blockquote>"},
		{name: "block quote with a list", in: "> - a", want: "<blockquote>\n<ul>\n<li>a</li>\n</ul>\n</blockquote>"},
		{name: "block quote of two lines", in: "> a\n> b\nc",
			want: "<blockquote>\n<p>a\nb</p>\n</blockquote>\n<p>c</p>"},
		{name: "empty block quote", in: ">", want: "<blockquote>\n</blockquote>"},
		{name: "escaped stars", in: `\*no\*`, want: "<p>*no*</p>"},
		{name: "escaped brackets", in: `\[x\](https://a.example)`, want: "<p>[x](https://a.example)</p>"},
		{name: "backslash before a letter", in: `a\b`, want: `<p>a\b</p>`},
		{name: "ampersand", in: "a & b", want: "<p>a &amp; b</p>"},
		{name: "entity", in: "&lt;", want: "<p>&amp;lt;</p>"},
		{name: "mention, native", in: "[@Alice Smith](muster-mention:m1)", mentions: alice, native: true,
			want: `<p><span class="mention" data-person="alice">@Alice Smith</span></p>`},
		{name: "mention in strong", in: "**[@Alice Smith](muster-mention:m1)**", mentions: alice, native: true,
			want: `<p><strong><span class="mention" data-person="alice">@Alice Smith</span></strong></p>`},
		{name: "mention, not native", in: "[@Alice Smith](muster-mention:m1)", mentions: alice,
			want: "<p>@Alice Smith</p>"},
		{name: "mention of an unknown ref", in: "[@Bob](muster-mention:zz)", mentions: alice, native: true,
			want: "<p>@Bob</p>"},
		{name: "mention with an escaped name", in: `[@Alice\_Smith](muster-mention:m1)`, mentions: alice,
			want: "<p>@Alice_Smith</p>"},
		{name: "mention with HTML in the name and a quote in the id", in: "[@x](muster-mention:m2)",
			mentions: []bridge.Mention{{Ref: "m2", ExternalId: `a" onclick="x`, DisplayName: "<b>x</b>"}},
			native:   true,
			want:     `<p><span class="mention" data-person="a&#34; onclick=&#34;x">@&lt;b&gt;x&lt;/b&gt;</span></p>`},
		{name: "mention characters in the input", in: "\uE0000\uE001", native: true, want: "<p>\uFFFD0\uFFFD</p>"},
		{name: "invalid UTF-8", in: "\xff<", want: "<p>\uFFFD&lt;</p>"},
		{name: "carriage returns", in: "a\r\n\r\nb", want: "<p>a</p>\n<p>b</p>"},
		{name: "the specification's example", mentions: []bridge.Mention{{Ref: "m1", ExternalId: "u-alice",
			DisplayName: "Alice Smith"}}, native: true, in: "[@Alice Smith](muster-mention:m1)\n\n" +
			"prod · started 14:03\n\n> **namespace** payments\n\n- `api-7f9c` error rate 12 %\n" +
			"- `api-5d2a` error rate 9 %\n\n[Runbook](https://wiki.example.org/runbooks/high-error-rate)",
			want: `<p><span class="mention" data-person="u-alice">@Alice Smith</span></p>` + "\n" +
				"<p>prod · started 14:03</p>\n" +
				"<blockquote>\n<p><strong>namespace</strong> payments</p>\n</blockquote>\n" +
				"<ul>\n<li><code>api-7f9c</code> error rate 12 %</li>\n<li><code>api-5d2a</code> error rate 9 %</li>\n" +
				"</ul>\n" +
				`<p><a href="https://wiki.example.org/runbooks/high-error-rate"` + attrs + `>Runbook</a></p>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderBody(tt.in, tt.mentions, tt.native); got != tt.want {
				t.Errorf("renderBody(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRenderBodyNeverInjectsAnAttribute(t *testing.T) {
	got := renderBody(`[x](https://a.example/"onmouseover="alert(1))`, nil, false)
	if strings.Contains(got, `"onmouseover`) || strings.Contains(got, `onmouseover="`) {
		t.Errorf("the quote reached the tag: %q", got)
	}
	if strings.Contains(got, "<a ") && !strings.Contains(got, "&#34;onmouseover=&#34;") {
		t.Errorf("a link without the quotes escaped: %q", got)
	}
}

// TestRenderBodyPathological renders bodies of the longest size a bridge accepts in shapes that make a naive renderer
// quadratic; the test's timeout catches one that is.
func TestRenderBodyPathological(t *testing.T) {
	const n = 100000
	for name, body := range map[string]string{
		"stars":                strings.Repeat("*", n),
		"underscores":          strings.Repeat("_", n),
		"backticks":            strings.Repeat("`", n),
		"brackets":             strings.Repeat("[", n),
		"tildes":               strings.Repeat("~~", n/2),
		"single backticks":     strings.Repeat("`a", n/2),
		"growing backticks":    growingRuns(n),
		"unclosed links":       strings.Repeat("[a](", n/4),
		"unclosed angle links": strings.Repeat("[a](<", n/5),
		"open parentheses":     "[a](" + strings.Repeat("(", n),
		"nested destinations":  strings.Repeat("[](", n/4) + strings.Repeat(")", n/4),
		"nested angle links":   strings.Repeat("[](<", n/5) + strings.Repeat(">)", n/10),
		"one long destination": "[a](https://a.example/" + strings.Repeat("x", n-len("[a](https://a.example/)")) + ")",
		"alternating openers":  strings.Repeat("*a _a ", n/6),
		"quotes":               strings.Repeat(">", n),
		"list items":           strings.Repeat("- a\n", n/4),
	} {
		t.Run(name, func(t *testing.T) {
			got := renderBody(body, nil, false)
			if strings.Contains(got, "<em>") || strings.Contains(got, "<a ") {
				t.Errorf("unexpected emphasis or link in %.80q…", got)
			}
		})
	}
}

// growingRuns is runs of 1, 2, 3, … backticks, none of which closes another.
func growingRuns(n int) string {
	var b strings.Builder
	for k := 1; b.Len() < n; k++ {
		b.WriteString(strings.Repeat("`", k))
		b.WriteByte('a')
	}
	return b.String()
}

func TestRenderText(t *testing.T) {
	for in, want := range map[string]string{
		"<b>&\nx":  "<p>&lt;b&gt;&amp;<br>x</p>",
		"*a* `b`":  "<p>*a* `b`</p>",
		"a\r\nb":   "<p>a<br>b</p>",
		"\xff":     "<p>\uFFFD</p>",
		`"quoted"`: "<p>&#34;quoted&#34;</p>",
	} {
		if got := renderText(in); got != want {
			t.Errorf("renderText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLinkDestinationBound(t *testing.T) {
	const prefix = "https://a.example/"
	longest := prefix + strings.Repeat("x", maxDestBytes-len(prefix))
	for name, tt := range map[string]struct {
		in   string
		link bool
	}{
		"bare at the bound":           {in: "[a](" + longest + ")", link: true},
		"bare past the bound":         {in: "[a](" + longest + "x)"},
		"angle brackets at the bound": {in: "[a](<" + longest + ">)", link: true},
		"angle brackets past it":      {in: "[a](<" + longest + "x>)"},
	} {
		got := renderBody(tt.in, nil, false)
		if strings.Contains(got, "<a ") != tt.link {
			t.Errorf("%s: link %v in %.60q…", name, !tt.link, got)
		}
	}
}

func TestWebLink(t *testing.T) {
	for dest, want := range map[string]bool{
		"https://a.example/x?y=1":  true,
		"http://a.example":         true,
		"HTTPS://A.example/":       true,
		"javascript:alert(1)":      false,
		"JaVaScRiPt:alert(1)":      false,
		"\thttps://a.example":      false,
		"https://a.example/ x":     false,
		"https://a.example/\u00a0": false,
		"https://a.example/\n":     false,
		"https://a.example/\uE000": false,
		"data:text/html,x":         false,
		"vbscript:x":               false,
		"mailto:a@a.example":       false,
		"ftp://a.example/x":        false,
		"//a.example/x":            false,
		"/x":                       false,
		"x":                        false,
		"https:/x":                 false,
		"https:x":                  false,
		"http://":                  false,
		"":                         false,
	} {
		if got := webLink(dest); got != want {
			t.Errorf("webLink(%q) = %v, want %v", dest, got, want)
		}
	}
}
