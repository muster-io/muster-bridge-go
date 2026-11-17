// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGateway implements Gateway; each call is recorded and answered by its func, or by a default answer.
type fakeGateway struct {
	mu    sync.Mutex
	calls []fakeCall

	send   func(bot string, in NotificationInput) (NotificationSent, error)
	answer func(bot string, ev ReportedEvent, text string) (AnswerShownShownAs, error)
	err    error
}

func (g *fakeGateway) record(ctx context.Context, c fakeCall) {
	c.key = IdempotencyKeyFrom(ctx)
	g.mu.Lock()
	g.calls = append(g.calls, c)
	g.mu.Unlock()
}

func (g *fakeGateway) recorded() []fakeCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]fakeCall(nil), g.calls...)
}

func (g *fakeGateway) SendNotification(ctx context.Context, bot string, in NotificationInput) (NotificationSent, error) {
	g.record(ctx, fakeCall{method: "SendNotification", bot: bot, in: in})
	if g.err != nil {
		return NotificationSent{}, g.err
	}
	if g.send != nil {
		return g.send(bot, in)
	}
	return NotificationSent{NotificationId: fmt.Sprintf("n%d", len(g.recorded()))}, nil
}

func (g *fakeGateway) Answer(ctx context.Context, bot string, ev ReportedEvent, text string) (AnswerShownShownAs, error) {
	g.record(ctx, fakeCall{method: "Answer", bot: bot, event: ev, text: text})
	if g.err != nil {
		return "", g.err
	}
	if g.answer != nil {
		return g.answer(bot, ev, text)
	}
	return AnswerShownShownAsNotification, nil
}

// newGatewayBridge is a bridge of two bots whose only role is gateway, on a fake clock, with no Messenger.
func newGatewayBridge(t testing.TB, mutate func(*Config)) (*Bridge, *fakeGateway, *fakeClock, *logSink) {
	t.Helper()
	clk, logs, gw := newFakeClock(), &logSink{}, &fakeGateway{}
	cfg := testConfig(clk, logs)
	cfg.Roles, cfg.Gateway, cfg.Capabilities, cfg.UserLookup = []Role{RoleGateway}, gw, Capabilities{}, ""
	if mutate != nil {
		mutate(&cfg)
	}
	b, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := b.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return b, gw, clk, logs
}

const (
	notifyBody = `{"contact":"+10000000001","text":"Alert 12 is firing","language":"en",` +
		`"action":{"data":"ack-1","label":"Press 1 to acknowledge"}}`
	notifyNoAction = `{"contact":"+10000000001","text":"Code 1234","language":"ru"}`
)

func TestGatewayInfoDeclaresRoles(t *testing.T) {
	b, _, _, _ := newGatewayBridge(t, nil)
	rec := do(t, b.Handler(), http.MethodGet, "/v1/info", tokenMain, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("info answered %d %s", rec.Code, rec.Body)
	}
	if got := anyStrings(decodeJSON(t, rec.Body.Bytes())["roles"]); len(got) != 1 || got[0] != "gateway" {
		t.Fatalf("roles = %v in %s", got, rec.Body)
	}
	both, _, _, _ := newTestBridge(t, func(c *Config) {
		c.Roles, c.Gateway = []Role{RoleMessenger, RoleGateway}, &fakeGateway{}
	})
	rec = do(t, both.Handler(), http.MethodGet, "/v1/info", tokenMain, nil, "")
	if got := anyStrings(decodeJSON(t, rec.Body.Bytes())["roles"]); len(got) != 2 || got[0] != "messenger" ||
		got[1] != "gateway" {
		t.Fatalf("roles = %v in %s", got, rec.Body)
	}
	plain, _, _, _ := newTestBridge(t, nil)
	rec = do(t, plain.Handler(), http.MethodGet, "/v1/info", tokenMain, nil, "")
	if _, ok := decodeJSON(t, rec.Body.Bytes())["roles"]; ok {
		t.Fatalf("a default bridge declares roles: %s", rec.Body)
	}
}

func TestNewRoles(t *testing.T) {
	gw := &fakeGateway{}
	cases := []struct {
		name   string
		roles  []Role
		gw     Gateway
		m      Messenger
		errHas string
	}{
		{name: "messenger by default", m: &fakeMessenger{}},
		{name: "gateway only without a Messenger", roles: []Role{RoleGateway}, gw: gw},
		{name: "both", roles: []Role{RoleGateway, RoleMessenger}, gw: gw, m: &fakeMessenger{}},
		{name: "gateway without Gateway", roles: []Role{RoleGateway}, errHas: "Config.Gateway is nil"},
		{name: "messenger without Messenger", roles: []Role{RoleMessenger}, gw: nil,
			errHas: "the Messenger is nil, which the role messenger needs"},
		{name: "both without Messenger", roles: []Role{RoleMessenger, RoleGateway}, gw: gw,
			errHas: "the Messenger is nil, which the role messenger needs"},
		{name: "both without Gateway", roles: []Role{RoleMessenger, RoleGateway}, m: &fakeMessenger{},
			errHas: "Config.Gateway is nil"},
		{name: "Gateway without the role", gw: gw, m: &fakeMessenger{}, errHas: "roles has no gateway"},
		{name: "Messenger without the role", roles: []Role{RoleGateway}, gw: gw, m: &fakeMessenger{},
			errHas: "roles has no messenger"},
		{name: "empty", roles: []Role{}, m: &fakeMessenger{}, errHas: "roles is empty"},
		{name: "unknown", roles: []Role{"pager"}, m: &fakeMessenger{}, errHas: `roles[0] "pager" is not`},
		{name: "repeated", roles: []Role{RoleMessenger, RoleMessenger}, m: &fakeMessenger{},
			errHas: `roles[1] "messenger" is repeated`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(newFakeClock(), &logSink{})
			cfg.Roles, cfg.Gateway = tc.roles, tc.gw
			if tc.m == nil {
				cfg.Capabilities, cfg.UserLookup = Capabilities{}, ""
			}
			var m Messenger
			if tc.m != nil {
				m = tc.m
			}
			b, err := New(cfg, m)
			if b != nil {
				defer func() { _ = b.Close(context.Background()) }()
			}
			switch {
			case tc.errHas == "" && err != nil:
				t.Fatalf("New: %v", err)
			case tc.errHas != "" && (err == nil || !strings.Contains(err.Error(), tc.errHas)):
				t.Fatalf("New = %v, want an error with %q", err, tc.errHas)
			}
		})
	}
}

func TestNewRefusesMessengerFeaturesOfAGateway(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"capabilities":      {func(c *Config) { c.Capabilities = allCapabilities() }, "capabilities are a messenger's"},
		"user lookup":       {func(c *Config) { c.UserLookup = InfoUserLookupLogin }, "user_lookup is a messenger's"},
		"group mentions":    {func(c *Config) { c.GroupMentions = true }, "mentions are a messenger's"},
		"everyone mentions": {func(c *Config) { c.EveryoneMentions = []string{"here"} }, "mentions are a messenger's"},
		"link template":     {func(c *Config) { c.LinkURLTemplate = "https://x.example/?s={token}" }, "link_url_template"},
	} {
		cfg := testConfig(newFakeClock(), &logSink{})
		cfg.Roles, cfg.Gateway, cfg.Capabilities, cfg.UserLookup = []Role{RoleGateway}, &fakeGateway{}, Capabilities{}, ""
		tc.mutate(&cfg)
		if _, err := New(cfg, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: New = %v, want %q", name, err, tc.want)
		}
	}
}

// A lookup by Acknowledge holds the key of the notification for an instant; the action is kept all the same.
func TestNotificationKeptWhileALookupHoldsItsKey(t *testing.T) {
	store := newFailingStore()
	b, gw, _, _ := newGatewayBridge(t, func(c *Config) { c.Store = store })
	gw.send = func(_ string, _ NotificationInput) (NotificationSent, error) {
		if err := b.Acknowledge(context.Background(), "main", "n1"); !errors.Is(err, ErrUnknownNotification) {
			t.Errorf("a lookup before the notification is kept = %v", err)
		}
		return NotificationSent{NotificationId: "n1"}, nil
	}
	do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyBody)
	if err := b.Acknowledge(context.Background(), "main", "n1"); err != nil {
		t.Fatalf("the action was lost: %v", err)
	}
	// A lookup that holds the key for a moment, as a concurrent Acknowledge does: the first claims are running.
	busy := &busyNotifications{Store: NewMemoryStore(), running: 2}
	b2, _, _, logs := newGatewayBridge(t, func(c *Config) { c.Store = busy })
	rec := do(t, b2.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k2"), notifyBody)
	if rec.Code != http.StatusCreated || logs.String() != "" && strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("answered %d %s, log %s", rec.Code, rec.Body, logs)
	}
	if err := b2.Acknowledge(context.Background(), "main", "n1"); err != nil {
		t.Fatalf("the action was lost: %v", err)
	}
	// A key that stays claimed is not kept, and the request still succeeds.
	stuck := &busyNotifications{Store: NewMemoryStore(), running: 1 << 20}
	b3, _, _, logs3 := newGatewayBridge(t, func(c *Config) { c.Store = stuck })
	rec = do(t, b3.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k3"), notifyBody)
	if rec.Code != http.StatusCreated || !strings.Contains(logs3.String(), "stayed claimed") {
		t.Fatalf("answered %d %s, log %s", rec.Code, rec.Body, logs3)
	}
}

// busyNotifications answers the first claims of a notification's key as running, then works.
type busyNotifications struct {
	Store

	mu      sync.Mutex
	running int
}

func (s *busyNotifications) ClaimKey(ctx context.Context, bot, k string, now, until time.Time) (KeyClaim, error) {
	if strings.HasPrefix(k, " notification ") {
		s.mu.Lock()
		busy := s.running > 0
		s.running--
		s.mu.Unlock()
		if busy {
			return KeyClaim{State: KeyRunning}, nil
		}
	}
	return s.Store.ClaimKey(ctx, bot, k, now, until)
}

func TestSendNotification(t *testing.T) {
	b, gw, _, logs := newGatewayBridge(t, nil)
	rec := do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyBody)
	if rec.Code != http.StatusCreated || rec.Body.String() != `{"notification_id":"n1","replayed":false}` {
		t.Fatalf("answered %d %s", rec.Code, rec.Body)
	}
	calls := gw.recorded()
	in, _ := calls[0].in.(NotificationInput)
	if len(calls) != 1 || calls[0].bot != "main" || calls[0].key != "k1" || in.Contact != "+10000000001" ||
		in.Text != "Alert 12 is firing" || in.Language != NotificationInputLanguageEn || in.Action == nil ||
		in.Action.Data != "ack-1" || in.Action.Label != "Press 1 to acknowledge" {
		t.Fatalf("the gateway got %+v", calls)
	}
	if log := logs.String(); strings.Contains(log, "+10000000001") || strings.Contains(log, "firing") {
		t.Fatalf("the contact or the text reached the log: %s", log)
	}
}

func TestSendNotificationReplaysTheKey(t *testing.T) {
	b, gw, _, _ := newGatewayBridge(t, nil)
	first := do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyBody)
	again := do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyNoAction)
	if again.Code != http.StatusCreated || again.Body.String() != `{"notification_id":"n1","replayed":true}` {
		t.Fatalf("first %s, repeated %d %s", first.Body, again.Code, again.Body)
	}
	if n := len(gw.recorded()); n != 1 {
		t.Fatalf("the gateway was called %d times", n)
	}
	other := do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenOther, key("k1"), notifyBody)
	if other.Code != http.StatusCreated || decodeJSON(t, other.Body.Bytes())["replayed"] != false {
		t.Fatalf("another bot's key was replayed: %s", other.Body)
	}
	missing := do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, nil, notifyBody)
	wantProblem(t, missing, http.StatusBadRequest, ClassInvalid)
}

func TestSendNotificationRefusesBeforeTheGateway(t *testing.T) {
	b, gw, _, _ := newGatewayBridge(t, nil)
	body := func(text, data string) string {
		return fmt.Sprintf(`{"contact":"+1","text":%q,"language":"en","action":{"data":%q,"label":"Press 1"}}`, text, data)
	}
	for name, req := range map[string]string{
		"data of 65 bytes":    body("x", strings.Repeat("d", 65)),
		"text of 1001 points": body(strings.Repeat("я", 1001), "d"),
		"empty text":          body("", "d"),
		"data with a space":   body("x", "a b"),
		"unknown language":    `{"contact":"+1","text":"x","language":"de"}`,
		"no contact":          `{"text":"x","language":"en"}`,
	} {
		rec := do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k-"+name), req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: answered %d %s", name, rec.Code, rec.Body)
		}
	}
	if n := len(gw.recorded()); n != 0 {
		t.Fatalf("the gateway was called %d times", n)
	}
	edge := body(strings.Repeat("я", 1000), strings.Repeat("d", 64))
	if rec := do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("edge"), edge); rec.Code !=
		http.StatusCreated {
		t.Fatalf("1000 code points and 64 bytes answered %d %s", rec.Code, rec.Body)
	}
}

func TestSendNotificationNeedsTheRole(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	rec := do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyBody)
	p := wantProblem(t, rec, http.StatusBadRequest, ClassInvalid)
	if !strings.Contains(detailOf(p), "not in the role gateway") || len(m.recorded()) != 0 {
		t.Fatalf("detail %q", detailOf(p))
	}
	again := do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyBody)
	wantProblem(t, again, http.StatusBadRequest, ClassInvalid)
}

func TestSendNotificationErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		class  Class
	}{
		"invalid contact": {InvalidError("The number is not a phone number."), 400, ClassInvalid},
		"forbidden":       {ForbiddenError("The provider refused the credentials."), 403, ClassForbidden},
		"rate limited":    {RateLimitedError(time.Second, ProblemScopeBot, "Slow down."), 429, ClassRateLimited},
		"transient":       {TransientError("The provider is down."), 500, ClassTransient},
		"plain error":     {errors.New("the provider's socket broke for +10000000001"), 500, ClassTransient},
		"gone is a bug":   {GoneError("gone"), 500, ClassTransient},
	} {
		t.Run(name, func(t *testing.T) {
			b, gw, _, logs := newGatewayBridge(t, nil)
			gw.err = tc.err
			rec := do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyBody)
			wantProblem(t, rec, tc.status, tc.class)
			if log := logs.String(); strings.Contains(log, "+10000000001") {
				t.Fatalf("the contact reached the log: %s", log)
			}
			gw.err = nil
			ok := do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyBody)
			if ok.Code != http.StatusCreated || decodeJSON(t, ok.Body.Bytes())["replayed"] != false {
				t.Fatalf("a failed request kept its key: %d %s", ok.Code, ok.Body)
			}
		})
	}
}

func TestSendNotificationRejectsAMismatchingAnswer(t *testing.T) {
	b, gw, _, logs := newGatewayBridge(t, nil)
	gw.send = func(string, NotificationInput) (NotificationSent, error) { return NotificationSent{}, nil }
	rec := do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyBody)
	wantProblem(t, rec, http.StatusInternalServerError, ClassTransient)
	if !strings.Contains(logs.String(), "does not match api/bridge.yaml") {
		t.Fatalf("log = %s", logs)
	}
}

func TestGatewayOnlyBridgeDoesNotServeMessengerOperations(t *testing.T) {
	b, _, _, _ := newGatewayBridge(t, nil)
	for method, paths := range map[string][]string{
		http.MethodPost:   {"/v1/messages", "/v1/channels/alerts/check", "/v1/direct-messages", "/v1/messages/m1/thread-replies"},
		http.MethodPatch:  {"/v1/messages/m1"},
		http.MethodDelete: {"/v1/messages/m1"},
		http.MethodGet:    {"/v1/users?handle=alice"},
	} {
		for _, path := range paths {
			rec := do(t, b.Handler(), method, path, tokenMain, key("k1"), "")
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s answered %d %s", method, path, rec.Code, rec.Body)
			}
		}
	}
	for _, op := range []string{"/v1/info", "/v1/events?wait=0"} {
		if rec := do(t, b.Handler(), http.MethodGet, op, tokenMain, nil, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s answered %d %s", op, rec.Code, rec.Body)
		}
	}
}

func TestGatewayOnlyBridgeReportsNoMessengerEvents(t *testing.T) {
	b, _, _, _ := newGatewayBridge(t, nil)
	ctx := context.Background()
	for name, err := range map[string]error{
		"press":     b.Press(ctx, "main", Press{MessageID: "m1", User: alice, Data: "d"}),
		"thread":    b.ThreadMessage(ctx, "main", ThreadMessage{MessageID: "m1", User: alice, Text: "t"}),
		"deleted":   b.MessageDeleted(ctx, "main", MessageDeleted{MessageID: "m1"}),
		"direct":    b.DirectMessage(ctx, "main", DirectMessage{User: alice, Text: linkToken}),
		"messenger": nil,
	} {
		if name != "messenger" && !errors.Is(err, ErrNotOffered) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// sentID is the notification_id of an answer of sendNotification.
func sentID(t testing.TB, rec fmt.Stringer) string {
	t.Helper()
	m := decodeJSON(t, []byte(rec.String()))
	id, _ := m["notification_id"].(string)
	return id
}

func TestAcknowledgeQueuesTheEvent(t *testing.T) {
	b, _, _, _ := newGatewayBridge(t, nil)
	rec := do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyBody)
	id := sentID(t, rec.Body)
	if err := b.Acknowledge(context.Background(), "main", id); err != nil {
		t.Fatal(err)
	}
	if err := b.Acknowledge(context.Background(), "main", id); err != nil {
		t.Fatalf("the second acknowledgement: %v", err)
	}
	got := poll(t, b, tokenMain, "?wait=0")
	if len(got.Raw) != 1 {
		t.Fatalf("events %v", got.Raw)
	}
	ev := decodeJSON(t, got.Raw[0])
	if ev["type"] != "notification_acknowledged" || ev["notification_id"] != id || ev["data"] != "ack-1" ||
		ev["id"] != eventIDOf("main", EventTypeNotificationAcknowledged, id) || ev["occurred_at"] != "2026-10-18T10:00:00Z" {
		t.Fatalf("event %v", ev)
	}
	if other := poll(t, b, tokenOther, "?wait=0"); len(other.Raw) != 0 {
		t.Fatalf("another bot got %v", other.Raw)
	}
}

func TestAcknowledgeRefusals(t *testing.T) {
	b, _, clk, _ := newGatewayBridge(t, nil)
	ctx := context.Background()
	withAction := sentID(t, do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyBody).Body)
	without := sentID(t, do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k2"), notifyNoAction).Body)
	for name, tc := range map[string]struct {
		bot, id string
		want    error
	}{
		"unknown id":         {"main", "n99", ErrUnknownNotification},
		"no action":          {"main", without, ErrNoAction},
		"another bot's":      {"other", withAction, ErrUnknownNotification},
		"unknown bot":        {"nobody", withAction, ErrUnknownBot},
		"empty id":           {"main", "", ErrInvalidEvent},
		"id over 512":        {"main", strings.Repeat("n", 513), ErrInvalidEvent},
		"an idempotency key": {"main", "k1", ErrUnknownNotification},
	} {
		if err := b.Acknowledge(ctx, tc.bot, tc.id); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if got := poll(t, b, tokenMain, "?wait=0"); len(got.Raw) != 0 {
		t.Fatalf("a refused acknowledgement queued %v", got.Raw)
	}
	// Notifications are kept for 24 hours.
	clk.Advance(25 * time.Hour)
	if err := b.Acknowledge(ctx, "main", withAction); !errors.Is(err, ErrUnknownNotification) {
		t.Fatalf("after 24 hours: %v", err)
	}
}

func TestAcknowledgeNeedsTheRole(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	if err := b.Acknowledge(context.Background(), "main", "n1"); !errors.Is(err, ErrNotOffered) {
		t.Fatalf("Acknowledge on a messenger = %v", err)
	}
}

func TestAcknowledgeStoreFailures(t *testing.T) {
	store := newFailingStore()
	b, _, _, logs := newGatewayBridge(t, func(c *Config) { c.Store = store })
	store.fail("SaveKey", errors.New("down"))
	rec := do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyBody)
	if rec.Code != http.StatusCreated || !strings.Contains(logs.String(), "cannot keep the action of a notification") {
		t.Fatalf("answered %d %s, log %s", rec.Code, rec.Body, logs)
	}
	store.fail("SaveKey", nil)
	if err := b.Acknowledge(context.Background(), "main", "n1"); !errors.Is(err, ErrUnknownNotification) {
		t.Fatalf("an action that was not kept: %v", err)
	}
	do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k2"), notifyBody)
	store.fail("ClaimKey", errors.New("down"))
	if err := b.Acknowledge(context.Background(), "main", "n2"); err == nil || errors.Is(err, ErrUnknownNotification) {
		t.Fatalf("a failing lookup = %v", err)
	}
	store.fail("ClaimKey", nil)
	store.fail("ReleaseKey", errors.New("down"))
	if err := b.Acknowledge(context.Background(), "main", "n99"); err == nil || errors.Is(err, ErrUnknownNotification) {
		t.Fatalf("a failing release = %v", err)
	}
	store.fail("ReleaseKey", nil)
	store.fail("AppendEvent", errors.New("down"))
	if err := b.Acknowledge(context.Background(), "main", "n2"); err == nil {
		t.Fatal("a failing queue was not reported")
	}
	store.claimAs(&KeyClaim{State: KeyRunning})
	if err := b.Acknowledge(context.Background(), "main", "n2"); !errors.Is(err, ErrUnknownNotification) {
		t.Fatalf("a running claim = %v", err)
	}
	store.claimAs(&KeyClaim{State: KeyAnswered, Answer: StoredAnswer{Status: 201, Body: []byte("not json")}})
	if err := b.Acknowledge(context.Background(), "main", "n2"); err == nil {
		t.Fatal("a stored record that is not JSON was accepted")
	}
}

func TestAnswerEventReachesTheGateway(t *testing.T) {
	b, gw, _, _ := newGatewayBridge(t, nil)
	sent := sentID(t, do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyBody).Body)
	if err := b.Acknowledge(context.Background(), "main", sent); err != nil {
		t.Fatal(err)
	}
	id := eventIDOf("main", EventTypeNotificationAcknowledged, sent)
	rec := do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("a1"), answerBody(id))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"replayed":false,"shown_as":"notification"}` {
		t.Fatalf("answered %d %s", rec.Code, rec.Body)
	}
	calls := gw.recorded()
	got := calls[len(calls)-1]
	if got.method != "Answer" || got.bot != "main" || got.text != "Done" || got.event.ID != id ||
		got.event.Type != EventTypeNotificationAcknowledged || got.event.NotificationID != sent ||
		got.event.Data != "ack-1" || got.event.OccurredAt.IsZero() {
		t.Fatalf("the gateway got %+v", got)
	}
	again := do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("a1"), answerBody(id))
	if decodeJSON(t, again.Body.Bytes())["replayed"] != true || len(gw.recorded()) != len(calls) {
		t.Fatalf("the answer was not replayed: %s", again.Body)
	}
	gw.err = ForbiddenError("no")
	wantProblem(t, do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("a2"), answerBody(id)),
		http.StatusInternalServerError, ClassTransient)
}

func TestAnswerEventRoutesByRole(t *testing.T) {
	gw := &fakeGateway{}
	b, m, _, _ := newTestBridge(t, func(c *Config) { c.Roles, c.Gateway = []Role{RoleMessenger, RoleGateway}, gw })
	sent := sentID(t, do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyBody).Body)
	if err := b.Acknowledge(context.Background(), "main", sent); err != nil {
		t.Fatal(err)
	}
	pressed := reportPress(t, b, "u1")
	rec := do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("a1"),
		answerBody(eventIDOf("main", EventTypeNotificationAcknowledged, sent)))
	rec2 := do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("a2"), answerBody(pressed))
	if rec.Code != 200 || rec2.Code != 200 || m.count("AnswerEvent") != 1 || len(gw.recorded()) != 2 {
		t.Fatalf("answers %d %d, messenger %d, gateway %d", rec.Code, rec2.Code, m.count("AnswerEvent"),
			len(gw.recorded()))
	}
}

func TestAnswerEventForAnotherRoleIsGone(t *testing.T) {
	b, gw, _, _ := newGatewayBridge(t, nil)
	// Events of the other role cannot be reported on this bridge, so the store is given one directly.
	store := b.store
	now := b.clock.Now()
	if _, err := store.AppendEvent(context.Background(), "main", StoredEvent{ID: "e1", Type: EventTypePress,
		Body: []byte(`{"id":"e1","type":"press"}`), Until: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	wantProblem(t, do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("a1"), answerBody("e1")),
		http.StatusNotFound, ClassGone)
	if len(gw.recorded()) != 0 {
		t.Fatal("the gateway was called")
	}
}

func TestAnswerReachesTheGatewayWhilePushDelivers(t *testing.T) {
	clk := newFakeClock()
	rc := newCallback(t, clk, testSecret)
	var b *Bridge
	answered := make(chan int, 1)
	rc.statusOf = func(id string) int {
		// Muster answers a pushed event before it answers the push.
		rec := do(t, b.Handler(), http.MethodPost, "/v1/answers", tokenMain, key("a-"+id), answerBody(id))
		answered <- rec.Code
		return http.StatusNoContent
	}
	var gw *fakeGateway
	b, gw, _, _ = newGatewayBridge(t, func(c *Config) { c.clock = clk; c.Bots[0].Push = rc.target() })
	sent := sentID(t, do(t, b.Handler(), http.MethodPost, "/v1/notifications", tokenMain, key("k1"), notifyBody).Body)
	if err := b.Acknowledge(context.Background(), "main", sent); err != nil {
		t.Fatal(err)
	}
	p := rc.next()
	if ev := decodeJSON(t, p.body); ev["type"] != "notification_acknowledged" || ev["data"] != "ack-1" {
		t.Fatalf("pushed %s", p.body)
	}
	if code := <-answered; code != http.StatusOK {
		t.Fatalf("the answer during the push was %d", code)
	}
	if calls := gw.recorded(); calls[len(calls)-1].method != "Answer" {
		t.Fatalf("the gateway was not answered: %+v", calls)
	}
}
