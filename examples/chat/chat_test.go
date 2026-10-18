// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	bridge "github.com/muster-io/muster-bridge-go"
)

func newTestChat(t *testing.T, prefix string) (*chat, *fakeClock) {
	t.Helper()
	people, err := parseUsers("alice:Alice Smith,bob:Bob Brown")
	if err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	return newChat([]string{"alerts", "ops"}, people, clock.Now, prefix), clock
}

func botRoot(channel, title string, labels ...string) post {
	p := post{Channel: channel, Bot: true, Kind: bridge.MessageKindRoot, Status: bridge.StatusFiring, Title: title,
		Body: title, HTML: "<p>" + title + "</p>"}
	for i, label := range labels {
		p.Buttons = append(p.Buttons, pageButton{Label: label, Style: bridge.ButtonStylePrimary})
		p.data = append(p.data, fmt.Sprintf("data-%d", i))
	}
	return p
}

func mustPost(t *testing.T, c *chat, p post) string {
	t.Helper()
	id, err := c.postRoot(p)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustReply(t *testing.T, c *chat, parent string, p post) placed {
	t.Helper()
	at, err := c.postInThread(parent, "", p)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// find is the Root post with the id among the channel's posts, with its Thread.
func find(t *testing.T, c *chat, channel, id string) post {
	t.Helper()
	posts, _ := c.channelPosts(channel)
	for _, p := range posts {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no post %s in %s", id, channel)
	return post{}
}

func TestIDsCarryTheStartPrefix(t *testing.T) {
	seen := map[string]string{}
	for _, prefix := range []string{"aaaaaa", "bbbbbb"} {
		c, _ := newTestChat(t, prefix)
		root := mustPost(t, c, botRoot("alerts", "a"))
		reply := mustReply(t, c, root, post{Bot: true, Body: "r"}).id
		direct, err := c.addDirect("alice", directEntry{Bot: true, Body: "d"})
		if err != nil {
			t.Fatal(err)
		}
		update := c.nextUpdate()
		for _, id := range []string{root, reply, direct} {
			if !strings.HasPrefix(id, prefix+"-") {
				t.Errorf("id %q lacks the prefix %s", id, prefix)
			}
		}
		if !strings.HasPrefix(update, prefix+"-u") {
			t.Errorf("update id %q lacks the prefix %s-u", update, prefix)
		}
		for _, id := range []string{root, reply, direct, update} {
			if other, ok := seen[id]; ok {
				t.Errorf("id %q of %s repeats one of %s", id, prefix, other)
			}
			seen[id] = prefix
		}
	}
}

func TestThreadPostsResolveToTheRoot(t *testing.T) {
	c, _ := newTestChat(t, "thread")
	root := mustPost(t, c, botRoot("alerts", "a"))
	first := mustReply(t, c, root, post{Bot: true, Body: "first"})
	second := mustReply(t, c, first.id, post{Author: "alice", Text: "second"})
	if first.root != root || second.root != root || second.channel != "alerts" {
		t.Fatalf("placed %+v and %+v, want both under %s in alerts", first, second, root)
	}
	got := find(t, c, "alerts", root)
	if len(got.Thread) != 2 || got.Thread[0].ID != first.id || got.Thread[1].ID != second.id {
		t.Fatalf("Thread %+v, want %s then %s", got.Thread, first.id, second.id)
	}
	if got.Thread[1].Root != root || got.Thread[1].Thread != nil {
		t.Errorf("Thread post %+v, want root %s and no Thread of its own", got.Thread[1], root)
	}
	if r, err := c.rootOf(second.id); err != nil || r != root {
		t.Errorf("rootOf = %q, %v; want %s", r, err, root)
	}
	if _, err := c.postInThread(root, "ops", post{Text: "x"}); !errors.Is(err, errNoPost) {
		t.Errorf("posting in a Thread of another channel: %v, want errNoPost", err)
	}
}

func TestEditMarksTheEdit(t *testing.T) {
	c, clock := newTestChat(t, "edit")
	root := mustPost(t, c, botRoot("alerts", "before", "Ack"))
	posted := clock.Now()
	reply := mustReply(t, c, root, post{Bot: true, Body: "r"})
	clock.Advance(10 * time.Second)
	if err := c.edit(root, botRoot("alerts", "after"), 0); err != nil {
		t.Fatal(err)
	}
	got := find(t, c, "alerts", root)
	if got.Title != "after" || len(got.Buttons) != 0 || got.Edits != 1 {
		t.Errorf("edited post %+v, want title after, no buttons, 1 edit", got)
	}
	if got.EditedAt == nil || !got.EditedAt.Equal(clock.Now()) || !got.PostedAt.Equal(posted) {
		t.Errorf("posted %v, edited %v; want posted %v and edited %v", got.PostedAt, got.EditedAt, posted,
			clock.Now())
	}
	if len(got.Thread) != 1 || got.Thread[0].ID != reply.id {
		t.Errorf("Thread %+v after the edit, want %s", got.Thread, reply.id)
	}
	if err := c.edit("nope", botRoot("alerts", "x"), 0); !errors.Is(err, errNoPost) {
		t.Errorf("editing an unknown post: %v, want errNoPost", err)
	}
	person := mustReply(t, c, root, post{Author: "alice", Text: "hi"})
	if err := c.edit(person.id, post{Text: "changed"}, 0); !errors.Is(err, errNoPost) {
		t.Errorf("editing a person's post: %v, want errNoPost", err)
	}
}

func TestEditWindow(t *testing.T) {
	c, clock := newTestChat(t, "window")
	root := mustPost(t, c, botRoot("alerts", "a"))
	clock.Advance(300 * time.Second)
	if err := c.edit(root, botRoot("alerts", "b"), 300*time.Second); err != nil {
		t.Errorf("an edit at the edge of the window: %v", err)
	}
	clock.Advance(time.Second)
	if err := c.edit(root, botRoot("alerts", "c"), 300*time.Second); !errors.Is(err, errEditWindow) {
		t.Errorf("an edit past the window: %v, want errEditWindow", err)
	}
	clock.Advance(365 * 24 * time.Hour)
	if err := c.edit(root, botRoot("alerts", "d"), 0); err != nil {
		t.Errorf("an edit without a window: %v", err)
	}
}

func TestRemoveLeavesATombstone(t *testing.T) {
	c, _ := newTestChat(t, "remove")
	root := mustPost(t, c, botRoot("alerts", "a", "Ack"))
	reply := mustReply(t, c, root, post{Author: "alice", Text: "mine"})
	if err := c.addNote(root, "alerts", "alice", "only alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.remove(reply.id, true); !errors.Is(err, errNoPost) {
		t.Errorf("the bot removing a person's post: %v, want errNoPost", err)
	}
	wasBot, err := c.remove(root, false)
	if err != nil || !wasBot {
		t.Fatalf("remove = %v, %v; want the bot's post removed", wasBot, err)
	}
	got := find(t, c, "alerts", root)
	if !got.Deleted || got.Title != "" || got.HTML != "" || got.Body != "" || len(got.Buttons) != 0 ||
		len(got.Notes) != 0 || got.Buttons == nil || got.Notes == nil {
		t.Errorf("tombstone %+v, want deleted and empty", got)
	}
	if len(got.Thread) != 1 || got.Thread[0].ID != reply.id {
		t.Errorf("tombstone's Thread %+v, want %s kept", got.Thread, reply.id)
	}
	for name, err := range map[string]error{
		"edit":   c.edit(root, botRoot("alerts", "b"), 0),
		"reply":  func() error { _, err := c.postInThread(root, "", post{Text: "x"}); return err }(),
		"press":  func() error { _, err := c.pressTarget(root, 0); return err }(),
		"note":   c.addNote(root, "alerts", "alice", "x"),
		"remove": func() error { _, err := c.remove(root, false); return err }(),
		"reply under a Thread post": func() error {
			_, err := c.postInThread(reply.id, "", post{Text: "x"})
			return err
		}(),
	} {
		if !errors.Is(err, errNoPost) {
			t.Errorf("%s of a tombstone: %v, want errNoPost", name, err)
		}
	}
	if wasBot, err := c.remove(reply.id, false); err != nil || wasBot {
		t.Errorf("removing a person's post = %v, %v; want not the bot's", wasBot, err)
	}
}

func TestNotesAndDirectChats(t *testing.T) {
	c, clock := newTestChat(t, "notes")
	root := mustPost(t, c, botRoot("alerts", "a", "Ack"))
	if err := c.addNote(root, "alerts", "alice", "Done"); err != nil {
		t.Fatal(err)
	}
	got := find(t, c, "alerts", root)
	if len(got.Notes) != 1 || got.Notes[0] != (note{For: "alice", Text: "Done", At: clock.Now()}) {
		t.Errorf("notes %+v, want one for alice", got.Notes)
	}
	if err := c.addNote(root, "alerts", "carol", "x"); !errors.Is(err, errNoPerson) {
		t.Errorf("a note for an unknown person: %v, want errNoPerson", err)
	}
	if err := c.addNote(root, "ops", "alice", "x"); !errors.Is(err, errNoPost) {
		t.Errorf("a note on a post of another channel: %v, want errNoPost", err)
	}
	if _, err := c.addDirect("carol", directEntry{Text: "x"}); !errors.Is(err, errNoPerson) {
		t.Errorf("a direct message to an unknown person: %v, want errNoPerson", err)
	}
	id, err := c.addDirect("alice", directEntry{Author: "alice", Text: "hi", HTML: "<p>hi</p>"})
	if err != nil {
		t.Fatal(err)
	}
	entries, ok := c.directChat("alice")
	if !ok || len(entries) != 1 || entries[0].ID != id || entries[0].Text != "hi" || !entries[0].At.Equal(clock.Now()) {
		t.Errorf("alice's direct chat %+v, want her message %s", entries, id)
	}
	if entries, ok := c.directChat("bob"); !ok || entries == nil || len(entries) != 0 {
		t.Errorf("bob's direct chat %v, %v; want empty", entries, ok)
	}
	if _, ok := c.directChat("carol"); ok {
		t.Error("an unknown person has a direct chat")
	}
}

func TestPressTarget(t *testing.T) {
	c, _ := newTestChat(t, "press")
	root := mustPost(t, c, botRoot("ops", "a", "Ack", "Resolve"))
	first, err := c.pressTarget(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.pressTarget(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.data != "data-0" || first.label != "Ack" || first.channel != "ops" ||
		second.data != "data-1" || second.label != "Resolve" {
		t.Errorf("targets %+v and %+v", first, second)
	}
	if first.updateID == second.updateID || !strings.HasPrefix(first.updateID, "press-u") {
		t.Errorf("update ids %q and %q, want two of the prefix", first.updateID, second.updateID)
	}
	for _, button := range []int{2, -1} {
		if _, err := c.pressTarget(root, button); !errors.Is(err, errNoButton) {
			t.Errorf("button %d: %v, want errNoButton", button, err)
		}
	}
	plain := mustPost(t, c, botRoot("ops", "no buttons"))
	if _, err := c.pressTarget(plain, 0); !errors.Is(err, errNoButton) {
		t.Errorf("a post without buttons: %v, want errNoButton", err)
	}
	if _, err := c.pressTarget("nope", 0); !errors.Is(err, errNoPost) {
		t.Errorf("an unknown post: %v, want errNoPost", err)
	}
}

func TestLogCollapsesAndCaps(t *testing.T) {
	c, clock := newTestChat(t, "log")
	c.record("call", "GET /v1/events", "200")
	clock.Advance(time.Minute)
	c.record("call", "GET /v1/events", "200")
	c.record("report", "press", "reported")
	log := c.state().Log
	if len(log) != 2 || log[0].What != "press" || log[0].Count != 1 || log[1].Count != 2 ||
		!log[1].At.Equal(clock.Now()) || log[0].Seq <= log[1].Seq {
		t.Fatalf("log %+v, want the press, then two polls in one line", log)
	}
	for i := range maxJournal + 10 {
		c.record("call", fmt.Sprintf("GET /v1/messages/%d", i), "200")
	}
	if len(c.journal) != maxJournal {
		t.Errorf("the log keeps %d lines, want %d", len(c.journal), maxJournal)
	}
	log = c.state().Log
	if len(log) != stateLog || log[0].What != fmt.Sprintf("GET /v1/messages/%d", maxJournal+9) {
		t.Errorf("the state has %d lines starting with %q, want %d starting with the newest", len(log),
			log[0].What, stateLog)
	}
	for i := 1; i < len(log); i++ {
		if log[i].Seq >= log[i-1].Seq {
			t.Fatalf("line %d has seq %d after %d, want newest first", i, log[i].Seq, log[i-1].Seq)
		}
	}
}

func TestCapsDropTheOldest(t *testing.T) {
	c, _ := newTestChat(t, "caps")
	first := mustPost(t, c, botRoot("alerts", "first"))
	reply := mustReply(t, c, first, post{Bot: true, Body: "r"})
	for i := range maxRoots {
		mustPost(t, c, botRoot("alerts", fmt.Sprint(i)))
	}
	if posts, _ := c.channelPosts("alerts"); len(posts) != maxRoots || posts[0].Title != "0" {
		t.Errorf("the channel keeps %d posts, want %d from the second", len(posts), maxRoots)
	}
	if err := c.edit(first, botRoot("alerts", "x"), 0); !errors.Is(err, errNoPost) {
		t.Errorf("editing a forgotten post: %v, want errNoPost", err)
	}
	if _, err := c.rootOf(reply.id); !errors.Is(err, errNoPost) {
		t.Errorf("a Thread post of a forgotten post: %v, want errNoPost", err)
	}
	root := mustPost(t, c, botRoot("ops", "root"))
	oldest := mustReply(t, c, root, post{Bot: true, Body: "oldest"})
	for range maxThread {
		mustReply(t, c, root, post{Bot: true, Body: "r"})
	}
	if got := find(t, c, "ops", root); len(got.Thread) != maxThread || got.Thread[0].ID == oldest.id {
		t.Errorf("the Thread keeps %d posts, want %d without the oldest", len(got.Thread), maxThread)
	}
	for range maxDirect + 1 {
		if _, err := c.addDirect("bob", directEntry{Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if entries, _ := c.directChat("bob"); len(entries) != maxDirect {
		t.Errorf("the direct chat keeps %d entries, want %d", len(entries), maxDirect)
	}
}

func TestVersionCountsChanges(t *testing.T) {
	c, _ := newTestChat(t, "version")
	v := c.state().Version
	step := func(name string, change bool, f func()) {
		t.Helper()
		f()
		got := c.state().Version
		if change && got != v+1 || !change && got != v {
			t.Errorf("%s: version %d after %d", name, got, v)
		}
		v = got
	}
	var root string
	step("post", true, func() { root = mustPost(t, c, botRoot("alerts", "a", "Ack")) })
	step("read posts", false, func() { c.channelPosts("alerts") })
	step("read a direct chat", false, func() { c.directChat("alice") })
	step("press", false, func() { _, _ = c.pressTarget(root, 0) })
	step("edit", true, func() { _ = c.edit(root, botRoot("alerts", "b"), 0) })
	step("reply", true, func() { mustReply(t, c, root, post{Text: "r"}) })
	step("note", true, func() { _ = c.addNote(root, "alerts", "alice", "n") })
	step("direct", true, func() { _, _ = c.addDirect("alice", directEntry{Text: "d"}) })
	step("record", false, func() { c.record("call", "x", "200") })
	step("record again", false, func() { c.record("call", "x", "200") })
	step("record a report", false, func() { c.record("report", "press", "reported") })
	step("remove", true, func() { _, _ = c.remove(root, false) })
}

func TestCopiesAreDeep(t *testing.T) {
	c, _ := newTestChat(t, "copies")
	root := mustPost(t, c, botRoot("alerts", "a", "Ack"))
	mustReply(t, c, root, post{Bot: true, Body: "r", Buttons: []pageButton{{Label: "Snooze"}}, data: []string{"s"}})
	if err := c.edit(root, botRoot("alerts", "a", "Ack"), 0); err != nil {
		t.Fatal(err)
	}
	if err := c.addNote(root, "alerts", "alice", "n"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.addDirect("alice", directEntry{Text: "d"}); err != nil {
		t.Fatal(err)
	}
	posts, _ := c.channelPosts("alerts")
	posts[0].Buttons[0].Label = "changed"
	posts[0].Notes[0].Text = "changed"
	*posts[0].EditedAt = time.Time{}
	posts[0].Thread[0].Body = "changed"
	posts[0].Thread[0].Buttons[0].Label = "changed"
	entries, _ := c.directChat("alice")
	entries[0].Text = "changed"
	people := c.state().People
	people[0].Name = "changed"
	person, _ := c.person("alice")
	*person.Handle = "changed"

	got := find(t, c, "alerts", root)
	if got.Buttons[0].Label != "Ack" || got.Notes[0].Text != "n" || got.EditedAt.IsZero() ||
		got.Thread[0].Body != "r" || got.Thread[0].Buttons[0].Label != "Snooze" {
		t.Errorf("a change to a copy reached the chat: %+v", got)
	}
	if entries, _ := c.directChat("alice"); entries[0].Text != "d" {
		t.Error("a change to a copy of a direct chat reached the chat")
	}
	if p, _ := c.person("alice"); *p.Handle != "alice" || c.state().People[0].Name != "Alice Smith" {
		t.Error("a change to a copy of a person reached the chat")
	}
	if posts[0].Thread[0].data != nil || posts[0].data != nil {
		t.Error("a copy carries the buttons' data")
	}
}

func TestChatIsSafeForConcurrentUse(t *testing.T) {
	c, _ := newTestChat(t, "race")
	root := mustPost(t, c, botRoot("alerts", "a", "Ack"))
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 50 {
				id, err := c.postRoot(botRoot("alerts", fmt.Sprint(i, j), "Ack"))
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = c.postInThread(id, "", post{Text: "r"})
				_, _ = c.pressTarget(root, 0)
				_ = c.addNote(root, "alerts", "bob", "n")
				c.record("call", "GET /v1/events", "200")
				_, _ = c.channelPosts("alerts")
				_ = c.state()
				_, _ = c.directChat("bob")
			}
		})
	}
	wg.Wait()
	if posts, _ := c.channelPosts("alerts"); len(posts) != 1+8*50 {
		t.Errorf("%d posts, want %d", len(posts), 1+8*50)
	}
}
