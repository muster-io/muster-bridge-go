// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestErrorClassesBecomeProblems(t *testing.T) {
	tests := []struct {
		name       string
		err        *Error
		status     int
		retryAfter string
		scope      ProblemScope
		reason     ProblemReason
	}{
		{"transient", TransientError("The messenger is down."), http.StatusInternalServerError, "", "", ""},
		{"rate_limited", RateLimitedError(1200*time.Millisecond, "", "Slow down."), http.StatusTooManyRequests, "2",
			ProblemScopeChannel, ""},
		{"rate_limited bot", RateLimitedError(30*time.Second, ProblemScopeBot, "Slow down."),
			http.StatusTooManyRequests, "30", ProblemScopeBot, ""},
		{"rate_limited no wait", RateLimitedError(0, ProblemScopeChannel, "Slow down."), http.StatusTooManyRequests,
			"0", ProblemScopeChannel, ""},
		{"rate_limited negative", &Error{Class: ClassRateLimited, Detail: "x", RetryAfter: -time.Second},
			http.StatusTooManyRequests, "0", ProblemScopeChannel, ""},
		{"gone", GoneError("The message no longer exists."), http.StatusNotFound, "", "", ""},
		{"gone edit window", GoneError("Too old.", ReasonEditWindowClosed), http.StatusNotFound, "", "",
			ProblemReasonEditWindowClosed},
		{"forbidden", ForbiddenError("The bot is not a member of this channel."), http.StatusForbidden, "", "", ""},
		{"invalid", InvalidError("Too long for this messenger."), http.StatusBadRequest, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, m, _, _ := newTestBridge(t, nil)
			m.edit = func(string, string, MessageContent) (MessageEdited, error) { return MessageEdited{}, tt.err }
			rec := do(t, b.Handler(), http.MethodPatch, "/v1/messages/m1", tokenMain, nil, editBody)
			p := wantProblem(t, rec, tt.status, tt.err.Class)
			if detailOf(p) != tt.err.Detail {
				t.Fatalf("detail %q, want %q", detailOf(p), tt.err.Detail)
			}
			if got := rec.Header().Get("Retry-After"); got != tt.retryAfter {
				t.Fatalf("Retry-After %q, want %q", got, tt.retryAfter)
			}
			if (p.Scope == nil) != (tt.scope == "") || (p.Scope != nil && *p.Scope != tt.scope) {
				t.Fatalf("scope %v, want %q", p.Scope, tt.scope)
			}
			if (p.Reason == nil) != (tt.reason == "") || (p.Reason != nil && *p.Reason != tt.reason) {
				t.Fatalf("reason %v, want %q", p.Reason, tt.reason)
			}
			if p.Title == nil || *p.Title == "" {
				t.Fatal("the problem has no title")
			}
		})
	}
}

func TestUndeclaredClassesAreMapped(t *testing.T) {
	type request struct {
		method, path string
		header       http.Header
		body         string
	}
	post := request{http.MethodPost, "/v1/messages", key("k1"), postBody}
	check := request{http.MethodPost, "/v1/channels/alerts/check", nil, ""}
	del := request{http.MethodDelete, "/v1/messages/m1", nil, ""}
	dm := request{http.MethodPost, "/v1/direct-messages", key("k1"), dmBody}
	users := request{http.MethodGet, "/v1/users?handle=alice", nil, ""}
	tests := []struct {
		name    string
		req     request
		class   Class
		status  int
		want    Class
		mistake bool
	}{
		{"deleteMessage gone", del, ClassGone, http.StatusNoContent, "", false},
		{"postMessage gone", post, ClassGone, http.StatusForbidden, ClassForbidden, false},
		{"sendDirectMessage gone", dm, ClassGone, http.StatusForbidden, ClassForbidden, false},
		{"checkChannel gone", check, ClassGone, http.StatusForbidden, ClassForbidden, false},
		{"checkChannel invalid", check, ClassInvalid, http.StatusForbidden, ClassForbidden, false},
		{"deleteMessage invalid", del, ClassInvalid, http.StatusInternalServerError, ClassTransient, true},
		{"findUsers forbidden", users, ClassForbidden, http.StatusInternalServerError, ClassTransient, true},
		{"findUsers gone", users, ClassGone, http.StatusInternalServerError, ClassTransient, true},
		{"answerEvent forbidden", request{}, ClassForbidden, http.StatusInternalServerError, ClassTransient, true},
		{"no class", post, "", http.StatusInternalServerError, ClassTransient, true},
		{"unknown class", post, "weird", http.StatusInternalServerError, ClassTransient, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, m, _, logs := newTestBridge(t, nil)
			e := &Error{Class: tt.class, Detail: "The account is missing."}
			m.post = func(string, MessageInput) (MessagePosted, error) { return MessagePosted{}, e }
			m.check = func(string, string) (string, error) { return "", e }
			m.del = func(string, string) error { return e }
			m.dm = func(string, DirectMessageInput) (DirectMessageSent, error) { return DirectMessageSent{}, e }
			m.find = func(string, string) (*Person, error) { return nil, e }
			m.answer = func(string, ReportedEvent, string) (AnswerShownShownAs, error) { return "", e }
			req := tt.req
			if req.path == "" {
				req = request{http.MethodPost, "/v1/answers", key("k1"), answerBody(reportPress(t, b, "u1"))}
			}
			rec := do(t, b.Handler(), req.method, req.path, tokenMain, req.header, req.body)
			if tt.status == http.StatusNoContent {
				if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
					t.Fatalf("answered %d %s", rec.Code, rec.Body)
				}
				return
			}
			p := wantProblem(t, rec, tt.status, tt.want)
			log := logs.String()
			if tt.mistake {
				if detailOf(p) != detailMessenger {
					t.Fatalf("detail %q", detailOf(p))
				}
				if !strings.Contains(log, "level=ERROR") || !strings.Contains(log,
					`msg="bridge: the messenger answered a class the operation does not declare"`) ||
					!strings.Contains(log, fmt.Sprintf("class=%s", tt.class)) {
					t.Fatalf("log = %s", log)
				}
				return
			}
			if detailOf(p) != "The account is missing." || strings.Contains(log, "level=ERROR") {
				t.Fatalf("detail %q, log %s", detailOf(p), log)
			}
		})
	}
}

func TestMapClassTable(t *testing.T) {
	for op, classes := range declared {
		for _, c := range classes {
			if got, mistake := mapClass(op, c); got != c || mistake {
				t.Errorf("%s %s mapped to %s", op, c, got)
			}
		}
	}
}

// secretToken is a token whose escaped forms differ from it.
const secretToken = "s3cret/token+x?y"

func TestOtherErrorsAreTransient(t *testing.T) {
	for _, wrap := range []bool{false, true} {
		t.Run(fmt.Sprint("wrapped ", wrap), func(t *testing.T) {
			b, m, _, logs := newTestBridge(t, func(c *Config) { c.Bots[0].Token = secretToken })
			err := errors.New("dial https://chat.example.org/bot" + secretToken + "/send?token=" +
				url.QueryEscape(secretToken) + "&p=" + url.PathEscape(secretToken) + " failed")
			if wrap {
				err = fmt.Errorf("posting: %w", err)
			}
			m.post = func(string, MessageInput) (MessagePosted, error) { return MessagePosted{}, err }
			rec := do(t, b.Handler(), http.MethodPost, "/v1/messages", secretToken, key("k1"), postBody)
			p := wantProblem(t, rec, http.StatusInternalServerError, ClassTransient)
			if detailOf(p) != detailMessenger {
				t.Fatalf("detail %q", detailOf(p))
			}
			if body := rec.Body.String(); strings.Contains(body, "dial") || strings.Contains(body, "s3cret") {
				t.Fatalf("the answer carries the error's text: %s", body)
			}
			log := logs.String()
			if !strings.Contains(log, "level=ERROR") || !strings.Contains(log, `msg="bridge: messenger call failed"`) ||
				!strings.Contains(log, "operation=postMessage") || !strings.Contains(log, "bot=main") ||
				!strings.Contains(log, "dial https://chat.example.org/bot[redacted]/send?token=[redacted]&p=[redacted]") {
				t.Fatalf("log = %s", log)
			}
			if strings.Contains(log, "s3cret") {
				t.Fatalf("the log carries the token: %s", log)
			}
		})
	}
}

func TestClientGoneIsWarned(t *testing.T) {
	b, m, _, logs := newTestBridge(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.edit = func(string, string, MessageContent) (MessageEdited, error) {
		cancel()
		return MessageEdited{}, ctx.Err()
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPatch, "/v1/messages/m1", strings.NewReader(editBody))
	r.Header.Set("Authorization", "Bearer "+tokenMain)
	r.Header.Set("Content-Type", "application/json")
	b.Handler().ServeHTTP(httptest.NewRecorder(), r.WithContext(ctx))
	if log := logs.String(); !strings.Contains(log, "level=WARN") ||
		!strings.Contains(log, `msg="bridge: messenger call failed"`) {
		t.Fatalf("log = %s", log)
	}
}

func TestPanicIsTransient(t *testing.T) {
	b, m, _, logs := newTestBridge(t, func(c *Config) { c.Bots[0].Token = secretToken })
	m.check = func(string, string) (string, error) { panic("cannot use " + secretToken + " now") }
	rec := do(t, b.Handler(), http.MethodPost, "/v1/channels/alerts/check", secretToken, nil, "")
	p := wantProblem(t, rec, http.StatusInternalServerError, ClassTransient)
	if detailOf(p) != detailMessenger || strings.Contains(rec.Body.String(), "cannot use") {
		t.Fatalf("answer %s", rec.Body)
	}
	log := logs.String()
	if !strings.Contains(log, "level=ERROR") || !strings.Contains(log, `msg="bridge: panic"`) ||
		!strings.Contains(log, "operation=checkChannel") || !strings.Contains(log, `panic="cannot use [redacted] now"`) {
		t.Fatalf("log = %s", log)
	}
	if strings.Contains(log, "s3cret") {
		t.Fatalf("the log carries the token: %s", log)
	}
}

func TestPanicAfterTheAnswerStarted(t *testing.T) {
	b, _, _, logs := newTestBridge(t, nil)
	h := b.recoverer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("late")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Fatalf("answered %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(logs.String(), `panic=late`) {
		t.Fatalf("log = %s", logs)
	}
}

func TestAbortHandlerPanicsOn(t *testing.T) {
	b, _, _, _ := newTestBridge(t, nil)
	h := b.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer func() {
		if v := recover(); v != http.ErrAbortHandler { //nolint:errorlint // the panic value itself is compared
			t.Fatalf("recovered %v", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	t.Fatal("http.ErrAbortHandler was swallowed")
}

func TestDetailIsCutAndRedacted(t *testing.T) {
	b, m, _, _ := newTestBridge(t, nil)
	m.check = func(string, string) (string, error) { return "", ForbiddenError(strings.Repeat("é", 1001)) }
	rec := do(t, b.Handler(), http.MethodPost, "/v1/channels/alerts/check", tokenMain, nil, "")
	if d := detailOf(decodeProblem(t, rec)); d != strings.Repeat("é", 1000) {
		t.Fatalf("detail of %d code points", len([]rune(d)))
	}
	m.check = func(string, string) (string, error) {
		return "", ForbiddenError("The token " + tokenMain + " or " + tokenOther + " was refused.")
	}
	rec = do(t, b.Handler(), http.MethodPost, "/v1/channels/alerts/check", tokenMain, nil, "")
	if d := detailOf(decodeProblem(t, rec)); d != "The token [redacted] or [redacted] was refused." {
		t.Fatalf("detail %q", d)
	}
	m.check = func(string, string) (string, error) { return "", ForbiddenError("") }
	rec = do(t, b.Handler(), http.MethodPost, "/v1/channels/alerts/check", tokenMain, nil, "")
	if p := decodeProblem(t, rec); p.Detail != nil {
		t.Fatalf("an empty detail was sent: %s", rec.Body)
	}
}

func TestErrorString(t *testing.T) {
	if got := TransientError("down").Error(); got != "transient: down" {
		t.Fatalf("Error() = %q", got)
	}
	if got := GoneError("").Error(); got != "gone" {
		t.Fatalf("Error() = %q", got)
	}
	if e := RateLimitedError(time.Second, "", ""); e.Scope != ProblemScopeChannel {
		t.Fatalf("scope %q", e.Scope)
	}
	err := fmt.Errorf("wrapped: %w", InvalidError("bad"))
	if e, ok := errors.AsType[*Error](err); !ok || e.Class != ClassInvalid {
		t.Fatal("a wrapped *Error is not found")
	}
}

func TestCutRunes(t *testing.T) {
	for _, tt := range []struct {
		s    string
		n    int
		want string
	}{{"abc", 5, "abc"}, {"abc", 2, "ab"}, {"ééé", 2, "éé"}, {"", 1, ""}, {"ab", 0, ""}} {
		if got := cutRunes(tt.s, tt.n); got != tt.want {
			t.Errorf("cutRunes(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
		}
	}
}

func TestRequestProblemOfOtherErrors(t *testing.T) {
	if got := requestProblem(errors.New("x")); got != "The request does not match api/bridge.yaml: it does not "+
		"match api/bridge.yaml." {
		t.Fatalf("requestProblem = %q", got)
	}
	if got := schemaProblem(errors.New("x")); got != "does not match api/bridge.yaml" {
		t.Fatalf("schemaProblem = %q", got)
	}
}

func TestDescribeSchemaErrorOf2020Messages(t *testing.T) {
	tests := []struct{ reason, pointer, want string }{
		{"at '': maxLength: got 1,025, want 1,024", "", "maximum string length is 1024"},
		{"at '/n': minimum: got 1, want 5", "/n", "number must be at least 5"},
		{"at '/n': multipleOf: got 3, want 2", "/n", "does not match api/bridge.yaml"},
		{`error at "/id": at '/id': 'a ': b' does not match pattern '^[!-~]+$'`, "/id",
			`string doesn't match the regular expression "^[!-~]+$"`},
		{"at '/notify': got string, want boolean", "/notify", "got string, want boolean"},
		{"at '': missing property 'kind'", "", "missing property 'kind'"},
		{"at '/url': 'SECRET' is not valid uri: no scheme", "/url", "string doesn't match its format"},
		{"at '/x': 'const' failed", "/x", "does not match api/bridge.yaml"},
		{"no location", "", "does not match api/bridge.yaml"},
		{"at 'unterminated", "", "does not match api/bridge.yaml"},
	}
	for _, tt := range tests {
		pointer, reason := describeSchemaError(&openapi3.SchemaError{Reason: tt.reason})
		if pointer != tt.pointer || reason != tt.want {
			t.Errorf("%q: %q %q, want %q %q", tt.reason, pointer, reason, tt.pointer, tt.want)
		}
		if strings.Contains(reason, "SECRET") {
			t.Errorf("%q: the reason carries the value", tt.reason)
		}
	}
	format := &openapi3.SchemaError{SchemaField: "format", Schema: &openapi3.Schema{Format: "uri"},
		Reason: `string doesn't match the format "uri" (parse "SECRET": invalid)`}
	if _, reason := describeSchemaError(format); reason != `string doesn't match the format "uri"` {
		t.Errorf("format: %q", reason)
	}
}
