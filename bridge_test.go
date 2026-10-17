// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is the library's clock in tests: time moves only with Advance, which fires the due timers.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*fakeTimer
	changed chan struct{}
}

type fakeTimer struct {
	at time.Time
	d  time.Duration
	ch chan time.Time
}

var testStart = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)

func newFakeClock() *fakeClock { return &fakeClock{now: testStart, changed: make(chan struct{})} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Timer(d time.Duration) (<-chan time.Time, func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), d: d, ch: make(chan time.Time, 1)}
	if d <= 0 {
		t.ch <- c.now
		return t.ch, func() bool { return false }
	}
	c.timers = append(c.timers, t)
	c.signal()
	return t.ch, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		i := slices.Index(c.timers, t)
		if i < 0 {
			return false
		}
		c.timers = slices.Delete(c.timers, i, i+1)
		c.signal()
		return true
	}
}

// Advance moves the clock and fires every timer that is due, the earliest first.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	slices.SortStableFunc(c.timers, func(a, b *fakeTimer) int { return a.at.Compare(b.at) })
	var pending []*fakeTimer
	for _, t := range c.timers {
		if t.at.After(c.now) {
			pending = append(pending, t)
			continue
		}
		t.ch <- t.at
	}
	c.timers = pending
	c.signal()
}

func (c *fakeClock) signal() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// awaitTimer blocks until a timer of duration d is pending. The 10 s are a guard against a hang, not a wait the test
// relies on.
func (c *fakeClock) awaitTimer(t testing.TB, d time.Duration) {
	t.Helper()
	guard := time.NewTimer(10 * time.Second)
	defer guard.Stop()
	for {
		c.mu.Lock()
		found := slices.ContainsFunc(c.timers, func(x *fakeTimer) bool { return x.d == d })
		changed := c.changed
		c.mu.Unlock()
		if found {
			return
		}
		select {
		case <-changed:
		case <-guard.C:
			t.Fatalf("no timer of %s within 10 s", d)
		}
	}
}

// awaitNoTimers blocks until no timer is pending; the 10 s are a guard.
func (c *fakeClock) awaitNoTimers(t testing.TB) {
	t.Helper()
	guard := time.NewTimer(10 * time.Second)
	defer guard.Stop()
	for {
		c.mu.Lock()
		n, changed := len(c.timers), c.changed
		c.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-changed:
		case <-guard.C:
			t.Fatalf("%d timers still pending after 10 s", n)
		}
	}
}

func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// fakeMessenger implements every interface of a Messenger; each call is recorded and answered by its func, or by a
// default answer.
type fakeMessenger struct {
	mu    sync.Mutex
	calls []fakeCall

	post   func(bot string, in MessageInput) (MessagePosted, error)
	edit   func(bot, id string, in MessageContent) (MessageEdited, error)
	reply  func(bot, id string, in ThreadReplyInput) (ThreadReplyPosted, error)
	check  func(bot, id string) (string, error)
	answer func(bot string, ev ReportedEvent, text string) (AnswerShownShownAs, error)
	find   func(bot, handle string) (*Person, error)
	dm     func(bot string, in DirectMessageInput) (DirectMessageSent, error)
	del    func(bot, id string) error

	// err, when set, is the error of every call.
	err error
	// entered receives the method of every call, and block, when set, holds every call until it is closed.
	entered chan string
	block   chan struct{}
}

type fakeCall struct {
	method string
	bot    string
	key    string
	id     string
	event  ReportedEvent
	text   string
	in     any
}

func (m *fakeMessenger) record(ctx context.Context, c fakeCall) {
	c.key = IdempotencyKeyFrom(ctx)
	m.mu.Lock()
	m.calls = append(m.calls, c)
	entered, block := m.entered, m.block
	m.mu.Unlock()
	if entered != nil {
		entered <- c.method
	}
	if block != nil {
		<-block
	}
}

func (m *fakeMessenger) recorded() []fakeCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

func (m *fakeMessenger) count(method string) int {
	n := 0
	for _, c := range m.recorded() {
		if c.method == method {
			n++
		}
	}
	return n
}

func (m *fakeMessenger) PostMessage(ctx context.Context, bot string, in MessageInput) (MessagePosted, error) {
	m.record(ctx, fakeCall{method: "PostMessage", bot: bot, in: in})
	if m.err != nil {
		return MessagePosted{}, m.err
	}
	if m.post != nil {
		return m.post(bot, in)
	}
	return MessagePosted{MessageId: "m1"}, nil
}

func (m *fakeMessenger) EditMessage(ctx context.Context, bot, id string, in MessageContent) (MessageEdited, error) {
	m.record(ctx, fakeCall{method: "EditMessage", bot: bot, id: id, in: in})
	if m.err != nil {
		return MessageEdited{}, m.err
	}
	if m.edit != nil {
		return m.edit(bot, id, in)
	}
	return MessageEdited{}, nil
}

func (m *fakeMessenger) ReplyInThread(ctx context.Context, bot, id string, in ThreadReplyInput) (ThreadReplyPosted,
	error,
) {
	m.record(ctx, fakeCall{method: "ReplyInThread", bot: bot, id: id, in: in})
	if m.err != nil {
		return ThreadReplyPosted{}, m.err
	}
	if m.reply != nil {
		return m.reply(bot, id, in)
	}
	return ThreadReplyPosted{MessageId: "r1", ThreadRef: "t-" + id}, nil
}

func (m *fakeMessenger) CheckChannel(ctx context.Context, bot, id string) (string, error) {
	m.record(ctx, fakeCall{method: "CheckChannel", bot: bot, id: id})
	if m.err != nil {
		return "", m.err
	}
	if m.check != nil {
		return m.check(bot, id)
	}
	return "Alerts", nil
}

func (m *fakeMessenger) AnswerEvent(ctx context.Context, bot string, ev ReportedEvent, text string) (AnswerShownShownAs,
	error,
) {
	m.record(ctx, fakeCall{method: "AnswerEvent", bot: bot, event: ev, text: text})
	if m.err != nil {
		return "", m.err
	}
	if m.answer != nil {
		return m.answer(bot, ev, text)
	}
	return AnswerShownShownAsThread, nil
}

func (m *fakeMessenger) FindUser(ctx context.Context, bot, handle string) (*Person, error) {
	m.record(ctx, fakeCall{method: "FindUser", bot: bot, id: handle})
	if m.err != nil {
		return nil, m.err
	}
	if m.find != nil {
		return m.find(bot, handle)
	}
	if handle == "alice" {
		return &Person{ExternalId: "u-alice", DisplayName: "Alice Smith", Handle: new("alice")}, nil
	}
	return nil, nil
}

func (m *fakeMessenger) SendDirectMessage(ctx context.Context, bot string, in DirectMessageInput) (DirectMessageSent,
	error,
) {
	m.record(ctx, fakeCall{method: "SendDirectMessage", bot: bot, in: in})
	if m.err != nil {
		return DirectMessageSent{}, m.err
	}
	if m.dm != nil {
		return m.dm(bot, in)
	}
	return DirectMessageSent{MessageId: "d1"}, nil
}

func (m *fakeMessenger) DeleteMessage(ctx context.Context, bot, id string) error {
	m.record(ctx, fakeCall{method: "DeleteMessage", bot: bot, id: id})
	if m.err != nil {
		return m.err
	}
	if m.del != nil {
		return m.del(bot, id)
	}
	return nil
}

// onlyMessenger has the methods of Messenger and no other.
type onlyMessenger struct{ Messenger }

// logSink collects the log lines of a test bridge, which handlers and pushers write from their own goroutines.
type logSink struct {
	sync.Mutex
	text strings.Builder
}

func (l *logSink) Write(line []byte) (int, error) {
	l.Lock()
	n, err := l.text.Write(line)
	l.Unlock()
	return n, err
}

func (l *logSink) String() string {
	l.Lock()
	text := l.text.String()
	l.Unlock()
	return text
}

const (
	tokenMain  = "token-main"
	tokenOther = "token-other"
)

// testSecret is a push secret: whsec_ and the base64 of 32 bytes.
var testSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func allCapabilities() Capabilities {
	return Capabilities{
		Buttons: &Capabilities_Buttons{MaxPerMessage: 3, MaxPerRow: 3, DataMaxBytes: 64,
			Styles: []ButtonStyle{ButtonStylePrimary, ButtonStyleDanger}},
		ThreadMessages: new(CapabilitiesThreadMessagesAll),
		DirectMessages: &Capabilities_DirectMessages{BotCanStart: true},
		PrivateAnswers: &[]CapabilitiesPrivateAnswers{CapabilitiesPrivateAnswersEphemeral},
		NativeMentions: new(true),
		QuietMessages:  new(true),
		StatusColor:    new(true),
		Delete:         &Capabilities_Delete{},
		DeletionEvents: new(true),
	}
}

func testConfig(clk *fakeClock, logs io.Writer) Config {
	return Config{
		Name:          "test bridge",
		Version:       "1.0.0",
		MessengerName: "Test",
		Bots:          []Bot{{Name: "main", Token: tokenMain}, {Name: "other", Token: tokenOther}},
		Capabilities:  allCapabilities(),
		Limits:        Limits{MaxBodyLength: 500},
		UserLookup:    InfoUserLookupLogin,
		Logger:        slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		clock:         clk,
	}
}

// newTestBridge is a bridge of two bots, main and other, with every feature, on a fake clock, logging into a buffer.
func newTestBridge(t testing.TB, mutate func(*Config)) (*Bridge, *fakeMessenger, *fakeClock, *logSink) {
	t.Helper()
	clk, logs, m := newFakeClock(), &logSink{}, &fakeMessenger{}
	cfg := testConfig(clk, logs)
	if mutate != nil {
		mutate(&cfg)
	}
	b, err := New(cfg, m)
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
	return b, m, clk, logs
}

// do sends a request to the bridge's handler with the token, as JSON when it has a body.
func do(t testing.TB, h http.Handler, method, path, token string, header http.Header, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for k, vs := range header {
		r.Header[k] = vs
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func key(k string) http.Header { return http.Header{"Idempotency-Key": {k}} }

// decodeProblem decodes a problem answer.
func decodeProblem(t testing.TB, rec *httptest.ResponseRecorder) Problem {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json; body %s", ct, rec.Body)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem: %v", err)
	}
	return p
}

// wantProblem checks the status and the class of a problem answer and returns it.
func wantProblem(t testing.TB, rec *httptest.ResponseRecorder, status int, class Class) Problem {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body)
	}
	p := decodeProblem(t, rec)
	if p.Class == nil || Class(*p.Class) != class {
		t.Fatalf("class = %v, want %s; body %s", p.Class, class, rec.Body)
	}
	if p.Status == nil || *p.Status != status {
		t.Fatalf("problem status = %v, want %d", p.Status, status)
	}
	return p
}

func detailOf(p Problem) string {
	if p.Detail == nil {
		return ""
	}
	return *p.Detail
}

func decodeJSON(t testing.TB, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return m
}

func TestNewBuildsInfo(t *testing.T) {
	m := &fakeMessenger{}
	b, err := New(Config{Name: "n", Version: "v", MessengerName: "Chat", Bots: []Bot{{Name: "main", Token: "t"}},
		Limits: Limits{MaxBodyLength: 4000}, clock: newFakeClock()}, onlyMessenger{m})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close(context.Background()) }()
	rec := do(t, b.Handler(), http.MethodGet, "/v1/info", "t", nil, "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("info answered %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	want := `{"bridge":{"name":"n","version":"v"},"capabilities":{},"event_delivery":["poll","push"],` +
		`"limits":{"edit_max_age_seconds":null,"max_body_length":4000},"messenger":{"name":"Chat"},` +
		`"protocol_version":"1","user_lookup":"none"}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("info =\n%s\nwant\n%s", got, want)
	}
}

func TestNewInfoWithEveryMember(t *testing.T) {
	b, _, _, _ := newTestBridge(t, func(c *Config) {
		c.LinkURLTemplate = "https://chat.example.org/bot?start={token}"
		c.EventDelivery = []InfoEventDelivery{InfoEventDeliveryPush}
		c.Limits.EditMaxAgeSeconds = new(3600)
		c.Limits.SendPerSecond = new(float32(30))
	})
	info := decodeJSON(t, do(t, b.Handler(), http.MethodGet, "/v1/info", tokenMain, nil, "").Body.Bytes())
	if info["link_url_template"] != "https://chat.example.org/bot?start={token}" ||
		info["user_lookup"] != "login" || len(info["event_delivery"].([]any)) != 1 {
		t.Fatalf("info = %v", info)
	}
	caps := info["capabilities"].(map[string]any)
	if caps["delete"].(map[string]any)["max_age_seconds"] != nil || caps["deletion_events"] != true {
		t.Fatalf("capabilities = %v", caps)
	}
}

func TestNewCopiesTheConfig(t *testing.T) {
	clk, logs, m := newFakeClock(), &logSink{}, &fakeMessenger{}
	cfg := testConfig(clk, logs)
	b, err := New(cfg, m)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close(context.Background()) }()
	cfg.Capabilities.Buttons.MaxPerMessage = 99
	cfg.Bots[0].Token = "changed"
	if b.info.Capabilities.Buttons.MaxPerMessage != 3 {
		t.Fatal("a change of the Config after New changed the bridge")
	}
	if rec := do(t, b.Handler(), http.MethodGet, "/v1/info", tokenMain, nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("the original token answered %d", rec.Code)
	}
}

func TestNewRefusesBots(t *testing.T) {
	tests := []struct {
		name string
		bots []Bot
		want []string
	}{
		{"no bot", nil, []string{"bots is empty"}},
		{"empty name", []Bot{{Name: "", Token: "t"}}, []string{"bots[0].name is empty"}},
		{"name with spaces", []Bot{{Name: " a", Token: "t"}}, []string{"bots[0].name has white space around it"}},
		{"empty token", []Bot{{Name: "a", Token: ""}}, []string{"bots[0].token is empty"}},
		{"token with spaces", []Bot{{Name: "a", Token: "t\n"}}, []string{"bots[0].token has white space around it"}},
		{"same name", []Bot{{Name: "a", Token: "t1"}, {Name: "a", Token: "t2"}},
			[]string{`bots[1].name "a" is the name of bots[0] too`}},
		{"same token", []Bot{{Name: "a", Token: "same-token"}, {Name: "b", Token: "same-token"}},
			[]string{"bots[1].token is the token of bots[0] too"}},
		{"two problems", []Bot{{Name: "", Token: ""}}, []string{"bots[0].name is empty", "bots[0].token is empty"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(newFakeClock(), io.Discard)
			cfg.Bots = tt.bots
			_, err := New(cfg, &fakeMessenger{})
			if err == nil {
				t.Fatal("New accepted the bots")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not say %q", err, w)
				}
			}
			if strings.Contains(err.Error(), "same-token") {
				t.Errorf("error %q carries the token", err)
			}
		})
	}
}

func TestNewRefusesFeaturesWithoutInterfaces(t *testing.T) {
	cfg := testConfig(newFakeClock(), io.Discard)
	_, err := New(cfg, onlyMessenger{&fakeMessenger{}})
	if err == nil {
		t.Fatal("New accepted features without their interfaces")
	}
	for _, w := range []string{
		`user_lookup "login" needs a Messenger that implements UserFinder`,
		"capabilities.direct_messages needs a Messenger that implements DirectMessenger",
		"capabilities.delete needs a Messenger that implements Deleter",
	} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not say %q", err, w)
		}
	}
	cfg.UserLookup, cfg.Capabilities.DirectMessages, cfg.Capabilities.Delete = InfoUserLookupNone, nil, nil
	b, err := New(cfg, onlyMessenger{&fakeMessenger{}})
	if err != nil {
		t.Fatalf("New refused a bridge without those features: %v", err)
	}
	_ = b.Close(context.Background())
}

func TestNewRefusesNilMessenger(t *testing.T) {
	if _, err := New(testConfig(newFakeClock(), io.Discard), nil); err == nil ||
		!strings.Contains(err.Error(), "the Messenger is nil") {
		t.Fatalf("New(nil) = %v", err)
	}
}

func TestNewRefusesInfoAgainstSpec(t *testing.T) {
	long := strings.Repeat("x", 101)
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"empty name", func(c *Config) { c.Name = "" }, "/bridge/name: minimum string length is 1"},
		{"long name", func(c *Config) { c.Name = long }, "/bridge/name: maximum string length is 100"},
		{"empty version", func(c *Config) { c.Version = "" }, "/bridge/version"},
		{"empty messenger", func(c *Config) { c.MessengerName = "" }, "/messenger/name"},
		{"max_body_length 499", func(c *Config) { c.Limits.MaxBodyLength = 499 },
			"/limits/max_body_length: number must be at least 500"},
		{"max_body_length 100001", func(c *Config) { c.Limits.MaxBodyLength = 100001 },
			"limits.max_body_length is above 100000, the most this library accepts"},
		{"edit_max_age_seconds 299", func(c *Config) { c.Limits.EditMaxAgeSeconds = new(299) },
			"/limits/edit_max_age_seconds: number must be at least 300"},
		{"send_per_second 0", func(c *Config) { c.Limits.SendPerSecond = new(float32(0)) },
			"/limits/send_per_second"},
		{"data_max_bytes 63", func(c *Config) { c.Capabilities.Buttons.DataMaxBytes = 63 },
			"/capabilities/buttons/data_max_bytes: number must be at least 64"},
		{"max_per_message 1", func(c *Config) { c.Capabilities.Buttons.MaxPerMessage = 1 },
			"/capabilities/buttons/max_per_message: number must be at least 2"},
		{"max_per_row 0", func(c *Config) { c.Capabilities.Buttons.MaxPerRow = 0 },
			"/capabilities/buttons/max_per_row: number must be at least 1"},
		{"duplicate style", func(c *Config) {
			c.Capabilities.Buttons.Styles = []ButtonStyle{ButtonStylePrimary, ButtonStylePrimary}
		}, "/capabilities/buttons/styles: duplicate items found"},
		{"unknown style", func(c *Config) { c.Capabilities.Buttons.Styles = []ButtonStyle{"pink"} },
			"/capabilities/buttons/styles/0: value is not one of the allowed values"},
		{"delete max_age_seconds 0", func(c *Config) { c.Capabilities.Delete.MaxAgeSeconds = new(0) },
			"/capabilities/delete/max_age_seconds: number must be at least 1"},
		{"empty event_delivery", func(c *Config) { c.EventDelivery = []InfoEventDelivery{} },
			"/event_delivery: minimum number of items is 1"},
		{"duplicate event_delivery", func(c *Config) {
			c.EventDelivery = []InfoEventDelivery{InfoEventDeliveryPoll, InfoEventDeliveryPoll}
		}, "/event_delivery: duplicate items found"},
		{"link without http", func(c *Config) { c.LinkURLTemplate = "ftp://x/{token}" }, "/link_url_template"},
		{"link without token", func(c *Config) { c.LinkURLTemplate = "https://x/start" }, "/link_url_template"},
		{"link too long", func(c *Config) {
			c.LinkURLTemplate = "https://x/{token}" + strings.Repeat("a", 1010)
		}, "/link_url_template: maximum string length is 1024"},
		{"unknown user_lookup", func(c *Config) { c.UserLookup = "phone" }, "/user_lookup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(newFakeClock(), io.Discard)
			tt.mutate(&cfg)
			_, err := New(cfg, &fakeMessenger{})
			if err == nil {
				t.Fatal("New accepted the configuration")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not say %q", err, tt.want)
			}
		})
	}
}

// The specification requires capabilities.buttons.styles; a Config without them offers no style shown as such.
func TestNewDefaultsButtonStyles(t *testing.T) {
	b, _, _, _ := newTestBridge(t, func(c *Config) {
		c.Capabilities.Buttons = &Capabilities_Buttons{MaxPerMessage: 2, MaxPerRow: 2, DataMaxBytes: 64}
	})
	info := decodeJSON(t, do(t, b.Handler(), http.MethodGet, "/v1/info", tokenMain, nil, "").Body.Bytes())
	buttons := info["capabilities"].(map[string]any)["buttons"].(map[string]any)
	if styles, ok := buttons["styles"].([]any); !ok || len(styles) != 0 {
		t.Fatalf("buttons = %v, want styles []", buttons)
	}
	cfg := Config{Name: "n", Version: "v", MessengerName: "m", Bots: []Bot{{Name: "main", Token: "t"}},
		Limits: Limits{MaxBodyLength: 500}, Capabilities: Capabilities{Buttons: &Capabilities_Buttons{MaxPerMessage: 2,
			MaxPerRow: 2, DataMaxBytes: 64}}, clock: newFakeClock()}
	other, err := New(cfg, onlyMessenger{&fakeMessenger{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close(context.Background()) }()
	if cfg.Capabilities.Buttons.Styles != nil {
		t.Fatal("New changed the caller's Capabilities_Buttons")
	}
}

func TestNewRefusesPushTargets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"relative url", func(c *Config) { c.Bots[0].Push = &PushTarget{URL: "/callback", Secret: testSecret} },
			"bots[0].push: the url needs the http or https scheme, a host and no user information"},
		{"ftp url", func(c *Config) { c.Bots[0].Push = &PushTarget{URL: "ftp://muster/cb", Secret: testSecret} },
			"bots[0].push: the url"},
		{"user info", func(c *Config) {
			c.Bots[0].Push = &PushTarget{URL: "https://u:p@muster/cb", Secret: testSecret}
		}, "bots[0].push: the url"},
		{"no prefix", func(c *Config) {
			c.Bots[0].Push = &PushTarget{URL: "https://muster/cb", Secret: strings.TrimPrefix(testSecret, "whsec_")}
		}, "bots[0].push: the secret needs the whsec_ prefix and then a non-empty key in standard base64"},
		{"not base64", func(c *Config) {
			c.Bots[0].Push = &PushTarget{URL: "https://muster/cb", Secret: "whsec_not base64!"}
		}, "bots[0].push: the secret"},
		{"empty key", func(c *Config) { c.Bots[0].Push = &PushTarget{URL: "https://muster/cb", Secret: "whsec_"} },
			"bots[0].push: the secret"},
		{"poll only", func(c *Config) {
			c.EventDelivery = []InfoEventDelivery{InfoEventDeliveryPoll}
			c.Bots[1].Push = &PushTarget{URL: "https://muster/cb", Secret: testSecret}
		}, "bots[1].push is set, but event_delivery does not offer push"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(newFakeClock(), io.Discard)
			tt.mutate(&cfg)
			_, err := New(cfg, &fakeMessenger{})
			if err == nil {
				t.Fatal("New accepted the push target")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not say %q", err, tt.want)
			}
			secret := strings.TrimPrefix(testSecret, "whsec_")
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "not base64!") {
				t.Fatalf("error %q carries the secret", err)
			}
		})
	}
}

func TestNewAcceptsMinimalConfig(t *testing.T) {
	b, err := New(Config{Name: "n", Version: "v", MessengerName: "m", Bots: []Bot{{Name: "main", Token: "t"}},
		Limits: Limits{MaxBodyLength: 500}}, onlyMessenger{&fakeMessenger{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.store.(*memoryStore); !ok || b.log != slog.Default() {
		t.Fatal("New did not default the store and the logger")
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewRefusesUnencodableInfo(t *testing.T) {
	cfg := testConfig(newFakeClock(), io.Discard)
	nan := float32(0)
	nan /= nan
	cfg.Limits.SendPerSecond = &nan
	if _, err := New(cfg, &fakeMessenger{}); err == nil || !strings.Contains(err.Error(), "cannot be encoded") {
		t.Fatalf("New = %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	for range 2 {
		if err := b.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.SetPush("main", &PushTarget{URL: "https://muster/cb", Secret: testSecret}); !errors.Is(err, ErrClosed) {
		t.Fatalf("SetPush after Close = %v", err)
	}
	if rec := do(t, b.Handler(), http.MethodGet, "/v1/info", tokenMain, nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("the handler answered %d after Close", rec.Code)
	}
}

func TestRealClock(t *testing.T) {
	var c clock = realClock{}
	if c.Now().IsZero() {
		t.Fatal("the real clock has no time")
	}
	fired, stop := c.Timer(time.Nanosecond)
	<-fired
	stop()
}
