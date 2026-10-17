// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"regexp"
	"strings"
)

// mentionMarker is a Mention marker of a body, [@Display Name](muster-mention:ref), whose name Muster escaped for
// CommonMark: the link text, then the ref.
var mentionMarker = regexp.MustCompile(`\[(@(?:\\.|[^\\\]\n])*)\]\(muster-mention:([a-z0-9]{1,16})\)`)

// ReplaceMentions replaces each Mention marker [@name](muster-mention:ref) of body whose ref names an entry of
// mentions by what render makes of that entry: the messenger's own mention for a bridge with native_mentions, or the
// plain name. A marker whose ref names no entry, or every marker when render is nil, becomes its link text, the @name
// as the body escapes it. The rest of body is unchanged.
func ReplaceMentions(body string, mentions []Mention, render func(Mention) string) string {
	byRef := make(map[string]Mention, len(mentions))
	for _, m := range mentions {
		if _, ok := byRef[m.Ref]; !ok {
			byRef[m.Ref] = m
		}
	}
	var out strings.Builder
	last := 0
	for _, loc := range mentionMarker.FindAllStringSubmatchIndex(body, -1) {
		out.WriteString(body[last:loc[0]])
		text, ref := body[loc[2]:loc[3]], body[loc[4]:loc[5]]
		if m, ok := byRef[ref]; ok && render != nil {
			out.WriteString(render(m))
		} else {
			out.WriteString(text)
		}
		last = loc[1]
	}
	if last == 0 {
		return body
	}
	out.WriteString(body[last:])
	return out.String()
}
