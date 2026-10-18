// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	bridge "github.com/muster-io/muster-bridge-go"
)

func TestPageAndAssets(t *testing.T) {
	e := newTestEnv(t)
	for path, contentType := range map[string]string{
		"/":        "text/html; charset=utf-8",
		"/app.js":  "text/javascript; charset=utf-8",
		"/app.css": "text/css; charset=utf-8",
	} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, e.srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		h := resp.Header
		if resp.StatusCode != http.StatusOK || h.Get("Content-Type") != contentType {
			t.Errorf("%s answered %d %s", path, resp.StatusCode, h.Get("Content-Type"))
		}
		if h.Get("Content-Security-Policy") != pagePolicy || h.Get("X-Content-Type-Options") != "nosniff" ||
			h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s has the headers %v", path, h)
		}
	}
	if status, _ := e.page(t, http.MethodGet, "/nope", nil); status != http.StatusNotFound {
		t.Errorf("/nope answered %d", status)
	}
	index, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)
	if !strings.HasPrefix(html, "<!DOCTYPE html>\n") || !strings.Contains(html, `src="/app.js"`) ||
		!strings.Contains(html, `href="/app.css"`) {
		t.Errorf("index.html does not load /app.js and /app.css")
	}
	for _, refs := range regexp.MustCompile(`(?:src|href)="([^"]*)"`).FindAllStringSubmatch(html, -1) {
		if refs[1] != "/app.js" && refs[1] != "/app.css" {
			t.Errorf("index.html loads %s", refs[1])
		}
	}
	if strings.Contains(html, "http://") || strings.Contains(html, "https://") ||
		regexp.MustCompile(`<script[^>]*>[^<]`).MatchString(html) || strings.Contains(html, "<style") ||
		regexp.MustCompile(`\son[a-z]+=`).MatchString(html) {
		t.Error("index.html has an external URL, an inline script or style, or an inline handler")
	}
}

func TestMessagesIsAnArray(t *testing.T) {
	e := newTestEnv(t)
	first := e.post(t, message("alerts", "first"))
	second := e.post(t, message("alerts", "second", ackButtons...))
	status, data := e.page(t, http.MethodGet, "/chat/api/messages?channel=alerts", nil)
	var posts []map[string]any
	if err := json.Unmarshal(data, &posts); status != http.StatusOK || err != nil {
		t.Fatalf("messages answered %d %s", status, data)
	}
	if len(posts) != 2 || posts[0]["id"] != first || posts[1]["id"] != second || posts[0]["title"] != "first" {
		t.Fatalf("messages %v, want first then second", posts)
	}
	button := posts[1]["buttons"].([]any)[0].(map[string]any)
	if len(button) != 2 || button["label"] != "Ack" || button["style"] != "primary" {
		t.Errorf("a button is shown as %v", button)
	}
	if strings.Contains(string(data), "ack-data") {
		t.Error("the page is given the buttons' data")
	}
	if thread, ok := posts[0]["thread"].([]any); !ok || len(thread) != 0 {
		t.Errorf("a Root post without replies has the Thread %v", posts[0]["thread"])
	}
	if status, data := e.page(t, http.MethodGet, "/chat/api/messages?channel=ops", nil); status != http.StatusOK ||
		string(data) != "[]" {
		t.Errorf("an empty channel answered %d %s", status, data)
	}
	for path, want := range map[string]int{
		"/chat/api/messages":               http.StatusBadRequest,
		"/chat/api/messages?channel=nobot": http.StatusNotFound,
	} {
		if status, data := e.page(t, http.MethodGet, path, nil); status != want ||
			!strings.Contains(string(data), `"error"`) {
			t.Errorf("%s answered %d %s, want %d", path, status, data, want)
		}
	}
}

func TestPressIsConfirmedAndReported(t *testing.T) {
	e := newTestEnv(t)
	id := e.post(t, message("alerts", "#42", ackButtons...))
	got := e.pageJSON(t, "/chat/api/press", map[string]any{"message_id": id, "user": "alice"}, http.StatusOK)
	if got["label"] != "Ack" || got["button"] != 0.0 || got["confirmation"] != "Pressed Ack" ||
		got["reported"] != "reported" || got["message_id"] != id {
		t.Errorf("press answered %v", got)
	}
	ev := e.oneEvent(t, "press")
	user := ev["user"].(map[string]any)
	if ev["message_id"] != id || ev["data"] != "a1.ack-data" || user["external_id"] != "alice" ||
		user["display_name"] != "Alice Smith" || ev["occurred_at"] != "2026-10-18T12:00:00Z" {
		t.Errorf("event %v", ev)
	}
	got = e.pageJSON(t, "/chat/api/press", map[string]any{"message_id": id, "user": "bob", "button": 1},
		http.StatusOK)
	if got["label"] != "Resolve" || got["confirmation"] != "Pressed Resolve" {
		t.Errorf("press of the second button answered %v", got)
	}
	if ev := e.oneEvent(t, "press"); ev["data"] != "a2.resolve-data" {
		t.Errorf("event %v, want the second button's data", ev)
	}
}

func TestPressOfAThreadReply(t *testing.T) {
	e := newTestEnv(t)
	id := e.post(t, message("alerts", "x"))
	in := reply(nil, "Snooze?")
	in.Buttons = []bridge.Button{{Label: "1h", Style: bridge.ButtonStyleNeutral, Data: "s1"}}
	_, body := e.call(t, http.MethodPost, "/v1/messages/"+id+"/thread-replies", in)
	replyID := body["message_id"].(string)
	ev := e.press(t, replyID, "alice")
	if ev["message_id"] != replyID || ev["data"] != "s1" {
		t.Errorf("event %v", ev)
	}
	status, answered := answer(t, e, ev["id"].(string), "Snoozed")
	if status != http.StatusOK || answered["shown_as"] != "ephemeral" {
		t.Errorf("answering answered %d %v", status, answered)
	}
	if p := e.root(t, id); len(p.Thread) != 1 || len(p.Thread[0].Notes) != 1 {
		t.Errorf("Thread %+v, want the note under the reply", p.Thread)
	}
}

func TestPressRefusals(t *testing.T) {
	e := newTestEnv(t)
	id := e.post(t, message("alerts", "x", ackButtons...))
	plain := e.post(t, message("alerts", "plain"))
	gone := e.post(t, message("alerts", "gone", ackButtons...))
	e.pageJSON(t, "/chat/api/delete", map[string]any{"message_id": gone, "user": "bob"}, http.StatusOK)
	e.events(t)
	tests := []struct {
		name string
		body any
		want int
	}{
		{"an unknown user", map[string]any{"message_id": id, "user": "carol"}, http.StatusBadRequest},
		{"no user", map[string]any{"message_id": id}, http.StatusBadRequest},
		{"no message", map[string]any{"user": "alice"}, http.StatusBadRequest},
		{"an unknown message", map[string]any{"message_id": "nope", "user": "alice"}, http.StatusNotFound},
		{"a deleted message", map[string]any{"message_id": gone, "user": "alice"}, http.StatusNotFound},
		{"a message without buttons", map[string]any{"message_id": plain, "user": "alice"}, http.StatusBadRequest},
		{"a button too far", map[string]any{"message_id": id, "user": "alice", "button": 9}, http.StatusBadRequest},
		{"a negative button", map[string]any{"message_id": id, "user": "alice", "button": -1}, http.StatusBadRequest},
		{"a button that is no number", map[string]any{"message_id": id, "user": "alice", "button": "0"},
			http.StatusBadRequest},
		{"no JSON", "{", http.StatusBadRequest},
		{"a body too large", `{"message_id":"` + strings.Repeat("a", 70000) + `","user":"alice"}`,
			http.StatusBadRequest},
	}
	for _, tt := range tests {
		if status, data := e.page(t, http.MethodPost, "/chat/api/press", tt.body); status != tt.want ||
			!strings.Contains(string(data), `"error"`) {
			t.Errorf("%s: %d %s, want %d", tt.name, status, data, tt.want)
		}
	}
	for _, contentType := range []string{"text/plain", "", "application/x-www-form-urlencoded"} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, e.srv.URL+"/chat/api/press",
			strings.NewReader(`{"message_id":"`+id+`","user":"alice"}`))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if status, _ := do(t, req); status != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q answered %d, want 415", contentType, status)
		}
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, e.srv.URL+"/chat/api/press",
		strings.NewReader(`{"message_id":"`+id+`","user":"alice"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if status, data := do(t, req); status != http.StatusOK {
		t.Errorf("application/json with a charset answered %d %s", status, data)
	}
	if events := e.events(t); len(events) != 1 {
		t.Errorf("events %v, want only the accepted press", events)
	}
}

func TestEachPressIsANewEvent(t *testing.T) {
	e := newTestEnv(t)
	id := e.post(t, message("alerts", "x", ackButtons...))
	first := e.press(t, id, "alice")
	second := e.press(t, id, "alice")
	if first["id"] == second["id"] {
		t.Errorf("two presses share the event id %v", first["id"])
	}
}

func TestThreadMessageIsReported(t *testing.T) {
	e := newTestEnv(t)
	id := e.post(t, message("alerts", "x"))
	got := e.pageJSON(t, "/chat/api/thread", map[string]any{"message_id": id, "user": "bob", "text": "  ack \n"},
		http.StatusCreated)
	if got["message_id"] != id || got["reported"] != "reported" || got["id"] == "" || got["update_id"] == "" {
		t.Errorf("thread answered %v", got)
	}
	ev := e.oneEvent(t, "thread_message")
	if ev["message_id"] != id || ev["thread_ref"] != id || ev["text"] != "ack" ||
		ev["user"].(map[string]any)["external_id"] != "bob" {
		t.Errorf("event %v", ev)
	}
	e.pageJSON(t, "/chat/api/thread", map[string]any{"message_id": got["id"], "user": "alice", "text": "me too"},
		http.StatusCreated)
	if ev := e.oneEvent(t, "thread_message"); ev["message_id"] != id {
		t.Errorf("a message under a Thread post names %v, want the Root message %s", ev["message_id"], id)
	}
	p := e.root(t, id)
	if len(p.Thread) != 2 || p.Thread[0].Author != "bob" || p.Thread[0].Text != "ack" || p.Thread[0].Bot ||
		p.Thread[0].HTML != "<p>ack</p>" {
		t.Errorf("Thread %+v", p.Thread)
	}
	for name, body := range map[string]map[string]any{
		"no text":   {"message_id": id, "user": "bob", "text": " "},
		"long text": {"message_id": id, "user": "bob", "text": strings.Repeat("é", 4001)},
	} {
		if status, _ := e.page(t, http.MethodPost, "/chat/api/thread", body); status != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", name, status)
		}
	}
	if status, _ := e.page(t, http.MethodPost, "/chat/api/thread",
		map[string]any{"message_id": "nope", "user": "bob", "text": "x"}); status != http.StatusNotFound {
		t.Errorf("an unknown message answered %d, want 404", status)
	}

	e = newTestEnv(t, withFeatures("buttons"))
	id = e.post(t, message("alerts", "x"))
	got = e.pageJSON(t, "/chat/api/thread", map[string]any{"message_id": id, "user": "bob", "text": "ack"},
		http.StatusCreated)
	if got["reported"] != "not_offered" {
		t.Errorf("without thread_messages: %v", got)
	}
	if events := e.events(t); len(events) != 0 {
		t.Errorf("without thread_messages, events %v", events)
	}
	if p := e.root(t, id); len(p.Thread) != 1 {
		t.Errorf("without thread_messages, Thread %+v", p.Thread)
	}
}

func TestDirectMessageTokenBecomesAccountLink(t *testing.T) {
	e := newTestEnv(t)
	token := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq"
	for _, text := range []string{token, "/start " + token} {
		got := e.pageJSON(t, "/chat/api/direct-message", map[string]any{"user": "alice", "text": text},
			http.StatusCreated)
		if got["reported"] != "reported" || got["id"] == "" {
			t.Errorf("direct-message answered %v", got)
		}
		ev := e.oneEvent(t, "account_link")
		if ev["token"] != token || ev["user"].(map[string]any)["external_id"] != "alice" {
			t.Errorf("event %v", ev)
		}
	}
	e.pageJSON(t, "/chat/api/direct-message", map[string]any{"user": "alice", "text": "hello"}, http.StatusCreated)
	if events := e.events(t); len(events) != 0 {
		t.Errorf("a plain message became %v", events)
	}
	status, data := e.page(t, http.MethodGet, "/chat/api/direct-messages?user=alice", nil)
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); status != http.StatusOK || err != nil {
		t.Fatalf("direct-messages answered %d %s", status, data)
	}
	if len(entries) != 3 || entries[2]["text"] != "hello" || entries[2]["author"] != "alice" ||
		entries[2]["bot"] != false || entries[2]["html"] != "<p>hello</p>" {
		t.Errorf("alice's direct chat %v", entries)
	}
	for path, want := range map[string]int{
		"/chat/api/direct-messages":            http.StatusBadRequest,
		"/chat/api/direct-messages?user=carol": http.StatusNotFound,
	} {
		if status, _ := e.page(t, http.MethodGet, path, nil); status != want {
			t.Errorf("%s answered %d, want %d", path, status, want)
		}
	}
	if status, _ := e.page(t, http.MethodPost, "/chat/api/direct-message",
		map[string]any{"user": "carol", "text": "x"}); status != http.StatusBadRequest {
		t.Errorf("a message from carol answered %d, want 400", status)
	}
}

func TestDeletionOnThePageIsReported(t *testing.T) {
	e := newTestEnv(t)
	id := e.post(t, message("alerts", "x", ackButtons...))
	got := e.pageJSON(t, "/chat/api/delete", map[string]any{"message_id": id, "user": "alice"}, http.StatusOK)
	if got["reported"] != "reported" || got["message_id"] != id || got["update_id"] == nil {
		t.Errorf("delete answered %v", got)
	}
	if ev := e.oneEvent(t, "message_deleted"); ev["message_id"] != id {
		t.Errorf("event %v", ev)
	}
	if p := e.root(t, id); !p.Deleted || p.Title != "" {
		t.Errorf("post %+v, want a tombstone", p)
	}
	if status, _ := e.page(t, http.MethodPost, "/chat/api/delete",
		map[string]any{"message_id": id, "user": "alice"}); status != http.StatusNotFound {
		t.Errorf("deleting again answered %d, want 404", status)
	}

	root := e.post(t, message("alerts", "y"))
	mine := e.pageJSON(t, "/chat/api/thread", map[string]any{"message_id": root, "user": "bob", "text": "oops"},
		http.StatusCreated)
	e.events(t)
	got = e.pageJSON(t, "/chat/api/delete", map[string]any{"message_id": mine["id"], "user": "bob"}, http.StatusOK)
	if got["reported"] != "none" || got["update_id"] != nil {
		t.Errorf("deleting a person's message answered %v", got)
	}
	if events := e.events(t); len(events) != 0 {
		t.Errorf("deleting a person's message reported %v", events)
	}

	e = newTestEnv(t, withFeatures("buttons"))
	id = e.post(t, message("alerts", "x"))
	got = e.pageJSON(t, "/chat/api/delete", map[string]any{"message_id": id, "user": "alice"}, http.StatusOK)
	if got["reported"] != "not_offered" {
		t.Errorf("without deletion_events: %v", got)
	}
	if events := e.events(t); len(events) != 0 {
		t.Errorf("without deletion_events, events %v", events)
	}
	if p := e.root(t, id); !p.Deleted {
		t.Error("without deletion_events, the message is not deleted")
	}
}

func TestStateAndLog(t *testing.T) {
	e := newTestEnv(t)
	id := e.post(t, message("alerts", "x", ackButtons...))
	if status, _ := e.callAs(t, "wrong", http.MethodPost, "/v1/messages", message("alerts", "y")); status !=
		http.StatusUnauthorized {
		t.Fatalf("a wrong token answered %d", status)
	}
	e.pageJSON(t, "/chat/api/press", map[string]any{"message_id": id, "user": "alice"}, http.StatusOK)
	const dmText = "a private message nobody else may read"
	e.pageJSON(t, "/chat/api/direct-message", map[string]any{"user": "bob", "text": dmText}, http.StatusCreated)
	for range 3 {
		e.events(t)
	}

	status, data := e.page(t, http.MethodGet, "/chat/api/state", nil)
	if status != http.StatusOK {
		t.Fatalf("state answered %d", status)
	}
	if strings.Contains(string(data), testToken) || strings.Contains(string(data), dmText) {
		t.Errorf("the state holds the token or a direct message: %s", data)
	}
	var s stateView
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s.Bridge != (bridgeView{Name: "Muster example bridge", Version: "v1.2.3-test"}) || s.Messenger != "Example chat" ||
		s.UserLookup != "login" || s.Push || s.Limits.MaxBodyLength != 4000 || s.Limits.EditMaxAgeSeconds != nil ||
		len(s.Features) != len(featureNames) || !s.Features["delete"] || s.Version == 0 {
		t.Errorf("state %+v", s)
	}
	if len(s.Channels) != 2 || s.Channels[1] != (channelView{ID: "ops", Name: "#ops"}) || len(s.People) != 2 ||
		s.People[0] != (personView{Login: "alice", Name: "Alice Smith"}) {
		t.Errorf("channels %v, people %v", s.Channels, s.People)
	}
	want := []logEntry{
		{Kind: "call", What: "GET /v1/events", Outcome: "200", Count: 3},
		{Kind: "report", What: "direct message from bob", Outcome: "reported", Count: 1},
		{Kind: "report", What: "press on " + id + " by alice (Ack)", Outcome: "reported", Count: 1},
		{Kind: "call", What: "POST /v1/messages", Outcome: "401", Count: 1},
		{Kind: "call", What: "POST /v1/messages", Outcome: "201", Count: 1},
	}
	if len(s.Log) != len(want) {
		t.Fatalf("log %+v, want %d lines", s.Log, len(want))
	}
	for i, w := range want {
		g := s.Log[i]
		if g.Kind != w.Kind || g.What != w.What || g.Outcome != w.Outcome || g.Count != w.Count {
			t.Errorf("log line %d is %+v, want %+v", i, g, w)
		}
	}

	_, data = newTestEnv(t, withFeatures("buttons"), withEditMaxAge(600)).page(t, http.MethodGet,
		"/chat/api/state", nil)
	s = stateView{}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s.UserLookup != "none" || s.Features["delete"] || !s.Features["buttons"] || len(s.Features) != len(featureNames) ||
		s.Limits.EditMaxAgeSeconds == nil || *s.Limits.EditMaxAgeSeconds != 600 {
		t.Errorf("state %+v", s)
	}
}

func TestPersonTextIsEscaped(t *testing.T) {
	e := newTestEnv(t)
	id := e.post(t, message("alerts", "x"))
	e.pageJSON(t, "/chat/api/thread", map[string]any{"message_id": id, "user": "bob", "text": "<script>x</script>"},
		http.StatusCreated)
	if p := e.root(t, id); p.Thread[0].HTML != "<p>&lt;script&gt;x&lt;/script&gt;</p>" {
		t.Errorf("html %q", p.Thread[0].HTML)
	}
}

func TestRestartNeverReusesIDs(t *testing.T) {
	seen := map[string]int{}
	for start := range 2 {
		e := newTestEnv(t)
		id := e.post(t, message("alerts", "x", ackButtons...))
		pressed := e.pageJSON(t, "/chat/api/press", map[string]any{"message_id": id, "user": "alice"},
			http.StatusOK)
		ev := e.oneEvent(t, "press")
		thread := e.pageJSON(t, "/chat/api/thread", map[string]any{"message_id": id, "user": "bob", "text": "ack"},
			http.StatusCreated)
		threadEvent := e.oneEvent(t, "thread_message")
		for _, v := range []any{id, pressed["update_id"], ev["id"], thread["id"], thread["update_id"],
			threadEvent["id"]} {
			s, _ := v.(string)
			if s == "" {
				t.Fatalf("start %d gave an empty id", start)
			}
			if other, ok := seen[s]; ok {
				t.Errorf("start %d repeats %q of start %d", start, s, other)
			}
			seen[s] = start
		}
	}
}
