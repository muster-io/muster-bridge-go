// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import "testing"

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
