// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	linkJSON = `"link":{"url":"https://muster.example.org/alert-groups/1","label":"Open in Muster"}`
	postBody = `{"channel_id":"alerts","kind":"root","status":"firing","color":"#d32f2f","emoji":"x",` +
		`"title":"#42 HighErrorRate",` + linkJSON + `,"body":"b","buttons":[],"mentions":[],"notify":true}`
	editBody = `{"status":"acknowledged","color":"#f57c00","emoji":"x","title":"#42 HighErrorRate",` + linkJSON +
		`,"body":"b","buttons":[]}`
	replyBody = `{"thread_ref":null,"body":"b","buttons":[],"mentions":[],"notify":false}`
	dmBody    = `{"user_external_id":"u-alice","body":"Your code is 123456."}`
)

// withBody is a request body with its "body" replaced.
func withBody(body, text string) string {
	data, _ := json.Marshal(text)
	return strings.Replace(body, `"body":"b"`, `"body":`+string(data), 1)
}

// withButtons is a request body with n buttons whose data is data.
func withButtons(body string, n int, data string) string {
	buttons := make([]string, n)
	for i := range buttons {
		buttons[i] = fmt.Sprintf(`{"label":"B%d","style":"primary","data":%q}`, i, data)
	}
	return strings.Replace(body, `"buttons":[]`, `"buttons":[`+strings.Join(buttons, ",")+`]`, 1)
}

func answerBody(id string) string { return fmt.Sprintf(`{"event_id":%q,"text":"Done"}`, id) }

// pressID is the id of a press of the bot main with the update id.
func pressID(t testing.TB, b *Bridge, update string) string {
	t.Helper()
	id, err := b.eventID("main", EventTypePress, update)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// reportPress reports a press of message m1 by Alice for the bot main.
func reportPress(t testing.TB, b *Bridge, update string) string {
	t.Helper()
	err := b.Press(context.Background(), "main", Press{UpdateID: update, MessageID: "m1", Data: "d1", Chat: "chat-1",
		User: Person{ExternalId: "u-alice", DisplayName: "Alice Smith", Handle: new("alice")}})
	if err != nil {
		t.Fatal(err)
	}
	return pressID(t, b, update)
}

func TestHandlerServesEveryOperation(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	h := b.Handler()
	id := reportPress(t, b, "u1")
	tests := []struct {
		method, path string
		header       http.Header
		body         string
		status       int
		calls        string
	}{
		{http.MethodGet, "/v1/info", nil, "", http.StatusOK, ""},
		{http.MethodPost, "/v1/messages", key("k1"), postBody, http.StatusCreated, "PostMessage"},
		{http.MethodPatch, "/v1/messages/m1", nil, editBody, http.StatusOK, "EditMessage"},
		{http.MethodDelete, "/v1/messages/m1", nil, "", http.StatusNoContent, "DeleteMessage"},
		{http.MethodPost, "/v1/messages/m1/thread-replies", key("k2"), replyBody, http.StatusCreated, "ReplyInThread"},
		{http.MethodPost, "/v1/channels/alerts/check", nil, "", http.StatusOK, "CheckChannel"},
		{http.MethodGet, "/v1/users?handle=alice", nil, "", http.StatusOK, "FindUser"},
		{http.MethodPost, "/v1/direct-messages", key("k3"), dmBody, http.StatusCreated, "SendDirectMessage"},
		{http.MethodPost, "/v1/answers", key("k4"), answerBody(id), http.StatusOK, "AnswerEvent"},
		{http.MethodGet, "/v1/events?wait=0", nil, "", http.StatusOK, ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			before := m.count(tt.calls)
			rec := do(t, h, tt.method, tt.path, tokenMain, tt.header, tt.body)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body)
			}
			if tt.calls != "" && m.count(tt.calls) != before+1 {
				t.Fatalf("%s was not called once", tt.calls)
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("the answer may be cached")
			}
		})
	}
}

func TestHandlerAnswers(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	h := b.Handler()
	tests := []struct {
		method, path string
		header       http.Header
		body, want   string
	}{
		{http.MethodPost, "/v1/messages", key("k1"), postBody,
			`{"message_id":"m1","replayed":false,"truncated":false}`},
		{http.MethodPatch, "/v1/messages/m1", nil, editBody, `{"truncated":false}`},
		{http.MethodPost, "/v1/messages/m1/thread-replies", key("k2"), replyBody,
			`{"message_id":"r1","replayed":false,"thread_ref":"t-m1","truncated":false}`},
		{http.MethodPost, "/v1/channels/alerts/check", nil, "", `{"channel_id":"alerts","name":"Alerts"}`},
		{http.MethodGet, "/v1/users?handle=alice", nil, "",
			`{"items":[{"display_name":"Alice Smith","external_id":"u-alice","handle":"alice"}]}`},
		{http.MethodGet, "/v1/users?handle=nobody", nil, "", `{"items":[]}`},
		{http.MethodPost, "/v1/direct-messages", key("k3"), dmBody, `{"message_id":"d1","replayed":false}`},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := do(t, h, tt.method, tt.path, tokenMain, tt.header, tt.body)
			if got := rec.Body.String(); got != tt.want {
				t.Fatalf("answer = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestHandlerUnderPrefix(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	h := http.StripPrefix("/bridge", b.Handler())
	if rec := do(t, h, http.MethodGet, "/bridge/v1/info", tokenMain, nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("info under a prefix answered %d", rec.Code)
	}
	if rec := do(t, h, http.MethodPatch, "/bridge/v1/messages/a%2Fb", tokenMain, nil, editBody); rec.Code != http.StatusOK {
		t.Fatalf("edit under a prefix answered %d: %s", rec.Code, rec.Body)
	}
	if calls := m.recorded(); calls[0].id != "a/b" {
		t.Fatalf("the messenger got the message id %q", calls[0].id)
	}
}

func TestUnauthorized(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	tests := []struct {
		name, path, authorization string
	}{
		{"no header", "/v1/info", ""},
		{"wrong token", "/v1/info", "Bearer wrong"},
		{"Basic scheme", "/v1/info", "Basic " + tokenMain},
		{"no token", "/v1/info", "Bearer"},
		{"empty token", "/v1/info", "Bearer "},
		{"token without scheme", "/v1/info", tokenMain},
		{"unknown path", "/v1/nothing", "Bearer wrong"},
		{"post", "/v1/messages", "Bearer wrong"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tt.path, strings.NewReader(postBody))
			if tt.path == "/v1/info" {
				r.Method = http.MethodGet
			}
			r.Header.Set("Idempotency-Key", "k1")
			r.Header.Set("Content-Type", "application/json")
			if tt.authorization != "" {
				r.Header.Set("Authorization", tt.authorization)
			}
			rec := httptest.NewRecorder()
			b.Handler().ServeHTTP(rec, r)
			p := wantProblem(t, rec, http.StatusUnauthorized, ClassForbidden)
			if rec.Header().Get("WWW-Authenticate") != "Bearer" || detailOf(p) != "The token is missing or wrong." {
				t.Fatalf("401 without WWW-Authenticate or detail: %s", rec.Body)
			}
		})
	}
	if calls := m.recorded(); len(calls) != 0 {
		t.Fatalf("the messenger was called: %v", calls)
	}
	if rec := do(t, b.Handler(), http.MethodGet, "/v1/info", "", http.Header{"Authorization": {"bearer  " +
		tokenMain}}, ""); rec.Code != http.StatusOK {
		t.Fatalf("a lower-case scheme answered %d", rec.Code)
	}
}

func TestTokenSelectsBot(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	for _, tt := range []struct{ token, bot string }{{tokenMain, "main"}, {tokenOther, "other"}} {
		if rec := do(t, b.Handler(), http.MethodPost, "/v1/channels/alerts/check", tt.token, nil, ""); rec.Code != http.StatusOK {
			t.Fatalf("check answered %d", rec.Code)
		}
		calls := m.recorded()
		if got := calls[len(calls)-1].bot; got != tt.bot {
			t.Fatalf("the token of %s called the messenger with the bot %s", tt.bot, got)
		}
	}
}

func TestNotServedOperations(t *testing.T) {
	b, m, _, _ := newTestBridge(t, func(c *Config) {
		c.UserLookup = ""
		c.Capabilities.DirectMessages = nil
		c.Capabilities.Delete = nil
		c.EventDelivery = []InfoEventDelivery{InfoEventDeliveryPush}
	})
	for _, tt := range []struct {
		method, path string
		header       http.Header
		body         string
	}{
		{http.MethodGet, "/v1/users?handle=alice", nil, ""},
		{http.MethodPost, "/v1/direct-messages", key("k1"), dmBody},
		{http.MethodDelete, "/v1/messages/m1", nil, ""},
		{http.MethodGet, "/v1/events", nil, ""},
	} {
		rec := do(t, b.Handler(), tt.method, tt.path, tokenMain, tt.header, tt.body)
		p := wantProblem(t, rec, http.StatusNotFound, ClassInvalid)
		if detailOf(p) != "This bridge does not serve this operation." {
			t.Fatalf("%s %s: detail %q", tt.method, tt.path, detailOf(p))
		}
	}
	if len(m.recorded()) != 0 {
		t.Fatal("the messenger was called")
	}
}

func TestUnknownPathAndMethod(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, "/v1/nothing"},
		{http.MethodPut, "/v1/info"},
		{http.MethodPost, "/v1/info"},
		{http.MethodGet, "/v2/info"},
		{http.MethodGet, "/"},
	} {
		rec := do(t, b.Handler(), tt.method, tt.path, tokenMain, nil, "")
		wantProblem(t, rec, http.StatusNotFound, ClassInvalid)
	}
}

func TestRefusesSpecMismatch(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	long := strings.Repeat("S", 513)
	ct := func(v string) http.Header { return http.Header{"Idempotency-Key": {"k1"}, "Content-Type": {v}} }
	tests := []struct {
		name, method, path string
		header             http.Header
		body, want         string
	}{
		{"missing member", http.MethodPost, "/v1/messages", key("k1"), strings.Replace(postBody, `"kind":"root",`, "", 1),
			`body /kind: property "kind" is missing`},
		{"wrong type", http.MethodPost, "/v1/messages", key("k1"), strings.Replace(postBody, `"notify":true`,
			`"notify":"SECRETX"`, 1), "body /notify: value must be a boolean"},
		{"bad colour", http.MethodPost, "/v1/messages", key("k1"), strings.Replace(postBody, `"#d32f2f"`,
			`"SECRETX"`, 1), `body /color: string doesn't match the regular expression "^#[0-9a-f]{6}$"`},
		{"bad button data", http.MethodPost, "/v1/messages", key("k1"), withButtons(postBody, 1, "SECRET X"),
			"body /buttons/0/data: string doesn't match the regular expression"},
		{"text/plain", http.MethodPost, "/v1/messages", ct("text/plain"), postBody,
			`body: header Content-Type has unexpected value "text/plain"`},
		{"not JSON", http.MethodPost, "/v1/messages", key("k1"), `{"SECRETX`, "body: failed to decode request body"},
		{"no body", http.MethodPost, "/v1/messages", ct("application/json"), "",
			"body: value is required but missing"},
		{"missing thread_ref", http.MethodPost, "/v1/messages/m1/thread-replies", key("k1"),
			strings.Replace(replyBody, `"thread_ref":null,`, "", 1), `body /thread_ref: property "thread_ref" is missing`},
		{"wait 61", http.MethodGet, "/v1/events?wait=61", nil, "",
			"parameter wait in query: number must be at most 60"},
		{"wait not a number", http.MethodGet, "/v1/events?wait=SECRETX", nil, "", "parameter wait in query:"},
		{"long cursor", http.MethodGet, "/v1/events?cursor=" + strings.Repeat("c", 1025), nil, "",
			"parameter cursor in query: maximum string length is 1024"},
		{"empty handle", http.MethodGet, "/v1/users?handle=", nil, "",
			"parameter handle in query: minimum string length is 1"},
		{"no handle", http.MethodGet, "/v1/users", nil, "", "parameter handle in query: value is required but missing"},
		{"long channel id", http.MethodPost, "/v1/channels/" + long + "/check", nil, "",
			"parameter channel_id in path: maximum string length is 512"},
		{"long message id", http.MethodPatch, "/v1/messages/" + long, nil, editBody,
			"parameter message_id in path: maximum string length is 512"},
		{"bad event id", http.MethodPost, "/v1/answers", key("k1"), `{"event_id":"SECRET X","text":"t"}`,
			`body /event_id: string doesn't match the regular expression "^[!-~]{1,200}$"`},
		{"long direct message", http.MethodPost, "/v1/direct-messages", key("k1"),
			`{"user_external_id":"u","body":"` + strings.Repeat("S", 2001) + `"}`,
			"body /body: maximum string length is 2000"},
		{"no key", http.MethodPost, "/v1/messages", nil, postBody,
			"Send an Idempotency-Key header of 1 to 255 visible ASCII characters (no spaces)."},
		{"key with a space", http.MethodPost, "/v1/messages", key("SECRET X"), postBody,
			"Send an Idempotency-Key header of 1 to 255 visible ASCII characters (no spaces)."},
		{"long key", http.MethodPost, "/v1/messages", key(strings.Repeat("k", 256)), postBody,
			"Send an Idempotency-Key header"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.header.Get("Idempotency-Key") == "k1" {
				tt.header.Set("Idempotency-Key", "k-"+strings.ReplaceAll(tt.name, " ", "-"))
			}
			rec := do(t, b.Handler(), tt.method, tt.path, tokenMain, tt.header, tt.body)
			p := wantProblem(t, rec, http.StatusBadRequest, ClassInvalid)
			d := detailOf(p)
			t.Logf("detail: %s", d)
			if !strings.Contains(d, tt.want) {
				t.Fatalf("detail %q does not say %q", d, tt.want)
			}
			if strings.Contains(d, "SECRET") || strings.Contains(d, "SSSS") {
				t.Fatalf("detail %q carries the value", d)
			}
		})
	}
	if calls := m.recorded(); len(calls) != 0 {
		t.Fatalf("the messenger was called: %v", calls)
	}
}

func TestRefusesBodyOverOneMiB(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	padded := func(n int) string { return postBody + strings.Repeat(" ", n-len(postBody)) }
	if rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), padded(1<<20)); rec.Code != http.StatusCreated {
		t.Fatalf("a body of 1 MiB answered %d: %s", rec.Code, rec.Body)
	}
	rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k2"), padded(1<<20+1))
	if p := wantProblem(t, rec, http.StatusBadRequest, ClassInvalid); detailOf(p) != "The request body exceeds 1 MiB, the most this bridge reads." {
		t.Fatalf("detail %q", detailOf(p))
	}
	if m.count("PostMessage") != 1 {
		t.Fatal("the messenger was called for the body over 1 MiB")
	}
}

func TestRefusesLongBody(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	tests := []struct {
		method, path string
		header       http.Header
		body         string
		status       int
	}{
		{http.MethodPost, "/v1/messages", key("k1"), postBody, http.StatusCreated},
		{http.MethodPatch, "/v1/messages/m1", nil, editBody, http.StatusOK},
		{http.MethodPost, "/v1/messages/m1/thread-replies", key("k2"), replyBody, http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			calls := len(m.recorded())
			rec := do(t, b.Handler(), tt.method, tt.path, tokenMain, tt.header,
				withBody(tt.body, strings.Repeat("é", 500)))
			if rec.Code != tt.status {
				t.Fatalf("a body of 500 code points answered %d: %s", rec.Code, rec.Body)
			}
			if tt.header != nil {
				tt.header = key(tt.header.Get("Idempotency-Key") + "-long")
			}
			rec = do(t, b.Handler(), tt.method, tt.path, tokenMain, tt.header, withBody(tt.body, strings.Repeat("é", 501)))
			p := wantProblem(t, rec, http.StatusBadRequest, ClassInvalid)
			want := "The body is 501 code points long; this bridge takes at most 500 (limits.max_body_length)."
			if detailOf(p) != want {
				t.Fatalf("detail %q, want %q", detailOf(p), want)
			}
			if len(m.recorded()) != calls+1 {
				t.Fatal("the messenger was called for the long body")
			}
		})
	}
}

func TestRefusesButtonsWithoutFeature(t *testing.T) {
	b, m, _, _ := newTestBridge(t, func(c *Config) { c.Capabilities.Buttons = nil })
	rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), withButtons(postBody, 1, "d"))
	if p := wantProblem(t, rec, http.StatusBadRequest, ClassInvalid); detailOf(p) != "This bridge does not offer "+
		"buttons (capabilities.buttons)." {
		t.Fatalf("detail %q", detailOf(p))
	}
	if len(m.recorded()) != 0 {
		t.Fatal("the messenger was called")
	}
}

func TestRefusesTooManyButtons(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	if rec := do(t, b.Handler(), http.MethodPatch, "/v1/messages/m1", tokenMain, nil, withButtons(editBody, 3, "d")); rec.Code != http.StatusOK {
		t.Fatalf("3 buttons answered %d", rec.Code)
	}
	rec := do(t, b.Handler(), http.MethodPatch, "/v1/messages/m1", tokenMain, nil, withButtons(editBody, 4, "d"))
	if p := wantProblem(t, rec, http.StatusBadRequest, ClassInvalid); detailOf(p) != "The message has 4 buttons; "+
		"this bridge takes at most 3 (capabilities.buttons.max_per_message)." {
		t.Fatalf("detail %q", detailOf(p))
	}
	if m.count("EditMessage") != 1 {
		t.Fatal("the messenger was called for 4 buttons")
	}
}

func TestCheckButtonsDataMaxBytes(t *testing.T) {
	caps := &Capabilities_Buttons{MaxPerMessage: 5, MaxPerRow: 5, DataMaxBytes: 64}
	ok := Button{Label: "a", Style: ButtonStylePrimary, Data: strings.Repeat("d", 64)}
	if e := checkButtons([]Button{ok}, caps); e != nil {
		t.Fatalf("64 bytes of data: %v", e)
	}
	long := Button{Label: "b", Style: ButtonStylePrimary, Data: strings.Repeat("d", 65)}
	e := checkButtons([]Button{ok, long}, caps)
	want := "Button 2 carries 65 bytes of data; this bridge takes at most 64 (capabilities.buttons.data_max_bytes)."
	if e == nil || e.Class != ClassInvalid || e.Detail != want {
		t.Fatalf("65 bytes of data: %v", e)
	}
}

func TestPathValuesAreDecoded(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	for _, tt := range []struct{ path, id string }{
		{"/v1/messages/a%2Fb", "a/b"},
		{"/v1/messages/%2E%2E", ".."},
		{"/v1/messages/x%20y", "x y"},
	} {
		if rec := do(t, b.Handler(), http.MethodPatch, tt.path, tokenMain, nil, editBody); rec.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", tt.path, rec.Code, rec.Body)
		}
		calls := m.recorded()
		if got := calls[len(calls)-1].id; got != tt.id {
			t.Fatalf("%s: the messenger got %q, want %q", tt.path, got, tt.id)
		}
	}
}

func TestInvalidMessengerAnswer(t *testing.T) {
	b, m, _, logs := newTestBridge(t, nil)
	m.post = func(string, MessageInput) (MessagePosted, error) { return MessagePosted{MessageId: ""}, nil }
	rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody)
	if p := wantProblem(t, rec, http.StatusInternalServerError, ClassTransient); detailOf(p) != detailMessenger {
		t.Fatalf("detail %q", detailOf(p))
	}
	log := logs.String()
	if !strings.Contains(log, "level=ERROR") || !strings.Contains(log,
		`msg="bridge: the messenger's answer does not match api/bridge.yaml"`) ||
		!strings.Contains(log, "problem=\"/message_id: minimum string length is 1\"") {
		t.Fatalf("log = %s", log)
	}
	m.post = nil
	if rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody); rec.Code != http.StatusCreated ||
		m.count("PostMessage") != 2 {
		t.Fatalf("the retry answered %d after %d calls", rec.Code, m.count("PostMessage"))
	}
}

func TestInvalidAnswersOfEachOperation(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	m.reply = func(string, string, ThreadReplyInput) (ThreadReplyPosted, error) {
		return ThreadReplyPosted{MessageId: "r1", ThreadRef: strings.Repeat("t", 513)}, nil
	}
	m.find = func(string, string) (*Person, error) { return &Person{ExternalId: "u"}, nil }
	m.answer = func(string, ReportedEvent, string) (AnswerShownShownAs, error) { return "banner", nil }
	id := reportPress(t, b, "u1")
	for _, tt := range []struct {
		method, path string
		header       http.Header
		body         string
	}{
		{http.MethodPost, "/v1/messages/m1/thread-replies", key("k1"), replyBody},
		{http.MethodGet, "/v1/users?handle=x", nil, ""},
		{http.MethodPost, "/v1/answers", key("k2"), answerBody(id)},
	} {
		rec := do(t, b.Handler(), tt.method, tt.path, tokenMain, tt.header, tt.body)
		wantProblem(t, rec, http.StatusInternalServerError, ClassTransient)
	}
}

func TestChannelNameIsCut(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	m.check = func(string, string) (string, error) { return strings.Repeat("é", 250), nil }
	var got ChannelCheck
	rec := do(t, b.Handler(), http.MethodPost, "/v1/channels/alerts/check", tokenMain, nil, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Name != strings.Repeat("é", 200) {
		t.Fatalf("name %q (%v)", got.Name, err)
	}
	m.check = func(string, string) (string, error) { return "", nil }
	rec = do(t, b.Handler(), http.MethodPost, "/v1/channels/ops/check", tokenMain, nil, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Name != "ops" || got.ChannelId != "ops" {
		t.Fatalf("answer %s (%v)", rec.Body, err)
	}
}

func TestIdempotencyKeyFromContext(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("key-1"), postBody)
	do(t, b.Handler(), http.MethodPatch, "/v1/messages/m1", tokenMain, nil, editBody)
	calls := m.recorded()
	if calls[0].key != "key-1" || calls[1].key != "" {
		t.Fatalf("keys %q and %q", calls[0].key, calls[1].key)
	}
	if IdempotencyKeyFrom(context.Background()) != "" {
		t.Fatal("a context without a key has one")
	}
}

// idempotentRequests are the operations with an Idempotency-Key, each with a valid body.
func idempotentRequests(t testing.TB, b *Bridge) []struct{ name, path, body, method string } {
	t.Helper()
	id := reportPress(t, b, "u-idempotent")
	return []struct{ name, path, body, method string }{
		{"PostMessage", "/v1/messages", postBody, http.MethodPost},
		{"ReplyInThread", "/v1/messages/m1/thread-replies", replyBody, http.MethodPost},
		{"SendDirectMessage", "/v1/direct-messages", dmBody, http.MethodPost},
		{"AnswerEvent", "/v1/answers", answerBody(id), http.MethodPost},
	}
}

func TestIdempotentReplay(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	for _, tt := range idempotentRequests(t, b) {
		t.Run(tt.name, func(t *testing.T) {
			first := do(t, b.Handler(), tt.method, tt.path, tokenMain, key("same-"+tt.name), tt.body)
			if first.Code >= 300 {
				t.Fatalf("first answered %d: %s", first.Code, first.Body)
			}
			for _, body := range []string{tt.body, `{"other":"body"}`, "not even JSON"} {
				again := do(t, b.Handler(), tt.method, tt.path, tokenMain, key("same-"+tt.name), body)
				if again.Code != first.Code {
					t.Fatalf("the repeat answered %d, the first %d", again.Code, first.Code)
				}
				want := decodeJSON(t, first.Body.Bytes())
				if want["replayed"] != false {
					t.Fatalf("the first answer is replayed: %s", first.Body)
				}
				want["replayed"] = true
				got := decodeJSON(t, again.Body.Bytes())
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("the repeat answered %v, want %v", got, want)
				}
			}
			if n := m.count(tt.name); n != 1 {
				t.Fatalf("the messenger's %s was called %d times", tt.name, n)
			}
		})
	}
}

func TestIdempotencyPerToken(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody)
	rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenOther, key("k1"), postBody)
	if rec.Code != http.StatusCreated || decodeJSON(t, rec.Body.Bytes())["replayed"] != false {
		t.Fatalf("the key under another token answered %d %s", rec.Code, rec.Body)
	}
	calls := m.recorded()
	if len(calls) != 2 || calls[0].bot != "main" || calls[1].bot != "other" {
		t.Fatalf("calls %v", calls)
	}
}

func TestIdempotencyInFlight(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	m.entered, m.block = make(chan string, 1), make(chan struct{})
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody) }()
	<-m.entered
	rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody)
	if p := wantProblem(t, rec, http.StatusInternalServerError, ClassTransient); detailOf(p) != "A request with "+
		"this Idempotency-Key is still running." {
		t.Fatalf("detail %q", detailOf(p))
	}
	close(m.block)
	if first := <-done; first.Code != http.StatusCreated {
		t.Fatalf("the first answered %d", first.Code)
	}
	if rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody); decodeJSON(t,
		rec.Body.Bytes())["replayed"] != true {
		t.Fatalf("after the first ended: %s", rec.Body)
	}
}

func TestIdempotencyFailureStoresNothing(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	m.post = func(string, MessageInput) (MessagePosted, error) { return MessagePosted{}, TransientError("down") }
	if rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody); rec.Code != http.StatusInternalServerError {
		t.Fatalf("the failure answered %d", rec.Code)
	}
	m.post = nil
	if rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody); rec.Code != http.StatusCreated ||
		decodeJSON(t, rec.Body.Bytes())["replayed"] != false {
		t.Fatalf("the retry answered %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k2"), "{}"); rec.Code != http.StatusBadRequest {
		t.Fatalf("an invalid request answered %d", rec.Code)
	}
	if rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k2"), postBody); rec.Code != http.StatusCreated ||
		decodeJSON(t, rec.Body.Bytes())["replayed"] != false {
		t.Fatalf("the retry of an invalid request answered %d %s", rec.Code, rec.Body)
	}
	if m.count("PostMessage") != 3 {
		t.Fatalf("the messenger was called %d times", m.count("PostMessage"))
	}
}

func TestIdempotencyKeysLive24Hours(t *testing.T) {
	b, m, clk, _ := newTestBridge(t, nil)
	do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody)
	clk.Advance(24*time.Hour - time.Nanosecond)
	if rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody); decodeJSON(t,
		rec.Body.Bytes())["replayed"] != true {
		t.Fatalf("the key was gone before 24 hours: %s", rec.Body)
	}
	clk.Advance(time.Nanosecond)
	if rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody); decodeJSON(t,
		rec.Body.Bytes())["replayed"] != false {
		t.Fatalf("the key was kept after 24 hours: %s", rec.Body)
	}
	if m.count("PostMessage") != 2 {
		t.Fatalf("the messenger was called %d times", m.count("PostMessage"))
	}
}

// A request whose messenger ignores ctx can outlive its claim of the key. A retry then claims the key anew, and when
// the first request fails at last, its release must not free the retry's claim: a third request still finds the key
// running, and the retry's answer is the one stored.
func TestStaleRequestKeepsTheRetrysClaim(t *testing.T) {
	b, m, clk, _ := newTestBridge(t, nil)
	// The first two calls wait for their gate; a third, which only a freed claim lets through, answers at once.
	gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
	entered := make(chan int, 3)
	var calls atomic.Int32
	m.post = func(string, MessageInput) (MessagePosted, error) {
		n := int(calls.Add(1)) - 1
		entered <- n
		if n < len(gates) {
			<-gates[n]
		}
		if n == 0 {
			return MessagePosted{}, errors.New("the messenger never answered")
		}
		return MessagePosted{MessageId: fmt.Sprint("m-call-", n)}, nil
	}
	answers := make(chan *httptest.ResponseRecorder, 2)
	send := func() { answers <- do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody) }

	go send()
	<-entered
	clk.Advance(keyClaimTTL)
	go send()
	if n := <-entered; n != 1 {
		t.Fatalf("call %d entered, want the retry's", n)
	}
	close(gates[0])
	if first := <-answers; first.Code != http.StatusInternalServerError {
		t.Fatalf("the stale request answered %d", first.Code)
	}
	rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody)
	if p := wantProblem(t, rec, http.StatusInternalServerError, ClassTransient); detailOf(p) != "A request with "+
		"this Idempotency-Key is still running." {
		t.Fatalf("detail %q: the stale request freed the retry's claim", detailOf(p))
	}
	close(gates[1])
	if retry := <-answers; retry.Code != http.StatusCreated {
		t.Fatalf("the retry answered %d", retry.Code)
	}
	again := decodeJSON(t, do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody).Body.Bytes())
	if again["replayed"] != true || again["message_id"] != "m-call-1" {
		t.Fatalf("the key answers %v, want the retry's answer replayed", again)
	}
	if n := m.count("PostMessage"); n != 2 {
		t.Fatalf("the messenger was called %d times", n)
	}
}

func TestPanicReleasesKey(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	m.post = func(string, MessageInput) (MessagePosted, error) { panic("boom") }
	rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody)
	wantProblem(t, rec, http.StatusInternalServerError, ClassTransient)
	m.post = nil
	if rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody); rec.Code != http.StatusCreated {
		t.Fatalf("the retry after a panic answered %d: %s", rec.Code, rec.Body)
	}
}

// failingStore is a memory store whose methods fail while their error is set; it is safe while pushers use it.
type failingStore struct {
	Store

	mu     sync.Mutex
	errs   map[string]error
	state  *KeyClaim
	before func(method string)
}

func newFailingStore() *failingStore {
	return &failingStore{Store: NewMemoryStore(), errs: map[string]error{}}
}

// fail makes the method fail with err, or work again with nil.
func (s *failingStore) fail(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs[method] = err
}

func (s *failingStore) claimAs(c *KeyClaim) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = c
}

func (s *failingStore) err(method string) error {
	s.mu.Lock()
	before := s.before
	err := s.errs[method]
	s.mu.Unlock()
	if before != nil {
		before(method)
	}
	return err
}

func (s *failingStore) ClaimKey(ctx context.Context, bot, key string, now, until time.Time) (KeyClaim, error) {
	if err := s.err("ClaimKey"); err != nil {
		return KeyClaim{}, err
	}
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	if state != nil {
		return *state, nil
	}
	return s.Store.ClaimKey(ctx, bot, key, now, until)
}

func (s *failingStore) SaveKey(ctx context.Context, bot, key, claimID string, a StoredAnswer, until time.Time) error {
	if err := s.err("SaveKey"); err != nil {
		return err
	}
	return s.Store.SaveKey(ctx, bot, key, claimID, a, until)
}

func (s *failingStore) ReleaseKey(ctx context.Context, bot, key, claimID string) error {
	if err := s.err("ReleaseKey"); err != nil {
		return err
	}
	return s.Store.ReleaseKey(ctx, bot, key, claimID)
}

func (s *failingStore) AppendEvent(ctx context.Context, bot string, ev StoredEvent, now time.Time) (bool, error) {
	if err := s.err("AppendEvent"); err != nil {
		return false, err
	}
	return s.Store.AppendEvent(ctx, bot, ev, now)
}

func (s *failingStore) EventsAfter(ctx context.Context, bot, cursor string, limit int, now time.Time) ([]StoredEvent,
	string, error,
) {
	if err := s.err("EventsAfter"); err != nil {
		return nil, "", err
	}
	return s.Store.EventsAfter(ctx, bot, cursor, limit, now)
}

func (s *failingStore) FindEvent(ctx context.Context, bot, id string, now time.Time) (StoredEvent, bool, error) {
	if err := s.err("FindEvent"); err != nil {
		return StoredEvent{}, false, err
	}
	return s.Store.FindEvent(ctx, bot, id, now)
}

func TestStoreFailures(t *testing.T) {
	store := newFailingStore()
	b, m, _, logs := newTestBridge(t, func(c *Config) { c.Store = store })
	h := b.Handler()

	store.fail("ClaimKey", errors.New("claim failed for "+tokenMain))
	rec := do(t, h, http.MethodPost, "/v1/messages", tokenMain, key("k1"), postBody)
	if p := wantProblem(t, rec, http.StatusInternalServerError, ClassTransient); detailOf(p) != detailStore {
		t.Fatalf("detail %q", detailOf(p))
	}
	store.fail("ClaimKey", nil)

	unknown := KeyState(7)
	store.claimAs(&KeyClaim{State: unknown})
	wantProblem(t, do(t, h, http.MethodPost, "/v1/messages", tokenMain, key("k2"), postBody),
		http.StatusInternalServerError, ClassTransient)
	answered := KeyAnswered
	store.claimAs(&KeyClaim{State: answered, Answer: StoredAnswer{Status: http.StatusCreated, Body: []byte("[1]")}})
	wantProblem(t, do(t, h, http.MethodPost, "/v1/messages", tokenMain, key("k3"), postBody),
		http.StatusInternalServerError, ClassTransient)
	store.claimAs(&KeyClaim{State: answered, Answer: StoredAnswer{Status: http.StatusOK,
		Body: []byte(`{"message_id":"m1","replayed":false}`)}})
	wantProblem(t, do(t, h, http.MethodPost, "/v1/messages", tokenMain, key("k3"), postBody),
		http.StatusInternalServerError, ClassTransient)
	store.claimAs(nil)

	store.fail("SaveKey", errors.New("save failed"))
	if rec := do(t, h, http.MethodPost, "/v1/messages", tokenMain, key("k4"), postBody); rec.Code != http.StatusCreated {
		t.Fatalf("a failed SaveKey answered %d", rec.Code)
	}
	store.fail("SaveKey", nil)
	if rec := do(t, h, http.MethodPost, "/v1/messages", tokenMain, key("k4"), postBody); decodeJSON(t,
		rec.Body.Bytes())["replayed"] != false || m.count("PostMessage") != 2 {
		t.Fatalf("after a failed SaveKey the retry answered %s", rec.Body)
	}

	store.fail("ReleaseKey", errors.New("release failed"))
	do(t, h, http.MethodPost, "/v1/messages", tokenMain, key("k5"), "{}")
	store.fail("ReleaseKey", nil)

	store.fail("FindEvent", errors.New("find failed"))
	wantProblem(t, do(t, h, http.MethodPost, "/v1/answers", tokenMain, key("k6"), answerBody("e1")),
		http.StatusInternalServerError, ClassTransient)
	store.fail("FindEvent", nil)

	store.fail("EventsAfter", errors.New("events failed"))
	wantProblem(t, do(t, h, http.MethodGet, "/v1/events", tokenMain, nil, ""), http.StatusInternalServerError,
		ClassTransient)
	store.fail("EventsAfter", nil)

	store.fail("AppendEvent", errors.New("append failed"))
	if err := b.Press(context.Background(), "main", Press{MessageID: "m1", Data: "d",
		User: Person{ExternalId: "u", DisplayName: "U"}}); err == nil || !strings.Contains(err.Error(), "append failed") {
		t.Fatalf("Press with a failing store = %v", err)
	}
	store.fail("AppendEvent", nil)

	log := logs.String()
	for _, want := range []string{"claim failed for [redacted]", "save failed", "release failed", "find failed",
		"events failed", `msg="bridge: the store failed"`, "cannot store the answer of an Idempotency-Key",
		"cannot release an Idempotency-Key", "cannot be sent again: it is not the JSON of the operation's answer",
		"cannot be sent again: api/bridge.yaml declares no 200 answer of postMessage"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log does not say %q", want)
		}
	}
	if strings.Contains(log, tokenMain) {
		t.Errorf("the log carries the token: %s", log)
	}
}

func TestStoredEventThatIsNotJSON(t *testing.T) {
	store := newFailingStore()
	b, m, clk, _ := newTestBridge(t, func(c *Config) { c.Store = store })
	if _, err := store.AppendEvent(context.Background(), "main", StoredEvent{ID: "e1", Type: EventTypePress,
		Body: []byte("not json"), Until: clk.Now().Add(time.Hour)}, clk.Now()); err != nil {
		t.Fatal(err)
	}
	wantProblem(t, do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("k1"), answerBody("e1")),
		http.StatusInternalServerError, ClassTransient)
	if m.count("AnswerEvent") != 0 {
		t.Fatal("the messenger was called")
	}
}

func TestRequestWithoutBody(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/channels/alerts/check", nil)
	r.Body = nil
	r.Header.Set("Authorization", "Bearer "+tokenMain)
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("a request without a body answered %d", rec.Code)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the client went away") }

func TestUnreadableBody(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPatch, "/v1/messages/m1", failingReader{})
	r.Header.Set("Authorization", "Bearer "+tokenMain)
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, r)
	if p := wantProblem(t, rec, http.StatusBadRequest, ClassInvalid); detailOf(p) != "Reading the request body failed." {
		t.Fatalf("detail %q", detailOf(p))
	}
}
