// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	bridge "github.com/muster-io/muster-bridge-go"
)

// notebook is a messenger that keeps the bot's messages in memory: a whole Messenger in a few lines.
type notebook struct {
	mu       sync.Mutex
	messages []bridge.MessageInput
}

func (n *notebook) PostMessage(_ context.Context, _ string, in bridge.MessageInput) (bridge.MessagePosted, error) {
	if in.ChannelId != "alerts" {
		return bridge.MessagePosted{}, bridge.ForbiddenError("The bot has not joined this channel.")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.messages = append(n.messages, in)
	return bridge.MessagePosted{MessageId: "n" + strconv.Itoa(len(n.messages))}, nil
}

func (n *notebook) EditMessage(_ context.Context, _, id string, in bridge.MessageContent) (bridge.MessageEdited,
	error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	i, err := strconv.Atoi(strings.TrimPrefix(id, "n"))
	if err != nil || i < 1 || i > len(n.messages) {
		return bridge.MessageEdited{}, bridge.GoneError("That message is gone.")
	}
	n.messages[i-1].Title, n.messages[i-1].Body = in.Title, in.Body
	return bridge.MessageEdited{}, nil
}

func (n *notebook) ReplyInThread(_ context.Context, _, id string, _ bridge.ThreadReplyInput) (bridge.ThreadReplyPosted,
	error) {
	return bridge.ThreadReplyPosted{MessageId: id + "-reply", ThreadRef: id}, nil
}

func (n *notebook) CheckChannel(_ context.Context, _, channelID string) (string, error) {
	if channelID != "alerts" {
		return "", bridge.ForbiddenError("The bot has not joined this channel.")
	}
	return "Alerts", nil
}

func (n *notebook) AnswerEvent(context.Context, string, bridge.ReportedEvent, string) (bridge.AnswerShownShownAs,
	error) {
	return bridge.AnswerShownShownAsThread, nil
}

// newNotebookBridge is a bridge with one bot, main, whose token is example-token.
func newNotebookBridge(m bridge.Messenger) *bridge.Bridge {
	b, err := bridge.New(bridge.Config{
		Name:          "notebook-bridge",
		Version:       "1.0.0",
		MessengerName: "Notebook",
		Bots:          []bridge.Bot{{Name: "main", Token: "example-token"}},
		Limits:        bridge.Limits{MaxBodyLength: 4000},
		Capabilities: bridge.Capabilities{Buttons: &bridge.Capabilities_Buttons{MaxPerMessage: 5, MaxPerRow: 5,
			DataMaxBytes: 64, Styles: []bridge.ButtonStyle{bridge.ButtonStylePrimary}}},
		Logger: slog.New(slog.DiscardHandler),
	}, m)
	if err != nil {
		panic(err)
	}
	return b
}

// get is the answer of the handler to a GET of path with the token, as Muster sends it.
func get(h http.Handler, path, token string) *http.Response {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

// pollTypes polls the events of the bot main and returns their types.
func pollTypes(b *bridge.Bridge) []string {
	res := get(b.Handler(), "/v1/events?wait=0", "example-token")
	defer res.Body.Close()
	var batch struct {
		Events []struct {
			Type      string `json:"type"`
			MessageID string `json:"message_id"`
		} `json:"events"`
	}
	if err := json.NewDecoder(res.Body).Decode(&batch); err != nil {
		panic(err)
	}
	types := []string{}
	for _, ev := range batch.Events {
		types = append(types, strings.TrimSuffix(ev.Type+" "+ev.MessageID, " "))
	}
	return types
}

// A bridge is a Config, a Messenger and the handler that serves them to Muster.
func Example() {
	b := newNotebookBridge(&notebook{})
	defer func() { _ = b.Close(context.Background()) }()

	res := get(b.Handler(), "/v1/info", "example-token")
	defer res.Body.Close()
	var info bridge.Info
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		panic(err)
	}
	fmt.Println(res.StatusCode, info.ProtocolVersion, info.UserLookup, info.EventDelivery)
	// Output: 200 1 none [poll push]
}

// The handler mounted under a path prefix: Muster's Bridge Connection then has the base URL
// https://bridge.example.org/bridge.
func ExampleBridge_Handler() {
	b := newNotebookBridge(&notebook{})
	defer func() { _ = b.Close(context.Background()) }()

	mux := http.NewServeMux()
	mux.Handle("/bridge/", http.StripPrefix("/bridge", b.Handler()))

	for _, token := range []string{"example-token", "wrong-token"} {
		res := get(mux, "/bridge/v1/info", token)
		_ = res.Body.Close()
		fmt.Println(token, res.StatusCode)
	}
	// Output:
	// example-token 200
	// wrong-token 401
}

// A press is reported once the messenger has told the bridge about it; Muster then polls it, or the bridge pushes it.
func ExampleBridge_Press() {
	b := newNotebookBridge(&notebook{})
	defer func() { _ = b.Close(context.Background()) }()

	alice := bridge.Person{ExternalId: "U1", DisplayName: "Alice Smith"}
	press := bridge.Press{UpdateID: "callback-17", MessageID: "n1", User: alice, Data: "ack"}
	for range 2 {
		// The messenger delivered the update twice; it is queued once.
		if err := b.Press(context.Background(), "main", press); err != nil {
			panic(err)
		}
	}
	fmt.Println(pollTypes(b))
	// Output: [press n1]
}

// A direct message is an Account link token when its whole text is one, after an optional /start; any other is
// ignored.
func ExampleBridge_DirectMessage() {
	b := newNotebookBridge(&notebook{})
	defer func() { _ = b.Close(context.Background()) }()

	alice := bridge.Person{ExternalId: "U1", DisplayName: "Alice Smith"}
	for _, text := range []string{"/start " + strings.Repeat("A", 43), "hello"} {
		if err := b.DirectMessage(context.Background(), "main", bridge.DirectMessage{User: alice, Text: text}); err != nil {
			panic(err)
		}
	}
	fmt.Println(pollTypes(b))
	// Output: [account_link]
}

func ExampleReplaceMentions() {
	body := `[@Alice Smith](muster-mention:a1) acknowledged; [@Bob \[ops\]](muster-mention:b2) is next.`
	mentions := []bridge.Mention{{Ref: "a1", ExternalId: "U1", DisplayName: "Alice Smith"}}

	// A bridge with native_mentions writes the messenger's own mention.
	fmt.Println(bridge.ReplaceMentions(body, mentions, func(m bridge.Mention) string { return "@" + m.ExternalId }))
	// Output: @U1 acknowledged; @Bob \[ops\] is next.
}

// inkNotebook refuses every edit, as a messenger does for messages older than its edit window.
type inkNotebook struct{ *notebook }

func (inkNotebook) EditMessage(context.Context, string, string, bridge.MessageContent) (bridge.MessageEdited, error) {
	return bridge.MessageEdited{}, bridge.GoneError("Messages older than 48 hours cannot be edited.",
		bridge.ReasonEditWindowClosed)
}

// A Messenger answers a class of the protocol with an *Error; Muster then posts a new Root message instead of editing.
func ExampleGoneError() {
	b := newNotebookBridge(inkNotebook{&notebook{}})
	defer func() { _ = b.Close(context.Background()) }()

	body := `{"status":"acknowledged","color":"#f9a825","emoji":"👀","title":"Disk full","body":"",` +
		`"link":{"url":"https://muster.example.org/g/1","label":"Muster"},"buttons":[]}`
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPatch, "/v1/messages/n1",
		strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer example-token")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	b.Handler().ServeHTTP(w, r)

	var p bridge.Problem
	if err := json.NewDecoder(w.Body).Decode(&p); err != nil {
		panic(err)
	}
	fmt.Println(w.Code, *p.Class, *p.Reason, *p.Detail)
	// Output: 404 gone edit_window_closed Messages older than 48 hours cannot be edited.
}

// A Store implementation follows the memory store: a key is claimed while its request runs, then holds its answer.
// Only the holder of a claim, named by its ID, saves or releases it.
func ExampleNewMemoryStore() {
	ctx := context.Background()
	store := bridge.NewMemoryStore()
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)

	claim, _ := store.ClaimKey(ctx, "main", "key-1", now, now.Add(2*time.Minute))
	fmt.Println(claim.State == bridge.KeyClaimed)
	running, _ := store.ClaimKey(ctx, "main", "key-1", now, now.Add(2*time.Minute))
	fmt.Println(running.State == bridge.KeyRunning)

	answer := bridge.StoredAnswer{Status: http.StatusCreated, Body: []byte(`{"message_id":"n1","replayed":false}`)}
	_ = store.SaveKey(ctx, "main", "key-1", claim.ID, answer, now.Add(24*time.Hour))
	later, _ := store.ClaimKey(ctx, "main", "key-1", now.Add(time.Hour), now.Add(time.Hour+2*time.Minute))
	fmt.Println(later.State == bridge.KeyAnswered, later.Answer.Status)
	// Output:
	// true
	// true
	// true 201
}
