// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bridge "github.com/muster-io/muster-bridge-go"
)

const testToken = "example-test-token-7Kq2"

// fakeClock is the chat's clock in tests: time moves only with Advance.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testEnv is the example served by httptest: the protocol under /v1/ and the page's API under /chat/api/.
type testEnv struct {
	srv    *httptest.Server
	chat   *chat
	clock  *fakeClock
	b      *bridge.Bridge
	cursor string
	keys   int
}

type option func(*settings)

// withFeatures sets BRIDGE_FEATURES to list.
func withFeatures(list string) option {
	return func(s *settings) { s.features, _ = parseFeatures(list, true) }
}

func withEditMaxAge(seconds int) option { return func(s *settings) { s.editMaxAge = seconds } }

func withPush(target, secret string) option {
	return func(s *settings) { s.push = &bridge.PushTarget{URL: target, Secret: secret} }
}

func testSettings(t *testing.T) settings {
	t.Helper()
	people, err := parseUsers("alice:Alice Smith,bob:Bob Brown")
	if err != nil {
		t.Fatal(err)
	}
	on, _ := parseFeatures("", false)
	return settings{listen: "127.0.0.1:0", token: testToken, channels: []string{"alerts", "ops"}, people: people,
		maxBodyLength: 4000, features: on, version: "v1.2.3-test"}
}

func newTestEnv(t *testing.T, opts ...option) *testEnv {
	t.Helper()
	s := testSettings(t)
	for _, o := range opts {
		o(&s)
	}
	clock := newFakeClock()
	c := newChat(s.channels, s.people, clock.Now, strings.ToLower(rand.Text()[:6]))
	m := &messenger{chat: c, on: s.features, editMaxAge: time.Duration(s.editMaxAge) * time.Second}
	log := slog.New(slog.DiscardHandler)
	b, err := bridge.New(bridgeConfig(s, log), m)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newMux(b, c, aboutOf(s), log))
	t.Cleanup(func() {
		srv.Close()
		_ = b.Close(context.WithoutCancel(t.Context()))
	})
	return &testEnv{srv: srv, chat: c, clock: clock, b: b}
}

// call makes a protocol call with the bot's token, and an Idempotency-Key for a POST; it returns the status and the
// JSON object of the answer.
func (e *testEnv) call(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	return e.callAs(t, testToken, method, path, body)
}

func (e *testEnv) callAs(t *testing.T, token, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, e.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodPost {
		e.keys++
		req.Header.Set("Idempotency-Key", fmt.Sprintf("test-key-%d", e.keys))
	}
	status, data := do(t, req)
	var out map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("%s %s answered %d with %q", method, path, status, data)
		}
	}
	return status, out
}

// page calls the page's JSON API as the page does; a string body is sent as it is.
func (e *testEnv) page(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, e.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return do(t, req)
}

// pageJSON calls the page's JSON API and decodes an object; it fails the test on another status than want.
func (e *testEnv) pageJSON(t *testing.T, path string, body any, want int) map[string]any {
	t.Helper()
	status, data := e.page(t, http.MethodPost, path, body)
	if status != want {
		t.Fatalf("POST %s %v answered %d %s, want %d", path, body, status, data, want)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("POST %s answered %q", path, data)
	}
	return out
}

func do(t *testing.T, req *http.Request) (int, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

// events polls the events after the last cursor without waiting.
func (e *testEnv) events(t *testing.T) []map[string]any {
	t.Helper()
	path := "/v1/events?wait=0"
	if e.cursor != "" {
		path += "&cursor=" + url.QueryEscape(e.cursor)
	}
	status, body := e.call(t, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("polling answered %d %v", status, body)
	}
	e.cursor, _ = body["next_cursor"].(string)
	var out []map[string]any
	for _, ev := range body["events"].([]any) {
		out = append(out, ev.(map[string]any))
	}
	return out
}

// oneEvent polls and expects exactly one event of the type.
func (e *testEnv) oneEvent(t *testing.T, eventType string) map[string]any {
	t.Helper()
	events := e.events(t)
	if len(events) != 1 || events[0]["type"] != eventType {
		t.Fatalf("events %v, want one %s", events, eventType)
	}
	return events[0]
}

var ackButtons = []bridge.Button{
	{Label: "Ack", Style: bridge.ButtonStylePrimary, Data: "a1.ack-data"},
	{Label: "Resolve", Style: bridge.ButtonStyleSuccess, Data: "a2.resolve-data"},
}

func message(channel, title string, buttons ...bridge.Button) bridge.MessageInput {
	return bridge.MessageInput{ChannelId: channel, Kind: bridge.MessageKindRoot, Status: bridge.StatusFiring,
		Color: "#d32f2f", Emoji: "🔴", Title: title,
		Link: bridge.Link{Url: "https://muster.example.org/alert-groups/AG1", Label: "Open in Muster"},
		Body: "**ns** payments", Buttons: append([]bridge.Button{}, buttons...), Mentions: []bridge.Mention{},
		Notify: true}
}

func reply(threadRef *string, body string) bridge.ThreadReplyInput {
	return bridge.ThreadReplyInput{ThreadRef: threadRef, Body: body, Buttons: []bridge.Button{},
		Mentions: []bridge.Mention{}, Notify: true}
}

// post posts the message through the protocol and returns its id.
func (e *testEnv) post(t *testing.T, in bridge.MessageInput) string {
	t.Helper()
	status, body := e.call(t, http.MethodPost, "/v1/messages", in)
	if status != http.StatusCreated {
		t.Fatalf("posting answered %d %v", status, body)
	}
	return body["message_id"].(string)
}

// root is the Root post with the id in alerts as the chat keeps it.
func (e *testEnv) root(t *testing.T, id string) post {
	t.Helper()
	return find(t, e.chat, "alerts", id)
}

// press presses a button of the message on the page as the person and returns the event the bridge reports.
func (e *testEnv) press(t *testing.T, id, user string) map[string]any {
	t.Helper()
	e.pageJSON(t, "/chat/api/press", map[string]any{"message_id": id, "user": user}, http.StatusOK)
	return e.oneEvent(t, "press")
}

func class(body map[string]any) string {
	s, _ := body["class"].(string)
	return s
}

func TestFeaturesShapeInfo(t *testing.T) {
	all := featureNames
	tests := []struct {
		name   string
		list   string
		set    bool
		caps   []string
		lookup string
	}{
		{name: "unset", caps: all, lookup: "login"},
		{name: "thread_messages", list: "thread_messages", set: true, caps: []string{"thread_messages"},
			lookup: "none"},
		{name: "direct_messages", list: " direct_messages , buttons", set: true,
			caps: []string{"buttons", "direct_messages"}, lookup: "login"},
		{name: "empty", list: "", set: true, caps: nil, lookup: "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			on, err := parseFeatures(tt.list, tt.set)
			if err != nil {
				t.Fatal(err)
			}
			e := newTestEnv(t, func(s *settings) { s.features = on })
			status, info := e.call(t, http.MethodGet, "/v1/info", nil)
			if status != http.StatusOK {
				t.Fatalf("info answered %d", status)
			}
			caps := info["capabilities"].(map[string]any)
			var got []string
			for name := range caps {
				got = append(got, name)
			}
			slices.Sort(got)
			want := slices.Sorted(slices.Values(tt.caps))
			if !slices.Equal(got, want) {
				t.Errorf("capabilities %v, want %v", got, want)
			}
			if info["user_lookup"] != tt.lookup {
				t.Errorf("user_lookup %v, want %s", info["user_lookup"], tt.lookup)
			}
			if d := fmt.Sprint(info["event_delivery"]); d != "[poll push]" {
				t.Errorf("event_delivery %s, want [poll push]", d)
			}
		})
	}
	if _, err := parseFeatures("buttons,bogus", true); err == nil ||
		!strings.Contains(err.Error(), `"bogus"`) || !strings.Contains(err.Error(), strings.Join(all, ", ")) {
		t.Errorf("an unknown feature: %v, want an error naming it and the features", err)
	}
}

func TestInfoNamesTheBridge(t *testing.T) {
	e := newTestEnv(t)
	_, info := e.call(t, http.MethodGet, "/v1/info", nil)
	b, _ := info["bridge"].(map[string]any)
	m, _ := info["messenger"].(map[string]any)
	limits, _ := info["limits"].(map[string]any)
	if b["name"] != "Muster example bridge" || b["version"] != "v1.2.3-test" || m["name"] != "Example chat" {
		t.Errorf("bridge %v, messenger %v", b, m)
	}
	if limits["max_body_length"] != 4000.0 || limits["edit_max_age_seconds"] != nil {
		t.Errorf("limits %v, want 4000 and no edit window", limits)
	}
	buttons := info["capabilities"].(map[string]any)["buttons"].(map[string]any)
	if buttons["max_per_row"] != 3.0 || buttons["max_per_message"] != 5.0 {
		t.Errorf("buttons %v", buttons)
	}
	_, info = newTestEnv(t, withEditMaxAge(600)).call(t, http.MethodGet, "/v1/info", nil)
	if got := info["limits"].(map[string]any)["edit_max_age_seconds"]; got != 600.0 {
		t.Errorf("edit_max_age_seconds %v, want 600", got)
	}
}

func TestPostMessageAppears(t *testing.T) {
	e := newTestEnv(t)
	status, body := e.call(t, http.MethodPost, "/v1/messages", message("alerts", "#42 HighErrorRate", ackButtons...))
	if status != http.StatusCreated || body["thread_ref"] != body["message_id"] || body["replayed"] != false {
		t.Fatalf("posting answered %d %v", status, body)
	}
	p := e.root(t, body["message_id"].(string))
	if p.Title != "#42 HighErrorRate" || p.Kind != bridge.MessageKindRoot || p.Status != bridge.StatusFiring ||
		p.HTML != "<p><strong>ns</strong> payments</p>" || p.Link.URL != "https://muster.example.org/alert-groups/AG1" ||
		len(p.Buttons) != 2 || p.Buttons[1] != (pageButton{Label: "Resolve", Style: bridge.ButtonStyleSuccess}) {
		t.Errorf("post %+v", p)
	}
	if p.Color != "#d32f2f" || p.Emoji != "" {
		t.Errorf("with status_color: color %q, emoji %q", p.Color, p.Emoji)
	}

	e = newTestEnv(t, withFeatures("buttons"))
	p = e.root(t, e.post(t, message("alerts", "x")))
	if p.Color != "" || p.Emoji != "🔴" {
		t.Errorf("without status_color: color %q, emoji %q", p.Color, p.Emoji)
	}
}

func TestPostMessageKeepsAnUnsafeLinkOffThePage(t *testing.T) {
	e := newTestEnv(t)
	in := message("alerts", "x")
	in.Link.Url = "javascript:alert(1)"
	if p := e.root(t, e.post(t, in)); p.Link.URL != "" || p.Link.Label != "Open in Muster" {
		t.Errorf("link %+v, want no URL", p.Link)
	}
}

func TestForbiddenChannel(t *testing.T) {
	e := newTestEnv(t)
	status, body := e.call(t, http.MethodPost, "/v1/messages", message("nobot", "x"))
	if status != http.StatusForbidden || class(body) != "forbidden" {
		t.Errorf("posting to nobot answered %d %v", status, body)
	}
	status, body = e.call(t, http.MethodPost, "/v1/channels/nobot/check", nil)
	if status != http.StatusForbidden || class(body) != "forbidden" {
		t.Errorf("checking nobot answered %d %v", status, body)
	}
	status, body = e.call(t, http.MethodPost, "/v1/channels/alerts/check", nil)
	if status != http.StatusOK || body["name"] != "#alerts" || body["channel_id"] != "alerts" {
		t.Errorf("checking alerts answered %d %v", status, body)
	}
}

func TestEditMessage(t *testing.T) {
	e := newTestEnv(t, withEditMaxAge(600))
	id := e.post(t, message("alerts", "#42 firing", ackButtons...))
	edit := message("alerts", "#42 acknowledged")
	content := bridge.MessageContent{Status: bridge.StatusAcknowledged, Color: "#f9a825", Emoji: "🟠",
		Title: edit.Title, Link: edit.Link, Body: "Acknowledged by [@Alice Smith](muster-mention:m1)",
		Buttons: []bridge.Button{}}
	status, body := e.call(t, http.MethodPatch, "/v1/messages/"+id, content)
	if status != http.StatusOK || body["truncated"] != false {
		t.Fatalf("editing answered %d %v", status, body)
	}
	p := e.root(t, id)
	if p.Title != "#42 acknowledged" || p.Status != bridge.StatusAcknowledged || len(p.Buttons) != 0 ||
		p.Edits != 1 || p.HTML != "<p>Acknowledged by @Alice Smith</p>" {
		t.Errorf("edited post %+v", p)
	}
	status, body = e.call(t, http.MethodPatch, "/v1/messages/nope", content)
	if status != http.StatusNotFound || class(body) != "gone" || body["reason"] != nil {
		t.Errorf("editing an unknown message answered %d %v", status, body)
	}
	e.clock.Advance(601 * time.Second)
	status, body = e.call(t, http.MethodPatch, "/v1/messages/"+id, content)
	if status != http.StatusNotFound || class(body) != "gone" || body["reason"] != "edit_window_closed" {
		t.Errorf("editing past the window answered %d %v", status, body)
	}
}

func TestReplyInThread(t *testing.T) {
	e := newTestEnv(t)
	id := e.post(t, message("alerts", "x"))
	status, body := e.call(t, http.MethodPost, "/v1/messages/"+id+"/thread-replies", reply(nil, "first"))
	if status != http.StatusCreated || body["thread_ref"] != id {
		t.Fatalf("the first reply answered %d %v", status, body)
	}
	first := body["message_id"].(string)
	status, body = e.call(t, http.MethodPost, "/v1/messages/"+id+"/thread-replies", reply(new(id), "second"))
	if status != http.StatusCreated || body["thread_ref"] != id {
		t.Errorf("a reply with the thread_ref answered %d %v", status, body)
	}
	status, body = e.call(t, http.MethodPost, "/v1/messages/"+first+"/thread-replies", reply(new(id), "third"))
	if status != http.StatusCreated || body["thread_ref"] != id {
		t.Errorf("a reply under a reply answered %d %v", status, body)
	}
	if p := e.root(t, id); len(p.Thread) != 3 || p.Thread[2].HTML != "<p>third</p>" {
		t.Errorf("Thread %+v, want three replies", p.Thread)
	}
	status, body = e.call(t, http.MethodPost, "/v1/messages/"+id+"/thread-replies", reply(new("other"), "x"))
	if status != http.StatusNotFound || class(body) != "gone" {
		t.Errorf("a reply with a wrong thread_ref answered %d %v", status, body)
	}
	status, body = e.call(t, http.MethodPost, "/v1/messages/nope/thread-replies", reply(nil, "x"))
	if status != http.StatusNotFound || class(body) != "gone" {
		t.Errorf("a reply under an unknown message answered %d %v", status, body)
	}
}

func TestQuietMessages(t *testing.T) {
	for features, want := range map[string]bool{"quiet_messages": true, "buttons": false} {
		e := newTestEnv(t, withFeatures(features))
		in := message("alerts", "x")
		in.Notify = false
		if p := e.root(t, e.post(t, in)); p.Quiet != want {
			t.Errorf("with %s: quiet %v, want %v", features, p.Quiet, want)
		}
		in.Notify = true
		if p := e.root(t, e.post(t, in)); p.Quiet {
			t.Errorf("with %s: a loud message is quiet", features)
		}
	}
}

func TestMentionsFollowNativeMentions(t *testing.T) {
	for features, want := range map[string]string{
		"native_mentions": `<p>Paged <span class="mention" data-person="alice">@Alice Smith</span></p>`,
		"buttons":         "<p>Paged @Alice Smith</p>",
	} {
		e := newTestEnv(t, withFeatures(features))
		in := message("alerts", "x")
		in.Body = "Paged [@Alice Smith](muster-mention:m1)"
		in.Mentions = alice
		if p := e.root(t, e.post(t, in)); p.HTML != want {
			t.Errorf("with %s: %q, want %q", features, p.HTML, want)
		}
	}
}

func TestFindUser(t *testing.T) {
	e := newTestEnv(t)
	status, body := e.call(t, http.MethodGet, "/v1/users?handle=alice", nil)
	items, _ := body["items"].([]any)
	if status != http.StatusOK || len(items) != 1 {
		t.Fatalf("looking up alice answered %d %v", status, body)
	}
	if p := items[0].(map[string]any); p["external_id"] != "alice" || p["display_name"] != "Alice Smith" ||
		p["handle"] != "alice" {
		t.Errorf("alice is %v", p)
	}
	status, body = e.call(t, http.MethodGet, "/v1/users?handle=carol", nil)
	if items, _ := body["items"].([]any); status != http.StatusOK || items == nil || len(items) != 0 {
		t.Errorf("looking up carol answered %d %v", status, body)
	}
	status, _ = newTestEnv(t, withFeatures("thread_messages")).call(t, http.MethodGet, "/v1/users?handle=alice", nil)
	if status != http.StatusNotFound {
		t.Errorf("looking up without direct_messages answered %d, want 404", status)
	}
}

func TestSendDirectMessage(t *testing.T) {
	e := newTestEnv(t)
	status, body := e.call(t, http.MethodPost, "/v1/direct-messages",
		bridge.DirectMessageInput{UserExternalId: "alice", Body: "Your code is **123456**"})
	if status != http.StatusCreated {
		t.Fatalf("sending answered %d %v", status, body)
	}
	entries, _ := e.chat.directChat("alice")
	if len(entries) != 1 || entries[0].ID != body["message_id"] || !entries[0].Bot ||
		entries[0].HTML != "<p>Your code is <strong>123456</strong></p>" {
		t.Errorf("alice's direct chat %+v", entries)
	}
	status, body = e.call(t, http.MethodPost, "/v1/direct-messages",
		bridge.DirectMessageInput{UserExternalId: "carol", Body: "x"})
	if status != http.StatusForbidden || class(body) != "forbidden" {
		t.Errorf("sending to carol answered %d %v", status, body)
	}
}

func TestDeleteMessageIsNotReported(t *testing.T) {
	e := newTestEnv(t)
	id := e.post(t, message("alerts", "x"))
	if status, body := e.call(t, http.MethodDelete, "/v1/messages/"+id, nil); status != http.StatusNoContent {
		t.Fatalf("deleting answered %d %v", status, body)
	}
	if p := e.root(t, id); !p.Deleted {
		t.Error("the message is not deleted")
	}
	if events := e.events(t); len(events) != 0 {
		t.Errorf("a deletion by Muster was reported: %v", events)
	}
	if status, _ := e.call(t, http.MethodDelete, "/v1/messages/"+id, nil); status != http.StatusNoContent {
		t.Errorf("deleting again answered %d, want 204", status)
	}
	status, _ := newTestEnv(t, withFeatures("buttons")).call(t, http.MethodDelete, "/v1/messages/"+id, nil)
	if status != http.StatusNotFound {
		t.Errorf("deleting without delete answered %d, want 404", status)
	}
}

func answer(t *testing.T, e *testEnv, eventID, text string) (int, map[string]any) {
	t.Helper()
	return e.call(t, http.MethodPost, "/v1/answers", bridge.AnswerInput{EventId: eventID, Text: text})
}

func TestAnswerPress(t *testing.T) {
	e := newTestEnv(t)
	id := e.post(t, message("alerts", "x", ackButtons...))
	ev := e.press(t, id, "alice")
	status, body := answer(t, e, ev["id"].(string), "Done: Acknowledged #42")
	if status != http.StatusOK || body["shown_as"] != "ephemeral" {
		t.Fatalf("answering answered %d %v", status, body)
	}
	if p := e.root(t, id); len(p.Notes) != 1 || p.Notes[0].For != "alice" ||
		p.Notes[0].Text != "Done: Acknowledged #42" || len(p.Thread) != 0 {
		t.Errorf("post %+v, want a note for alice", p)
	}

	e = newTestEnv(t, withFeatures("buttons,thread_messages"))
	id = e.post(t, message("alerts", "x", ackButtons...))
	ev = e.press(t, id, "bob")
	status, body = answer(t, e, ev["id"].(string), "Done")
	if status != http.StatusOK || body["shown_as"] != "thread" {
		t.Fatalf("answering without private_answers answered %d %v", status, body)
	}
	if p := e.root(t, id); len(p.Thread) != 1 || !p.Thread[0].Bot || p.Thread[0].Text != "Done" ||
		p.Thread[0].HTML != "<p>Done</p>" || len(p.Notes) != 0 {
		t.Errorf("post %+v, want the answer in the Thread", p)
	}
}

func TestAnswerThreadMessage(t *testing.T) {
	e := newTestEnv(t)
	id := e.post(t, message("alerts", "x"))
	e.pageJSON(t, "/chat/api/thread", map[string]any{"message_id": id, "user": "bob", "text": "ack"},
		http.StatusCreated)
	ev := e.oneEvent(t, "thread_message")
	status, body := answer(t, e, ev["id"].(string), "Acknowledged #42")
	if status != http.StatusOK || body["shown_as"] != "thread" {
		t.Fatalf("answering answered %d %v", status, body)
	}
	p := e.root(t, id)
	if len(p.Thread) != 2 || p.Thread[0].Author != "bob" || !p.Thread[1].Bot || p.Thread[1].Text != "Acknowledged #42" {
		t.Errorf("Thread %+v, want bob's message and the answer", p.Thread)
	}
}

func TestAnswerAccountLinkInDirectChat(t *testing.T) {
	e := newTestEnv(t)
	token := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJK-_0123"
	e.pageJSON(t, "/chat/api/direct-message", map[string]any{"user": "alice", "text": token}, http.StatusCreated)
	ev := e.oneEvent(t, "account_link")
	if ev["token"] != token || ev["user"].(map[string]any)["external_id"] != "alice" {
		t.Fatalf("event %v", ev)
	}
	status, body := answer(t, e, ev["id"].(string), "Linked to Muster user alice@example.org")
	if status != http.StatusOK || body["shown_as"] != "direct" {
		t.Fatalf("answering answered %d %v", status, body)
	}
	entries, _ := e.chat.directChat("alice")
	if len(entries) != 2 || entries[0].Text != token || !entries[1].Bot ||
		entries[1].Text != "Linked to Muster user alice@example.org" {
		t.Errorf("alice's direct chat %+v", entries)
	}
}

func TestAnswerAfterDeletion(t *testing.T) {
	e := newTestEnv(t)
	id := e.post(t, message("alerts", "x", ackButtons...))
	ev := e.press(t, id, "alice")
	e.pageJSON(t, "/chat/api/delete", map[string]any{"message_id": id, "user": "alice"}, http.StatusOK)
	status, body := answer(t, e, ev["id"].(string), "Done")
	if status != http.StatusNotFound || class(body) != "gone" {
		t.Errorf("answering a press of a deleted message answered %d %v", status, body)
	}
	status, body = answer(t, e, "unknown-event", "Done")
	if status != http.StatusNotFound || class(body) != "gone" {
		t.Errorf("answering an unknown event answered %d %v", status, body)
	}
}

func TestPushDeliversPress(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	secret := "whsec_" + base64.StdEncoding.EncodeToString(key)
	pushed := make(chan map[string]any, 8)
	refused := make(chan string, 8)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		id, at := r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp")
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(id + "." + at + "." + string(body)))
		want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if !slices.Contains(strings.Fields(r.Header.Get("webhook-signature")), want) {
			refused <- "a wrong signature"
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var ev map[string]any
		if err := json.Unmarshal(body, &ev); err != nil || ev["id"] != id {
			refused <- "not the event of webhook-id"
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		pushed <- ev
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(receiver.Close)

	e := newTestEnv(t, withPush(receiver.URL+"/callback", secret))
	id := e.post(t, message("alerts", "x", ackButtons...))
	e.pageJSON(t, "/chat/api/press", map[string]any{"message_id": id, "user": "alice"}, http.StatusOK)
	// The pusher posts at once; the timer only guards against a test that would otherwise hang.
	guard := time.NewTimer(30 * time.Second)
	defer guard.Stop()
	select {
	case ev := <-pushed:
		if ev["type"] != "press" || ev["message_id"] != id || ev["data"] != "a1.ack-data" {
			t.Errorf("pushed %v", ev)
		}
	case why := <-refused:
		t.Fatalf("the receiver refused a push: %s", why)
	case <-guard.C:
		t.Fatal("no push arrived")
	}
}

func TestSettings(t *testing.T) {
	dir := t.TempDir()
	file := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tokenFile := file("token", "  file-token\n")
	emptyFile := file("empty", " \n")
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	secretFile := file("secret", secret+"\n")
	tests := []struct {
		name  string
		args  []string
		env   map[string]string
		check func(t *testing.T, s settings)
		fault string
	}{
		{name: "defaults", env: map[string]string{"BRIDGE_TOKEN": " env-token "}, check: func(t *testing.T, s settings) {
			if s.listen != "127.0.0.1:18095" || s.token != "env-token" || s.push != nil || s.maxBodyLength != 4000 ||
				s.editMaxAge != 0 || len(s.features.names()) != len(featureNames) ||
				!slices.Equal(s.channels, []string{"alerts", "ops"}) || len(s.people) != 2 ||
				s.people[1].DisplayName != "Bob Brown" || *s.people[1].Handle != "bob" {
				t.Errorf("settings %+v", s)
			}
		}},
		{name: "flags", args: []string{"-token-file", tokenFile, "-listen", ":8095", "-channels", "a, b",
			"-users", "carol:Carol C", "-edit-max-age", "600", "-max-body-length", "500"},
			check: func(t *testing.T, s settings) {
				if s.token != "file-token" || s.listen != ":8095" || !slices.Equal(s.channels, []string{"a", "b"}) ||
					len(s.people) != 1 || s.people[0].ExternalId != "carol" || s.editMaxAge != 600 ||
					s.maxBodyLength != 500 {
					t.Errorf("settings %+v", s)
				}
			}},
		{name: "no token", fault: "the token is missing: set BRIDGE_TOKEN or -token-file"},
		{name: "an empty token counts as unset", env: map[string]string{"BRIDGE_TOKEN": "  "},
			fault: "the token is missing"},
		{name: "two tokens", args: []string{"-token-file", tokenFile}, env: map[string]string{"BRIDGE_TOKEN": "t"},
			fault: "set BRIDGE_TOKEN or -token-file, not both"},
		{name: "an empty token file", args: []string{"-token-file", emptyFile}, fault: "-token-file: it holds no token"},
		{name: "a missing token file", args: []string{"-token-file", filepath.Join(dir, "nope")},
			fault: "-token-file: open"},
		{name: "push", args: []string{"-token-file", tokenFile, "-push-secret-file", secretFile},
			env: map[string]string{"BRIDGE_PUSH_URL": "http://127.0.0.1:18096/"},
			check: func(t *testing.T, s settings) {
				if s.push == nil || s.push.URL != "http://127.0.0.1:18096/" || s.push.Secret != secret {
					t.Errorf("push %+v", s.push)
				}
			}},
		{name: "push from the environment",
			env: map[string]string{"BRIDGE_TOKEN": "t", "BRIDGE_PUSH_URL": "http://a.example/", "BRIDGE_PUSH_SECRET": secret},
			check: func(t *testing.T, s settings) {
				if s.push == nil || s.push.Secret != secret {
					t.Errorf("push %+v", s.push)
				}
			}},
		{name: "an empty push URL counts as unset", env: map[string]string{"BRIDGE_TOKEN": "t", "BRIDGE_PUSH_URL": ""},
			check: func(t *testing.T, s settings) {
				if s.push != nil {
					t.Errorf("push %+v", s.push)
				}
			}},
		{name: "a push URL without a secret", env: map[string]string{"BRIDGE_TOKEN": "t", "BRIDGE_PUSH_URL": "http://a/"},
			fault: "BRIDGE_PUSH_URL needs BRIDGE_PUSH_SECRET or -push-secret-file"},
		{name: "a secret without a push URL", env: map[string]string{"BRIDGE_TOKEN": "t", "BRIDGE_PUSH_SECRET": secret},
			fault: "BRIDGE_PUSH_SECRET and -push-secret-file need BRIDGE_PUSH_URL"},
		{name: "a secret file without a push URL", args: []string{"-push-secret-file", secretFile},
			env: map[string]string{"BRIDGE_TOKEN": "t"}, fault: "need BRIDGE_PUSH_URL"},
		{name: "two secrets", args: []string{"-push-secret-file", secretFile},
			env:   map[string]string{"BRIDGE_TOKEN": "t", "BRIDGE_PUSH_URL": "http://a/", "BRIDGE_PUSH_SECRET": secret},
			fault: "set BRIDGE_PUSH_SECRET or -push-secret-file, not both"},
		{name: "a channel twice", args: []string{"-channels", "a,b,a"}, env: map[string]string{"BRIDGE_TOKEN": "t"},
			fault: `-channels: the channel "a" appears twice`},
		{name: "no channel", args: []string{"-channels", " , "}, env: map[string]string{"BRIDGE_TOKEN": "t"},
			fault: "-channels: name at least one channel"},
		{name: "a login twice", args: []string{"-users", "a:A,a:B"}, env: map[string]string{"BRIDGE_TOKEN": "t"},
			fault: `-users: the login "a" appears twice`},
		{name: "a user without a name", args: []string{"-users", "a"}, env: map[string]string{"BRIDGE_TOKEN": "t"},
			fault: `-users: "a" is not login:display name`},
		{name: "an unknown feature", env: map[string]string{"BRIDGE_TOKEN": "t", "BRIDGE_FEATURES": "bogus"},
			fault: `BRIDGE_FEATURES: unknown feature "bogus"`},
		{name: "no features", env: map[string]string{"BRIDGE_TOKEN": "t", "BRIDGE_FEATURES": ""},
			check: func(t *testing.T, s settings) {
				if len(s.features.names()) != 0 {
					t.Errorf("features %v", s.features.names())
				}
			}},
		{name: "an argument", args: []string{"extra"}, env: map[string]string{"BRIDGE_TOKEN": "t"},
			fault: `unexpected argument "extra"`},
		{name: "an unknown flag", args: []string{"-bogus"}, fault: "flag provided but not defined: -bogus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(name string) (string, bool) {
				v, ok := tt.env[name]
				return v, ok
			}
			s, help, err := loadSettings(tt.args, lookup, io.Discard)
			switch {
			case help:
				t.Fatal("help reported")
			case tt.fault != "" && (err == nil || !strings.Contains(err.Error(), tt.fault)):
				t.Fatalf("error %v, want %q", err, tt.fault)
			case tt.fault == "" && err != nil:
				t.Fatal(err)
			case tt.check != nil:
				tt.check(t, s)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	for _, arg := range []string{"-help", "-h", "--help"} {
		var stdout, stderr bytes.Buffer
		if status := run(t.Context(), []string{arg}, noEnv, &stdout, &stderr); status != 0 {
			t.Errorf("%s exited %d", arg, status)
		}
		out := stdout.String()
		lines := strings.SplitN(out, "\n", 4)
		if len(lines) < 4 || lines[0] != "Usage: example-bridge [flags]" ||
			!strings.HasPrefix(lines[1], "Muster example bridge ") ||
			lines[2] != "The page and its JSON API under /chat/api/ have no authentication: put them behind some "+
				"before exposing them." {
			t.Errorf("%s printed %q", arg, out)
		}
		for _, want := range []string{"-listen", "-token-file", "-push-secret-file", "-channels", "-users",
			"-edit-max-age", "-max-body-length", "BRIDGE_TOKEN", "BRIDGE_PUSH_URL", "BRIDGE_PUSH_SECRET",
			"BRIDGE_FEATURES", strings.Join(featureNames, ", ")} {
			if !strings.Contains(out, want) {
				t.Errorf("%s does not mention %s", arg, want)
			}
		}
		if stderr.Len() != 0 {
			t.Errorf("%s wrote to stderr: %q", arg, stderr.String())
		}
	}
}

func noEnv(string) (string, bool) { return "", false }

func envOf(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

// stopOnLine is a log output that cancels when a line containing line was written.
type stopOnLine struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	line   string
	cancel context.CancelFunc
}

func (w *stopOnLine) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.line) {
		w.cancel()
	}
	return len(p), nil
}

func (w *stopOnLine) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestNoSecretInOutput(t *testing.T) {
	const token = "very-secret-token-31337"
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("a push key that nobody may see!!"))
	pushURL := "http://127.0.0.1:1/hidden-callback"
	refusals := []struct {
		name  string
		args  []string
		env   map[string]string
		fault string
	}{
		{name: "an edit window too short", args: []string{"-edit-max-age", "5"},
			env: map[string]string{"BRIDGE_TOKEN": token}, fault: "example-bridge: the configuration: "},
		{name: "a malformed push secret",
			env:   map[string]string{"BRIDGE_TOKEN": token, "BRIDGE_PUSH_URL": pushURL, "BRIDGE_PUSH_SECRET": "x" + secret},
			fault: "example-bridge: the configuration: "},
		{name: "a token with an unknown flag", args: []string{"-nope"}, env: map[string]string{"BRIDGE_TOKEN": token},
			fault: "Run example-bridge -help for the flags."},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if status := run(t.Context(), tt.args, envOf(tt.env), &stdout, &stderr); status != 2 {
				t.Errorf("exit %d, want 2", status)
			}
			out := stderr.String()
			if !strings.Contains(out, tt.fault) {
				t.Errorf("stderr %q, want %q", out, tt.fault)
			}
			for _, s := range []string{token, secret, secret[len("whsec_"):]} {
				if strings.Contains(out+stdout.String(), s) {
					t.Errorf("the output holds a secret: %q", out)
				}
			}
		})
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &stopOnLine{line: "example-bridge: listening", cancel: cancel}
	env := envOf(map[string]string{"BRIDGE_TOKEN": token, "BRIDGE_PUSH_URL": pushURL, "BRIDGE_PUSH_SECRET": secret})
	if status := run(ctx, []string{"-listen", "127.0.0.1:0"}, env, io.Discard, stderr); status != 0 {
		t.Fatalf("exit %d: %s", status, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, "example-bridge: listening") || !strings.Contains(out, "push=true") {
		t.Errorf("the log %q lacks the start", out)
	}
	for _, s := range []string{token, secret, secret[len("whsec_"):], "hidden-callback"} {
		if strings.Contains(out, s) {
			t.Errorf("the log holds %q: %s", s, out)
		}
	}
}

// TestStopEndsALongPoll holds a request open the way a long poll of /v1/events does and ends the server's context: the
// request must end at once, and the shutdown that follows must find nothing left to wait for.
func TestStopEndsALongPoll(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiting, ended := make(chan struct{}), make(chan struct{})
	srv := newServer(ctx, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(waiting)
		<-r.Context().Done()
		close(ended)
	}))
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	answered := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+
			"/v1/events?wait=60", nil)
		if err == nil {
			var resp *http.Response
			if resp, err = http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}
		answered <- err
	}()

	<-waiting
	cancel()
	<-ended
	if err := within(ctx, time.Minute, srv.Shutdown); err != nil {
		t.Errorf("the shutdown: %v", err)
	}
	if err := <-answered; err != nil {
		t.Errorf("the poll got no answer: %v", err)
	}
}

func TestStopStepsHaveDeadlinesOfTheirOwn(t *testing.T) {
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	err := within(ended, time.Minute, func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
			t.Errorf("the step's context: deadline %v, error %v", ok, ctx.Err())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
