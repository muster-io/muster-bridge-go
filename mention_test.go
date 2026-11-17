// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"slices"
	"strings"
	"testing"
)

func TestReplaceMentions(t *testing.T) {
	mentions := []Mention{
		{Ref: "m1", ExternalId: "u-alice", DisplayName: "Alice Smith"},
		{Ref: "m2", ExternalId: "u-bob", DisplayName: "Bob [ops]"},
		{Ref: "m1", ExternalId: "u-other", DisplayName: "Not the first"},
	}
	native := func(m Mention) string { return "<@" + m.ExternalId + ">" }
	tests := []struct {
		name, body string
		mentions   []Mention
		render     func(Mention) string
		want       string
	}{
		{"one marker", "[@Alice Smith](muster-mention:m1) please look", mentions, native, "<@u-alice> please look"},
		{"several", "[@Alice Smith](muster-mention:m1), [@Bob](muster-mention:m2)", mentions, native,
			"<@u-alice>, <@u-bob>"},
		{"unknown ref", "[@Carol](muster-mention:m9) and [@Alice Smith](muster-mention:m1)", mentions, native,
			"@Carol and <@u-alice>"},
		{"escaped name", `[@Bob \[ops\]](muster-mention:m2)`, mentions, native, "<@u-bob>"},
		{"escaped name kept", `[@Bob \[ops\]](muster-mention:m3)`, mentions, native, `@Bob \[ops\]`},
		{"other links untouched", "[@Alice Smith](muster-mention:m1) [Runbook](https://wiki.example.org/x)",
			mentions, native, "<@u-alice> [Runbook](https://wiki.example.org/x)"},
		{"no markers", "**prod** · started 14:03", mentions, native, "**prod** · started 14:03"},
		{"ref of 17 characters", "[@A](muster-mention:abcdefghijklmnopq)", mentions, native,
			"[@A](muster-mention:abcdefghijklmnopq)"},
		{"upper-case ref", "[@A](muster-mention:M1)", mentions, native, "[@A](muster-mention:M1)"},
		{"no @", "[Alice](muster-mention:m1)", mentions, native, "[Alice](muster-mention:m1)"},
		{"no mentions", "[@Alice Smith](muster-mention:m1)", nil, native, "@Alice Smith"},
		{"plain names", "[@Alice Smith](muster-mention:m1)!", mentions,
			func(m Mention) string { return m.DisplayName }, "Alice Smith!"},
		{"nil render", "[@Alice Smith](muster-mention:m1)", mentions, nil, "@Alice Smith"},
		{"first entry wins", "[@Alice Smith](muster-mention:m1)", mentions, native, "<@u-alice>"},
		{"no line break in a name", "[@Alice\nSmith](muster-mention:m1)", mentions, native,
			"[@Alice\nSmith](muster-mention:m1)"},
		{"empty", "", mentions, native, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReplaceMentions(tt.body, tt.mentions, tt.render); got != tt.want {
				t.Fatalf("ReplaceMentions(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func kind(k MentionKind) *MentionKind { return &k }

func TestReplaceMentionsKinds(t *testing.T) {
	mentions := []Mention{
		{Ref: "u1", ExternalId: "u-alice", DisplayName: "Alice Smith"},
		{Ref: "u2", ExternalId: "u-bob", DisplayName: "Bob", Kind: kind(MentionKindMentionKindUser)},
		{Ref: "g1", ExternalId: "payments_team", DisplayName: "payments_team", Kind: kind(MentionKindMentionKindGroup)},
		{Ref: "e1", ExternalId: "here", DisplayName: "here", Kind: kind(MentionKindMentionKindEveryone)},
		{Ref: "e2", ExternalId: "channel", DisplayName: "channel", Kind: kind(MentionKindMentionKindEveryone)},
	}
	body := "[@Alice Smith](muster-mention:u1) [@Bob](muster-mention:u2) [@payments\\_team](muster-mention:g1) " +
		"[@here](muster-mention:e1) [@channel](muster-mention:e2)"
	var seen []string
	render := func(m Mention) string {
		if m.Kind == nil {
			t.Fatalf("render got %+v without a kind", m)
		}
		seen = append(seen, m.Ref+"="+string(*m.Kind))
		switch *m.Kind {
		case MentionKindMentionKindGroup:
			return "<!subteam:" + m.ExternalId + ">"
		case MentionKindMentionKindEveryone:
			return "<!" + m.ExternalId + ">"
		case MentionKindMentionKindUser:
		}
		return "<@" + m.ExternalId + ">"
	}

	got := ReplaceMentions(body, mentions, render)
	if want := "<@u-alice> <@u-bob> <!subteam:payments_team> <!here> <!channel>"; got != want {
		t.Fatalf("ReplaceMentions = %q, want %q", got, want)
	}
	if want := []string{"u1=user", "u2=user", "g1=group", "e1=everyone", "e2=everyone"}; !slices.Equal(seen, want) {
		t.Fatalf("render saw %v, want %v", seen, want)
	}
	if mentions[0].Kind != nil || mentions[1].Kind == nil {
		t.Fatal("ReplaceMentions changed the caller's entries")
	}
}

func TestBridgeReplaceMentionsShowsUndeclaredKindsAsLinkText(t *testing.T) {
	mentions := []Mention{
		{Ref: "u1", ExternalId: "u-alice", DisplayName: "Alice Smith"},
		{Ref: "g1", ExternalId: "payments_team", DisplayName: "payments_team", Kind: kind(MentionKindMentionKindGroup)},
		{Ref: "e1", ExternalId: "here", DisplayName: "here", Kind: kind(MentionKindMentionKindEveryone)},
		{Ref: "e2", ExternalId: "channel", DisplayName: "channel", Kind: kind(MentionKindMentionKindEveryone)},
	}
	body := "[@Alice Smith](muster-mention:u1) [@payments\\_team](muster-mention:g1) [@here](muster-mention:e1) " +
		"[@channel](muster-mention:e2)"
	render := func(m Mention) string { return "<" + string(*m.Kind) + ":" + m.ExternalId + ">" }
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"nothing declared", func(*Config) {},
			"<user:u-alice> @payments\\_team @here @channel"},
		{"groups only", func(c *Config) { c.GroupMentions = true },
			"<user:u-alice> <group:payments_team> @here @channel"},
		{"here only", func(c *Config) { c.EveryoneMentions = []string{"here"} },
			"<user:u-alice> @payments\\_team <everyone:here> @channel"},
		{"both", func(c *Config) { c.GroupMentions = true; c.EveryoneMentions = []string{"here", "channel"} },
			"<user:u-alice> <group:payments_team> <everyone:here> <everyone:channel>"},
		{"through Capabilities", func(c *Config) {
			c.Capabilities.GroupMentions = new(true)
			c.Capabilities.EveryoneMentions = &[]CapabilitiesEveryoneMentions{CapabilitiesEveryoneMentionsEveryoneMentionChannel}
		}, "<user:u-alice> <group:payments_team> @here <everyone:channel>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _, _, _ := newTestBridge(t, tt.mutate)
			if got := b.ReplaceMentions(body, mentions, render); got != tt.want {
				t.Fatalf("Bridge.ReplaceMentions = %q, want %q", got, tt.want)
			}
		})
	}
	b, _, _, _ := newTestBridge(t, nil)
	if got := b.ReplaceMentions(body, mentions, nil); !strings.HasPrefix(got, "@Alice Smith @payments") {
		t.Fatalf("a nil render gave %q", got)
	}
}
