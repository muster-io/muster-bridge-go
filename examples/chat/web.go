// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package main

import (
	"cmp"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	bridge "github.com/muster-io/muster-bridge-go"
)

//go:embed web/index.html web/app.js web/app.css
var webFiles embed.FS

const (
	maxPostBytes = 64 << 10
	maxTextRunes = 4000
	logPathBytes = 256 // the most of a request's path the event log keeps
	pagePolicy   = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
)

// pageAbout is what the page shows of the bridge: never its token or push address.
type pageAbout struct {
	version       string
	features      features
	maxBodyLength int
	editMaxAge    *int
	push          bool
}

type page struct {
	b     *bridge.Bridge
	chat  *chat
	about pageAbout
	log   *slog.Logger
}

// newMux serves the Bridge protocol under /v1/, and the page with its JSON API under /chat/api/, which a person
// uses, or a script in place of one.
func newMux(b *bridge.Bridge, c *chat, about pageAbout, log *slog.Logger) http.Handler {
	p := &page{b: b, chat: c, about: about, log: log}
	mux := http.NewServeMux()
	mux.Handle("/v1/", p.recordCalls(b.Handler()))
	for pattern, file := range map[string]struct{ name, contentType string }{
		"GET /{$}":     {"web/index.html", "text/html; charset=utf-8"},
		"GET /app.js":  {"web/app.js", "text/javascript; charset=utf-8"},
		"GET /app.css": {"web/app.css", "text/css; charset=utf-8"},
	} {
		mux.Handle(pattern, asset(file.name, file.contentType))
	}
	mux.HandleFunc("GET /chat/api/state", p.state)
	mux.HandleFunc("GET /chat/api/messages", p.messages)
	mux.HandleFunc("GET /chat/api/direct-messages", p.directMessages)
	mux.HandleFunc("POST /chat/api/press", p.press)
	mux.HandleFunc("POST /chat/api/thread", p.thread)
	mux.HandleFunc("POST /chat/api/direct-message", p.directMessage)
	mux.HandleFunc("POST /chat/api/delete", p.remove)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			h.Set("Content-Security-Policy", pagePolicy)
		}
		next.ServeHTTP(w, r)
	})
}

func asset(name, contentType string) http.Handler {
	data, err := webFiles.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("example-bridge: %s is not embedded: %v", name, err))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(data)
	})
}

// recordCalls writes every protocol call to the event log: its method, path and status, never its query or headers,
// which may carry the token. The polls for events, which Muster makes all the time, are not written to the log
// output.
func (p *page) recordCalls(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen := &seenStatus{ResponseWriter: w}
		next.ServeHTTP(seen, r)
		code := cmp.Or(seen.code, http.StatusOK)
		path := r.URL.Path
		if len(path) > logPathBytes {
			path = path[:logPathBytes]
		}
		call := r.Method + " " + strings.ToValidUTF8(path, "\uFFFD")
		p.chat.record("call", call, strconv.Itoa(code))
		if call != "GET /v1/events" || code != http.StatusOK {
			p.log.Info("example-bridge: protocol call", "call", call, "status", code)
		}
	})
}

// seenStatus passes an answer of the library through untouched and keeps the first status code set on it; 0 means
// the body went out with the implicit 200. The library writes each answer whole and never flushes, so WriteHeader is
// the only method it needs to see.
type seenStatus struct {
	http.ResponseWriter
	code int
}

func (s *seenStatus) WriteHeader(code int) {
	s.code = cmp.Or(s.code, code)
	s.ResponseWriter.WriteHeader(code)
}

type stateView struct {
	Bridge     bridgeView            `json:"bridge"`
	Messenger  string                `json:"messenger"`
	Features   map[string]bool       `json:"features"`
	UserLookup bridge.InfoUserLookup `json:"user_lookup"`
	Limits     limitsView            `json:"limits"`
	Push       bool                  `json:"push"`
	Channels   []channelView         `json:"channels"`
	People     []personView          `json:"people"`
	Version    uint64                `json:"version"`
	Log        []logEntry            `json:"log"`
}

type bridgeView struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type limitsView struct {
	MaxBodyLength     int  `json:"max_body_length"`
	EditMaxAgeSeconds *int `json:"edit_max_age_seconds"`
}

// state is the bridge, its features, the channels and people, the version of the chat and the newest lines of the
// event log.
func (p *page) state(w http.ResponseWriter, _ *http.Request) {
	s := p.chat.state()
	view := stateView{
		Bridge:     bridgeView{Name: bridgeName, Version: p.about.version},
		Messenger:  messengerName,
		Features:   make(map[string]bool, len(featureNames)),
		UserLookup: userLookup(p.about.features),
		Limits:     limitsView{MaxBodyLength: p.about.maxBodyLength, EditMaxAgeSeconds: p.about.editMaxAge},
		Push:       p.about.push,
		Channels:   s.Channels,
		People:     s.People,
		Version:    s.Version,
		Log:        s.Log,
	}
	for _, name := range featureNames {
		view.Features[name] = p.about.features[name]
	}
	writeJSON(w, http.StatusOK, view)
}

// messages are the Root posts of a channel with their Threads, oldest first.
func (p *page) messages(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("channel")
	if id == "" {
		writeError(w, http.StatusBadRequest, "the parameter channel is missing")
		return
	}
	posts, ok := p.chat.channelPosts(id)
	if !ok {
		writeError(w, http.StatusNotFound, "the bot is not in the channel "+strconv.Quote(id))
		return
	}
	writeJSON(w, http.StatusOK, posts)
}

// directMessages is the direct chat between a person and the bot, oldest first.
func (p *page) directMessages(w http.ResponseWriter, r *http.Request) {
	login := r.URL.Query().Get("user")
	if login == "" {
		writeError(w, http.StatusBadRequest, "the parameter user is missing")
		return
	}
	entries, ok := p.chat.directChat(login)
	if !ok {
		writeError(w, http.StatusNotFound, "no person has the login "+strconv.Quote(login))
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

type pressRequest struct {
	MessageID string `json:"message_id"`
	User      string `json:"user"`
	Button    *int   `json:"button"`
}

type pressResponse struct {
	MessageID    string `json:"message_id"`
	Button       int    `json:"button"`
	Label        string `json:"label"`
	UpdateID     string `json:"update_id"`
	Reported     string `json:"reported"`
	Confirmation string `json:"confirmation"`
}

// press acts out a person's click on one of a message's buttons. The library hears of the click first; the JSON answer
// then carries the short confirmation a messenger would flash at once.
func (p *page) press(w http.ResponseWriter, r *http.Request) {
	var req pressRequest
	person, ok := p.decode(w, r, &req, &req.User)
	if !ok || !hasMessageID(w, req.MessageID) {
		return
	}
	button := 0
	if req.Button != nil {
		button = *req.Button
	}
	target, err := p.chat.pressTarget(req.MessageID, button)
	switch {
	case errors.Is(err, errNoButton):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("the message %q has no button %d", req.MessageID, button))
		return
	case err != nil:
		writeError(w, http.StatusNotFound, "no message has the id "+strconv.Quote(req.MessageID))
		return
	}
	err = p.b.Press(r.Context(), botName, bridge.Press{UpdateID: target.updateID, MessageID: req.MessageID,
		User: person, Data: target.data, At: p.chat.now(), Chat: target.channel})
	reported, failed := p.report(w, fmt.Sprintf("press on %s by %s (%s)", req.MessageID, req.User, target.label), err,
		true)
	if failed {
		return
	}
	writeJSON(w, http.StatusOK, pressResponse{MessageID: req.MessageID, Button: button, Label: target.label,
		UpdateID: target.updateID, Reported: reported, Confirmation: "Pressed " + target.label})
}

type threadRequest struct {
	MessageID string `json:"message_id"`
	User      string `json:"user"`
	Text      string `json:"text"`
}

type postedResponse struct {
	ID        string `json:"id"`
	MessageID string `json:"message_id,omitempty"`
	UpdateID  string `json:"update_id"`
	Reported  string `json:"reported"`
}

// thread posts a person's message in the Thread of a message and reports it; without thread_messages it is posted
// all the same.
func (p *page) thread(w http.ResponseWriter, r *http.Request) {
	var req threadRequest
	person, ok := p.decode(w, r, &req, &req.User)
	if !ok || !hasMessageID(w, req.MessageID) {
		return
	}
	text, ok := checkText(w, req.Text)
	if !ok {
		return
	}
	at, err := p.chat.postInThread(req.MessageID, "", post{Author: req.User, Text: text, HTML: renderText(text)})
	if err != nil {
		writeError(w, http.StatusNotFound, "no message has the id "+strconv.Quote(req.MessageID))
		return
	}
	update := p.chat.nextUpdate()
	err = p.b.ThreadMessage(r.Context(), botName, bridge.ThreadMessage{UpdateID: update, MessageID: at.root,
		ThreadRef: at.root, User: person, Text: text, At: p.chat.now(), Chat: at.channel})
	reported, failed := p.report(w, fmt.Sprintf("thread message on %s by %s", at.root, req.User), err, false)
	if failed {
		return
	}
	writeJSON(w, http.StatusCreated, postedResponse{ID: at.id, MessageID: at.root, UpdateID: update,
		Reported: reported})
}

type directRequest struct {
	User string `json:"user"`
	Text string `json:"text"`
}

// directMessage writes a person's message in their direct chat with the bot and gives it to the library, which makes
// an account_link event of an Account link token and ignores anything else. The text is never logged.
func (p *page) directMessage(w http.ResponseWriter, r *http.Request) {
	var req directRequest
	person, ok := p.decode(w, r, &req, &req.User)
	if !ok {
		return
	}
	text, ok := checkText(w, req.Text)
	if !ok {
		return
	}
	id, err := p.chat.addDirect(req.User, directEntry{Author: req.User, Text: text, HTML: renderText(text)})
	if err != nil {
		writeError(w, http.StatusBadRequest, "no person has the login "+strconv.Quote(req.User))
		return
	}
	update := p.chat.nextUpdate()
	err = p.b.DirectMessage(r.Context(), botName, bridge.DirectMessage{UpdateID: update, User: person, Text: text,
		At: p.chat.now(), Chat: "direct:" + req.User})
	reported, failed := p.report(w, "direct message from "+req.User, err, false)
	if failed {
		return
	}
	writeJSON(w, http.StatusCreated, postedResponse{ID: id, UpdateID: update, Reported: reported})
}

type deleteRequest struct {
	MessageID string `json:"message_id"`
	User      string `json:"user"`
}

type deleteResponse struct {
	MessageID string `json:"message_id"`
	UpdateID  string `json:"update_id,omitempty"`
	Reported  string `json:"reported"`
}

// remove deletes a message on the page; the deletion of a message of the bot is reported.
func (p *page) remove(w http.ResponseWriter, r *http.Request) {
	var req deleteRequest
	if _, ok := p.decode(w, r, &req, &req.User); !ok || !hasMessageID(w, req.MessageID) {
		return
	}
	wasBot, err := p.chat.remove(req.MessageID, false)
	if err != nil {
		writeError(w, http.StatusNotFound, "no message has the id "+strconv.Quote(req.MessageID))
		return
	}
	what := fmt.Sprintf("deletion of %s by %s", req.MessageID, req.User)
	if !wasBot {
		p.chat.record("report", what, "none")
		writeJSON(w, http.StatusOK, deleteResponse{MessageID: req.MessageID, Reported: "none"})
		return
	}
	update := p.chat.nextUpdate()
	err = p.b.MessageDeleted(r.Context(), botName, bridge.MessageDeleted{UpdateID: update,
		MessageID: req.MessageID, At: p.chat.now()})
	reported, failed := p.report(w, what, err, false)
	if failed {
		return
	}
	writeJSON(w, http.StatusOK, deleteResponse{MessageID: req.MessageID, UpdateID: update, Reported: reported})
}

// decode reads the JSON body of a POST into v and finds the person whose login *login names.
func (p *page) decode(w http.ResponseWriter, r *http.Request, v any, login *string) (bridge.Person, bool) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "send the body as Content-Type: application/json")
		return bridge.Person{}, false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPostBytes)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "the body is not the JSON this endpoint takes")
		return bridge.Person{}, false
	}
	person, ok := p.chat.person(*login)
	if !ok {
		writeError(w, http.StatusBadRequest, "no person has the login "+strconv.Quote(*login))
	}
	return person, ok
}

// report writes the outcome of a report to the event log: "reported", or "not_offered" for a feature the bridge does
// not offer, which is an answer 409 when notOffered is fatal. Any other failure is answered 500.
func (p *page) report(w http.ResponseWriter, what string, err error, notOfferedFails bool) (string, bool) {
	outcome := "reported"
	switch {
	case errors.Is(err, bridge.ErrNotOffered):
		outcome = "not_offered"
	case err != nil:
		outcome = "failed"
	}
	p.chat.record("report", what, outcome)
	p.log.Info("example-bridge: report", "what", what, "outcome", outcome)
	switch {
	case outcome == "not_offered" && notOfferedFails:
		writeError(w, http.StatusConflict, "this bridge does not offer the feature this needs")
		return outcome, true
	case outcome == "failed":
		p.log.Error("example-bridge: the report failed", "what", what, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "the bridge could not take the report")
		return outcome, true
	}
	return outcome, false
}

func hasMessageID(w http.ResponseWriter, id string) bool {
	if id == "" {
		writeError(w, http.StatusBadRequest, "message_id is missing")
	}
	return id != ""
}

// checkText is the text of a message without the white space around it, 1 to 4000 characters long.
func checkText(w http.ResponseWriter, text string) (string, bool) {
	text = strings.TrimSpace(text)
	if n := utf8.RuneCountInString(text); n == 0 || n > maxTextRunes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("the text must be 1 to %d characters long", maxTextRunes))
		return "", false
	}
	return text, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":"the answer cannot be encoded"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, text string) {
	writeJSON(w, status, map[string]string{"error": text})
}
