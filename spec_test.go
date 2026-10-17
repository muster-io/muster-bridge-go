// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
)

func testProtocol(t testing.TB) *protocol {
	t.Helper()
	p, err := loadProtocol()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEmbeddedSpecLoadsAndValidates(t *testing.T) {
	file, err := os.ReadFile("api/bridge.yaml")
	if err != nil || !bytes.Equal(file, specYAML) {
		t.Fatalf("the embedded spec is not api/bridge.yaml (%v)", err)
	}
	doc, err := openapi3.NewLoader().LoadFromData(specYAML)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("api/bridge.yaml is not a valid OpenAPI document: %v", err)
	}
	if !doc.IsOpenAPI31OrLater() || doc.Info.License == nil || doc.Info.License.Identifier != "MIT" {
		t.Fatal("api/bridge.yaml is not the 3.1 document under MIT")
	}
}

func TestEveryOperationIsServed(t *testing.T) {
	doc := testProtocol(t).doc
	var spec, table []string
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			spec = append(spec, method+" "+path+" "+op.OperationID)
		}
	}
	for _, op := range operations {
		table = append(table, op.method+" "+op.path+" "+op.id)
		if _, ok := declared[op.id]; !ok {
			t.Errorf("%s has no declared classes", op.id)
		}
	}
	slices.Sort(spec)
	slices.Sort(table)
	if !slices.Equal(spec, table) {
		t.Fatalf("api/bridge.yaml has the operations\n%s\nthe library routes\n%s", strings.Join(spec, "\n"),
			strings.Join(table, "\n"))
	}
}

func TestDeclaredClassesMatchSpec(t *testing.T) {
	statusClass := map[string]Class{"400": ClassInvalid, "403": ClassForbidden, "404": ClassGone,
		"429": ClassRateLimited, "500": ClassTransient}
	for id, r := range testProtocol(t).routes {
		var classes []Class
		for status := range r.route.Operation.Responses.Map() {
			if c, ok := statusClass[status]; ok {
				classes = append(classes, c)
			} else if status != "401" && status[0] != '2' {
				t.Errorf("%s declares %s, which no class has", id, status)
			}
		}
		want := slices.Clone(declared[id])
		slices.Sort(classes)
		slices.Sort(want)
		if !slices.Equal(classes, want) {
			t.Errorf("%s declares %v in api/bridge.yaml and %v in the library", id, classes, want)
		}
		for c, status := range classStatus {
			if slices.Contains(want, c) && r.route.Operation.Responses.Status(status) == nil {
				t.Errorf("%s answers %s with %d, which it does not declare", id, c, status)
			}
		}
	}
}

// pathParams are the decoded path parameters of an escaped path matched by the template of api/bridge.yaml.
func pathParams(template, escaped string) map[string]string {
	params := map[string]string{}
	names, values := strings.Split(template, "/"), strings.Split(escaped, "/")
	for i, name := range names {
		if strings.HasPrefix(name, "{") && i < len(values) {
			v, _ := url.PathUnescape(values[i])
			params[strings.Trim(name, "{}")] = v
		}
	}
	return params
}

// checkExchange validates a request, when it is meant to be valid, and its answer against api/bridge.yaml.
func checkExchange(t *testing.T, op string, r *http.Request, body []byte, rec *httptest.ResponseRecorder,
	validRequest bool,
) {
	t.Helper()
	route := testProtocol(t).routes[op].route
	req := r.Clone(context.Background())
	req.Body = io.NopCloser(bytes.NewReader(body))
	in := &openapi3filter.RequestValidationInput{Request: req, Route: route,
		PathParams: pathParams(route.Path, r.URL.EscapedPath()),
		Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}}
	if validRequest {
		if err := openapi3filter.ValidateRequest(context.Background(), in); err != nil {
			t.Fatalf("the request does not match api/bridge.yaml: %v", err)
		}
	}
	out := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: rec.Code,
		Header: rec.Header(), Options: &openapi3filter.Options{IncludeResponseStatus: true}}
	out.SetBodyBytes(rec.Body.Bytes())
	if err := openapi3filter.ValidateResponse(context.Background(), out); err != nil {
		t.Fatalf("the answer %d %s does not match api/bridge.yaml: %v", rec.Code, rec.Body, err)
	}
}

type exchange struct {
	name         string
	op           string
	method, path string
	body         string
	token        string
	err          error
	setup        func(t *testing.T, b *Bridge, m *fakeMessenger, s *failingStore)
	status       int
	invalid      bool
}

func TestRequestsAndAnswersMatchSpec(t *testing.T) {
	id := eventIDOf("main", EventTypePress, "u1")
	running := KeyClaim{State: KeyRunning}
	forbidden := ForbiddenError("The bot has not joined this channel.")
	limited := RateLimitedError(1500*time.Millisecond, ProblemScopeBot, "Slow down.")
	gone := GoneError("That message is gone.")
	tooOld := GoneError("The message can no longer be edited.", ReasonEditWindowClosed)
	invalid := InvalidError("This handle cannot be searched for.")
	broken := errors.New("the messenger's socket broke")
	replay := func(path, body string) func(*testing.T, *Bridge, *fakeMessenger, *failingStore) {
		return func(t *testing.T, b *Bridge, _ *fakeMessenger, _ *failingStore) {
			do(t, b.Handler(), http.MethodPost, path, tokenMain, key("k1"), body)
		}
	}
	runningKey := func(_ *testing.T, _ *Bridge, _ *fakeMessenger, s *failingStore) { s.claimAs(&running) }
	failingEvents := func(_ *testing.T, _ *Bridge, _ *fakeMessenger, s *failingStore) {
		s.fail("EventsAfter", errors.New("down"))
	}
	everyEvent := func(t *testing.T, b *Bridge, _ *fakeMessenger, _ *failingStore) {
		ctx := context.Background()
		for _, err := range []error{
			b.Press(ctx, "main", Press{UpdateID: "1", MessageID: "m1", User: alice, Data: "d"}),
			b.ThreadMessage(ctx, "main", ThreadMessage{UpdateID: "2", MessageID: "m1", User: alice, Text: "ack"}),
			b.ThreadMessage(ctx, "main", ThreadMessage{UpdateID: "3", MessageID: "m1", ThreadRef: "t-m1",
				User: Person{ExternalId: "u-bob", DisplayName: "Bob"}, Text: "ack"}),
			b.DirectMessage(ctx, "main", DirectMessage{UpdateID: "4", User: alice, Text: linkToken}),
			b.MessageDeleted(ctx, "main", MessageDeleted{UpdateID: "5", MessageID: "m1"}),
		} {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	post, edit, reply := "/v1/messages", "/v1/messages/m1", "/v1/messages/m1/thread-replies"
	check, users, dm, answers, events := "/v1/channels/alerts/check", "/v1/users", "/v1/direct-messages",
		"/v1/answers", "/v1/events"
	long := withBody(postBody, strings.Repeat("x", 501))
	rows := []exchange{
		{name: "200", op: opGetInfo, method: http.MethodGet, path: "/v1/info", status: 200},
		{name: "401", op: opGetInfo, method: http.MethodGet, path: "/v1/info", token: "wrong", status: 401},

		{name: "201", op: opPostMessage, method: http.MethodPost, path: post, body: postBody, status: 201},
		{name: "201 replayed", op: opPostMessage, method: http.MethodPost, path: post, body: postBody, status: 201,
			setup: replay(post, postBody)},
		{name: "400 spec", op: opPostMessage, method: http.MethodPost, path: post, body: "{}", status: 400,
			invalid: true},
		{name: "400 limit", op: opPostMessage, method: http.MethodPost, path: post, body: long, status: 400},
		{name: "401", op: opPostMessage, method: http.MethodPost, path: post, body: postBody, token: "wrong",
			status: 401},
		{name: "403", op: opPostMessage, method: http.MethodPost, path: post, body: postBody, err: forbidden,
			status: 403},
		{name: "403 from gone", op: opPostMessage, method: http.MethodPost, path: post, body: postBody, err: gone,
			status: 403},
		{name: "429", op: opPostMessage, method: http.MethodPost, path: post, body: postBody, err: limited,
			status: 429},
		{name: "500", op: opPostMessage, method: http.MethodPost, path: post, body: postBody, err: broken,
			status: 500},
		{name: "500 running", op: opPostMessage, method: http.MethodPost, path: post, body: postBody, status: 500,
			setup: runningKey},

		{name: "200", op: opEditMessage, method: http.MethodPatch, path: edit, body: editBody, status: 200},
		{name: "400", op: opEditMessage, method: http.MethodPatch, path: edit, body: `{"status":"x"}`, status: 400,
			invalid: true},
		{name: "401", op: opEditMessage, method: http.MethodPatch, path: edit, body: editBody, token: "wrong",
			status: 401},
		{name: "403", op: opEditMessage, method: http.MethodPatch, path: edit, body: editBody, err: forbidden,
			status: 403},
		{name: "404", op: opEditMessage, method: http.MethodPatch, path: edit, body: editBody, err: gone, status: 404},
		{name: "404 edit window", op: opEditMessage, method: http.MethodPatch, path: edit, body: editBody,
			err: tooOld, status: 404},
		{name: "429", op: opEditMessage, method: http.MethodPatch, path: edit, body: editBody, err: limited,
			status: 429},
		{name: "500", op: opEditMessage, method: http.MethodPatch, path: edit, body: editBody, err: broken,
			status: 500},

		{name: "204", op: opDeleteMessage, method: http.MethodDelete, path: edit, status: 204},
		{name: "204 gone", op: opDeleteMessage, method: http.MethodDelete, path: edit, err: gone, status: 204},
		{name: "401", op: opDeleteMessage, method: http.MethodDelete, path: edit, token: "wrong", status: 401},
		{name: "403", op: opDeleteMessage, method: http.MethodDelete, path: edit, err: forbidden, status: 403},
		{name: "429", op: opDeleteMessage, method: http.MethodDelete, path: edit, err: limited, status: 429},
		{name: "500", op: opDeleteMessage, method: http.MethodDelete, path: edit, err: broken, status: 500},

		{name: "201", op: opReplyInThread, method: http.MethodPost, path: reply, body: replyBody, status: 201},
		{name: "400", op: opReplyInThread, method: http.MethodPost, path: reply, body: "{}", status: 400,
			invalid: true},
		{name: "401", op: opReplyInThread, method: http.MethodPost, path: reply, body: replyBody, token: "wrong",
			status: 401},
		{name: "403", op: opReplyInThread, method: http.MethodPost, path: reply, body: replyBody, err: forbidden,
			status: 403},
		{name: "404", op: opReplyInThread, method: http.MethodPost, path: reply, body: replyBody, err: gone,
			status: 404},
		{name: "429", op: opReplyInThread, method: http.MethodPost, path: reply, body: replyBody, err: limited,
			status: 429},
		{name: "500", op: opReplyInThread, method: http.MethodPost, path: reply, body: replyBody, err: broken,
			status: 500},

		{name: "200", op: opCheckChannel, method: http.MethodPost, path: check, status: 200},
		{name: "401", op: opCheckChannel, method: http.MethodPost, path: check, token: "wrong", status: 401},
		{name: "403", op: opCheckChannel, method: http.MethodPost, path: check, err: forbidden, status: 403},
		{name: "403 from gone", op: opCheckChannel, method: http.MethodPost, path: check, err: gone, status: 403},
		{name: "403 from invalid", op: opCheckChannel, method: http.MethodPost, path: check, err: invalid,
			status: 403},
		{name: "429", op: opCheckChannel, method: http.MethodPost, path: check, err: limited, status: 429},
		{name: "500", op: opCheckChannel, method: http.MethodPost, path: check, err: broken, status: 500},

		{name: "200 found", op: opFindUsers, method: http.MethodGet, path: users + "?handle=alice", status: 200},
		{name: "200 nobody", op: opFindUsers, method: http.MethodGet, path: users + "?handle=nobody", status: 200},
		{name: "400 spec", op: opFindUsers, method: http.MethodGet, path: users + "?handle=", status: 400,
			invalid: true},
		{name: "400 finder", op: opFindUsers, method: http.MethodGet, path: users + "?handle=a%20b", err: invalid,
			status: 400},
		{name: "401", op: opFindUsers, method: http.MethodGet, path: users + "?handle=alice", token: "wrong",
			status: 401},
		{name: "429", op: opFindUsers, method: http.MethodGet, path: users + "?handle=alice", err: limited,
			status: 429},
		{name: "500", op: opFindUsers, method: http.MethodGet, path: users + "?handle=alice", err: broken,
			status: 500},

		{name: "201", op: opSendDirectMessage, method: http.MethodPost, path: dm, body: dmBody, status: 201},
		{name: "400", op: opSendDirectMessage, method: http.MethodPost, path: dm, body: `{"body":""}`, status: 400,
			invalid: true},
		{name: "401", op: opSendDirectMessage, method: http.MethodPost, path: dm, body: dmBody, token: "wrong",
			status: 401},
		{name: "403", op: opSendDirectMessage, method: http.MethodPost, path: dm, body: dmBody, err: forbidden,
			status: 403},
		{name: "429", op: opSendDirectMessage, method: http.MethodPost, path: dm, body: dmBody, err: limited,
			status: 429},
		{name: "500", op: opSendDirectMessage, method: http.MethodPost, path: dm, body: dmBody, err: broken,
			status: 500},

		{name: "200", op: opAnswerEvent, method: http.MethodPost, path: answers, body: answerBody(id), status: 200},
		{name: "400", op: opAnswerEvent, method: http.MethodPost, path: answers, body: `{"text":""}`, status: 400,
			invalid: true},
		{name: "401", op: opAnswerEvent, method: http.MethodPost, path: answers, body: answerBody(id), token: "wrong",
			status: 401},
		{name: "404", op: opAnswerEvent, method: http.MethodPost, path: answers, body: answerBody("nope"),
			status: 404},
		{name: "429", op: opAnswerEvent, method: http.MethodPost, path: answers, body: answerBody(id), err: limited,
			status: 429},
		{name: "500", op: opAnswerEvent, method: http.MethodPost, path: answers, body: answerBody(id), err: broken,
			status: 500},

		{name: "200 every event", op: opPollEvents, method: http.MethodGet, path: events + "?wait=0", status: 200,
			setup: everyEvent},
		{name: "200 empty", op: opPollEvents, method: http.MethodGet, path: events + "?wait=0", status: 200},
		{name: "400 cursor", op: opPollEvents, method: http.MethodGet, path: events + "?cursor=x", status: 400},
		{name: "400 wait", op: opPollEvents, method: http.MethodGet, path: events + "?wait=61", status: 400,
			invalid: true},
		{name: "401", op: opPollEvents, method: http.MethodGet, path: events, token: "wrong", status: 401},
		{name: "500", op: opPollEvents, method: http.MethodGet, path: events + "?wait=0", status: 500,
			setup: failingEvents},
	}
	covered := map[string]bool{}
	for _, row := range rows {
		covered[row.op] = true
		t.Run(row.op+" "+row.name, func(t *testing.T) {
			store := newFailingStore()
			b, m, _, _ := newTestBridge(t, func(c *Config) { c.Store = store })
			if row.op == opAnswerEvent {
				reportPress(t, b, "u1")
			}
			if row.setup != nil {
				row.setup(t, b, m, store)
			}
			m.err = row.err
			token := tokenMain
			if row.token != "" {
				token = row.token
			}
			r := httptest.NewRequestWithContext(t.Context(), row.method, row.path, strings.NewReader(row.body))
			r.Header.Set("Authorization", "Bearer "+token)
			if row.body != "" {
				r.Header.Set("Content-Type", "application/json")
			}
			if row.op == opPostMessage || row.op == opReplyInThread || row.op == opSendDirectMessage ||
				row.op == opAnswerEvent {
				r.Header.Set("Idempotency-Key", "k1")
			}
			rec := httptest.NewRecorder()
			b.Handler().ServeHTTP(rec, r)
			if rec.Code != row.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, row.status, rec.Body)
			}
			checkExchange(t, row.op, r, []byte(row.body), rec, !row.invalid && row.token == "")
		})
	}
	for _, op := range operations {
		if !covered[op.id] {
			t.Errorf("no exchange of %s", op.id)
		}
	}
}

func TestInfoMatchesSpec(t *testing.T) {
	b, _, _, _ := newTestBridge(t, func(c *Config) {
		c.LinkURLTemplate = "https://chat.example.org/bots/muster?start={token}"
		c.Limits = Limits{MaxBodyLength: maxBodyLengthCap, EditMaxAgeSeconds: new(172800),
			SendPerSecond: new(float32(30)), SendPerSecondPerChannel: new(float32(0.5))}
		c.Capabilities.Buttons.Styles = []ButtonStyle{ButtonStylePrimary, ButtonStyleDanger, ButtonStyleNeutral,
			ButtonStyleSuccess}
		c.Capabilities.PrivateAnswers = &[]CapabilitiesPrivateAnswers{CapabilitiesPrivateAnswersEphemeral,
			CapabilitiesPrivateAnswersPopup}
		c.Capabilities.Delete.MaxAgeSeconds = new(86400)
	})
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/info", nil)
	r.Header.Set("Authorization", "Bearer "+tokenMain)
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, r)
	checkExchange(t, opGetInfo, r, nil, rec, true)
}

func TestPushesMatchSpec(t *testing.T) {
	clk := newFakeClock()
	rc := newCallback(t, clk, testSecret)
	b, _, _, _ := newTestBridge(t, func(c *Config) { c.clock = clk; c.Bots[0].Push = rc.target() })
	ctx := context.Background()
	for _, err := range []error{
		b.Press(ctx, "main", Press{UpdateID: "1", MessageID: "m1", User: alice, Data: "d"}),
		b.ThreadMessage(ctx, "main", ThreadMessage{UpdateID: "2", MessageID: "m1", User: alice, Text: "ack"}),
		b.ThreadMessage(ctx, "main", ThreadMessage{UpdateID: "3", MessageID: "m1", ThreadRef: "t", User: alice}),
		b.DirectMessage(ctx, "main", DirectMessage{UpdateID: "4", User: alice, Text: linkToken}),
		b.MessageDeleted(ctx, "main", MessageDeleted{UpdateID: "5", MessageID: "m1"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	doc := testProtocol(t).doc
	item := doc.Webhooks["bridgeEvent"]
	route := &routers.Route{Spec: doc, Path: "bridgeEvent", PathItem: item, Method: http.MethodPost,
		Operation: item.Post}
	types := map[string]bool{}
	for range 5 {
		p := rc.next()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/callbacks/bridge/c1", bytes.NewReader(p.body))
		r.Header = p.header
		if err := openapi3filter.ValidateRequest(ctx, &openapi3filter.RequestValidationInput{Request: r,
			Route: route, Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}}); err != nil {
			t.Fatalf("the push %s does not match webhooks.bridgeEvent: %v", p.body, err)
		}
		types[decodeJSON(t, p.body)["type"].(string)] = true
	}
	if len(types) != 4 {
		t.Fatalf("pushed types %v", types)
	}
}
