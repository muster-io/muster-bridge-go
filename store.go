// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Store keeps, per bot, the first answers of Idempotency-Keys and the queued events. Every method is given the time of
// the library's clock and must be safe for concurrent use. A bridge that must keep its keys and events across restarts
// implements it over its own database; NewMemoryStore keeps them in memory.
type Store interface {
	// ClaimKey returns KeyAnswered with the answer when one is stored for (bot, key) until after now; else KeyRunning
	// when another claim holds the key until after now; else it claims the key until `until` and returns KeyClaimed
	// with an ID that tells this claim from any later claim of the same key.
	ClaimKey(ctx context.Context, bot, key string, now, until time.Time) (KeyClaim, error)
	// SaveKey stores the answer of (bot, key), kept until `until`, in place of the claim claimID. When another claim
	// or an answer holds the key, SaveKey leaves it as it is: the first answer stays, and so does a newer request's
	// claim.
	SaveKey(ctx context.Context, bot, key, claimID string, answer StoredAnswer, until time.Time) error
	// ReleaseKey removes the claim claimID of (bot, key), so that a retry with the key runs again. It removes nothing
	// else: not an answer, and not the claim of a request that took the key over after claimID expired.
	ReleaseKey(ctx context.Context, bot, key, claimID string) error
	// AppendEvent queues the event for the bot unless an event with its ID is kept, and reports whether it queued it.
	AppendEvent(ctx context.Context, bot string, event StoredEvent, now time.Time) (bool, error)
	// EventsAfter returns up to limit of the bot's kept events, oldest first, that follow cursor ("" starts before the
	// oldest), with the cursor that follows the last one returned. Returning none, it gives cursor back unchanged, or,
	// for "", the cursor at the end of the queue. A cursor that this store did not issue for the bot is
	// ErrUnknownCursor.
	EventsAfter(ctx context.Context, bot, cursor string, limit int, now time.Time) ([]StoredEvent, string, error)
	// FindEvent returns the kept event of the bot with the id, and whether there is one.
	FindEvent(ctx context.Context, bot, id string, now time.Time) (StoredEvent, bool, error)
}

// KeyState is the outcome of Store.ClaimKey.
type KeyState int

// The outcomes of Store.ClaimKey.
const (
	// KeyClaimed means the key was free and the caller runs the request.
	KeyClaimed KeyState = iota
	// KeyRunning means another request with the key is still running.
	KeyRunning
	// KeyAnswered means the key has a stored answer, in KeyClaim.Answer.
	KeyAnswered
)

// KeyClaim is the result of Store.ClaimKey.
type KeyClaim struct {
	State KeyState
	// Answer is the stored answer, with KeyAnswered.
	Answer StoredAnswer
	// ID names the claim, with KeyClaimed; the library gives it back to SaveKey and ReleaseKey.
	ID string
}

// StoredAnswer is the first successful answer of an Idempotency-Key.
type StoredAnswer struct {
	Status int
	// Body is the JSON of the first answer, with replayed false.
	Body []byte
}

// StoredEvent is an event queued for a bot.
type StoredEvent struct {
	ID   string
	Type EventType
	// Body is the event exactly as pollEvents and the push send it.
	Body []byte
	// Chat is what the bridge gave with the report; it is never sent to Muster.
	Chat string
	// Until is the time until which the event is kept, at least.
	Until time.Time
}

// ErrUnknownCursor is the error of Store.EventsAfter for a cursor the store did not give for the bot.
var ErrUnknownCursor = errors.New("bridge: unknown cursor")

// NewMemoryStore returns a Store that keeps keys and events in memory, for one process: a restart forgets them, and
// Muster then polls again without a cursor.
//
// Its memory is bounded by time, not by size: it holds every event of the last 24 hours, whatever Muster has already
// polled or acknowledged (a Thread message carries up to 4000 characters), and the answers of the Idempotency-Keys of
// the last 24 hours. A bridge whose day of traffic does not fit in memory implements Store over its database, which
// may also drop the events before the cursor that Muster last sent.
func NewMemoryStore() Store {
	return &memoryStore{keys: map[keyRef]*keyEntry{}, queues: map[string]*eventQueue{}}
}

type memoryStore struct {
	mu        sync.Mutex
	keys      map[keyRef]*keyEntry
	keyExpiry keyHeap
	queues    map[string]*eventQueue
	claims    uint64
}

type keyRef struct{ bot, key string }

// keyEntry is a claim, named by claimID, or the stored answer of a key.
type keyEntry struct {
	answered bool
	answer   StoredAnswer
	claimID  string
	until    time.Time
}

type keyDue struct {
	ref   keyRef
	until time.Time
}

// keyHeap orders the expiry times of keys, the soonest first.
type keyHeap []keyDue

func (h keyHeap) Len() int           { return len(h) }
func (h keyHeap) Less(i, j int) bool { return h[i].until.Before(h[j].until) }
func (h keyHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *keyHeap) Push(x any)        { *h = append(*h, x.(keyDue)) }
func (h *keyHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

type eventQueue struct {
	epoch  string
	head   uint64
	events []queuedEvent
	byID   map[string]uint64
}

type queuedEvent struct {
	seq uint64
	ev  StoredEvent
}

func (s *memoryStore) ClaimKey(_ context.Context, bot, key string, now, until time.Time) (KeyClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeKeys(now)
	ref := keyRef{bot, key}
	if e, ok := s.keys[ref]; ok && e.until.After(now) {
		if e.answered {
			return KeyClaim{State: KeyAnswered, Answer: e.answer}, nil
		}
		return KeyClaim{State: KeyRunning}, nil
	}
	s.claims++
	id := strconv.FormatUint(s.claims, 10)
	s.setKey(ref, &keyEntry{claimID: id, until: until})
	return KeyClaim{State: KeyClaimed, ID: id}, nil
}

func (s *memoryStore) SaveKey(_ context.Context, bot, key, claimID string, answer StoredAnswer, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := keyRef{bot, key}
	if held, ok := s.keys[ref]; ok && (held.answered || held.claimID != claimID) {
		return nil
	}
	s.setKey(ref, &keyEntry{answered: true, answer: answer, until: until})
	return nil
}

func (s *memoryStore) ReleaseKey(_ context.Context, bot, key, claimID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := keyRef{bot, key}
	if held, ok := s.keys[ref]; ok && !held.answered && held.claimID == claimID {
		delete(s.keys, ref)
	}
	return nil
}

func (s *memoryStore) setKey(ref keyRef, e *keyEntry) {
	s.keys[ref] = e
	heap.Push(&s.keyExpiry, keyDue{ref: ref, until: e.until})
}

// purgeKeys forgets the keys whose time ended; a later entry of the same key has its own expiry.
func (s *memoryStore) purgeKeys(now time.Time) {
	for len(s.keyExpiry) > 0 && !s.keyExpiry[0].until.After(now) {
		due := heap.Pop(&s.keyExpiry).(keyDue)
		if e, ok := s.keys[due.ref]; ok && e.until.Equal(due.until) {
			delete(s.keys, due.ref)
		}
	}
}

func (s *memoryStore) AppendEvent(_ context.Context, bot string, ev StoredEvent, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.queue(bot, now)
	if err != nil {
		return false, err
	}
	if seq, ok := q.byID[ev.ID]; ok {
		if e, found := q.find(seq); found && e.Until.After(now) {
			return false, nil
		}
	}
	q.head++
	q.events = append(q.events, queuedEvent{seq: q.head, ev: ev})
	q.byID[ev.ID] = q.head
	return true, nil
}

func (s *memoryStore) EventsAfter(_ context.Context, bot, cursor string, limit int, now time.Time) ([]StoredEvent,
	string, error,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.queue(bot, now)
	if err != nil {
		return nil, "", err
	}
	var after uint64
	if cursor != "" {
		seq, ok := q.parse(cursor)
		if !ok {
			return nil, "", ErrUnknownCursor
		}
		after = seq
	}
	if limit <= 0 {
		limit = len(q.events)
	}
	var out []StoredEvent
	last := after
	i := sort.Search(len(q.events), func(i int) bool { return q.events[i].seq > after })
	for ; i < len(q.events) && len(out) < limit; i++ {
		if e := q.events[i]; e.ev.Until.After(now) {
			out = append(out, e.ev)
			last = e.seq
		}
	}
	switch {
	case len(out) > 0:
		return out, q.cursor(last), nil
	case cursor == "":
		return nil, q.cursor(q.head), nil
	}
	return nil, cursor, nil
}

func (s *memoryStore) FindEvent(_ context.Context, bot, id string, now time.Time) (StoredEvent, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.queues[bot]
	if !ok {
		return StoredEvent{}, false, nil
	}
	q.purge(now)
	seq, ok := q.byID[id]
	if !ok {
		return StoredEvent{}, false, nil
	}
	ev, found := q.find(seq)
	if !found || !ev.Until.After(now) {
		return StoredEvent{}, false, nil
	}
	return ev, true, nil
}

// queue is the bot's queue, made on first use with an epoch of its own, so that the cursors of another bot or of an
// earlier process are unknown; its expired events are purged from the front.
func (s *memoryStore) queue(bot string, now time.Time) (*eventQueue, error) {
	q, ok := s.queues[bot]
	if !ok {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		q = &eventQueue{epoch: base64.RawURLEncoding.EncodeToString(b[:]), byID: map[string]uint64{}}
		s.queues[bot] = q
	}
	q.purge(now)
	return q, nil
}

func (q *eventQueue) purge(now time.Time) {
	n := 0
	for n < len(q.events) && !q.events[n].ev.Until.After(now) {
		e := q.events[n]
		if q.byID[e.ev.ID] == e.seq {
			delete(q.byID, e.ev.ID)
		}
		n++
	}
	if n > 0 {
		q.events = append(q.events[:0:0], q.events[n:]...)
	}
}

func (q *eventQueue) find(seq uint64) (StoredEvent, bool) {
	i := sort.Search(len(q.events), func(i int) bool { return q.events[i].seq >= seq })
	if i < len(q.events) && q.events[i].seq == seq {
		return q.events[i].ev, true
	}
	return StoredEvent{}, false
}

func (q *eventQueue) cursor(seq uint64) string {
	return q.epoch + "." + strconv.FormatUint(seq, 10)
}

// parse is the sequence number of a cursor of this queue, which is known when it is not past the head.
func (q *eventQueue) parse(cursor string) (uint64, bool) {
	epoch, n, ok := strings.Cut(cursor, ".")
	if !ok || epoch != q.epoch {
		return 0, false
	}
	seq, err := strconv.ParseUint(n, 10, 64)
	if err != nil || seq > q.head {
		return 0, false
	}
	return seq, true
}
