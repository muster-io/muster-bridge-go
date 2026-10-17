// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
)

// specYAML is the copy of Muster's api/bridge.yaml, which every request, answer, event and info is checked against.
//
//go:embed api/bridge.yaml
var specYAML []byte

// The operations of api/bridge.yaml, by operationId.
const (
	opGetInfo           = "getInfo"
	opPostMessage       = "postMessage"
	opEditMessage       = "editMessage"
	opDeleteMessage     = "deleteMessage"
	opReplyInThread     = "replyInThread"
	opCheckChannel      = "checkChannel"
	opFindUsers         = "findUsers"
	opSendDirectMessage = "sendDirectMessage"
	opAnswerEvent       = "answerEvent"
	opPollEvents        = "pollEvents"
)

const (
	// maxRequestBytes is the library's cap on a request body, which leaves room for any max_body_length New accepts.
	maxRequestBytes = 1 << 20
	keyClaimTTL     = 2 * time.Minute
	keyRetention    = 24 * time.Hour
	maxChannelName  = 200
)

// operation is one operation of the protocol: how it is routed, when it is served, and what serves it — run, within
// the common flow of claim, validation, messenger call, answer check and answer, or serve, which does it all.
type operation struct {
	id     string
	method string
	path   string
	status int
	// asReplay, set for the operations with an Idempotency-Key, re-encodes a stored first answer with replayed true.
	asReplay func(first []byte) ([]byte, error)
	served   func(*Bridge) bool
	run      func(*call) (any, error)
	serve    func(*Bridge, *operation, http.ResponseWriter, *http.Request)
}

var operations = []*operation{
	{id: opGetInfo, method: http.MethodGet, path: "/v1/info", status: http.StatusOK, serve: (*Bridge).getInfo},
	{id: opPostMessage, method: http.MethodPost, path: "/v1/messages", status: http.StatusCreated,
		asReplay: replayOf(func(a *MessagePosted) { a.Replayed = true }), run: (*call).postMessage},
	{id: opEditMessage, method: http.MethodPatch, path: "/v1/messages/{message_id}", status: http.StatusOK,
		run: (*call).editMessage},
	{id: opDeleteMessage, method: http.MethodDelete, path: "/v1/messages/{message_id}", status: http.StatusNoContent,
		served: func(b *Bridge) bool { return b.info.Capabilities.Delete != nil }, run: (*call).deleteMessage},
	{id: opReplyInThread, method: http.MethodPost, path: "/v1/messages/{message_id}/thread-replies",
		status: http.StatusCreated, asReplay: replayOf(func(a *ThreadReplyPosted) { a.Replayed = true }),
		run: (*call).replyInThread},
	{id: opCheckChannel, method: http.MethodPost, path: "/v1/channels/{channel_id}/check", status: http.StatusOK,
		run: (*call).checkChannel},
	{id: opFindUsers, method: http.MethodGet, path: "/v1/users", status: http.StatusOK,
		served: func(b *Bridge) bool { return b.info.UserLookup != InfoUserLookupNone }, run: (*call).findUsers},
	{id: opSendDirectMessage, method: http.MethodPost, path: "/v1/direct-messages", status: http.StatusCreated,
		asReplay: replayOf(func(a *DirectMessageSent) { a.Replayed = true }),
		served:   func(b *Bridge) bool { return b.info.Capabilities.DirectMessages != nil },
		run:      (*call).sendDirectMessage},
	{id: opAnswerEvent, method: http.MethodPost, path: "/v1/answers", status: http.StatusOK,
		asReplay: replayOf(func(a *AnswerShown) { a.Replayed = true }), run: (*call).answerEvent},
	{id: opPollEvents, method: http.MethodGet, path: "/v1/events", status: http.StatusOK,
		served: func(b *Bridge) bool { return b.pollOffered }, serve: (*Bridge).pollEvents},
}

// replayOf makes the asReplay of an operation whose answer type is A: decode the stored first answer into an A, let
// mark set its replayed member, encode it again.
func replayOf[A any](mark func(*A)) func([]byte) ([]byte, error) {
	return func(first []byte) ([]byte, error) {
		var answer A
		if err := json.Unmarshal(first, &answer); err != nil {
			return nil, err
		}
		mark(&answer)
		return json.Marshal(answer)
	}
}

// protocol is the loaded api/bridge.yaml with the route of each operation, for kin-openapi's validation.
type protocol struct {
	doc    *openapi3.T
	routes map[string]*opRoute
}

type opRoute struct {
	route      *routers.Route
	pathParams []string
}

var loadProtocol = sync.OnceValues(func() (*protocol, error) {
	doc, err := openapi3.NewLoader().LoadFromData(specYAML)
	if err != nil {
		return nil, fmt.Errorf("bridge: cannot load the embedded api/bridge.yaml: %w", err)
	}
	p := &protocol{doc: doc, routes: map[string]*opRoute{}}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			r := &opRoute{route: &routers.Route{Spec: doc, Path: path, PathItem: item, Method: method, Operation: op}}
			for _, params := range []openapi3.Parameters{item.Parameters, op.Parameters} {
				for _, param := range params {
					if param.Value != nil && param.Value.In == openapi3.ParameterInPath {
						r.pathParams = append(r.pathParams, param.Value.Name)
					}
				}
			}
			p.routes[op.OperationID] = r
		}
	}
	return p, nil
})

// check is where and why the JSON data does not match the named schema of components, "" when it does.
func (p *protocol) check(schema string, data []byte) string {
	ref := p.doc.Components.Schemas[schema]
	if ref == nil || ref.Value == nil {
		return "api/bridge.yaml has no schema " + schema
	}
	return checkJSON(ref.Value, data)
}

// checkAnswer is where and why the JSON data does not match the answer of the operation with the status.
func (p *protocol) checkAnswer(op string, status int, data []byte) string {
	resp := p.routes[op].route.Operation.Responses.Status(status)
	if resp == nil || resp.Value == nil {
		return fmt.Sprintf("api/bridge.yaml declares no %d answer of %s", status, op)
	}
	media := resp.Value.Content.Get("application/json")
	if media == nil || media.Schema == nil || media.Schema.Value == nil {
		return fmt.Sprintf("api/bridge.yaml declares no JSON for the %d answer of %s", status, op)
	}
	return checkJSON(media.Schema.Value, data)
}

// checkJSON validates with kin-openapi's built-in validator, which handles every construct of api/bridge.yaml and
// names the JSON pointer of a mismatch without its value.
func checkJSON(s *openapi3.Schema, data []byte) string {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return "it is not JSON"
	}
	if err := s.VisitJSON(v); err != nil {
		return schemaProblem(err)
	}
	return ""
}

func (b *Bridge) newHandler() http.Handler {
	mux := http.NewServeMux()
	for _, op := range operations {
		if op.served == nil || op.served(b) {
			mux.Handle(op.method+" "+op.path, b.operationHandler(op))
		}
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		b.writeProblem(w, http.StatusNotFound, InvalidError("This bridge does not serve this operation."))
	})
	return b.recoverer(b.authenticate(mux))
}

// requestNote is what the recoverer logs of the request: the operation, the bot and the secrets the request handles.
type requestNote struct {
	op, bot string
	secrets []string
}

type (
	noteKey           struct{}
	botKey            struct{}
	idempotencyKeyCtx struct{}
)

func noteFrom(ctx context.Context) *requestNote {
	if n, ok := ctx.Value(noteKey{}).(*requestNote); ok {
		return n
	}
	return &requestNote{}
}

// IdempotencyKeyFrom is the Idempotency-Key of the request that a Messenger call serves, "" for an operation without
// one, so that a messenger with an idempotency mechanism of its own can be given it. Keys are per bot.
func IdempotencyKeyFrom(ctx context.Context) string {
	key, _ := ctx.Value(idempotencyKeyCtx{}).(string)
	return key
}

// responseWriter notes whether anything was written, so that a panic is answered only before.
type responseWriter struct {
	http.ResponseWriter
	wrote bool
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.wrote = true
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseWriter) Write(p []byte) (int, error) {
	rw.wrote = true
	return rw.ResponseWriter.Write(p)
}

func (rw *responseWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

// recoverer answers a panic below it as transient, unless an answer was started, and logs it redacted.
func (b *Bridge) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		note := &requestNote{}
		rw := &responseWriter{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			b.log.LogAttrs(r.Context(), slog.LevelError, "bridge: panic", slog.String("operation", note.op),
				slog.String("bot", note.bot), slog.String("panic", b.redact(fmt.Sprint(v), note.secrets...)),
				slog.String("stack", string(debug.Stack())))
			if !rw.wrote {
				b.writeError(rw, TransientError(detailMessenger))
			}
		}()
		next.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), noteKey{}, note)))
	})
}

// authenticate puts the request under the bot whose token it presents, hashing the token and comparing the hash with
// every bot's in constant time; a request that presents no bot's token is answered 401.
func (b *Bridge) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs := b.botOf(r.Header.Get("Authorization"))
		if bs == nil {
			b.writeUnauthorized(w)
			return
		}
		noteFrom(r.Context()).bot = bs.name
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), botKey{}, bs.name)))
	})
}

func (b *Bridge) botOf(authorization string) *botState {
	scheme, token, _ := strings.Cut(authorization, " ")
	token = strings.TrimLeft(token, " ")
	sum := sha256.Sum256([]byte(token))
	var found *botState
	for _, bs := range b.botList {
		if subtle.ConstantTimeCompare(sum[:], bs.tokenHash[:]) == 1 {
			found = bs
		}
	}
	if !strings.EqualFold(scheme, "Bearer") || token == "" {
		return nil
	}
	return found
}

func (b *Bridge) operationHandler(op *operation) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		note := noteFrom(r.Context())
		note.op = op.id
		if op.serve != nil {
			op.serve(b, op, w, r)
			return
		}
		bot, _ := r.Context().Value(botKey{}).(string)
		c := &call{b: b, op: op, w: w, r: r, ctx: r.Context(), bot: bot, note: note}
		c.serve()
	})
}

// call is one request of an operation that calls the messenger.
type call struct {
	b       *Bridge
	op      *operation
	w       http.ResponseWriter
	r       *http.Request
	ctx     context.Context
	bot     string
	note    *requestNote
	body    []byte
	key     string
	claimID string
	claimed bool
	saved   bool
}

// refusal is an answer of the library itself, written as it is.
type refusal struct{ e *Error }

func (r *refusal) Error() string { return r.e.Error() }

func refuse(e *Error) error { return &refusal{e} }

func (c *call) serve() {
	if c.op.asReplay != nil {
		if !c.claim() {
			return
		}
		defer c.release()
	}
	if !c.read() {
		return
	}
	answer, err := c.op.run(c)
	if err != nil {
		c.fail(err)
		return
	}
	c.answer(answer)
}

// claim claims the request's Idempotency-Key, or answers: the first result again, or transient while it runs.
func (c *call) claim() bool {
	key := c.r.Header.Get("Idempotency-Key")
	if !validKey(key) {
		c.b.writeError(c.w, InvalidError("Send an Idempotency-Key header of 1 to 255 visible ASCII characters "+
			"(no spaces)."))
		return false
	}
	now := c.b.clock.Now()
	claim, err := c.b.store.ClaimKey(c.ctx, c.bot, key, now, now.Add(keyClaimTTL))
	switch {
	case err != nil:
		c.fail(c.storeFailed(err))
		return false
	case claim.State == KeyAnswered:
		c.answerAgain(claim.Answer)
		return false
	case claim.State == KeyRunning:
		c.b.writeError(c.w, TransientError("A request with this Idempotency-Key is still running."))
		return false
	case claim.State != KeyClaimed:
		c.fail(c.storeFailed(fmt.Errorf("ClaimKey returned the unknown state %d", claim.State)))
		return false
	}
	c.key, c.claimID, c.claimed = key, claim.ID, true
	c.ctx = context.WithValue(c.ctx, idempotencyKeyCtx{}, key)
	return true
}

func validKey(key string) bool {
	if key == "" || len(key) > 255 {
		return false
	}
	for i := range len(key) {
		if key[i] < '!' || key[i] > '~' {
			return false
		}
	}
	return true
}

// answerAgain sends the first answer of the key once more, flagged as replayed. The request's own body is never read:
// a repeated key gets the first answer whatever it carries. A stored answer that no longer fits the operation is a
// fault of the store.
func (c *call) answerAgain(first StoredAnswer) {
	body, err := c.op.asReplay(first.Body)
	fault := "it is not the JSON of the operation's answer"
	if err == nil {
		fault = c.b.proto.checkAnswer(c.op.id, first.Status, body)
	}
	if fault != "" {
		c.fail(c.storeFailed(fmt.Errorf("the first answer stored for the Idempotency-Key cannot be sent again: %s",
			fault)))
		return
	}
	writeJSON(c.w, first.Status, "application/json", body)
}

// release frees the claimed key unless its answer was saved, so that a retry runs again; it runs on a panic too. The
// store frees it only while this request's claim holds it.
func (c *call) release() {
	if !c.claimed || c.saved {
		return
	}
	if err := c.b.store.ReleaseKey(context.WithoutCancel(c.ctx), c.bot, c.key, c.claimID); err != nil {
		c.log(slog.LevelError, "bridge: cannot release an Idempotency-Key", slog.String("error",
			c.b.redact(err.Error())))
	}
}

// read reads the request body, at most maxRequestBytes, and validates the request against api/bridge.yaml.
func (c *call) read() bool {
	body := []byte{}
	if c.r.Body != nil {
		var err error
		if body, err = io.ReadAll(http.MaxBytesReader(c.w, c.r.Body, maxRequestBytes)); err != nil {
			detail := "Reading the request body failed."
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				detail = "The request body exceeds 1 MiB, the most this bridge reads."
			}
			c.b.writeError(c.w, InvalidError(detail))
			return false
		}
	}
	c.body = body
	if err := c.b.validateRequest(c.ctx, c.op, c.r, body); err != nil {
		c.b.writeError(c.w, InvalidError(requestProblem(err)))
		return false
	}
	return true
}

func (b *Bridge) validateRequest(ctx context.Context, op *operation, r *http.Request, body []byte) error {
	route := b.proto.routes[op.id]
	vr := r.Clone(ctx)
	vr.Body = io.NopCloser(bytes.NewReader(body))
	vr.ContentLength = int64(len(body))
	vr.GetBody = nil
	params := make(map[string]string, len(route.pathParams))
	for _, name := range route.pathParams {
		params[name] = r.PathValue(name)
	}
	return openapi3filter.ValidateRequest(ctx, &openapi3filter.RequestValidationInput{
		Request:    vr,
		PathParams: params,
		Route:      route.route,
		Options: &openapi3filter.Options{
			AuthenticationFunc:  openapi3filter.NoopAuthenticationFunc,
			SkipSettingDefaults: true,
		},
	})
}

// decode decodes the validated body into v.
func (c *call) decode(v any) error {
	if err := json.Unmarshal(c.body, v); err != nil {
		return refuse(InvalidError("The request body does not match api/bridge.yaml."))
	}
	return nil
}

// checkContent refuses a body or buttons beyond the bridge's limits and capabilities.
func (c *call) checkContent(body string, buttons []Button) error {
	if n, most := utf8.RuneCountInString(body), c.b.info.Limits.MaxBodyLength; n > most {
		return refuse(InvalidError(fmt.Sprintf("The body is %d code points long; this bridge takes at most %d "+
			"(limits.max_body_length).", n, most)))
	}
	if e := checkButtons(buttons, c.b.info.Capabilities.Buttons); e != nil {
		return refuse(e)
	}
	return nil
}

func checkButtons(buttons []Button, caps *Capabilities_Buttons) *Error {
	switch {
	case len(buttons) == 0:
		return nil
	case caps == nil:
		return InvalidError("This bridge does not offer buttons (capabilities.buttons).")
	case len(buttons) > caps.MaxPerMessage:
		return InvalidError(fmt.Sprintf("The message has %d buttons; this bridge takes at most %d "+
			"(capabilities.buttons.max_per_message).", len(buttons), caps.MaxPerMessage))
	}
	for i, button := range buttons {
		if len(button.Data) > caps.DataMaxBytes {
			return InvalidError(fmt.Sprintf("Button %d carries %d bytes of data; this bridge takes at most %d "+
				"(capabilities.buttons.data_max_bytes).", i+1, len(button.Data), caps.DataMaxBytes))
		}
	}
	return nil
}

// fail answers an error: a refusal of the library as it is, an *Error of the messenger in the class the operation
// declares, any other error as transient with a generic detail, logged.
func (c *call) fail(err error) {
	if ref, ok := errors.AsType[*refusal](err); ok {
		c.b.writeError(c.w, ref.e, c.note.secrets...)
		return
	}
	if e, ok := errors.AsType[*Error](err); ok && e != nil {
		class, mistake := mapClass(c.op.id, e.Class)
		if mistake {
			c.log(slog.LevelError, "bridge: the messenger answered a class the operation does not declare",
				slog.String("class", string(e.Class)))
			c.b.writeError(c.w, TransientError(detailMessenger))
			return
		}
		if e.Reason != "" && (e.Reason != ReasonEditWindowClosed || class != ClassGone) {
			c.log(slog.LevelError, "bridge: the messenger gave a reason the protocol does not define for this answer; "+
				"it is not sent", slog.String("class", string(class)),
				slog.String("reason", c.b.redact(string(e.Reason), c.note.secrets...)))
		}
		if class != e.Class {
			mapped := *e
			mapped.Class = class
			e = &mapped
		}
		c.b.writeError(c.w, e, c.note.secrets...)
		return
	}
	level := slog.LevelError
	if c.ctx.Err() != nil {
		level = slog.LevelWarn
	}
	c.log(level, "bridge: messenger call failed", slog.String("error", c.b.redact(err.Error(), c.note.secrets...)))
	c.b.writeError(c.w, TransientError(detailMessenger))
}

// storeFailed logs a failure of the Store and is the refusal that answers it.
func (c *call) storeFailed(err error) error {
	c.log(slog.LevelError, "bridge: the store failed", slog.String("error", c.b.redact(err.Error())))
	return refuse(TransientError(detailStore))
}

// answer checks the messenger's answer against api/bridge.yaml, stores it for the Idempotency-Key and writes it.
func (c *call) answer(v any) {
	if c.op.status == http.StatusNoContent {
		c.w.Header().Set("Cache-Control", "no-store")
		c.w.WriteHeader(http.StatusNoContent)
		return
	}
	body, err := json.Marshal(v)
	problem := "it cannot be encoded"
	if err == nil {
		problem = c.b.proto.checkAnswer(c.op.id, c.op.status, body)
	}
	if problem != "" {
		c.log(slog.LevelError, "bridge: the messenger's answer does not match api/bridge.yaml",
			slog.String("problem", problem))
		c.b.writeError(c.w, TransientError(detailMessenger))
		return
	}
	if c.claimed {
		until := c.b.clock.Now().Add(keyRetention)
		err := c.b.store.SaveKey(context.WithoutCancel(c.ctx), c.bot, c.key, c.claimID,
			StoredAnswer{Status: c.op.status, Body: body}, until)
		if err != nil {
			c.log(slog.LevelError, "bridge: cannot store the answer of an Idempotency-Key", slog.String("error",
				c.b.redact(err.Error())))
		} else {
			c.saved = true
		}
	}
	writeJSON(c.w, c.op.status, "application/json", body)
}

func (c *call) log(level slog.Level, msg string, attrs ...slog.Attr) {
	c.b.log.LogAttrs(c.ctx, level, msg, append([]slog.Attr{slog.String("operation", c.op.id),
		slog.String("bot", c.bot)}, attrs...)...)
}

func writeJSON(w http.ResponseWriter, status int, contentType string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (b *Bridge) getInfo(_ *operation, w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, "application/json", b.infoBody)
}

func (c *call) postMessage() (any, error) {
	var in MessageInput
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	if err := c.checkContent(in.Body, in.Buttons); err != nil {
		return nil, err
	}
	out, err := c.b.messenger.PostMessage(c.ctx, c.bot, in)
	out.Replayed = false
	return out, err
}

func (c *call) editMessage() (any, error) {
	var in MessageContent
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	if err := c.checkContent(in.Body, in.Buttons); err != nil {
		return nil, err
	}
	return c.b.messenger.EditMessage(c.ctx, c.bot, c.r.PathValue("message_id"), in)
}

func (c *call) deleteMessage() (any, error) {
	err := c.b.deleter.DeleteMessage(c.ctx, c.bot, c.r.PathValue("message_id"))
	if e, ok := errors.AsType[*Error](err); ok && e != nil && e.Class == ClassGone {
		return nil, nil
	}
	return nil, err
}

func (c *call) replyInThread() (any, error) {
	var in ThreadReplyInput
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	if err := c.checkContent(in.Body, in.Buttons); err != nil {
		return nil, err
	}
	out, err := c.b.messenger.ReplyInThread(c.ctx, c.bot, c.r.PathValue("message_id"), in)
	out.Replayed = false
	return out, err
}

func (c *call) checkChannel() (any, error) {
	id := c.r.PathValue("channel_id")
	name, err := c.b.messenger.CheckChannel(c.ctx, c.bot, id)
	if err != nil {
		return nil, err
	}
	if name = cutRunes(name, maxChannelName); name == "" {
		name = cutRunes(id, maxChannelName)
	}
	return ChannelCheck{ChannelId: id, Name: name}, nil
}

func (c *call) findUsers() (any, error) {
	p, err := c.b.finder.FindUser(c.ctx, c.bot, c.r.URL.Query().Get("handle"))
	if err != nil {
		return nil, err
	}
	list := PersonList{Items: []Person{}}
	if p != nil {
		list.Items = append(list.Items, *p)
	}
	return list, nil
}

func (c *call) sendDirectMessage() (any, error) {
	var in DirectMessageInput
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	out, err := c.b.dm.SendDirectMessage(c.ctx, c.bot, in)
	out.Replayed = false
	return out, err
}
