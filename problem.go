// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
)

// Class is the class of an error, which tells Muster what to do about it.
type Class string

// The classes of the protocol, the values of ErrorClass in api/bridge.yaml.
const (
	// ClassTransient is a temporary failure, such as a messenger that is down or a timeout; Muster retries.
	ClassTransient = Class(ErrorClassTransient)
	// ClassRateLimited means the messenger or the bridge throttles; Muster waits RetryAfter.
	ClassRateLimited = Class(ErrorClassRateLimited)
	// ClassGone means the message, its Thread or the event no longer exists.
	ClassGone = Class(ErrorClassGone)
	// ClassForbidden means the channel does not exist, the bot lacks the rights there, or a person blocked the bot.
	ClassForbidden = Class(ErrorClassForbidden)
	// ClassInvalid means the request cannot succeed as sent.
	ClassInvalid = Class(ErrorClassInvalid)
)

// ReasonEditWindowClosed tells Muster that an edit failed as gone because the messenger allows no edits to a message
// that old, while the message itself still exists; Muster then posts a new Root message.
const ReasonEditWindowClosed = ProblemReasonEditWindowClosed

// Error is an answer of the messenger that the bridge classifies for Muster. A Messenger method returns one to answer
// its class; any other error is answered as transient, with a generic detail.
type Error struct {
	// Class is the class Muster acts on.
	Class Class
	// Detail is shown to the Admin as the reason, as untrusted text. It must not carry a secret; the library replaces
	// the bridge's tokens and push secrets in it anyway, and cuts it to 1000 characters.
	Detail string
	// RetryAfter is how long Muster waits, for ClassRateLimited; it is sent in whole seconds, rounded up.
	RetryAfter time.Duration
	// Scope is what the wait of ClassRateLimited holds: ProblemScopeChannel (the default) or ProblemScopeBot.
	Scope ProblemScope
	// Reason is ReasonEditWindowClosed on a ClassGone answer to an edit when the message is too old to edit; empty
	// otherwise. The library sends no other reason, and logs one as a mistake of the bridge.
	Reason ProblemReason
}

// Error returns the class and the detail.
func (e *Error) Error() string {
	if e.Detail == "" {
		return string(e.Class)
	}
	return string(e.Class) + ": " + e.Detail
}

// TransientError is a temporary failure: the messenger is down, a timeout. Muster retries.
func TransientError(detail string) *Error {
	return &Error{Class: ClassTransient, Detail: detail}
}

// RateLimitedError asks Muster to wait before the next request, for the channel or, with ProblemScopeBot, for every
// channel of the bot. An empty scope is ProblemScopeChannel.
func RateLimitedError(wait time.Duration, scope ProblemScope, detail string) *Error {
	if scope == "" {
		scope = ProblemScopeChannel
	}
	return &Error{Class: ClassRateLimited, Detail: detail, RetryAfter: wait, Scope: scope}
}

// GoneError says that the message or its Thread no longer exists. With ReasonEditWindowClosed it says instead that
// the message is past the messenger's edit window. Only the first reason is used.
func GoneError(detail string, reason ...ProblemReason) *Error {
	e := &Error{Class: ClassGone, Detail: detail}
	if len(reason) > 0 {
		e.Reason = reason[0]
	}
	return e
}

// ForbiddenError says that the channel does not exist, the bot lacks the rights there, or the person blocked the bot.
// A channel that does not exist is forbidden, never gone.
func ForbiddenError(detail string) *Error {
	return &Error{Class: ClassForbidden, Detail: detail}
}

// InvalidError says that the request cannot succeed as sent: too long, malformed, an unknown id.
func InvalidError(detail string) *Error {
	return &Error{Class: ClassInvalid, Detail: detail}
}

const (
	// detailMessenger is the detail of a transient answer to any error that is not an *Error, and to a panic.
	detailMessenger = "The bridge could not reach its messenger."
	detailStore     = "The bridge could not use its store."
	maxDetail       = 1000
)

var (
	classStatus = map[Class]int{
		ClassTransient:   http.StatusInternalServerError,
		ClassRateLimited: http.StatusTooManyRequests,
		ClassGone:        http.StatusNotFound,
		ClassForbidden:   http.StatusForbidden,
		ClassInvalid:     http.StatusBadRequest,
	}
	classTitle = map[Class]string{
		ClassTransient:   "Temporary failure",
		ClassRateLimited: "Too many requests",
		ClassGone:        "Gone",
		ClassForbidden:   "Forbidden",
		ClassInvalid:     "Invalid request",
	}
	// declared are the classes each operation declares in api/bridge.yaml, besides 401.
	declared = map[string][]Class{
		opGetInfo:           {ClassTransient},
		opPostMessage:       {ClassInvalid, ClassForbidden, ClassRateLimited, ClassTransient},
		opEditMessage:       {ClassInvalid, ClassForbidden, ClassGone, ClassRateLimited, ClassTransient},
		opDeleteMessage:     {ClassForbidden, ClassRateLimited, ClassTransient},
		opReplyInThread:     {ClassInvalid, ClassForbidden, ClassGone, ClassRateLimited, ClassTransient},
		opCheckChannel:      {ClassForbidden, ClassRateLimited, ClassTransient},
		opFindUsers:         {ClassInvalid, ClassRateLimited, ClassTransient},
		opSendDirectMessage: {ClassInvalid, ClassForbidden, ClassRateLimited, ClassTransient},
		opAnswerEvent:       {ClassInvalid, ClassGone, ClassRateLimited, ClassTransient},
		opPollEvents:        {ClassInvalid, ClassRateLimited, ClassTransient},
	}
)

// mapClass is the class an operation answers for the class c of the messenger: c itself when the operation declares
// it, else the one the protocol's meaning maps it to (a missing channel or account is forbidden, never gone), else
// transient, which is a mistake of the bridge. deleteMessage's gone is a success and handled before.
func mapClass(op string, c Class) (Class, bool) {
	if slices.Contains(declared[op], c) {
		return c, false
	}
	switch {
	case c == ClassGone && (op == opPostMessage || op == opSendDirectMessage || op == opCheckChannel),
		c == ClassInvalid && op == opCheckChannel:
		return ClassForbidden, false
	}
	return ClassTransient, true
}

// writeError writes e as its problem, with e's detail redacted and cut.
func (b *Bridge) writeError(w http.ResponseWriter, e *Error, secrets ...string) {
	b.writeProblem(w, classStatus[e.Class], e, secrets...)
}

// writeProblem writes e as a problem with the status, which is the class's own except for the 404 of an operation the
// bridge does not serve. rate_limited adds Retry-After and its scope; gone adds the one reason the protocol defines.
func (b *Bridge) writeProblem(w http.ResponseWriter, status int, e *Error, secrets ...string) {
	body := Problem{
		Class:  new(ErrorClass(e.Class)),
		Status: new(status),
		Title:  new(classTitle[e.Class]),
		Detail: presentOrNil(cutRunes(b.redact(e.Detail, secrets...), maxDetail)),
	}
	if e.Class == ClassRateLimited {
		body.Scope = new(ProblemScopeChannel)
		if e.Scope == ProblemScopeBot {
			body.Scope = new(ProblemScopeBot)
		}
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds(e.RetryAfter), 10))
	}
	if e.Class == ClassGone && e.Reason == ReasonEditWindowClosed {
		body.Reason = new(ReasonEditWindowClosed)
	}
	writeJSON(w, status, "application/problem+json", mustMarshal(body))
}

// writeUnauthorized answers a request without a bot's token, as the Unauthorized answer of the specification.
func (b *Bridge) writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeJSON(w, http.StatusUnauthorized, "application/problem+json", mustMarshal(Problem{
		Class:  new(ErrorClassForbidden),
		Status: new(http.StatusUnauthorized),
		Title:  new("Unauthorized"),
		Detail: new("The token is missing or wrong."),
	}))
}

// presentOrNil points at s, or is nil for an empty s, so that an empty detail is left out of the problem.
func presentOrNil(s string) *string {
	if s != "" {
		return &s
	}
	return nil
}

// retryAfterSeconds is d in whole seconds, rounded up; a negative d is 0.
func retryAfterSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64((d + time.Second - 1) / time.Second)
}

// cutRunes keeps the first n code points of s.
func cutRunes(s string, n int) string {
	seen := 0
	for at := range s {
		if seen == n {
			return s[:at]
		}
		seen++
	}
	return s
}

// requestProblem is the detail of a request that does not match api/bridge.yaml: where and why, never the value.
func requestProblem(err error) string {
	const prefix = "The request does not match api/bridge.yaml: "
	var re *openapi3filter.RequestError
	if !errors.As(err, &re) {
		return prefix + "it does not match api/bridge.yaml."
	}
	where := "body"
	if re.Parameter != nil {
		where = fmt.Sprintf("parameter %s in %s", re.Parameter.Name, re.Parameter.In)
	}
	var reason string
	var pe *openapi3filter.ParseError
	switch se := leafSchemaError(re.Err); {
	case se != nil:
		var pointer string
		pointer, reason = describeSchemaError(se)
		if pointer != "" {
			where += " " + pointer
		}
	case re.Parameter != nil && errors.As(re.Err, &pe):
		reason = pe.Reason
	case errors.Is(re.Err, openapi3filter.ErrInvalidRequired):
		reason = "value is required but missing"
	}
	if reason == "" {
		reason = re.Reason
	}
	if reason == "" {
		reason = "does not match api/bridge.yaml"
	}
	return cutRunes(prefix+where+": "+reason+".", maxDetail)
}

// schemaProblem is where and why a value does not match a schema of api/bridge.yaml, from the error of VisitJSON,
// never with the value: "/limits/max_body_length: number must be at least 500".
func schemaProblem(err error) string {
	se := leafSchemaError(err)
	if se == nil {
		return "does not match api/bridge.yaml"
	}
	pointer, reason := describeSchemaError(se)
	if pointer == "" {
		return reason
	}
	return pointer + ": " + reason
}

// leafSchemaError is the innermost *openapi3.SchemaError of err: the first one that wraps no other.
func leafSchemaError(err error) *openapi3.SchemaError {
	var leaf *openapi3.SchemaError
	var walk func(error) bool
	walk = func(err error) bool {
		for err != nil {
			if se, ok := err.(*openapi3.SchemaError); ok { //nolint:errorlint // the walk looks at each link itself
				leaf = se
			}
			switch e := err.(type) { //nolint:errorlint // the walk looks at each link itself
			case openapi3.MultiError:
				for _, inner := range e {
					if walk(inner) {
						return true
					}
				}
				return leaf != nil
			case interface{ Unwrap() []error }:
				for _, inner := range e.Unwrap() {
					if walk(inner) {
						return true
					}
				}
				return leaf != nil
			}
			err = errors.Unwrap(err)
		}
		return leaf != nil
	}
	walk(err)
	return leaf
}

var (
	// boundMessage is a message of the JSON Schema 2020-12 validator about a bound: "maxLength: got 513, want 512".
	boundMessage = regexp.MustCompile(`^([A-Za-z]+): got .*, want ([0-9.,eE+-]+)$`)
	// typeMessage is its message about a type: "got string, want boolean".
	typeMessage = regexp.MustCompile(`^got [a-z]+, want [a-z, ]+$`)
	boundWords  = map[string]string{
		"minLength":        "minimum string length is %s",
		"maxLength":        "maximum string length is %s",
		"minimum":          "number must be at least %s",
		"maximum":          "number must be at most %s",
		"exclusiveMinimum": "number must be more than %s",
		"exclusiveMaximum": "number must be less than %s",
		"minItems":         "minimum number of items is %s",
		"maxItems":         "maximum number of items is %s",
	}
)

// describeSchemaError is the JSON pointer and the reason of a leaf error, without the value it is about. The built-in
// validator of kin-openapi gives both apart; its JSON Schema 2020-12 validator, which kin-openapi uses for the schemas
// of a 3.1 document that have no $ref, gives one text "at '<pointer>': <message>", whose messages may quote the value.
func describeSchemaError(se *openapi3.SchemaError) (string, string) {
	if se.SchemaField != "" {
		pointer := ""
		if p := se.JSONPointer(); len(p) > 0 {
			pointer = "/" + strings.Join(p, "/")
		}
		if se.SchemaField == "format" && se.Schema != nil {
			return pointer, fmt.Sprintf("string doesn't match the format %q", se.Schema.Format)
		}
		return pointer, se.Reason
	}
	i := strings.Index(se.Reason, "at '")
	if i < 0 {
		return "", "does not match api/bridge.yaml"
	}
	rest := se.Reason[i+len("at '"):]
	j := strings.Index(rest, "': ")
	if j < 0 {
		return "", "does not match api/bridge.yaml"
	}
	pointer, message := rest[:j], rest[j+len("': "):]
	if m := boundMessage.FindStringSubmatch(message); m != nil {
		if words, ok := boundWords[m[1]]; ok {
			return pointer, fmt.Sprintf(words, strings.ReplaceAll(m[2], ",", ""))
		}
	}
	switch k := strings.LastIndex(message, " does not match pattern "); {
	case k >= 0:
		pattern := strings.Trim(message[k+len(" does not match pattern "):], "'")
		return pointer, fmt.Sprintf("string doesn't match the regular expression %q", pattern)
	case typeMessage.MatchString(message), strings.HasPrefix(message, "missing propert"):
		return pointer, message
	case strings.Contains(message, " is not valid "):
		return pointer, "string doesn't match its format"
	}
	return pointer, "does not match api/bridge.yaml"
}
