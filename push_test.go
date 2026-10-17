// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// pushed is one push the receiver got.
type pushed struct {
	id, timestamp string
	body          []byte
	header        http.Header
	status        int
}

// receiver is Muster's push callback in tests: it verifies every signature and timestamp on its own, and answers from
// its script.
type receiver struct {
	t   *testing.T
	clk *fakeClock
	key []byte
	srv *httptest.Server
	got chan pushed

	mu         sync.Mutex
	statuses   []int
	statusOf   func(id string) int
	holdFirst  int
	inFlight   int
	maxFlight  int
	redirected int
	problems   []string
}

func newReceiver(t *testing.T, clk *fakeClock, secret string, statuses ...int) *receiver {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		t.Fatal(err)
	}
	rc := &receiver{t: t, clk: clk, key: key, got: make(chan pushed, 100), statuses: statuses}
	mux := http.NewServeMux()
	mux.HandleFunc("/elsewhere", func(http.ResponseWriter, *http.Request) {
		rc.mu.Lock()
		rc.redirected++
		rc.mu.Unlock()
	})
	mux.HandleFunc("/", rc.serve)
	rc.srv = httptest.NewServer(mux)
	t.Cleanup(rc.srv.Close)
	t.Cleanup(func() {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		for _, p := range rc.problems {
			t.Error(p)
		}
	})
	return rc
}

func (rc *receiver) target() *PushTarget {
	return &PushTarget{URL: rc.srv.URL + "/api/v1/callbacks/bridge/c1", Secret: testSecret}
}

func (rc *receiver) problem(format string, args ...any) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.problems = append(rc.problems, fmt.Sprintf(format, args...))
}

func (rc *receiver) serve(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	rc.inFlight++
	rc.maxFlight = max(rc.maxFlight, rc.inFlight)
	hold := rc.holdFirst > 0
	if hold {
		rc.holdFirst--
	}
	rc.mu.Unlock()
	defer func() {
		rc.mu.Lock()
		rc.inFlight--
		rc.mu.Unlock()
	}()
	body, _ := io.ReadAll(r.Body)
	p := pushed{id: r.Header.Get("webhook-id"), timestamp: r.Header.Get("webhook-timestamp"), body: body,
		header: r.Header.Clone()}
	mac := hmac.New(sha256.New, rc.key)
	mac.Write([]byte(p.id + "." + p.timestamp + "." + string(body)))
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if got := r.Header.Get("webhook-signature"); !hmac.Equal([]byte(got), []byte(want)) {
		rc.problem("push %s: signature %q, want %q", p.id, got, want)
	}
	if now := strconv.FormatInt(rc.clk.Now().Unix(), 10); p.timestamp != now {
		rc.problem("push %s: webhook-timestamp %s, want the clock's %s", p.id, p.timestamp, now)
	}
	var ev struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID != p.id {
		rc.problem("push %s: the body's id is %q (%v)", p.id, ev.ID, err)
	}
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" ||
		r.Header.Get("User-Agent") != "muster-bridge-go" {
		rc.problem("push %s: %s with Content-Type %q, User-Agent %q", p.id, r.Method, r.Header.Get("Content-Type"),
			r.Header.Get("User-Agent"))
	}
	rc.mu.Lock()
	p.status = http.StatusNoContent
	switch {
	case len(rc.statuses) > 0:
		p.status, rc.statuses = rc.statuses[0], rc.statuses[1:]
	case rc.statusOf != nil:
		p.status = rc.statusOf(p.id)
	}
	rc.mu.Unlock()
	rc.got <- p
	if hold {
		<-r.Context().Done()
		return
	}
	if p.status == http.StatusFound {
		w.Header().Set("Location", "/elsewhere")
	}
	w.WriteHeader(p.status)
}

// next is the next push, within 10 s.
func (rc *receiver) next() pushed {
	rc.t.Helper()
	select {
	case p := <-rc.got:
		return p
	case <-time.After(10 * time.Second):
		rc.t.Fatal("no push within 10 s")
	}
	return pushed{}
}

// none checks that no push is waiting.
func (rc *receiver) none() {
	rc.t.Helper()
	select {
	case p := <-rc.got:
		rc.t.Fatalf("unexpected push %s", p.id)
	default:
	}
}

// settle waits until the pusher waits for a timer of d, then lets it pass.
func settle(t *testing.T, clk *fakeClock, d time.Duration) {
	t.Helper()
	clk.awaitTimer(t, d)
	clk.Advance(d)
}

func TestPushSignsEventsInOrder(t *testing.T) {
	clk := newFakeClock()
	rc := newReceiver(t, clk, testSecret)
	b, _, _, _ := newTestBridge(t, func(c *Config) { c.clock = clk; c.Bots[0].Push = rc.target() })
	var ids []string
	for i := range 3 {
		ids = append(ids, reportPress(t, b, fmt.Sprint("u", i)))
	}
	for _, id := range ids {
		if p := rc.next(); p.id != id {
			t.Fatalf("push %s, want %s", p.id, id)
		}
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.maxFlight != 1 {
		t.Fatalf("%d pushes at once", rc.maxFlight)
	}
}

func TestPushOneAtATime(t *testing.T) {
	clk := newFakeClock()
	rc := newReceiver(t, clk, testSecret, http.StatusInternalServerError)
	b, _, _, _ := newTestBridge(t, func(c *Config) { c.clock = clk; c.Bots[0].Push = rc.target() })
	first := reportPress(t, b, "u1")
	second := reportPress(t, b, "u2")
	if p := rc.next(); p.id != first {
		t.Fatalf("push %s", p.id)
	}
	clk.awaitTimer(t, pushBackoffFirst)
	rc.none()
	clk.Advance(pushBackoffFirst)
	for _, want := range []string{first, second} {
		if p := rc.next(); p.id != want {
			t.Fatalf("push %s, want %s", p.id, want)
		}
	}
}

func TestPushRetriesWithBackoffUnderSameID(t *testing.T) {
	clk := newFakeClock()
	rc := newReceiver(t, clk, testSecret, 500, 500, 401, 503)
	b, _, _, logs := newTestBridge(t, func(c *Config) { c.clock = clk; c.Bots[0].Push = rc.target() })
	id := reportPress(t, b, "u1")
	var stamps []string
	for _, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 0} {
		p := rc.next()
		if p.id != id {
			t.Fatalf("push %s, want %s", p.id, id)
		}
		stamps = append(stamps, p.timestamp)
		if delay > 0 {
			settle(t, clk, delay)
		}
	}
	if stamps[0] == stamps[4] {
		t.Fatal("the timestamp was not renewed")
	}
	log := logs.String()
	if strings.Count(log, `msg="bridge: a push failed, retrying"`) != 4 ||
		strings.Count(log, "level=WARN") != 1 || !strings.Contains(log, "status=500") {
		t.Fatalf("log = %s", log)
	}
}

func TestPushKeepsOn401(t *testing.T) {
	clk := newFakeClock()
	rc := newReceiver(t, clk, testSecret, 401, 401)
	b, _, _, _ := newTestBridge(t, func(c *Config) { c.clock = clk; c.Bots[0].Push = rc.target() })
	id := reportPress(t, b, "u1")
	rc.next()
	settle(t, clk, time.Second)
	rc.next()
	settle(t, clk, 2*time.Second)
	if p := rc.next(); p.id != id || p.status != http.StatusNoContent {
		t.Fatalf("push %s answered %d", p.id, p.status)
	}
}

func TestPushDropsOn400(t *testing.T) {
	clk := newFakeClock()
	rc := newReceiver(t, clk, testSecret, 400)
	b, _, _, logs := newTestBridge(t, func(c *Config) { c.clock = clk; c.Bots[0].Push = rc.target() })
	first := reportPress(t, b, "u1")
	second := reportPress(t, b, "u2")
	if p := rc.next(); p.id != first || p.status != http.StatusBadRequest {
		t.Fatalf("push %s %d", p.id, p.status)
	}
	if p := rc.next(); p.id != second {
		t.Fatalf("push %s, want %s at once", p.id, second)
	}
	if events := poll(t, b, tokenMain, "?wait=0").Events; len(events) != 2 {
		t.Fatalf("%d events kept for polling", len(events))
	}
	if !strings.Contains(logs.String(), `msg="bridge: Muster refused a pushed event for good"`) {
		t.Fatalf("log = %s", logs)
	}
}

func TestPushTimeoutRetries(t *testing.T) {
	clk := newFakeClock()
	rc := newReceiver(t, clk, testSecret)
	rc.holdFirst = 1
	b, _, _, logs := newTestBridge(t, func(c *Config) { c.clock = clk; c.Bots[0].Push = rc.target() })
	id := reportPress(t, b, "u1")
	rc.next()
	settle(t, clk, pushTimeout)
	settle(t, clk, pushBackoffFirst)
	if p := rc.next(); p.id != id || p.status != http.StatusNoContent {
		t.Fatalf("push %s %d", p.id, p.status)
	}
	if !strings.Contains(logs.String(), "context canceled") {
		t.Fatalf("log = %s", logs)
	}
}

func TestPushBackoffCapsAtFiveMinutes(t *testing.T) {
	clk := newFakeClock()
	rc := newReceiver(t, clk, testSecret)
	rc.statusOf = func(string) int { return http.StatusServiceUnavailable }
	b, _, _, _ := newTestBridge(t, func(c *Config) { c.clock = clk; c.Bots[0].Push = rc.target() })
	reportPress(t, b, "u1")
	for _, d := range []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300} {
		rc.next()
		settle(t, clk, d*time.Second)
	}
	rc.next()
}

func TestPushGivesUpAfterAnHour(t *testing.T) {
	clk := newFakeClock()
	rc := newReceiver(t, clk, testSecret)
	b, _, _, logs := newTestBridge(t, func(c *Config) { c.clock = clk })
	first := reportPress(t, b, "u1")
	second := reportPress(t, b, "u2")
	rc.statusOf = func(id string) int {
		if id == first {
			return http.StatusInternalServerError
		}
		return http.StatusNoContent
	}
	if err := b.SetPush("main", rc.target()); err != nil {
		t.Fatal(err)
	}
	var elapsed time.Duration
	delay, attempts := pushBackoffFirst, 0
	for {
		p := rc.next()
		if p.id == second {
			break
		}
		attempts++
		if elapsed >= pushRetryFor {
			continue // the last attempt: the pusher gives up and pushes the next event at once
		}
		settle(t, clk, delay)
		elapsed += delay
		delay = min(2*delay, pushBackoffMax)
	}
	if attempts != 21 || elapsed < pushRetryFor {
		t.Fatalf("gave up after %d attempts and %s", attempts, elapsed)
	}
	if !strings.Contains(logs.String(), `msg="bridge: gave up pushing an event, it stays for polling"`) ||
		!strings.Contains(logs.String(), "attempts=21") {
		t.Fatalf("log = %s", logs)
	}
	if events := poll(t, b, tokenMain, "?wait=0").Events; len(events) != 2 {
		t.Fatalf("%d events kept for polling", len(events))
	}
}

func TestPushDoesNotFollowRedirects(t *testing.T) {
	clk := newFakeClock()
	rc := newReceiver(t, clk, testSecret, http.StatusFound)
	b, _, _, _ := newTestBridge(t, func(c *Config) { c.clock = clk; c.Bots[0].Push = rc.target() })
	id := reportPress(t, b, "u1")
	rc.next()
	settle(t, clk, time.Second)
	if p := rc.next(); p.id != id || p.status != http.StatusNoContent {
		t.Fatalf("push %s %d", p.id, p.status)
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.redirected != 0 {
		t.Fatal("the push followed the redirect")
	}
}

func TestPushWithoutTargetQueuesForLater(t *testing.T) {
	clk := newFakeClock()
	rc := newReceiver(t, clk, testSecret)
	b, _, _, _ := newTestBridge(t, func(c *Config) { c.clock = clk })
	first := reportPress(t, b, "u1")
	second := reportPress(t, b, "u2")
	if err := b.Press(context.Background(), "other", Press{UpdateID: "x", MessageID: "m1", User: alice,
		Data: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetPush("main", rc.target()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{first, second} {
		if p := rc.next(); p.id != want {
			t.Fatalf("push %s, want %s", p.id, want)
		}
	}
	third := reportPress(t, b, "u3")
	if p := rc.next(); p.id != third {
		t.Fatalf("push %s, want %s", p.id, third)
	}
	rc.none()
}

func TestSetPushRetargets(t *testing.T) {
	clk := newFakeClock()
	failing := newReceiver(t, clk, testSecret)
	failing.statusOf = func(string) int { return http.StatusInternalServerError }
	working := newReceiver(t, clk, testSecret)
	b, _, _, _ := newTestBridge(t, func(c *Config) { c.clock = clk; c.Bots[0].Push = failing.target() })
	id := reportPress(t, b, "u1")
	failing.next()
	clk.awaitTimer(t, pushBackoffFirst)
	if err := b.SetPush("main", working.target()); err != nil {
		t.Fatal(err)
	}
	if p := working.next(); p.id != id {
		t.Fatalf("push %s, want %s at once", p.id, id)
	}
	failing.none()
}

func TestSetPushNilStopsPushing(t *testing.T) {
	clk := newFakeClock()
	rc := newReceiver(t, clk, testSecret)
	rc.statuses = []int{http.StatusInternalServerError}
	b, _, _, _ := newTestBridge(t, func(c *Config) { c.clock = clk; c.Bots[0].Push = rc.target() })
	id := reportPress(t, b, "u1")
	rc.next()
	clk.awaitTimer(t, pushBackoffFirst)
	if err := b.SetPush("main", nil); err != nil {
		t.Fatal(err)
	}
	clk.awaitNoTimers(t)
	clk.Advance(time.Hour)
	reportPress(t, b, "u2")
	rc.none()
	if err := b.SetPush("main", rc.target()); err != nil {
		t.Fatal(err)
	}
	if p := rc.next(); p.id != id {
		t.Fatalf("push %s, want the event that was not delivered, %s", p.id, id)
	}
}

func TestSetPushNilBeforeAnyTargetLeavesNoSignal(t *testing.T) {
	clk := newFakeClock()
	rc := newReceiver(t, clk, testSecret, http.StatusInternalServerError)
	b, _, _, _ := newTestBridge(t, func(c *Config) { c.clock = clk })
	if err := b.SetPush("main", nil); err != nil {
		t.Fatal(err)
	}
	id := reportPress(t, b, "u1")
	if err := b.SetPush("main", rc.target()); err != nil {
		t.Fatal(err)
	}
	rc.next()
	clk.awaitTimer(t, pushBackoffFirst)
	rc.none()
	clk.Advance(pushBackoffFirst)
	if p := rc.next(); p.id != id {
		t.Fatalf("push %s", p.id)
	}
}

func TestSetPushRefuses(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	if err := b.SetPush("nobody", nil); !errors.Is(err, ErrUnknownBot) {
		t.Fatalf("unknown bot: %v", err)
	}
	err := b.SetPush("main", &PushTarget{URL: "ftp://x", Secret: "whsec_!!!secret"})
	if err == nil || !strings.Contains(err.Error(), "url is not an absolute http or https URL") ||
		!strings.Contains(err.Error(), "secret is not whsec_") || strings.Contains(err.Error(), "!!!secret") {
		t.Fatalf("invalid target: %v", err)
	}
	poll, _, _, _ := newTestBridge(t, func(c *Config) { c.EventDelivery = []InfoEventDelivery{InfoEventDeliveryPoll} })
	if err := poll.SetPush("main", &PushTarget{URL: "https://m/cb", Secret: testSecret}); err == nil ||
		!strings.Contains(err.Error(), "event_delivery does not offer push") {
		t.Fatalf("poll only: %v", err)
	}
	if err := poll.SetPush("main", nil); err != nil {
		t.Fatalf("no target on a poll-only bridge: %v", err)
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.SetPush("main", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("after Close: %v", err)
	}
}

func TestCloseStopsPushers(t *testing.T) {
	clk := newFakeClock()
	rc := newReceiver(t, clk, testSecret)
	rc.statusOf = func(string) int { return http.StatusInternalServerError }
	b, _, _, _ := newTestBridge(t, func(c *Config) { c.clock = clk; c.Bots[0].Push = rc.target() })
	reportPress(t, b, "u1")
	rc.next()
	clk.awaitTimer(t, pushBackoffFirst)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	reportPress(t, b, "u2")
	rc.none()
	if events := poll(t, b, tokenMain, "?wait=0").Events; len(events) != 2 {
		t.Fatal("the reports stopped after Close")
	}
}

func TestCloseDuringAnAttempt(t *testing.T) {
	clk := newFakeClock()
	rc := newReceiver(t, clk, testSecret)
	rc.holdFirst = 1
	b, _, _, _ := newTestBridge(t, func(c *Config) { c.clock = clk; c.Bots[0].Push = rc.target() })
	reportPress(t, b, "u1")
	rc.next()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCloseHonoursContext(t *testing.T) {
	clk := newFakeClock()
	store := newFailingStore()
	release := make(chan struct{})
	b, _, _, _ := newTestBridge(t, func(c *Config) { c.clock = clk; c.Store = store })
	entered := make(chan struct{}, 1)
	store.mu.Lock()
	store.before = func(method string) {
		if method == "EventsAfter" {
			entered <- struct{}{}
			<-release
		}
	}
	store.mu.Unlock()
	rc := newReceiver(t, clk, testSecret)
	if err := b.SetPush("main", rc.target()); err != nil {
		t.Fatal(err)
	}
	await(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v", err)
	}
	store.mu.Lock()
	store.before = nil
	store.mu.Unlock()
	close(release)
}

func TestPushStoreFailures(t *testing.T) {
	clk := newFakeClock()
	store := newFailingStore()
	rc := newReceiver(t, clk, testSecret)
	b, _, _, logs := newTestBridge(t, func(c *Config) { c.clock = clk; c.Store = store })
	first := reportPress(t, b, "u1")
	store.fail("EventsAfter", errors.New("events failed"))
	if err := b.SetPush("main", rc.target()); err != nil {
		t.Fatal(err)
	}
	clk.awaitTimer(t, pushBackoffFirst)
	store.fail("EventsAfter", nil)
	clk.Advance(pushBackoffFirst)
	if p := rc.next(); p.id != first {
		t.Fatalf("push %s", p.id)
	}
	store.fail("EventsAfter", ErrUnknownCursor)
	second := reportPress(t, b, "u2")
	clk.awaitTimer(t, pushBackoffFirst)
	store.fail("EventsAfter", nil)
	clk.Advance(pushBackoffFirst)
	for _, want := range []string{first, second} {
		if p := rc.next(); p.id != want {
			t.Fatalf("push %s, want %s", p.id, want)
		}
	}
	log := logs.String()
	if !strings.Contains(log, "events failed") || !strings.Contains(log,
		`msg="bridge: the store forgot the push cursor, pushing every kept event"`) {
		t.Fatalf("log = %s", log)
	}
}

func TestPushSecretNeverLogged(t *testing.T) {
	clk := newFakeClock()
	secret := strings.TrimPrefix(testSecret, "whsec_")
	b, _, _, logs := newTestBridge(t, func(c *Config) {
		c.clock = clk
		c.Bots[0].Push = &PushTarget{URL: "http://127.0.0.1:1/cb?s=" + testSecret, Secret: testSecret}
	})
	reportPress(t, b, "u1")
	clk.awaitTimer(t, pushBackoffFirst)
	log := logs.String()
	if !strings.Contains(log, `msg="bridge: a push failed, retrying"`) || !strings.Contains(log, "[redacted]") {
		t.Fatalf("log = %s", log)
	}
	if strings.Contains(log, secret) {
		t.Fatalf("the log carries the push secret: %s", log)
	}
}

func TestPushTargetsOfEachBot(t *testing.T) {
	clk := newFakeClock()
	rcMain, rcOther := newReceiver(t, clk, testSecret), newReceiver(t, clk, testSecret)
	b, _, _, _ := newTestBridge(t, func(c *Config) {
		c.clock = clk
		c.Bots[0].Push, c.Bots[1].Push = rcMain.target(), rcOther.target()
	})
	id := reportPress(t, b, "u1")
	if err := b.Press(context.Background(), "other", Press{UpdateID: "o1", MessageID: "m9", User: alice,
		Data: "d"}); err != nil {
		t.Fatal(err)
	}
	if p := rcMain.next(); p.id != id {
		t.Fatalf("main pushed %s", p.id)
	}
	if p := rcOther.next(); p.id != eventIDOf("other", EventTypePress, "o1") {
		t.Fatalf("other pushed %s", p.id)
	}
	rcMain.none()
}

func TestSignature(t *testing.T) {
	// The example of the Standard Webhooks specification.
	key, _ := base64.StdEncoding.DecodeString("MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw")
	got := sign(key, "msg_p5jXN8AQM9LWM0D4loKWxJek", "1614265330", []byte(`{"test": 2432232314}`))
	if got != "g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=" {
		t.Fatalf("signature %s", got)
	}
}
