// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package main

import (
	"html"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	bridge "github.com/muster-io/muster-bridge-go"
)

// While it renders, the renderer swaps the Mention marker of each highlighted mention for a placeholder:
// placeholderStart, the mention's index in decimal, placeholderEnd. Both are private-use characters that cleanText
// takes out of the input, so only the renderer writes them.
const (
	placeholderStart = '\uE000'
	placeholderEnd   = '\uE001'
	maxQuoteDepth    = 16
	maxDestBytes     = 2048 // the longest link destination; past it, [text](dest) stays text
	linkAttrs        = ` rel="noopener noreferrer nofollow" target="_blank"`
)

// renderBody renders Muster's CommonMark subset to HTML, every text escaped, with the Mention markers of mentions as
// highlighted names when native is set, and as their plain link text otherwise.
func renderBody(src string, mentions []bridge.Mention, native bool) string {
	src = cleanText(src)
	var used []bridge.Mention
	var stand func(bridge.Mention) string
	if native {
		stand = func(m bridge.Mention) string {
			used = append(used, m)
			return placeholder(len(used) - 1)
		}
	}
	src = bridge.ReplaceMentions(src, mentions, stand)
	r := renderer{spans: mentionSpans(used)}
	var out strings.Builder
	r.blocks(&out, strings.Split(src, "\n"), 0)
	return out.String()
}

// renderText renders plain text, such as a person's message or an answer, as one escaped paragraph, its line breaks
// as <br>.
func renderText(s string) string {
	return "<p>" + strings.ReplaceAll(html.EscapeString(cleanText(s)), "\n", "<br>") + "</p>"
}

// webLink reports whether dest may become the address of a link. The contract allows http and https links only, so
// dest must parse as an absolute URL of one of those schemes that names a host, and it must hold no white space, no
// control character and none of the placeholder characters.
func webLink(dest string) bool {
	u, err := url.Parse(dest)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return false
	}
	if u.Host == "" {
		return false
	}
	for _, r := range dest {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == placeholderStart || r == placeholderEnd {
			return false
		}
	}
	return true
}

// placeholder is what stands in for the used mention at index i until the text is escaped.
func placeholder(i int) string {
	return string(placeholderStart) + strconv.Itoa(i) + string(placeholderEnd)
}

// mentionSpans rewrites escaped text: the placeholder of each used mention becomes a span that names the person, and
// a placeholder character outside a whole placeholder goes. Escaping leaves placeholders as they are, and an earlier
// pair of a Replacer wins over a later one, so the whole placeholders come first.
func mentionSpans(used []bridge.Mention) *strings.Replacer {
	pairs := make([]string, 0, 2*len(used)+4)
	for i, m := range used {
		pairs = append(pairs, placeholder(i), `<span class="mention" data-person="`+html.EscapeString(m.ExternalId)+
			`">@`+html.EscapeString(m.DisplayName)+"</span>")
	}
	pairs = append(pairs, string(placeholderStart), "", string(placeholderEnd), "")
	return strings.NewReplacer(pairs...)
}

var cleaner = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\x00", "\uFFFD", string(placeholderStart), "\uFFFD",
	string(placeholderEnd), "\uFFFD")

// cleanText is s as valid UTF-8 with \n line ends, and without the characters the renderer marks mentions with.
func cleanText(s string) string {
	return cleaner.Replace(strings.ToValidUTF8(s, "\uFFFD"))
}

type renderer struct {
	spans *strings.Replacer
}

// blocks renders lines as paragraphs, bullet lists and block quotes, one block per line of the output.
func (r *renderer) blocks(out *strings.Builder, lines []string, depth int) {
	started := false
	next := func() {
		if started {
			out.WriteByte('\n')
		}
		started = true
	}
	quote := func(s string) bool { return depth < maxQuoteDepth && strings.HasPrefix(s, ">") }
	for i := 0; i < len(lines); {
		if blank(lines[i]) {
			i++
			continue
		}
		s := trimIndent(lines[i])
		switch {
		case quote(s):
			var inner []string
			for ; i < len(lines) && quote(trimIndent(lines[i])); i++ {
				inner = append(inner, strings.TrimPrefix(trimIndent(lines[i])[1:], " "))
			}
			next()
			var body strings.Builder
			r.blocks(&body, inner, depth+1)
			out.WriteString("<blockquote>\n")
			if body.Len() > 0 {
				out.WriteString(body.String())
				out.WriteByte('\n')
			}
			out.WriteString("</blockquote>")
		case bullet(s):
			next()
			out.WriteString("<ul>")
			for i < len(lines) && bullet(trimIndent(lines[i])) {
				content := indent(lines[i]) + 2
				item := []string{trimIndent(lines[i])[2:]}
				for i++; i < len(lines) && !blank(lines[i]) && indent(lines[i]) >= content; i++ {
					item = append(item, lines[i])
				}
				out.WriteString("\n<li>")
				r.inline(out, joinLines(item))
				out.WriteString("</li>")
			}
			out.WriteString("\n</ul>")
		default:
			para := []string{lines[i]}
			for i++; i < len(lines) && !blank(lines[i]); i++ {
				if s := trimIndent(lines[i]); quote(s) || bullet(s) {
					break
				}
				para = append(para, lines[i])
			}
			next()
			out.WriteString("<p>")
			r.inline(out, joinLines(para))
			out.WriteString("</p>")
		}
	}
}

func blank(line string) bool { return strings.TrimLeft(line, " \t") == "" }

func indent(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

// trimIndent is the line without the up to three spaces a block may start with.
func trimIndent(line string) string {
	for n := 0; n < 3 && strings.HasPrefix(line, " "); n++ {
		line = line[1:]
	}
	return line
}

func bullet(s string) bool {
	return len(s) >= 2 && strings.IndexByte("-*+", s[0]) >= 0 && (s[1] == ' ' || s[1] == '\t')
}

// joinLines is the text of a paragraph or list item: each line without its indentation, the last without the white
// space after it.
func joinLines(lines []string) string {
	for i, l := range lines {
		lines[i] = strings.TrimLeft(l, " \t")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " \t")
}

// inline renders the inlines of a paragraph or list item.
func (r *renderer) inline(out *strings.Builder, text string) {
	p := inlineParser{src: text, ticks: backtickRuns(text)}
	p.parse()
	r.nodes(out, p.root.first)
}

func (r *renderer) nodes(out *strings.Builder, n *node) {
	for ; n != nil; n = n.next {
		switch n.kind {
		case nodeText:
			r.text(out, n.text)
		case nodeCode:
			out.WriteString("<code>")
			r.text(out, n.text)
			out.WriteString("</code>")
		case nodeHardBreak:
			out.WriteString("<br>")
		case nodeSoftBreak:
			out.WriteByte('\n')
		case nodeEm:
			r.element(out, "em", n)
		case nodeStrong:
			r.element(out, "strong", n)
		case nodeDel:
			r.element(out, "del", n)
		case nodeLink:
			out.WriteString(`<a href="` + html.EscapeString(n.text) + `"` + linkAttrs + ">")
			r.nodes(out, n.first)
			out.WriteString("</a>")
		}
	}
}

func (r *renderer) element(out *strings.Builder, tag string, n *node) {
	out.WriteString("<" + tag + ">")
	r.nodes(out, n.first)
	out.WriteString("</" + tag + ">")
}

// text writes s escaped, with the span of each highlighted mention in place of its placeholder.
func (r *renderer) text(out *strings.Builder, s string) {
	_, _ = r.spans.WriteString(out, html.EscapeString(s))
}

type nodeKind uint8

const (
	nodeText nodeKind = iota
	nodeCode
	nodeHardBreak
	nodeSoftBreak
	nodeEm
	nodeStrong
	nodeDel
	nodeLink
)

// node is a piece of a paragraph: text, a code span or a break, or emphasis or a link around the nodes from first to
// last.
type node struct {
	kind        nodeKind
	text        string // the text, the code or the link's address
	prev, next  *node
	first, last *node
}

// delimiter is a run of *, _ or ~~ that may still open or close emphasis. seq orders the delimiters as they appear.
type delimiter struct {
	node              *node
	seq               int
	char              byte
	count, length     int // the characters left, and those of the whole run
	canOpen, canClose bool
	prev, next        *delimiter
}

// bracket is a [ that may still open a link, or the ![ of an image, which never does.
type bracket struct {
	node   *node
	below  *delimiter // the newest delimiter when the bracket was seen
	image  bool
	active bool
	prev   *bracket
}

// inlineParser turns a paragraph into nodes in one pass and then matches the emphasis delimiters, as CommonMark
// describes. Every search ahead is bounded or looked up, so a body of any shape renders in about linear time.
type inlineParser struct {
	src      string
	pos      int
	root     node
	pending  strings.Builder
	delims   *delimiter
	seq      int
	brackets *bracket
	ticks    map[int][]int // the starts of the runs of backticks, by length
	parens   []int32       // for each (, the ) that closes it within the same word, or -1
}

const specialBytes = "\\`*_~[]!\n "

func (p *inlineParser) parse() {
	s := p.src
	for p.pos < len(s) {
		switch c := s[p.pos]; c {
		case '\\':
			p.backslash()
		case '`':
			p.codeSpan()
		case '*', '_', '~':
			p.delimiterRun(c)
		case '[':
			p.openBracket(false, 1)
		case '!':
			if strings.HasPrefix(s[p.pos:], "![") {
				p.openBracket(true, 2)
			} else {
				p.pending.WriteByte('!')
				p.pos++
			}
		case ']':
			p.closeBracket()
		case '\n':
			p.lineBreak(false)
		case ' ':
			p.spaces()
		default:
			end := len(s)
			if i := strings.IndexAny(s[p.pos+1:], specialBytes); i >= 0 {
				end = p.pos + 1 + i
			}
			p.pending.WriteString(s[p.pos:end])
			p.pos = end
		}
	}
	p.flush()
	p.processEmphasis(nil)
}

func (p *inlineParser) backslash() {
	if p.pos+1 < len(p.src) {
		switch next := p.src[p.pos+1]; {
		case next == '\n':
			p.pos++
			p.lineBreak(true)
			return
		case isASCIIPunct(next):
			p.pending.WriteByte(next)
			p.pos += 2
			return
		}
	}
	p.pending.WriteByte('\\')
	p.pos++
}

// spaces drops the spaces before a line break, where two or more make it a hard one, and at the end.
func (p *inlineParser) spaces() {
	end := p.pos
	for end < len(p.src) && p.src[end] == ' ' {
		end++
	}
	switch {
	case end == len(p.src):
		p.pos = end
	case p.src[end] == '\n':
		hard := end-p.pos >= 2
		p.pos = end
		p.lineBreak(hard)
	default:
		p.pending.WriteString(p.src[p.pos:end])
		p.pos = end
	}
}

// lineBreak adds a break for the \n at p.pos.
func (p *inlineParser) lineBreak(hard bool) {
	p.flush()
	if hard {
		p.add(nodeHardBreak, "")
	} else {
		p.add(nodeSoftBreak, "")
	}
	p.pos++
}

// codeSpan makes a code span of the run of backticks at p.pos and the next run of the same length, or text of the run
// when there is none.
func (p *inlineParser) codeSpan() {
	start := p.pos
	end := start
	for end < len(p.src) && p.src[end] == '`' {
		end++
	}
	p.pos = end
	runs := p.ticks[end-start]
	k := sort.SearchInts(runs, end)
	if k == len(runs) {
		p.pending.WriteString(p.src[start:end])
		return
	}
	closer := runs[k]
	code := strings.ReplaceAll(p.src[end:closer], "\n", " ")
	if len(code) >= 2 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.Trim(code, " ") != "" {
		code = code[1 : len(code)-1]
	}
	p.flush()
	p.add(nodeCode, code)
	p.pos = closer + end - start
}

// delimiterRun reads a run of *, _ or ~ and notes whether it can open or close emphasis; ~ counts only as ~~.
func (p *inlineParser) delimiterRun(c byte) {
	start := p.pos
	end := start
	for end < len(p.src) && p.src[end] == c {
		end++
	}
	p.pos = end
	if c == '~' && end-start != 2 {
		p.pending.WriteString(p.src[start:end])
		return
	}
	before, after := '\n', '\n'
	if start > 0 {
		before, _ = utf8.DecodeLastRuneInString(p.src[:start])
	}
	if end < len(p.src) {
		after, _ = utf8.DecodeRuneInString(p.src[end:])
	}
	left := !unicode.IsSpace(after) && (!isPunct(after) || unicode.IsSpace(before) || isPunct(before))
	right := !unicode.IsSpace(before) && (!isPunct(before) || unicode.IsSpace(after) || isPunct(after))
	canOpen, canClose := left, right
	if c == '_' {
		canOpen = left && (!right || isPunct(before))
		canClose = right && (!left || isPunct(after))
	}
	p.flush()
	n := p.add(nodeText, p.src[start:end])
	if !canOpen && !canClose {
		return
	}
	p.seq++
	d := &delimiter{node: n, seq: p.seq, char: c, count: end - start, length: end - start, canOpen: canOpen,
		canClose: canClose, prev: p.delims}
	if p.delims != nil {
		p.delims.next = d
	}
	p.delims = d
}

func (p *inlineParser) openBracket(image bool, width int) {
	p.flush()
	n := p.add(nodeText, p.src[p.pos:p.pos+width])
	p.brackets = &bracket{node: n, below: p.delims, image: image, active: true, prev: p.brackets}
	p.pos += width
}

// closeBracket makes a link of the newest bracket and the nodes after it when the ] at p.pos is followed by a safe
// destination; otherwise the ] is text.
func (p *inlineParser) closeBracket() {
	p.pos++
	b := p.brackets
	if b == nil {
		p.pending.WriteByte(']')
		return
	}
	p.brackets = b.prev
	if !b.active || b.image {
		p.pending.WriteByte(']')
		return
	}
	dest, end, ok := p.destination(p.pos)
	if !ok || !webLink(dest) {
		p.pending.WriteByte(']')
		return
	}
	p.flush()
	p.processEmphasis(b.below)
	link := &node{kind: nodeLink, text: dest, prev: b.node.prev}
	if first := b.node.next; first != nil {
		link.first, link.last = first, p.root.last
		first.prev = nil
	}
	if link.prev != nil {
		link.prev.next = link
	} else {
		p.root.first = link
	}
	p.root.last = link
	// No link inside a link: every older bracket is text now, and so were those below an inactive one already.
	for o := p.brackets; o != nil && o.active; o = o.prev {
		o.active = false
	}
	p.pos = end
}

// destination reads (dest) or (<dest>) at pos: the destination unescaped and where the link ends. A destination longer
// than maxDestBytes is refused without reading further, which keeps nested destinations linear.
func (p *inlineParser) destination(pos int) (string, int, bool) {
	s := p.src
	if pos >= len(s) || s[pos] != '(' {
		return "", 0, false
	}
	if strings.HasPrefix(s[pos:], "(<") {
		// The scan ends on the byte after the longest destination allowed, where its > may stand.
		for i, stop := pos+2, min(len(s), pos+3+maxDestBytes); i < stop; i++ {
			switch s[i] {
			case '\\':
				if i+1 < len(s) && isASCIIPunct(s[i+1]) {
					i++
				}
			case '\n', '<':
				return "", 0, false
			case '>':
				if strings.HasPrefix(s[i:], ">)") {
					return unescape(s[pos+2 : i]), i + 2, true
				}
				return "", 0, false
			}
		}
		return "", 0, false
	}
	if p.parens == nil {
		p.parens = matchParens(s)
	}
	end := int(p.parens[pos])
	if end < 0 || end-pos-1 > maxDestBytes {
		return "", 0, false
	}
	return unescape(s[pos+1 : end]), end + 1, true
}

// processEmphasis matches the delimiters above bottom into emphasis, strong emphasis and strikethrough and drops them
// all from the stack; the unmatched stay text. openersBottom remembers, per kind of closer, below which no opener
// can match it, so that no search walks the same delimiters twice.
func (p *inlineParser) processEmphasis(bottom *delimiter) {
	floor := 0
	if bottom != nil {
		floor = bottom.seq
	}
	var first *delimiter
	for d := p.delims; d != nil && d != bottom; d = d.prev {
		first = d
	}
	var openersBottom [3][2][3]int
	for i := range openersBottom {
		for j := range openersBottom[i] {
			for k := range openersBottom[i][j] {
				openersBottom[i][j][k] = floor
			}
		}
	}
	for closer := first; closer != nil; {
		if !closer.canClose {
			closer = closer.next
			continue
		}
		limit := &openersBottom[strings.IndexByte("*_~", closer.char)][boolIndex(closer.canOpen)][closer.length%3]
		var opener *delimiter
		for o := closer.prev; o != nil && o.seq > *limit; o = o.prev {
			if o.char == closer.char && o.canOpen && !ruleOfThree(o, closer) {
				opener = o
				break
			}
		}
		if opener == nil {
			if closer.prev != nil {
				*limit = max(*limit, closer.prev.seq)
			}
			next := closer.next
			if !closer.canOpen {
				p.unlink(closer)
			}
			closer = next
			continue
		}
		use, kind := 1, nodeEm
		switch {
		case closer.char == '~':
			use, kind = 2, nodeDel
		case opener.count >= 2 && closer.count >= 2:
			use, kind = 2, nodeStrong
		}
		opener.count -= use
		closer.count -= use
		opener.node.text = opener.node.text[:opener.count]
		closer.node.text = closer.node.text[:closer.count]
		wrap(kind, opener.node, closer.node)
		opener.next, closer.prev = closer, opener
		if opener.count == 0 {
			p.unlink(opener)
		}
		if closer.count == 0 {
			next := closer.next
			p.unlink(closer)
			closer = next
		}
	}
	p.delims = bottom
	if bottom != nil {
		bottom.next = nil
	}
}

// ruleOfThree is CommonMark's rule that a run that can both open and close matches another only when the sum of
// their lengths is not a multiple of 3, unless both are.
func ruleOfThree(opener, closer *delimiter) bool {
	return closer.char != '~' && (opener.canClose || closer.canOpen) && (opener.length+closer.length)%3 == 0 &&
		(opener.length%3 != 0 || closer.length%3 != 0)
}

func (p *inlineParser) unlink(d *delimiter) {
	if d.prev != nil {
		d.prev.next = d.next
	}
	if d.next != nil {
		d.next.prev = d.prev
	}
	if p.delims == d {
		p.delims = d.prev
	}
}

// wrap moves the nodes between opener and closer into a new node of the kind between them.
func wrap(kind nodeKind, opener, closer *node) {
	n := &node{kind: kind, prev: opener, next: closer}
	if first := opener.next; first != closer {
		n.first, n.last = first, closer.prev
		first.prev, n.last.next = nil, nil
	}
	opener.next, closer.prev = n, n
}

func (p *inlineParser) add(kind nodeKind, text string) *node {
	n := &node{kind: kind, text: text, prev: p.root.last}
	if p.root.last != nil {
		p.root.last.next = n
	} else {
		p.root.first = n
	}
	p.root.last = n
	return n
}

func (p *inlineParser) flush() {
	if p.pending.Len() > 0 {
		p.add(nodeText, p.pending.String())
		p.pending.Reset()
	}
}

// backtickRuns are the starts of the runs of backticks in s by their length, in order.
func backtickRuns(s string) map[int][]int {
	runs := map[int][]int{}
	for i := strings.IndexByte(s, '`'); i >= 0; {
		end := i
		for end < len(s) && s[end] == '`' {
			end++
		}
		runs[end-i] = append(runs[end-i], i)
		next := strings.IndexByte(s[end:], '`')
		if next < 0 {
			break
		}
		i = end + next
	}
	return runs
}

// matchParens is, for each ( of s, the ) that closes it before the next white space, else -1. Escaped parentheses
// do not count.
func matchParens(s string) []int32 {
	match := make([]int32, len(s))
	var open []int32
	for i := 0; i < len(s); i++ {
		match[i] = -1
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s) && isASCIIPunct(s[i+1]):
			i++
			match[i] = -1
		case c <= ' ' || c == 0x7f:
			open = open[:0]
		case c == '(':
			open = append(open, int32(i))
		case c == ')' && len(open) > 0:
			match[open[len(open)-1]] = int32(i)
			open = open[:len(open)-1]
		}
	}
	return match
}

func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && isASCIIPunct(s[i+1]) {
			i++
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

func isASCIIPunct(c byte) bool {
	return c >= '!' && c <= '/' || c >= ':' && c <= '@' || c >= '[' && c <= '`' || c >= '{' && c <= '~'
}

func isPunct(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) }

func boolIndex(b bool) int {
	if b {
		return 1
	}
	return 0
}
