// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"regexp"
	"slices"
	"strings"
)

// mentionMarker is a Mention marker of a body, [@Display Name](muster-mention:ref), whose name Muster escaped for
// CommonMark: the link text, then the ref.
var mentionMarker = regexp.MustCompile(`\[(@(?:\\.|[^\\\]\n])*)\]\(muster-mention:([a-z0-9]{1,16})\)`)

// ReplaceMentions replaces each Mention marker [@name](muster-mention:ref) of body whose ref names an entry of
// mentions by what render makes of that entry: the messenger's own mention for a bridge with native_mentions, or the
// plain name. render gets the entry with its Kind set: an entry without kind is a user, a group entry carries the
// group's name in ExternalId and DisplayName, and an everyone entry the symbolic name ("channel", "here", "all" or
// "everyone"), which a bridge writes as its messenger's mention of everyone in the chat. A marker whose ref names no
// entry, or every marker when render is nil, becomes its link text, the @name as the body escapes it. The rest of body
// is unchanged. [Bridge.ReplaceMentions] also shows the entries of a kind the bridge did not declare as link text.
func ReplaceMentions(body string, mentions []Mention, render func(Mention) string) string {
	return replaceMentions(body, mentions, render, nil)
}

// ReplaceMentions is [ReplaceMentions] for this bridge's declarations: a group entry while capabilities.group_mentions
// is not declared, and an everyone entry whose name capabilities.everyone_mentions does not list, become their
// marker's link text and render never sees them. Muster decides from a copy of info that may be older than the
// bridge's own, so the library never refuses such an entry for its kind.
func (b *Bridge) ReplaceMentions(body string, mentions []Mention, render func(Mention) string) string {
	caps := b.info.Capabilities
	return replaceMentions(body, mentions, render, func(m Mention) bool {
		switch *m.Kind {
		case MentionKindMentionKindGroup:
			return caps.GroupMentions == nil || !*caps.GroupMentions
		case MentionKindMentionKindEveryone:
			return caps.EveryoneMentions == nil ||
				!slices.Contains(*caps.EveryoneMentions, CapabilitiesEveryoneMentions(m.ExternalId))
		case MentionKindMentionKindUser:
		}
		return false
	})
}

func replaceMentions(body string, mentions []Mention, render func(Mention) string, undeclared func(Mention) bool) string {
	byRef := make(map[string]Mention, len(mentions))
	for _, m := range mentions {
		if _, ok := byRef[m.Ref]; !ok {
			if m.Kind == nil || *m.Kind == "" {
				m.Kind = new(MentionKindMentionKindUser)
			}
			byRef[m.Ref] = m
		}
	}
	var out strings.Builder
	last := 0
	for _, loc := range mentionMarker.FindAllStringSubmatchIndex(body, -1) {
		out.WriteString(body[last:loc[0]])
		text, ref := body[loc[2]:loc[3]], body[loc[4]:loc[5]]
		if m, ok := byRef[ref]; ok && render != nil && (undeclared == nil || !undeclared(m)) {
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
