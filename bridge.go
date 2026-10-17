// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Messenger is what every bridge implements: the calls to its messenger. Each method is given the bot whose token
// Muster sent, by its name in Config.Bots, and returns the protocol's answer, or an error: an *Error to answer its
// class, any other error to answer transient with a generic detail. The library validates every request before it
// calls a method and every answer before it sends it.
type Messenger interface {
	// PostMessage posts a message to in.ChannelId: a Root message, a Storm summary or a test message.
	PostMessage(ctx context.Context, bot string, in MessageInput) (MessagePosted, error)
	// EditMessage replaces the whole message with in; editing to the state the message already shows is a success.
	EditMessage(ctx context.Context, bot, messageID string, in MessageContent) (MessageEdited, error)
	// ReplyInThread posts a reply in the Thread of the message. in.ThreadRef is the thread_ref Muster sent, nil for
	// none; the answer's ThreadRef is the one Muster keeps and sends with the next reply.
	ReplyInThread(ctx context.Context, bot, messageID string, in ThreadReplyInput) (ThreadReplyPosted, error)
	// CheckChannel posts nothing and returns the channel's name as people see it, or a forbidden *Error when the bot
	// cannot post, edit and reply there.
	CheckChannel(ctx context.Context, bot, channelID string) (string, error)
	// AnswerEvent shows text to the person behind the event, which the bridge reported, and returns where it was shown.
	AnswerEvent(ctx context.Context, bot string, event ReportedEvent, text string) (AnswerShownShownAs, error)
}

// UserFinder is the Messenger's lookup of people, which a UserLookup other than InfoUserLookupNone needs.
type UserFinder interface {
	// FindUser returns the one account with the handle, of the kind Config.UserLookup declares, or nil for nobody.
	FindUser(ctx context.Context, bot, handle string) (*Person, error)
}

// DirectMessenger is the Messenger's direct messages, which Capabilities.DirectMessages needs.
type DirectMessenger interface {
	// SendDirectMessage sends a direct message from the bot, opening the direct chat if needed.
	SendDirectMessage(ctx context.Context, bot string, in DirectMessageInput) (DirectMessageSent, error)
}

// Deleter is the Messenger's deletion of the bot's messages, which Capabilities.Delete needs.
type Deleter interface {
	// DeleteMessage deletes a message of the bot. A message that no longer exists is a success, also when this
	// returns a gone *Error.
	DeleteMessage(ctx context.Context, bot, messageID string) error
}

// Config is a bridge: what it tells Muster in info, its bots and where it keeps its state.
type Config struct {
	// Name and Version are the bridge's, as info shows them: 1 to 100 characters each.
	Name    string
	Version string
	// MessengerName is the messenger's name as people know it: 1 to 100 characters.
	MessengerName string
	// Bots are the bots the bridge serves, at least one; each is a Bridge Connection in Muster.
	Bots []Bot
	// Capabilities are the optional features; a nil member is a feature the bridge does not offer.
	Capabilities Capabilities
	// Limits are the bridge's limits; MaxBodyLength from 500 to 100000.
	Limits Limits
	// UserLookup is what findUsers searches by; "" is InfoUserLookupNone.
	UserLookup InfoUserLookup
	// EventDelivery are the event delivery modes the bridge offers; nil is both, poll and push.
	EventDelivery []InfoEventDelivery
	// LinkURLTemplate is an http or https link with {token} that hands the bot an Account link token; "" for none.
	LinkURLTemplate string
	// Store keeps the Idempotency-Keys and the events; nil is NewMemoryStore().
	Store Store
	// Logger receives the library's logs; nil is slog.Default().
	Logger *slog.Logger

	clock  clock
	random io.Reader
}

// Bot is one bot of the bridge.
type Bot struct {
	// Name names the bot in the Messenger calls and the reports, such as "main".
	Name string
	// Token is the token Muster sends for this bot, as saved on its Bridge Connection.
	Token string
	// Push is where the bot's events are pushed; nil for none.
	Push *PushTarget
}

// PushTarget is where the events of a bot are pushed, as the Bridge Connection in Muster shows it.
type PushTarget struct {
	// URL is the callback address: an absolute http or https URL.
	URL string
	// Secret is the push secret: whsec_ followed by standard base64.
	Secret string
}

// Bridge serves the Bridge protocol for the bots of its Config and calls the Messenger.
type Bridge struct {
	messenger Messenger
	finder    UserFinder
	dm        DirectMessenger
	deleter   Deleter
	store     Store
	log       *slog.Logger
	clock     clock
	random    io.Reader
	proto     *protocol
	info      Info
	infoBody  []byte
	bots      map[string]*botState
	botList   []*botState
	handler   http.Handler
	client    *http.Client

	redactor atomic.Pointer[strings.Replacer]
	tokens   []string

	mu      sync.Mutex
	secrets []string
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
	pushers sync.WaitGroup
}

type botState struct {
	name      string
	tokenHash [sha256.Size]byte

	mu       sync.Mutex
	changed  chan struct{}
	target   *pushTarget
	retarget chan struct{}
	pushing  bool
}

// pushTarget is a checked PushTarget with its decoded key.
type pushTarget struct {
	url string
	key []byte
}

var (
	// ErrUnknownBot is the error of a report or of SetPush for a bot that is not in Config.Bots.
	ErrUnknownBot = errors.New("bridge: no bot of that name")
	// ErrClosed is the error of SetPush after Close.
	ErrClosed = errors.New("bridge: closed")
)

const (
	maxBodyLengthCap = 100000
	pushSecretPrefix = "whsec_"
)

// New checks cfg and returns the bridge that serves it with m. It names every problem it finds: a bot without a name
// or a token of its own, a feature whose interface m lacks, a push target that is not valid, and the info that cfg
// makes when it does not match api/bridge.yaml. Bots with a push target start pushing at once.
func New(cfg Config, m Messenger) (*Bridge, error) {
	proto, err := loadProtocol()
	if err != nil {
		return nil, err
	}
	var errs []error
	if m == nil {
		errs = append(errs, errors.New("bridge: the Messenger is nil"))
	} else {
		errs = append(errs, checkFeatures(cfg, m)...)
	}
	errs = append(errs, checkBots(cfg)...)
	if cfg.Limits.MaxBodyLength > maxBodyLengthCap {
		errs = append(errs, fmt.Errorf("bridge: limits.max_body_length is above %d, the most this library accepts",
			maxBodyLengthCap))
	}
	infoBody, err := json.Marshal(buildInfo(cfg))
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("bridge: the info this configuration makes cannot be encoded: %w", err))
	default:
		if p := proto.check("Info", infoBody); p != "" {
			errs = append(errs, fmt.Errorf("bridge: the info this configuration makes does not match api/bridge.yaml: %s",
				p))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	b := &Bridge{
		messenger: m,
		store:     cfg.Store,
		log:       cfg.Logger,
		clock:     cfg.clock,
		random:    cfg.random,
		proto:     proto,
		infoBody:  infoBody,
		bots:      make(map[string]*botState, len(cfg.Bots)),
	}
	_ = json.Unmarshal(infoBody, &b.info) // a deep copy, so that the caller's Config cannot change the bridge
	b.finder, _ = m.(UserFinder)
	b.dm, _ = m.(DirectMessenger)
	b.deleter, _ = m.(Deleter)
	if b.store == nil {
		b.store = NewMemoryStore()
	}
	if b.log == nil {
		b.log = slog.Default()
	}
	if b.clock == nil {
		b.clock = realClock{}
	}
	if b.random == nil {
		b.random = rand.Reader
	}
	b.client = newPushClient()
	b.ctx, b.cancel = context.WithCancel(context.Background())
	for _, bot := range cfg.Bots {
		bs := &botState{name: bot.Name, tokenHash: sha256.Sum256([]byte(bot.Token)), changed: make(chan struct{}),
			retarget: make(chan struct{}, 1)}
		b.bots[bot.Name] = bs
		b.botList = append(b.botList, bs)
		b.tokens = append(b.tokens, bot.Token)
		if bot.Push != nil {
			bs.target, _ = checkPushTarget(bot.Push) // checked above
			b.secrets = append(b.secrets, bot.Push.Secret)
		}
	}
	b.rebuildRedactor()
	b.handler = b.newHandler()
	b.mu.Lock()
	for _, bs := range b.botList {
		if bs.target != nil {
			b.startPusher(bs)
		}
	}
	b.mu.Unlock()
	return b, nil
}

// Handler serves the protocol: every operation under /v1/. Mount it at the bridge's base URL, under a path prefix
// with http.StripPrefix.
func (b *Bridge) Handler() http.Handler { return b.handler }

// SetPush sets where the events of the bot are pushed, starting with the oldest event kept; nil stops pushing.
func (b *Bridge) SetPush(bot string, target *PushTarget) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	bs, ok := b.bots[bot]
	if !ok {
		return ErrUnknownBot
	}
	var t *pushTarget
	if target != nil {
		if !slices.Contains(b.info.EventDelivery, InfoEventDeliveryPush) {
			return errors.New("bridge: event_delivery does not offer push")
		}
		var problems []string
		if t, problems = checkPushTarget(target); len(problems) > 0 {
			return fmt.Errorf("bridge: the push target %s", strings.Join(problems, "; "))
		}
		b.secrets = append(b.secrets, target.Secret)
		b.rebuildRedactor()
	}
	bs.mu.Lock()
	bs.target = t
	bs.mu.Unlock()
	if t != nil && b.startPusher(bs) || !bs.pushing {
		return nil
	}
	select {
	case bs.retarget <- struct{}{}:
	default:
	}
	return nil
}

// Close stops the pushers and waits for them until ctx ends. The handler and the reports keep working; no event is
// pushed any more.
func (b *Bridge) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cancel()
	done := make(chan struct{})
	go func() {
		b.pushers.Wait()
		b.client.CloseIdleConnections()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func checkFeatures(cfg Config, m Messenger) []error {
	var errs []error
	if lookup := cfg.UserLookup; lookup != "" && lookup != InfoUserLookupNone {
		if _, ok := m.(UserFinder); !ok {
			errs = append(errs, fmt.Errorf("bridge: user_lookup %q needs a Messenger that implements UserFinder", lookup))
		}
	}
	if cfg.Capabilities.DirectMessages != nil {
		if _, ok := m.(DirectMessenger); !ok {
			errs = append(errs, errors.New("bridge: capabilities.direct_messages needs a Messenger that implements "+
				"DirectMessenger"))
		}
	}
	if cfg.Capabilities.Delete != nil {
		if _, ok := m.(Deleter); !ok {
			errs = append(errs, errors.New("bridge: capabilities.delete needs a Messenger that implements Deleter"))
		}
	}
	return errs
}

func checkBots(cfg Config) []error {
	if len(cfg.Bots) == 0 {
		return []error{errors.New("bridge: bots is empty: a bridge needs at least one bot")}
	}
	var errs []error
	pushOffered := cfg.EventDelivery == nil || slices.Contains(cfg.EventDelivery, InfoEventDeliveryPush)
	names := map[string]int{}
	tokens := map[string]int{}
	for i, bot := range cfg.Bots {
		switch {
		case bot.Name == "":
			errs = append(errs, fmt.Errorf("bridge: bots[%d].name is empty", i))
		case strings.TrimSpace(bot.Name) != bot.Name:
			errs = append(errs, fmt.Errorf("bridge: bots[%d].name has white space around it", i))
		default:
			if j, ok := names[bot.Name]; ok {
				errs = append(errs, fmt.Errorf("bridge: bots[%d].name %q is the name of bots[%d] too", i, bot.Name, j))
			} else {
				names[bot.Name] = i
			}
		}
		switch {
		case bot.Token == "":
			errs = append(errs, fmt.Errorf("bridge: bots[%d].token is empty", i))
		case strings.TrimSpace(bot.Token) != bot.Token:
			errs = append(errs, fmt.Errorf("bridge: bots[%d].token has white space around it", i))
		default:
			if j, ok := tokens[bot.Token]; ok {
				errs = append(errs, fmt.Errorf("bridge: bots[%d].token is the token of bots[%d] too", i, j))
			} else {
				tokens[bot.Token] = i
			}
		}
		if bot.Push == nil {
			continue
		}
		if !pushOffered {
			errs = append(errs, fmt.Errorf("bridge: bots[%d].push is set, but event_delivery does not offer push", i))
		}
		if _, problems := checkPushTarget(bot.Push); len(problems) > 0 {
			errs = append(errs, fmt.Errorf("bridge: bots[%d].push %s", i, strings.Join(problems, "; ")))
		}
	}
	return errs
}

// checkPushTarget is the target with its decoded key, or what is wrong with it, never with the secret.
func checkPushTarget(t *PushTarget) (*pushTarget, []string) {
	var problems []string
	u, err := url.Parse(t.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		problems = append(problems, "url is not an absolute http or https URL without user information")
	}
	rest, ok := strings.CutPrefix(t.Secret, pushSecretPrefix)
	key, err := base64.StdEncoding.DecodeString(rest)
	if !ok || err != nil || len(key) == 0 {
		problems = append(problems, "secret is not whsec_ followed by standard base64")
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return &pushTarget{url: t.URL, key: key}, nil
}

func buildInfo(cfg Config) Info {
	info := Info{
		ProtocolVersion: InfoProtocolVersionN1,
		Bridge:          Info_Bridge{Name: cfg.Name, Version: cfg.Version},
		Messenger:       Info_Messenger{Name: cfg.MessengerName},
		Capabilities:    cfg.Capabilities,
		Limits:          cfg.Limits,
		UserLookup:      cfg.UserLookup,
		EventDelivery:   cfg.EventDelivery,
	}
	if info.UserLookup == "" {
		info.UserLookup = InfoUserLookupNone
	}
	if info.EventDelivery == nil {
		info.EventDelivery = []InfoEventDelivery{InfoEventDeliveryPoll, InfoEventDeliveryPush}
	}
	if cfg.LinkURLTemplate != "" {
		link := cfg.LinkURLTemplate
		info.LinkUrlTemplate = &link
	}
	return info
}

// offers reports whether the bridge offers the event delivery mode.
func (b *Bridge) offers(mode InfoEventDelivery) bool {
	return slices.Contains(b.info.EventDelivery, mode)
}

// changes is closed by the next event queued for the bot.
func (bs *botState) changes() <-chan struct{} {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return bs.changed
}

func (bs *botState) notify() {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	close(bs.changed)
	bs.changed = make(chan struct{})
}

// rebuildRedactor makes the replacer of every token and push secret, in the forms a text may carry them, the longer
// first; b.mu is held, or b is not shared yet.
func (b *Bridge) rebuildRedactor() {
	var forms []string
	add := func(s string) {
		if s == "" {
			return
		}
		for _, f := range []string{s, url.PathEscape(s), url.QueryEscape(s)} {
			if !slices.Contains(forms, f) {
				forms = append(forms, f)
			}
		}
	}
	for _, t := range b.tokens {
		add(t)
	}
	for _, s := range b.secrets {
		add(s)
		add(strings.TrimPrefix(s, pushSecretPrefix))
	}
	slices.SortFunc(forms, func(x, y string) int { return len(y) - len(x) })
	pairs := make([]string, 0, 2*len(forms))
	for _, f := range forms {
		pairs = append(pairs, f, "[redacted]")
	}
	b.redactor.Store(strings.NewReplacer(pairs...))
}

// redact replaces the tokens, the push secrets and the extra secrets in s.
func (b *Bridge) redact(s string, extra ...string) string {
	if s == "" {
		return s
	}
	for _, e := range extra {
		if e != "" {
			s = strings.ReplaceAll(s, e, "[redacted]")
		}
	}
	return b.redactor.Load().Replace(s)
}

// clock is the library's time: the real clock, or a test's.
type clock interface {
	Now() time.Time
	// Timer fires once after d; the func stops it.
	Timer(d time.Duration) (<-chan time.Time, func())
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

func mustMarshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("bridge: cannot marshal %T: %v", v, err))
	}
	return data
}
