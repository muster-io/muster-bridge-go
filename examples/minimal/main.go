// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

// Command minimal is a bridge whose messenger lives in memory and writes everything the bot posts to its log, so that
// Muster, muster bridge check or a person with curl can try the Bridge protocol without a real messenger.
//
//	printf example-token > /tmp/bt
//	go run ./examples/minimal -token-file /tmp/bt
//
// It offers every feature, or with BRIDGE_FEATURES only those it lists, comma-separated: buttons, thread_messages,
// direct_messages (which also turns on user_lookup login), private_answers, native_mentions, quiet_messages,
// status_color, deletion_events and delete. Outside the protocol it serves POST /demo/press {"message_id", "user"},
// which presses the first button of the message as the person with that login. Nothing but the protocol's token
// guards it: it listens on loopback by default.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	bridge "github.com/muster-io/muster-bridge-go"
)

const botName = "main"

var featureNames = []string{"buttons", "thread_messages", "direct_messages", "private_answers", "native_mentions",
	"quiet_messages", "status_color", "deletion_events", "delete"}

func main() {
	listen := flag.String("listen", "127.0.0.1:18095", "the address to listen on")
	tokenFile := flag.String("token-file", "", "the file that holds the bot's token (required)")
	channels := flag.String("channels", "alerts,ops", "the channels the bot can use, comma-separated")
	users := flag.String("users", "alice:Alice Smith,bob:Bob Brown", "the people, as login:display name, comma-separated")
	pushURL := flag.String("push-url", "", "where to push the events, the callback URL of the Bridge Connection")
	pushSecretFile := flag.String("push-secret-file", "", "the file that holds the push secret, whsec_…")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, c, err := configure(*tokenFile, *channels, *users, *pushURL, *pushSecretFile, log)
	if err == nil {
		var b *bridge.Bridge
		if b, err = bridge.New(cfg, c); err == nil {
			err = serve(*listen, b, c, log)
		}
	}
	if err != nil {
		log.Error("minimal: stopped", "error", err)
		os.Exit(2)
	}
}

// configure builds the bridge's Config and its in-memory messenger from the flags and BRIDGE_FEATURES.
func configure(tokenFile, channels, users, pushURL, pushSecretFile string, log *slog.Logger) (bridge.Config, *chat,
	error) {
	on, err := features()
	if err != nil {
		return bridge.Config{}, nil, err
	}
	if tokenFile == "" {
		return bridge.Config{}, nil, errors.New("-token-file is required")
	}
	token, err := readSecret(tokenFile)
	if err != nil {
		return bridge.Config{}, nil, fmt.Errorf("-token-file: %w", err)
	}
	bot := bridge.Bot{Name: botName, Token: token}
	if (pushURL == "") != (pushSecretFile == "") {
		return bridge.Config{}, nil, errors.New("-push-url and -push-secret-file go together")
	}
	if pushURL != "" {
		secret, err := readSecret(pushSecretFile)
		if err != nil {
			return bridge.Config{}, nil, fmt.Errorf("-push-secret-file: %w", err)
		}
		bot.Push = &bridge.PushTarget{URL: pushURL, Secret: secret}
	}
	people, err := parseUsers(users)
	if err != nil {
		return bridge.Config{}, nil, err
	}

	cfg := bridge.Config{
		Name:          "muster-bridge-go minimal example",
		Version:       "0.0.0",
		MessengerName: "Minimal example",
		Bots:          []bridge.Bot{bot},
		Limits:        bridge.Limits{MaxBodyLength: 4000},
		UserLookup:    bridge.InfoUserLookupNone,
		Logger:        log,
	}
	caps := &cfg.Capabilities
	if on["buttons"] {
		caps.Buttons = &bridge.Capabilities_Buttons{MaxPerMessage: 5, MaxPerRow: 5, DataMaxBytes: 64,
			Styles: []bridge.ButtonStyle{bridge.ButtonStylePrimary, bridge.ButtonStyleSuccess,
				bridge.ButtonStyleNeutral, bridge.ButtonStyleDanger}}
	}
	if on["thread_messages"] {
		caps.ThreadMessages = new(bridge.CapabilitiesThreadMessagesAll)
	}
	if on["direct_messages"] {
		caps.DirectMessages = &bridge.Capabilities_DirectMessages{BotCanStart: true}
		cfg.UserLookup = bridge.InfoUserLookupLogin
	}
	if on["private_answers"] {
		caps.PrivateAnswers = &[]bridge.CapabilitiesPrivateAnswers{bridge.CapabilitiesPrivateAnswersEphemeral}
	}
	for name, field := range map[string]**bool{"native_mentions": &caps.NativeMentions,
		"quiet_messages": &caps.QuietMessages, "status_color": &caps.StatusColor,
		"deletion_events": &caps.DeletionEvents} {
		if on[name] {
			*field = new(true)
		}
	}
	if on["delete"] {
		caps.Delete = &bridge.Capabilities_Delete{}
	}

	c := &chat{log: log, idPrefix: strings.ToLower(rand.Text()[:6]), channels: map[string]bool{}, people: people,
		private: on["private_answers"], native: on["native_mentions"], messages: map[string]*message{}}
	for ch := range strings.SplitSeq(channels, ",") {
		if ch = strings.TrimSpace(ch); ch != "" {
			c.channels[ch] = true
		}
	}
	return cfg, c, nil
}

// features are the features BRIDGE_FEATURES lists, or all of them when it is not set.
func features() (map[string]bool, error) {
	on := map[string]bool{}
	list, set := os.LookupEnv("BRIDGE_FEATURES")
	if !set {
		for _, name := range featureNames {
			on[name] = true
		}
		return on, nil
	}
	for name := range strings.SplitSeq(list, ",") {
		name = strings.TrimSpace(name)
		switch {
		case name == "":
		case slices.Contains(featureNames, name):
			on[name] = true
		default:
			return nil, fmt.Errorf("BRIDGE_FEATURES: unknown feature %q; the features are %s", name,
				strings.Join(featureNames, ", "))
		}
	}
	return on, nil
}

// readSecret returns the token or push secret a file holds, without the white space around it.
func readSecret(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if secret := strings.TrimSpace(string(data)); secret != "" {
		return secret, nil
	}
	return "", errors.New("it holds no secret")
}

func parseUsers(list string) (map[string]bridge.Person, error) {
	people := map[string]bridge.Person{}
	for entry := range strings.SplitSeq(list, ",") {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		login, name, ok := strings.Cut(entry, ":")
		login, name = strings.TrimSpace(login), strings.TrimSpace(name)
		if !ok || login == "" || name == "" {
			return nil, fmt.Errorf("-users: %q is not login:display name", entry)
		}
		if _, dup := people[login]; dup {
			return nil, fmt.Errorf("-users: the login %q appears twice", login)
		}
		people[login] = bridge.Person{ExternalId: login, DisplayName: name, Handle: new(login)}
	}
	return people, nil
}

// serve serves the protocol and /demo/press until SIGINT or SIGTERM, then stops the server and the bridge's pushers.
func serve(addr string, b *bridge.Bridge, c *chat, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("/v1/", b.Handler())
	mux.HandleFunc("POST /demo/press", func(w http.ResponseWriter, r *http.Request) { c.demoPress(w, r, b) })

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	// No WriteTimeout: a long poll of /v1/events holds its answer up to 60 seconds, and a WriteTimeout shorter than
	// that, plus Muster's margin, would cut it off. A real bridge either sets none or one above 75 seconds.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("minimal: listening", "address", ln.Addr().String())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
	}
	return b.Close(shutdown)
}

// message is a message of the bot, or a reply in the Thread of its root.
type message struct {
	channel string
	root    string
	title   string
	body    string
	buttons []bridge.Button
	deleted bool
}

// chat is the in-memory messenger: it implements bridge.Messenger, bridge.UserFinder, bridge.DirectMessenger and
// bridge.Deleter. Every message it keeps is also written to the log, which stands in for the messenger's screen.
type chat struct {
	log *slog.Logger
	// idPrefix starts the ids of this process's messages, so that a restarted example never hands out an id that
	// Muster still holds for a message of the previous run: Muster's edits and presses of old ids find nothing.
	idPrefix string
	channels map[string]bool
	people   map[string]bridge.Person
	private  bool
	native   bool

	mu       sync.Mutex
	last     int
	lastDM   int
	messages map[string]*message
}

// chat is a Messenger, and offers every feature with UserFinder, DirectMessenger and Deleter.
var (
	_ bridge.Messenger       = (*chat)(nil)
	_ bridge.UserFinder      = (*chat)(nil)
	_ bridge.DirectMessenger = (*chat)(nil)
	_ bridge.Deleter         = (*chat)(nil)
)

var errNoMessage = bridge.GoneError("No message has this id.")

func (c *chat) PostMessage(_ context.Context, bot string, in bridge.MessageInput) (bridge.MessagePosted, error) {
	if !c.channels[in.ChannelId] {
		return bridge.MessagePosted{}, bridge.ForbiddenError("The bot has not joined this channel.")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID()
	c.messages[id] = &message{channel: in.ChannelId, title: in.Title, body: in.Body, buttons: in.Buttons}
	c.log.Info("minimal: message posted", "bot", bot, "message_id", id, "channel", in.ChannelId, "kind", in.Kind,
		"status", in.Status, "title", in.Title, "body", c.mentions(in.Body, in.Mentions), "buttons", labels(in.Buttons))
	return bridge.MessagePosted{MessageId: id}, nil
}

func (c *chat) EditMessage(_ context.Context, bot, messageID string, in bridge.MessageContent) (bridge.MessageEdited,
	error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.find(messageID)
	if m == nil {
		return bridge.MessageEdited{}, errNoMessage
	}
	m.title, m.body, m.buttons = in.Title, in.Body, in.Buttons
	c.log.Info("minimal: message edited", "bot", bot, "message_id", messageID, "status", in.Status, "title", in.Title,
		"body", in.Body, "buttons", labels(in.Buttons))
	return bridge.MessageEdited{}, nil
}

func (c *chat) ReplyInThread(_ context.Context, bot, messageID string, in bridge.ThreadReplyInput) (
	bridge.ThreadReplyPosted, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	parent := c.find(messageID)
	if parent == nil {
		return bridge.ThreadReplyPosted{}, errNoMessage
	}
	root := messageID
	if parent.root != "" {
		root = parent.root
	}
	id := c.nextID()
	c.messages[id] = &message{channel: parent.channel, root: root, body: in.Body, buttons: in.Buttons}
	ref := "t-" + root
	c.log.Info("minimal: thread reply posted", "bot", bot, "message_id", id, "thread_ref", ref, "notify", in.Notify,
		"body", c.mentions(in.Body, in.Mentions), "buttons", labels(in.Buttons))
	return bridge.ThreadReplyPosted{MessageId: id, ThreadRef: ref}, nil
}

func (c *chat) CheckChannel(_ context.Context, _, channelID string) (string, error) {
	if !c.channels[channelID] {
		return "", bridge.ForbiddenError("The bot has not joined this channel.")
	}
	return channelID, nil
}

func (c *chat) AnswerEvent(_ context.Context, bot string, ev bridge.ReportedEvent, text string) (
	bridge.AnswerShownShownAs, error) {
	shown := bridge.AnswerShownShownAsThread
	switch {
	case ev.Type == bridge.EventTypeAccountLink:
		shown = bridge.AnswerShownShownAsDirect
	case ev.Type == bridge.EventTypePress && c.private:
		shown = bridge.AnswerShownShownAsEphemeral
	}
	c.log.Info("minimal: answer shown", "bot", bot, "event_id", ev.ID, "type", ev.Type, "message_id", ev.MessageID,
		"person", ev.User.ExternalId, "chat", ev.Chat, "shown_as", shown, "text", text)
	return shown, nil
}

func (c *chat) FindUser(_ context.Context, _, handle string) (*bridge.Person, error) {
	p, ok := c.people[handle]
	if !ok {
		return nil, nil
	}
	return &p, nil
}

func (c *chat) SendDirectMessage(_ context.Context, bot string, in bridge.DirectMessageInput) (
	bridge.DirectMessageSent, error) {
	if _, ok := c.people[in.UserExternalId]; !ok {
		return bridge.DirectMessageSent{}, bridge.ForbiddenError("This person cannot receive a message from the bot.")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastDM++
	id := c.idPrefix + "-d" + strconv.Itoa(c.lastDM)
	// The log is the person's screen here, so the body is logged: it carries the Account link code that the person
	// must see. A real bridge never logs the body of a direct message; it may hold such a code or another secret.
	c.log.Info("minimal: direct message sent", "bot", bot, "message_id", id, "person", in.UserExternalId,
		"body", in.Body)
	return bridge.DirectMessageSent{MessageId: id}, nil
}

func (c *chat) DeleteMessage(_ context.Context, bot, messageID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.find(messageID)
	if m == nil {
		return errNoMessage
	}
	m.deleted = true
	c.log.Info("minimal: message deleted", "bot", bot, "message_id", messageID)
	return nil
}

// demoPress presses the first button of a message as a person: what a script does in place of a person.
func (c *chat) demoPress(w http.ResponseWriter, r *http.Request, b *bridge.Bridge) {
	var req struct {
		MessageID string `json:"message_id"`
		User      string `json:"user"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "the body is not {\"message_id\": …, \"user\": …}", http.StatusBadRequest)
		return
	}
	person, ok := c.people[req.User]
	if !ok {
		http.Error(w, "no person has the login "+strconv.Quote(req.User), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	var press bridge.Press
	var label string
	if m := c.find(req.MessageID); m != nil && len(m.buttons) > 0 {
		press = bridge.Press{MessageID: req.MessageID, User: person, Data: m.buttons[0].Data, Chat: m.channel}
		label = m.buttons[0].Label
	}
	c.mu.Unlock()
	if press.MessageID == "" {
		http.Error(w, "the message "+strconv.Quote(req.MessageID)+" does not exist or has no buttons",
			http.StatusBadRequest)
		return
	}
	switch err := b.Press(r.Context(), botName, press); {
	case errors.Is(err, bridge.ErrNotOffered):
		http.Error(w, "this bridge does not offer buttons", http.StatusConflict)
	case err != nil:
		c.log.Error("minimal: the press could not be reported", "error", err.Error())
		http.Error(w, "the press could not be reported", http.StatusInternalServerError)
	default:
		c.log.Info("minimal: button pressed", "bot", botName, "message_id", req.MessageID, "person", req.User,
			"label", label)
		w.WriteHeader(http.StatusNoContent)
	}
}

// find is the message with the id unless it was deleted; c.mu is held.
func (c *chat) find(id string) *message {
	if m := c.messages[id]; m != nil && !m.deleted {
		return m
	}
	return nil
}

// nextID is the id of a new message, unique across restarts by the process's prefix; c.mu is held.
func (c *chat) nextID() string {
	c.last++
	return c.idPrefix + "-m" + strconv.Itoa(c.last)
}

// mentions is body with its Mention markers as the messenger shows them: @login with native mentions, else the name.
func (c *chat) mentions(body string, mentions []bridge.Mention) string {
	return bridge.ReplaceMentions(body, mentions, func(m bridge.Mention) string {
		if c.native {
			return "@" + m.ExternalId
		}
		return m.DisplayName
	})
}

func labels(buttons []bridge.Button) []string {
	out := make([]string, 0, len(buttons))
	for _, b := range buttons {
		out = append(out, b.Label)
	}
	return out
}
