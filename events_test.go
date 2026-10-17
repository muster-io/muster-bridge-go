// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var alice = Person{ExternalId: "u-alice", DisplayName: "Alice Smith", Handle: new("alice")}

// linkToken is an Account link token: 43 characters of base64url.
var linkToken = strings.Repeat("Ab0_-", 8) + "xyz"

// polled holds what one GET /v1/events returned: the cursor to send next, and each event as its raw JSON, so that a
// test compares the exact bytes the bridge wrote.
type polled struct {
	Cursor string            `json:"next_cursor"`
	Raw    []json.RawMessage `json:"events"`
}

func poll(t testing.TB, b *Bridge, token, query string) polled {
	t.Helper()
	rec := do(t, b.Handler(), http.MethodGet, "/v1/events"+query, token, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("poll answered %d: %s", rec.Code, rec.Body)
	}
	var out polled
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Raw == nil {
		t.Fatalf("poll answer %s: %v", rec.Body, err)
	}
	return out
}

func eventIDOf(bot string, t EventType, update string) string {
	sum := sha256.Sum256([]byte(bot + "\x00" + string(t) + "\x00" + update))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestPressQueuesEvent(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	at := time.Date(2026, 10, 18, 12, 0, 0, 123000000, time.FixedZone("CEST", 2*3600))
	if err := b.Press(context.Background(), "main", Press{UpdateID: "u1", MessageID: "m2", User: alice,
		Data: "a1.k2.sig", At: at, Chat: "chat-1"}); err != nil {
		t.Fatal(err)
	}
	got := poll(t, b, tokenMain, "?wait=0")
	want := `{"data":"a1.k2.sig","id":"` + eventIDOf("main", EventTypePress, "u1") + `","message_id":"m2",` +
		`"occurred_at":"2026-10-18T10:00:00.123Z","type":"press",` +
		`"user":{"display_name":"Alice Smith","external_id":"u-alice","handle":"alice"}}`
	if len(got.Raw) != 1 || string(got.Raw[0]) != want {
		t.Fatalf("events %s, want [%s]", got.Raw, want)
	}
	if strings.Contains(string(got.Raw[0]), "chat-1") {
		t.Fatal("the chat was sent to Muster")
	}
}

func TestOccurredAtDefaultsToClock(t *testing.T) {
	b, _, clk, _ := newTestBridge(t, nil)
	clk.Advance(90 * time.Second)
	if err := b.MessageDeleted(context.Background(), "main", MessageDeleted{UpdateID: "u1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	ev := decodeJSON(t, poll(t, b, tokenMain, "?wait=0").Raw[0])
	if ev["occurred_at"] != "2026-10-18T10:01:30Z" {
		t.Fatalf("occurred_at %v", ev["occurred_at"])
	}
}

func TestRandomIDWithoutUpdateID(t *testing.T) {
	random := bytes.Repeat([]byte{0xfb}, 32)
	b, _, _, _ := newTestBridge(t, func(c *Config) { c.random = bytes.NewReader(random) })
	for range 2 {
		if err := b.Press(context.Background(), "main", Press{MessageID: "m1", User: alice, Data: "d"}); err != nil {
			t.Fatal(err)
		}
	}
	events := poll(t, b, tokenMain, "?wait=0").Raw
	want := base64.RawURLEncoding.EncodeToString(random[:16])
	if len(events) != 1 || decodeJSON(t, events[0])["id"] != want || len(want) != 22 {
		t.Fatalf("events %s, want one with the id %s", events, want)
	}
	err := b.Press(context.Background(), "main", Press{MessageID: "m1", User: alice, Data: "d"})
	if err == nil || !strings.Contains(err.Error(), "cannot make an event id") {
		t.Fatalf("Press without randomness = %v", err)
	}
}

func TestUpdateReportedTwiceQueuedOnce(t *testing.T) {
	b, _, clk, _ := newTestBridge(t, nil)
	for range 2 {
		if err := b.Press(context.Background(), "main", Press{UpdateID: "u1", MessageID: "m1", User: alice,
			Data: "d"}); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
	}
	if events := poll(t, b, tokenMain, "?wait=0").Raw; len(events) != 1 {
		t.Fatalf("%d events", len(events))
	}
}

func TestSameUpdateIDOtherTypeIsAnotherEvent(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	ctx := context.Background()
	if err := b.Press(ctx, "main", Press{UpdateID: "7", MessageID: "m1", User: alice, Data: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := b.ThreadMessage(ctx, "main", ThreadMessage{UpdateID: "7", MessageID: "m1", User: alice, Text: "ack"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Press(ctx, "other", Press{UpdateID: "7", MessageID: "m1", User: alice, Data: "d"}); err != nil {
		t.Fatal(err)
	}
	if events := poll(t, b, tokenMain, "?wait=0").Raw; len(events) != 2 {
		t.Fatalf("%d events of main", len(events))
	}
	if events := poll(t, b, tokenOther, "?wait=0").Raw; len(events) != 1 {
		t.Fatalf("%d events of other", len(events))
	}
}

func TestThreadMessageEvent(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	ctx := context.Background()
	if err := b.ThreadMessage(ctx, "main", ThreadMessage{UpdateID: "1", MessageID: "m1", User: alice,
		Text: "ack"}); err != nil {
		t.Fatal(err)
	}
	if err := b.ThreadMessage(ctx, "main", ThreadMessage{UpdateID: "2", MessageID: "m1", ThreadRef: "t-m1",
		User: alice, Text: strings.Repeat("é", 4001)}); err != nil {
		t.Fatal(err)
	}
	events := poll(t, b, tokenMain, "?wait=0").Raw
	if !strings.Contains(string(events[0]), `"thread_ref":null`) || !strings.Contains(string(events[0]),
		`"type":"thread_message"`) {
		t.Fatalf("event %s", events[0])
	}
	second := decodeJSON(t, events[1])
	if second["thread_ref"] != "t-m1" || second["text"] != strings.Repeat("é", 4000) {
		t.Fatalf("event %s", events[1])
	}
}

func TestMessageDeletedEvent(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	if err := b.MessageDeleted(context.Background(), "main", MessageDeleted{UpdateID: "d1", MessageID: "m1",
		At: testStart}); err != nil {
		t.Fatal(err)
	}
	want := `{"id":"` + eventIDOf("main", EventTypeMessageDeleted, "d1") + `","message_id":"m1",` +
		`"occurred_at":"2026-10-18T10:00:00Z","type":"message_deleted"}`
	if events := poll(t, b, tokenMain, "?wait=0").Raw; len(events) != 1 || string(events[0]) != want {
		t.Fatalf("events %s, want [%s]", events, want)
	}
}

func TestDirectMessageAccountLink(t *testing.T) {
	tests := []struct {
		name, text string
		link       bool
	}{
		{"token", linkToken, true},
		{"start", "/start " + linkToken, true},
		{"white space", "  \n" + linkToken + "\t ", true},
		{"start with spaces", "/start   " + linkToken, true},
		{"42 characters", linkToken[:42], false},
		{"44 characters", linkToken + "a", false},
		{"plus", strings.Replace(linkToken, "A", "+", 1), false},
		{"slash", strings.Replace(linkToken, "A", "/", 1), false},
		{"start alone", "/start", false},
		{"text", "hello there", false},
		{"token in text", "my token is " + linkToken, false},
		{"start without space", "/start" + linkToken, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _, _, _ := newTestBridge(t, nil)
			if err := b.DirectMessage(context.Background(), "main", DirectMessage{UpdateID: "1", User: alice,
				Text: tt.text, Chat: "dm-alice"}); err != nil {
				t.Fatal(err)
			}
			events := poll(t, b, tokenMain, "?wait=0").Raw
			if !tt.link {
				if len(events) != 0 {
					t.Fatalf("an ordinary direct message became %s", events)
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("%d events", len(events))
			}
			ev := decodeJSON(t, events[0])
			if ev["type"] != "account_link" || ev["token"] != linkToken || ev["user"].(map[string]any)["external_id"] !=
				"u-alice" {
				t.Fatalf("event %s", events[0])
			}
		})
	}
}

func TestReportsNeedTheirFeature(t *testing.T) {
	b, _, _, _ := newTestBridge(t, func(c *Config) {
		c.Capabilities.Buttons, c.Capabilities.ThreadMessages, c.Capabilities.DeletionEvents = nil, nil, new(false)
	})
	ctx := context.Background()
	for _, tt := range []struct {
		err  error
		want string
	}{
		{b.Press(ctx, "main", Press{MessageID: "m1", User: alice}), "press needs capabilities.buttons"},
		{b.ThreadMessage(ctx, "main", ThreadMessage{MessageID: "m1", User: alice}),
			"thread_message needs capabilities.thread_messages"},
		{b.MessageDeleted(ctx, "main", MessageDeleted{MessageID: "m1"}),
			"message_deleted needs capabilities.deletion_events"},
	} {
		if !errors.Is(tt.err, ErrNotOffered) || !strings.Contains(tt.err.Error(), tt.want) {
			t.Errorf("error %v, want ErrNotOffered: %s", tt.err, tt.want)
		}
	}
	if err := b.DirectMessage(ctx, "main", DirectMessage{User: alice, Text: linkToken}); err != nil {
		t.Fatalf("an Account link without features: %v", err)
	}
}

func TestReportUnknownBot(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	ctx := context.Background()
	for _, err := range []error{
		b.Press(ctx, "nobody", Press{MessageID: "m1", User: alice}),
		b.ThreadMessage(ctx, "nobody", ThreadMessage{MessageID: "m1", User: alice}),
		b.MessageDeleted(ctx, "nobody", MessageDeleted{MessageID: "m1"}),
		b.DirectMessage(ctx, "nobody", DirectMessage{User: alice, Text: linkToken}),
		b.SetPush("nobody", nil),
	} {
		if !errors.Is(err, ErrUnknownBot) {
			t.Errorf("error %v, want ErrUnknownBot", err)
		}
	}
}

func TestReportInvalidEvent(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	ctx := context.Background()
	for _, tt := range []struct {
		err  error
		want string
	}{
		{b.Press(ctx, "main", Press{User: alice}), "message_id is empty"},
		{b.ThreadMessage(ctx, "main", ThreadMessage{User: alice}), "message_id is empty"},
		{b.MessageDeleted(ctx, "main", MessageDeleted{}), "message_id is empty"},
		{b.Press(ctx, "main", Press{MessageID: "m1", User: Person{DisplayName: "A"}}),
			"/user/external_id: minimum string length is 1"},
		{b.Press(ctx, "main", Press{MessageID: "m1", User: Person{ExternalId: strings.Repeat("x", 513), DisplayName: "A"}}),
			"/user/external_id: maximum string length is 512"},
		{b.DirectMessage(ctx, "main", DirectMessage{User: Person{ExternalId: "u"}, Text: linkToken}),
			"/user/display_name: minimum string length is 1"},
		{b.Press(ctx, "main", Press{UpdateID: "u", MessageID: "m1", User: alice, At: time.Date(-1, 1, 1, 0, 0, 0, 0,
			time.UTC)}), "cannot be encoded"},
	} {
		if !errors.Is(tt.err, ErrInvalidEvent) || !strings.Contains(tt.err.Error(), tt.want) {
			t.Errorf("error %v, want ErrInvalidEvent: %s", tt.err, tt.want)
		}
	}
	if events := poll(t, b, tokenMain, "?wait=0").Raw; len(events) != 0 {
		t.Fatalf("invalid events were queued: %s", events)
	}
}

func TestDisplayNameCutAndHandleDropped(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	p := Person{ExternalId: "u", DisplayName: strings.Repeat("é", 250), Handle: new(strings.Repeat("h", 321))}
	if err := b.Press(context.Background(), "main", Press{MessageID: "m1", User: p, Data: "d"}); err != nil {
		t.Fatal(err)
	}
	user := decodeJSON(t, poll(t, b, tokenMain, "?wait=0").Raw[0])["user"].(map[string]any)
	if user["display_name"] != strings.Repeat("é", 200) || user["handle"] != nil {
		t.Fatalf("user %v", user)
	}
	if p.DisplayName != strings.Repeat("é", 250) {
		t.Fatal("the report changed the caller's person")
	}
}

func TestPollAnswersAtOnce(t *testing.T) {
	b, _, clk, _ := newTestBridge(t, nil)
	reportPress(t, b, "u1")
	if events := poll(t, b, tokenMain, "").Raw; len(events) != 1 {
		t.Fatalf("%d events", len(events))
	}
	if clk.pending() != 0 {
		t.Fatal("the wait's timer was not stopped")
	}
}

// pollAsync starts a long poll and returns the channel of its answer.
func pollAsync(ctx context.Context, t *testing.T, b *Bridge, query string) <-chan *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/events"+query, nil)
	r.Header.Set("Authorization", "Bearer "+tokenMain)
	go func() {
		rec := httptest.NewRecorder()
		b.Handler().ServeHTTP(rec, r)
		done <- rec
	}()
	return done
}

func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("nothing within 10 s")
	}
	var zero T
	return zero
}

func TestPollWaitsForEvent(t *testing.T) {
	b, _, clk, _ := newTestBridge(t, nil)
	done := pollAsync(context.Background(), t, b, "")
	clk.awaitTimer(t, defaultPollWait)
	reportPress(t, b, "u1")
	rec := await(t, done)
	var got polled
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Raw) != 1 {
		t.Fatalf("the long poll answered %s", rec.Body)
	}
	if clk.pending() != 0 {
		t.Fatal("the wait's timer was not stopped")
	}
}

func TestPollWaitEndsEmpty(t *testing.T) {
	b, _, clk, _ := newTestBridge(t, nil)
	first := poll(t, b, tokenMain, "?wait=0")
	done := pollAsync(context.Background(), t, b, "?wait=5&cursor="+first.Cursor)
	clk.awaitTimer(t, 5*time.Second)
	clk.Advance(5 * time.Second)
	rec := await(t, done)
	if rec.Body.String() != `{"events":[],"next_cursor":"`+first.Cursor+`"}` {
		t.Fatalf("the long poll answered %s", rec.Body)
	}
}

func TestPollWaitZero(t *testing.T) {
	b, _, clk, _ := newTestBridge(t, nil)
	got := poll(t, b, tokenMain, "?wait=0")
	if len(got.Raw) != 0 || got.Cursor == "" || clk.pending() != 0 {
		t.Fatalf("poll %v, %d timers", got, clk.pending())
	}
}

func TestPollUnknownCursor(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	rec := do(t, b.Handler(), http.MethodGet, "/v1/events?cursor=nope", tokenMain, nil, "")
	if p := wantProblem(t, rec, http.StatusBadRequest, ClassInvalid); detailOf(p) != "The cursor is not known to "+
		"this bridge; poll again without one." {
		t.Fatalf("detail %q", detailOf(p))
	}
}

func TestPollCursorAdvances(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	for i := range 150 {
		reportPress(t, b, fmt.Sprint("u", i))
	}
	seen := map[string]bool{}
	cursor := ""
	var order []string
	for _, want := range []int{100, 50, 0} {
		got := poll(t, b, tokenMain, "?wait=0&cursor="+cursor)
		if len(got.Raw) != want {
			t.Fatalf("%d events, want %d", len(got.Raw), want)
		}
		for _, raw := range got.Raw {
			id := decodeJSON(t, raw)["id"].(string)
			if seen[id] {
				t.Fatalf("the event %s came twice", id)
			}
			seen[id] = true
			order = append(order, id)
		}
		cursor = got.Cursor
	}
	for i, id := range order {
		if id != eventIDOf("main", EventTypePress, fmt.Sprint("u", i)) {
			t.Fatalf("event %d is not the %d-th reported", i, i)
		}
	}
}

func TestPollEventsPerBot(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	reportPress(t, b, "u1")
	if events := poll(t, b, tokenOther, "?wait=0").Raw; len(events) != 0 {
		t.Fatalf("the other bot got %s", events)
	}
	main := poll(t, b, tokenMain, "?wait=0")
	rec := do(t, b.Handler(), http.MethodGet, "/v1/events?wait=0&cursor="+main.Cursor, tokenOther, nil, "")
	wantProblem(t, rec, http.StatusBadRequest, ClassInvalid)
}

func TestPollKeepsEvents24Hours(t *testing.T) {
	b, _, clk, _ := newTestBridge(t, nil)
	reportPress(t, b, "u1")
	clk.Advance(24*time.Hour - time.Second)
	if events := poll(t, b, tokenMain, "?wait=0").Raw; len(events) != 1 {
		t.Fatal("the event was gone before 24 hours")
	}
	clk.Advance(time.Second)
	if events := poll(t, b, tokenMain, "?wait=0").Raw; len(events) != 0 {
		t.Fatal("the event was kept after 24 hours")
	}
}

func TestPollClientGone(t *testing.T) {
	b, _, clk, _ := newTestBridge(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := pollAsync(ctx, t, b, "?wait=60")
	clk.awaitTimer(t, time.Minute)
	cancel()
	if rec := await(t, done); rec.Body.Len() != 0 {
		t.Fatalf("a poll whose client left answered %s", rec.Body)
	}
}

func TestAnswerEventReachesMessenger(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	m.answer = func(string, ReportedEvent, string) (AnswerShownShownAs, error) {
		return AnswerShownShownAsEphemeral, nil
	}
	at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	if err := b.ThreadMessage(context.Background(), "main", ThreadMessage{UpdateID: "u1", MessageID: "m1",
		ThreadRef: "t-m1", User: alice, Text: "ack", At: at, Chat: "chat-9"}); err != nil {
		t.Fatal(err)
	}
	id := eventIDOf("main", EventTypeThreadMessage, "u1")
	rec := do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("k1"), answerBody(id))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"replayed":false,"shown_as":"ephemeral"}` {
		t.Fatalf("answered %d %s", rec.Code, rec.Body)
	}
	calls := m.recorded()
	want := ReportedEvent{ID: id, Type: EventTypeThreadMessage, MessageID: "m1", ThreadRef: "t-m1", User: alice,
		Chat: "chat-9", OccurredAt: at}
	got := calls[0].event
	if calls[0].bot != "main" || calls[0].text != "Done" || got.ID != want.ID || got.Type != want.Type ||
		got.MessageID != want.MessageID || got.ThreadRef != want.ThreadRef || got.Chat != want.Chat ||
		!got.OccurredAt.Equal(want.OccurredAt) || got.User.ExternalId != "u-alice" || *got.User.Handle != "alice" {
		t.Fatalf("the messenger got %+v", calls[0])
	}
}

func TestAnswerEventUnknownIsGone(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	rec := do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("k1"), answerBody("unknown-event"))
	if p := wantProblem(t, rec, http.StatusNotFound, ClassGone); detailOf(p) != "The bridge does not know this "+
		"event, or no longer keeps it." {
		t.Fatalf("detail %q", detailOf(p))
	}
	id := reportPress(t, b, "u1")
	rec = do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenOther, key("k1"), answerBody(id))
	wantProblem(t, rec, http.StatusNotFound, ClassGone)
	if m.count("AnswerEvent") != 0 {
		t.Fatal("the messenger was called")
	}
	rec = do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("k1"), answerBody(id))
	if rec.Code != http.StatusOK || decodeJSON(t, rec.Body.Bytes())["replayed"] != false {
		t.Fatalf("the key of a gone answer was stored: %s", rec.Body)
	}
}

func TestAnswerEventMessageDeletedIsGone(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	if err := b.MessageDeleted(context.Background(), "main", MessageDeleted{UpdateID: "d1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	id := eventIDOf("main", EventTypeMessageDeleted, "d1")
	wantProblem(t, do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("k1"), answerBody(id)),
		http.StatusNotFound, ClassGone)
	if m.count("AnswerEvent") != 0 {
		t.Fatal("the messenger was called")
	}
}

func TestAnswerEventAccountLinkTokenRedacted(t *testing.T) {
	b, m, _, logs := newTestBridge(t, nil)
	if err := b.DirectMessage(context.Background(), "main", DirectMessage{UpdateID: "1", User: alice,
		Text: "/start " + linkToken, Chat: "dm-1"}); err != nil {
		t.Fatal(err)
	}
	id := eventIDOf("main", EventTypeAccountLink, "1")
	m.answer = func(_ string, ev ReportedEvent, _ string) (AnswerShownShownAs, error) {
		if ev.Type != EventTypeAccountLink || ev.Chat != "dm-1" {
			t.Errorf("the messenger got %+v", ev)
		}
		return "", errors.New("cannot answer the link " + linkToken)
	}
	rec := do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("k1"), answerBody(id))
	wantProblem(t, rec, http.StatusInternalServerError, ClassTransient)
	m.answer = func(string, ReportedEvent, string) (AnswerShownShownAs, error) {
		panic("the link " + linkToken + " broke")
	}
	rec = do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("k2"), answerBody(id))
	wantProblem(t, rec, http.StatusInternalServerError, ClassTransient)
	m.answer = func(string, ReportedEvent, string) (AnswerShownShownAs, error) {
		return "", ForbiddenError("blocked " + linkToken)
	}
	rec = do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("k3"), answerBody(id))
	wantProblem(t, rec, http.StatusInternalServerError, ClassTransient)
	log := logs.String()
	if strings.Contains(log, linkToken) || strings.Contains(rec.Body.String(), linkToken) {
		t.Fatalf("the Account link token reached the log or the answer: %s", log)
	}
	if !strings.Contains(log, "cannot answer the link [redacted]") || !strings.Contains(log,
		`panic="the link [redacted] broke"`) {
		t.Fatalf("log = %s", log)
	}
}
