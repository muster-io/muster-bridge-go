// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var bg = context.Background()

func storedEvent(id string, until time.Time) StoredEvent {
	return StoredEvent{ID: id, Type: EventTypePress, Body: []byte(`{"id":"` + id + `"}`), Chat: "c-" + id, Until: until}
}

func mustClaim(t *testing.T, s Store, bot, key string, now time.Time, want KeyState) KeyClaim {
	t.Helper()
	c, err := s.ClaimKey(bg, bot, key, now, now.Add(keyClaimTTL))
	if err != nil {
		t.Fatal(err)
	}
	if c.State != want {
		t.Fatalf("ClaimKey(%s, %s) = %d, want %d", bot, key, c.State, want)
	}
	return c
}

func TestMemoryStoreClaimAndSave(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	c := mustClaim(t, s, "main", "k1", now, KeyClaimed)
	answer := StoredAnswer{Status: 201, Body: []byte(`{"message_id":"m1","replayed":false}`)}
	if err := s.SaveKey(bg, "main", "k1", c.ID, answer, now.Add(keyRetention)); err != nil {
		t.Fatal(err)
	}
	c = mustClaim(t, s, "main", "k1", now.Add(time.Hour), KeyAnswered)
	if c.Answer.Status != 201 || string(c.Answer.Body) != string(answer.Body) {
		t.Fatalf("answer = %+v", c.Answer)
	}
}

func TestMemoryStoreRunningClaim(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	mustClaim(t, s, "main", "k1", now, KeyClaimed)
	mustClaim(t, s, "main", "k1", now.Add(time.Second), KeyRunning)
}

func TestMemoryStoreClaimLeaseExpires(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	first := mustClaim(t, s, "main", "k1", now, KeyClaimed)
	mustClaim(t, s, "main", "k1", now.Add(keyClaimTTL-time.Nanosecond), KeyRunning)
	if again := mustClaim(t, s, "main", "k1", now.Add(keyClaimTTL), KeyClaimed); again.ID == first.ID {
		t.Fatalf("the claim after the lease has the first one's ID %q", again.ID)
	}
}

func TestMemoryStoreReleaseKeepsAnswer(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	c := mustClaim(t, s, "main", "k1", now, KeyClaimed)
	if err := s.ReleaseKey(bg, "main", "k1", c.ID); err != nil {
		t.Fatal(err)
	}
	c = mustClaim(t, s, "main", "k1", now, KeyClaimed)
	if err := s.SaveKey(bg, "main", "k1", c.ID, StoredAnswer{Status: 200, Body: []byte("{}")}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseKey(bg, "main", "k1", c.ID); err != nil {
		t.Fatal(err)
	}
	mustClaim(t, s, "main", "k1", now, KeyAnswered)
	if err := s.ReleaseKey(bg, "main", "never-claimed", "1"); err != nil {
		t.Fatal(err)
	}
}

// A request that outlives its claim neither frees nor overwrites the claim of the request that took the key over.
func TestMemoryStoreStaleClaimLeavesTheNewOne(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	stale := mustClaim(t, s, "main", "k1", now, KeyClaimed)
	later := now.Add(keyClaimTTL)
	fresh := mustClaim(t, s, "main", "k1", later, KeyClaimed)
	if err := s.ReleaseKey(bg, "main", "k1", stale.ID); err != nil {
		t.Fatal(err)
	}
	mustClaim(t, s, "main", "k1", later, KeyRunning)
	if err := s.SaveKey(bg, "main", "k1", stale.ID, StoredAnswer{Status: 201, Body: []byte(`{"stale":true}`)},
		later.Add(keyRetention)); err != nil {
		t.Fatal(err)
	}
	mustClaim(t, s, "main", "k1", later, KeyRunning)
	if err := s.SaveKey(bg, "main", "k1", fresh.ID, StoredAnswer{Status: 201, Body: []byte(`{"fresh":true}`)},
		later.Add(keyRetention)); err != nil {
		t.Fatal(err)
	}
	if c := mustClaim(t, s, "main", "k1", later, KeyAnswered); string(c.Answer.Body) != `{"fresh":true}` {
		t.Fatalf("the key answers %s", c.Answer.Body)
	}
}

func TestMemoryStoreFirstAnswerStays(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	c := mustClaim(t, s, "main", "k1", now, KeyClaimed)
	for _, body := range []string{`{"n":1}`, `{"n":2}`} {
		if err := s.SaveKey(bg, "main", "k1", c.ID, StoredAnswer{Status: 201, Body: []byte(body)},
			now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustClaim(t, s, "main", "k1", now, KeyAnswered); string(got.Answer.Body) != `{"n":1}` {
		t.Fatalf("the key answers %s", got.Answer.Body)
	}
}

func TestMemoryStoreSaveWithoutClaim(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	if err := s.SaveKey(bg, "main", "k1", "", StoredAnswer{Status: 200, Body: []byte("{}")}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	mustClaim(t, s, "main", "k1", now, KeyAnswered)
}

func TestMemoryStoreKeysPerBot(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	c := mustClaim(t, s, "main", "k1", now, KeyClaimed)
	if err := s.SaveKey(bg, "main", "k1", c.ID, StoredAnswer{Status: 201, Body: []byte("{}")}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	mustClaim(t, s, "other", "k1", now, KeyClaimed)
}

func TestMemoryStoreKeysExpireAfter24Hours(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	c := mustClaim(t, s, "main", "k1", now, KeyClaimed)
	if err := s.SaveKey(bg, "main", "k1", c.ID, StoredAnswer{Status: 201, Body: []byte("{}")}, now.Add(keyRetention)); err != nil {
		t.Fatal(err)
	}
	mustClaim(t, s, "main", "k1", now.Add(keyRetention-time.Nanosecond), KeyAnswered)
	mustClaim(t, s, "main", "k1", now.Add(keyRetention), KeyClaimed)
	ms := s.(*memoryStore)
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.keys) != 1 {
		t.Fatalf("the store keeps %d keys, want only the new claim", len(ms.keys))
	}
}

func TestMemoryStorePurgesKeys(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	for i := range 50 {
		mustClaim(t, s, "main", fmt.Sprint("k", i), now, KeyClaimed)
	}
	mustClaim(t, s, "main", "last", now.Add(keyClaimTTL), KeyClaimed)
	ms := s.(*memoryStore)
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.keys) != 1 || len(ms.keyExpiry) != 1 {
		t.Fatalf("the store keeps %d keys and %d expiries after they expired", len(ms.keys), len(ms.keyExpiry))
	}
}

func TestMemoryStoreEventsDedupe(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	for i, want := range []bool{true, false} {
		queued, err := s.AppendEvent(bg, "main", storedEvent("e1", now.Add(eventRetention)), now)
		if err != nil || queued != want {
			t.Fatalf("append %d: queued %v, %v", i, queued, err)
		}
	}
	if queued, _ := s.AppendEvent(bg, "other", storedEvent("e1", now.Add(eventRetention)), now); !queued {
		t.Fatal("an id of one bot hid the event of another")
	}
	events, _, _ := s.EventsAfter(bg, "main", "", 10, now)
	if len(events) != 1 {
		t.Fatalf("%d events", len(events))
	}
}

func TestMemoryStoreEventsAfter(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	events, head, err := s.EventsAfter(bg, "main", "", 10, now)
	if err != nil || len(events) != 0 || head == "" {
		t.Fatalf("empty queue: %v %q %v", events, head, err)
	}
	for i := range 5 {
		if _, err := s.AppendEvent(bg, "main", storedEvent(fmt.Sprint("e", i), now.Add(eventRetention)), now); err != nil {
			t.Fatal(err)
		}
	}
	first, c1, err := s.EventsAfter(bg, "main", "", 2, now)
	if err != nil || eventIDs(first) != "e0 e1" {
		t.Fatalf("first page %s, %v", eventIDs(first), err)
	}
	if first[0].Chat != "c-e0" || first[0].Type != EventTypePress {
		t.Fatalf("event %+v", first[0])
	}
	second, c2, _ := s.EventsAfter(bg, "main", c1, 10, now)
	if eventIDs(second) != "e2 e3 e4" {
		t.Fatalf("second page %s", eventIDs(second))
	}
	none, c3, _ := s.EventsAfter(bg, "main", c2, 10, now)
	if len(none) != 0 || c3 != c2 {
		t.Fatalf("after the end: %s %q, want the same cursor %q", eventIDs(none), c3, c2)
	}
	fromHead, _, _ := s.EventsAfter(bg, "main", head, 0, now)
	if eventIDs(fromHead) != "e0 e1 e2 e3 e4" {
		t.Fatalf("after the head cursor of the empty queue: %s", eventIDs(fromHead))
	}
	all, _, _ := s.EventsAfter(bg, "main", "", 0, now)
	if len(all) != 5 {
		t.Fatalf("limit 0 returned %d events", len(all))
	}
}

func eventIDs(events []StoredEvent) string {
	var out []string
	for _, e := range events {
		out = append(out, e.ID)
	}
	return strings.Join(out, " ")
}

func TestMemoryStoreUnknownCursor(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	if _, err := s.AppendEvent(bg, "main", storedEvent("e1", now.Add(eventRetention)), now); err != nil {
		t.Fatal(err)
	}
	_, mainCursor, _ := s.EventsAfter(bg, "main", "", 10, now)
	_, otherCursor, _ := s.EventsAfter(bg, "other", "", 10, now)
	epoch, _, _ := strings.Cut(mainCursor, ".")
	for _, cursor := range []string{"garbage", otherCursor, epoch + ".2", epoch + ".x", epoch, "." + "1"} {
		if _, _, err := s.EventsAfter(bg, "main", cursor, 10, now); !errors.Is(err, ErrUnknownCursor) {
			t.Errorf("cursor %q: %v", cursor, err)
		}
	}
	if _, _, err := NewMemoryStore().EventsAfter(bg, "main", mainCursor, 10, now); !errors.Is(err, ErrUnknownCursor) {
		t.Errorf("a cursor of another store: %v", err)
	}
}

func TestMemoryStoreEventsExpire(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	if _, err := s.AppendEvent(bg, "main", storedEvent("old", now.Add(time.Hour)), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(bg, "main", storedEvent("short", now.Add(time.Minute)), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(bg, "main", storedEvent("new", now.Add(2*time.Hour)), now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(30 * time.Minute)
	events, _, _ := s.EventsAfter(bg, "main", "", 10, later)
	if eventIDs(events) != "old new" {
		t.Fatalf("after 30 minutes: %s", eventIDs(events))
	}
	if _, ok, _ := s.FindEvent(bg, "main", "short", later); ok {
		t.Fatal("an expired event in the middle of the queue was found")
	}
	if queued, _ := s.AppendEvent(bg, "main", storedEvent("short", later.Add(2*time.Hour)), later); !queued {
		t.Fatal("the id of an expired event was refused")
	}
	events, _, _ = s.EventsAfter(bg, "main", "", 10, now.Add(90*time.Minute))
	if eventIDs(events) != "new short" {
		t.Fatalf("after 90 minutes: %s", eventIDs(events))
	}
	if _, ok, _ := s.FindEvent(bg, "main", "short", now.Add(90*time.Minute)); !ok {
		t.Fatal("the event queued again was not found")
	}
}

func TestMemoryStoreFindEvent(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	if _, ok, err := s.FindEvent(bg, "main", "e1", now); ok || err != nil {
		t.Fatal("an event of an empty store was found")
	}
	if _, err := s.AppendEvent(bg, "main", storedEvent("e1", now.Add(eventRetention)), now); err != nil {
		t.Fatal(err)
	}
	ev, ok, err := s.FindEvent(bg, "main", "e1", now)
	if !ok || err != nil || ev.ID != "e1" || ev.Chat != "c-e1" {
		t.Fatalf("FindEvent = %+v %v %v", ev, ok, err)
	}
	if _, ok, _ := s.FindEvent(bg, "other", "e1", now); ok {
		t.Fatal("the event of main was found for other")
	}
	if _, ok, _ := s.FindEvent(bg, "main", "e2", now); ok {
		t.Fatal("an unknown id was found")
	}
	if _, ok, _ := s.FindEvent(bg, "main", "e1", now.Add(eventRetention)); ok {
		t.Fatal("an expired event was found")
	}
}

func TestMemoryStoreConcurrent(t *testing.T) {
	s, now := NewMemoryStore(), testStart
	var wg sync.WaitGroup
	claimed := make(chan string, 400)
	for i := range 8 {
		wg.Go(func() {
			for j := range 50 {
				k := fmt.Sprint("k", j)
				if c, err := s.ClaimKey(bg, "main", k, now, now.Add(time.Minute)); err == nil && c.State == KeyClaimed {
					claimed <- k
				}
				_, _ = s.AppendEvent(bg, "main", storedEvent(fmt.Sprint("e", i, "-", j), now.Add(time.Hour)), now)
				_, _, _ = s.EventsAfter(bg, "main", "", 10, now)
			}
		})
	}
	wg.Wait()
	close(claimed)
	seen := map[string]bool{}
	for k := range claimed {
		if seen[k] {
			t.Fatalf("the key %s was claimed twice", k)
		}
		seen[k] = true
	}
	if events, _, _ := s.EventsAfter(bg, "main", "", 0, now); len(events) != 400 {
		t.Fatalf("%d events, want 400", len(events))
	}
}
