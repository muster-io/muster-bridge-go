// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// EventType is the type of an event the bridge reports.
type EventType string

// The event types of the protocol.
const (
	EventTypePress          EventType = "press"
	EventTypeThreadMessage  EventType = "thread_message"
	EventTypeAccountLink    EventType = "account_link"
	EventTypeMessageDeleted EventType = "message_deleted"
)

// Press is a press of a button of a message of the bot, already confirmed to the messenger.
type Press struct {
	// UpdateID is the messenger's own id of the update, "" when it has none; an update reported twice with the same
	// id is queued once.
	UpdateID string
	// MessageID is the message the button belongs to: a Root message or a Thread reply.
	MessageID string
	// User is the person who pressed.
	User Person
	// Data is the button's data, unchanged.
	Data string
	// At is when it happened; zero is now.
	At time.Time
	// Chat is anything the bridge needs to answer the person, such as a chat or callback id; the library keeps it with
	// the event, gives it back in ReportedEvent.Chat and never sends it to Muster.
	Chat string
}

// ThreadMessage is a person's message in the Thread of a message of the bot; never a message of the bot itself.
type ThreadMessage struct {
	// UpdateID is the messenger's own id of the update, "" when it has none.
	UpdateID string
	// MessageID is the message whose Thread it is in, the message_id of replyInThread; never the id of a reply.
	MessageID string
	// ThreadRef is the Thread's thread_ref, "" for none.
	ThreadRef string
	// User is the person who wrote.
	User Person
	// Text is the message as plain text, mentions of the bot removed; it is cut to 4000 characters.
	Text string
	// At is when it happened; zero is now.
	At time.Time
	// Chat is kept with the event and given back in ReportedEvent.Chat.
	Chat string
}

// MessageDeleted is the deletion of a message of the bot in the messenger.
type MessageDeleted struct {
	// UpdateID is the messenger's own id of the update, "" when it has none.
	UpdateID string
	// MessageID is the deleted message.
	MessageID string
	// At is when it happened; zero is now.
	At time.Time
}

// DirectMessage is a direct message a person sent the bot.
type DirectMessage struct {
	// UpdateID is the messenger's own id of the update, "" when it has none.
	UpdateID string
	// User is the person who wrote.
	User Person
	// Text is the whole text the person sent.
	Text string
	// At is when it happened; zero is now.
	At time.Time
	// Chat is the direct chat, so that AnswerEvent answers there.
	Chat string
}

// ReportedEvent is an event the bridge reported, as AnswerEvent is given it.
type ReportedEvent struct {
	ID   string
	Type EventType
	// MessageID is the message of a press or a Thread message.
	MessageID string
	// ThreadRef is the thread_ref of a Thread message, "" for none.
	ThreadRef  string
	User       Person
	Chat       string
	OccurredAt time.Time
}

var (
	// ErrNotOffered is the error of a report whose event needs a feature the bridge does not offer.
	ErrNotOffered = errors.New("bridge: the bridge does not offer the feature this event needs")
	// ErrInvalidEvent is the error of a report whose event does not match api/bridge.yaml.
	ErrInvalidEvent = errors.New("bridge: the event does not match api/bridge.yaml")
)

const (
	eventRetention  = 24 * time.Hour
	defaultPollWait = 25 * time.Second
	pollBatch       = 100
	maxDisplayName  = 200
	maxHandle       = 320
	maxThreadText   = 4000
	accountLinkLen  = 43
)

// Press reports a press. It returns once the event is queued; pushes happen in the background, so a bridge confirms
// the press to its messenger within its deadline independently of Muster. The bridge must offer buttons.
func (b *Bridge) Press(ctx context.Context, bot string, p Press) error {
	if _, ok := b.bots[bot]; !ok {
		return ErrUnknownBot
	}
	if b.info.Capabilities.Buttons == nil {
		return fmt.Errorf("%w: press needs capabilities.buttons", ErrNotOffered)
	}
	if p.MessageID == "" {
		return fmt.Errorf("%w: message_id is empty", ErrInvalidEvent)
	}
	ev := PressEvent{Type: string(EventTypePress), MessageId: p.MessageID, User: person(p.User), Data: p.Data}
	return b.report(ctx, bot, EventTypePress, p.UpdateID, p.At, p.Chat, func(id string, at time.Time) any {
		ev.Id, ev.OccurredAt = id, at
		return ev
	})
}

// ThreadMessage reports a person's message in a Thread. The bridge must offer thread_messages.
func (b *Bridge) ThreadMessage(ctx context.Context, bot string, m ThreadMessage) error {
	if _, ok := b.bots[bot]; !ok {
		return ErrUnknownBot
	}
	if b.info.Capabilities.ThreadMessages == nil {
		return fmt.Errorf("%w: thread_message needs capabilities.thread_messages", ErrNotOffered)
	}
	if m.MessageID == "" {
		return fmt.Errorf("%w: message_id is empty", ErrInvalidEvent)
	}
	ev := ThreadMessageEvent{Type: string(EventTypeThreadMessage), MessageId: m.MessageID, User: person(m.User),
		Text: cutRunes(m.Text, maxThreadText)}
	if m.ThreadRef != "" {
		ref := m.ThreadRef
		ev.ThreadRef = &ref
	}
	return b.report(ctx, bot, EventTypeThreadMessage, m.UpdateID, m.At, m.Chat, func(id string, at time.Time) any {
		ev.Id, ev.OccurredAt = id, at
		return ev
	})
}

// MessageDeleted reports the deletion of a message of the bot. The bridge must offer deletion_events.
func (b *Bridge) MessageDeleted(ctx context.Context, bot string, m MessageDeleted) error {
	if _, ok := b.bots[bot]; !ok {
		return ErrUnknownBot
	}
	if d := b.info.Capabilities.DeletionEvents; d == nil || !*d {
		return fmt.Errorf("%w: message_deleted needs capabilities.deletion_events", ErrNotOffered)
	}
	if m.MessageID == "" {
		return fmt.Errorf("%w: message_id is empty", ErrInvalidEvent)
	}
	ev := MessageDeletedEvent{Type: string(EventTypeMessageDeleted), MessageId: m.MessageID}
	return b.report(ctx, bot, EventTypeMessageDeleted, m.UpdateID, m.At, "", func(id string, at time.Time) any {
		ev.Id, ev.OccurredAt = id, at
		return ev
	})
}

// DirectMessage reports a direct message to the bot. A message whose whole text, trimmed and after an optional
// "/start ", is 43 characters of base64url is an Account link token and becomes an account_link event; any other is
// ignored, and DirectMessage returns nil. Every bridge takes Account link tokens.
func (b *Bridge) DirectMessage(ctx context.Context, bot string, m DirectMessage) error {
	if _, ok := b.bots[bot]; !ok {
		return ErrUnknownBot
	}
	token, ok := accountLinkToken(m.Text)
	if !ok {
		return nil
	}
	ev := AccountLinkEvent{Type: string(EventTypeAccountLink), Token: token, User: person(m.User)}
	return b.report(ctx, bot, EventTypeAccountLink, m.UpdateID, m.At, m.Chat, func(id string, at time.Time) any {
		ev.Id, ev.OccurredAt = id, at
		return ev
	})
}

// accountLinkToken is the Account link token of a direct message's text, if it is one.
func accountLinkToken(text string) (string, bool) {
	t := strings.TrimSpace(text)
	t, _ = strings.CutPrefix(t, "/start ")
	t = strings.TrimSpace(t)
	if len(t) != accountLinkLen {
		return "", false
	}
	for i := range len(t) {
		switch c := t[i]; {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return "", false
		}
	}
	return t, true
}

// person is p fitted to the protocol: the display name cut to 200 characters, a handle longer than 320 dropped.
func person(p Person) Person {
	p.DisplayName = cutRunes(p.DisplayName, maxDisplayName)
	if p.Handle != nil && utf8.RuneCountInString(*p.Handle) > maxHandle {
		p.Handle = nil
	}
	return p
}

// report gives the event its id and time, checks it against api/bridge.yaml and queues it for the bot.
func (b *Bridge) report(ctx context.Context, bot string, t EventType, updateID string, at time.Time, chat string,
	build func(id string, at time.Time) any,
) error {
	now := b.clock.Now()
	occurred := at.UTC()
	if at.IsZero() {
		occurred = now.UTC()
	}
	id, err := b.eventID(bot, t, updateID)
	if err != nil {
		return err
	}
	body, err := json.Marshal(build(id, occurred))
	if err != nil {
		return fmt.Errorf("%w: it cannot be encoded", ErrInvalidEvent)
	}
	if p := b.proto.check("Event", body); p != "" {
		return fmt.Errorf("%w: %s", ErrInvalidEvent, p)
	}
	queued, err := b.store.AppendEvent(ctx, bot, StoredEvent{ID: id, Type: t, Body: body, Chat: chat,
		Until: now.Add(eventRetention)}, now)
	if err != nil {
		return fmt.Errorf("bridge: cannot queue the %s event: %w", t, err)
	}
	if queued {
		b.bots[bot].notify()
	}
	return nil
}

// eventID is the base64url SHA-256 of the bot, the type and the messenger's id of the update, so that an update the
// messenger delivers twice is one event; random without an update id. Both fit webhook-id.
func (b *Bridge) eventID(bot string, t EventType, updateID string) (string, error) {
	if updateID != "" {
		sum := sha256.Sum256([]byte(bot + "\x00" + string(t) + "\x00" + updateID))
		return base64.RawURLEncoding.EncodeToString(sum[:]), nil
	}
	var r [16]byte
	if _, err := io.ReadFull(b.random, r[:]); err != nil {
		return "", fmt.Errorf("bridge: cannot make an event id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(r[:]), nil
}

// eventMembers are the members of a stored event that ReportedEvent and the redaction need.
type eventMembers struct {
	ID         string    `json:"id"`
	Type       EventType `json:"type"`
	OccurredAt time.Time `json:"occurred_at"`
	MessageID  string    `json:"message_id"`
	ThreadRef  *string   `json:"thread_ref"`
	User       Person    `json:"user"`
	Token      string    `json:"token"`
}

// pollEvents answers the events after the cursor at once, or the first that comes within wait, or none after it.
func (b *Bridge) pollEvents(op *operation, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bot, _ := ctx.Value(botKey{}).(string)
	if err := b.validateRequest(ctx, op, r, nil); err != nil {
		b.writeError(w, InvalidError(requestProblem(err)))
		return
	}
	query := r.URL.Query()
	cursor := query.Get("cursor")
	wait := defaultPollWait
	if s := query.Get("wait"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			b.writeError(w, InvalidError("The parameter wait is not a whole number of seconds."))
			return
		}
		wait = time.Duration(n) * time.Second
	}
	var timeout <-chan time.Time
	if wait > 0 {
		var stop func()
		timeout, stop = b.clock.Timer(wait)
		defer stop()
	}
	bs := b.bots[bot]
	for {
		changed := bs.changes()
		events, next, err := b.store.EventsAfter(ctx, bot, cursor, pollBatch, b.clock.Now())
		switch {
		case errors.Is(err, ErrUnknownCursor):
			b.writeError(w, InvalidError("The cursor is not known to this bridge; poll again without one."))
			return
		case err != nil:
			b.log.LogAttrs(ctx, slog.LevelError, "bridge: the store failed", slog.String("operation", op.id),
				slog.String("bot", bot), slog.String("error", b.redact(err.Error())))
			b.writeError(w, TransientError(detailStore))
			return
		case len(events) > 0 || wait <= 0:
			writeBatch(w, events, next)
			return
		}
		select {
		case <-changed:
		case <-timeout:
			writeBatch(w, nil, next)
			return
		case <-ctx.Done():
			return
		}
	}
}

func writeBatch(w http.ResponseWriter, events []StoredEvent, next string) {
	batch := struct {
		Events     []json.RawMessage `json:"events"`
		NextCursor string            `json:"next_cursor"`
	}{Events: make([]json.RawMessage, 0, len(events)), NextCursor: next}
	for _, ev := range events {
		batch.Events = append(batch.Events, ev.Body)
	}
	writeJSON(w, http.StatusOK, "application/json", mustMarshal(batch))
}

// answerEvent gives the messenger the event the answer is for; an event the library did not issue, no longer keeps,
// or a deletion, which no person caused, is gone.
func (c *call) answerEvent() (any, error) {
	var in AnswerInput
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	stored, ok, err := c.b.store.FindEvent(c.ctx, c.bot, in.EventId, c.b.clock.Now())
	if err != nil {
		return nil, c.storeFailed(err)
	}
	if !ok || stored.Type == EventTypeMessageDeleted {
		return nil, refuse(GoneError("The bridge does not know this event, or no longer keeps it."))
	}
	var m eventMembers
	if err := json.Unmarshal(stored.Body, &m); err != nil {
		return nil, c.storeFailed(errors.New("a stored event is not the JSON of an event"))
	}
	if m.Token != "" {
		c.note.secrets = append(c.note.secrets, m.Token)
	}
	ev := ReportedEvent{ID: stored.ID, Type: stored.Type, MessageID: m.MessageID, User: m.User, Chat: stored.Chat,
		OccurredAt: m.OccurredAt}
	if m.ThreadRef != nil {
		ev.ThreadRef = *m.ThreadRef
	}
	shown, err := c.b.messenger.AnswerEvent(c.ctx, c.bot, ev, in.Text)
	if err != nil {
		return nil, err
	}
	return AnswerShown{ShownAs: shown, Replayed: false}, nil
}
