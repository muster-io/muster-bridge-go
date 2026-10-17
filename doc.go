// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

// Package bridge serves Muster's Bridge protocol, so that a bridge between Muster and a messenger that Muster does not
// support natively is only the calls to that messenger.
//
// A bridge is a small HTTP service. Muster calls it to post, edit and delete messages, reply in their Threads, check
// channels, look up people, send direct messages and answer people; the bridge reports what people do in the messenger
// — button presses, messages in Threads, deleted messages, Account link tokens — as events that Muster polls or that
// the bridge pushes. The protocol is api/bridge.yaml of Muster's repository; this package embeds a byte-for-byte copy
// of it, generates its Go types from it (Info, Capabilities, Limits, MessageInput, MessageContent, ThreadReplyInput,
// Person, Problem, the events and the rest) and validates every request and answer against it.
//
// The library does the protocol: the bots' tokens, request validation and the bridge's limits, Idempotency-Keys,
// problem answers, the event queue, long polling and signed pushes. The bridge author writes a [Messenger] and reports
// events:
//
//	b, err := bridge.New(bridge.Config{
//		Name: "my-bridge", Version: "1.0.0", MessengerName: "My messenger",
//		Bots:   []bridge.Bot{{Name: "main", Token: token}},
//		Limits: bridge.Limits{MaxBodyLength: 4000},
//		Capabilities: bridge.Capabilities{Buttons: &bridge.Capabilities_Buttons{
//			MaxPerMessage: 5, MaxPerRow: 5, DataMaxBytes: 64,
//			Styles: []bridge.ButtonStyle{bridge.ButtonStylePrimary, bridge.ButtonStyleDanger}}},
//	}, myMessenger)
//	if err != nil {
//		return err
//	}
//	defer b.Close(ctx)
//	http.Handle("/", b.Handler())
//
//	// When a person presses a button in the messenger:
//	err = b.Press(ctx, "main", bridge.Press{UpdateID: callbackID, MessageID: messageID, User: person, Data: data})
//
// examples/minimal in the repository is a complete bridge whose messenger lives in memory.
//
// # Operations
//
// [Bridge.Handler] serves every operation under /v1/; mount it under a path prefix with [net/http.StripPrefix].
// Each request carries one bot's token, compared in constant time with every bot's, and is that bot's: the Messenger
// is called with the bot's name, and its Idempotency-Keys and events are its own. A missing or wrong token is 401.
//
//   - getInfo answers the info that [New] built from the [Config].
//   - postMessage, editMessage, replyInThread, checkChannel and answerEvent call the [Messenger].
//   - findUsers calls [UserFinder], served when Config.UserLookup is not InfoUserLookupNone.
//   - sendDirectMessage calls [DirectMessenger], served with Capabilities.DirectMessages.
//   - deleteMessage calls [Deleter], served with Capabilities.Delete. A message that is gone is deleted: 204.
//   - pollEvents answers the queued events, served when Config.EventDelivery has poll (the default).
//
// An operation the bridge does not offer answers 404 invalid. [New] refuses a configuration whose features need an
// interface the Messenger lacks.
//
// Before it calls the Messenger, the library refuses as 400 invalid a request that does not match the specification,
// a request body over 1 MiB, a body longer than Limits.MaxBodyLength characters, buttons sent to a bridge without
// buttons, more buttons than MaxPerMessage and button data longer than DataMaxBytes. It checks every answer of the
// Messenger against the specification too; one that does not match is logged and answered as transient.
//
// # Idempotency
//
// postMessage, replyInThread, sendDirectMessage and answerEvent carry an Idempotency-Key. The first successful answer
// of a key is kept for 24 hours per bot and answered again, with replayed true, without calling the Messenger,
// whatever the repeated body. A request whose key is still running is transient; a failed request keeps nothing, so
// its retry runs again. [IdempotencyKeyFrom] gives the key to a Messenger whose messenger has idempotency of its own.
//
// # Errors
//
// A Messenger method returns an [*Error] to answer a class of the protocol: [TransientError], [RateLimitedError] (with
// the wait, sent as Retry-After, and the scope), [GoneError] (optionally with [ReasonEditWindowClosed]),
// [ForbiddenError] and [InvalidError], each with a detail for the Admin. Classes are answered with 500, 429, 404, 403
// and 400. A class the operation does not declare is mapped to one it does: gone from deleteMessage is a success;
// gone from postMessage, checkChannel and sendDirectMessage, and invalid from checkChannel, are forbidden; any other is
// transient, logged as a mistake of the bridge. Any other error, and a panic, is transient with the detail "The bridge
// could not reach its messenger.", and logged at level error. The library never logs a token, a push secret or an
// Account link token, and replaces the bots' tokens and push secrets in every detail and log line.
//
// # Events
//
// [Bridge.Press], [Bridge.ThreadMessage], [Bridge.MessageDeleted] and [Bridge.DirectMessage] queue an event for the
// bot and return at once, so that a bridge confirms an update to its messenger within its deadline independently of
// Muster. An event's id is derived from the bot, the event type and the messenger's own id of the update when the
// bridge gives one, so that an update the messenger delivers twice is queued once; it is random otherwise. A direct
// message becomes an account_link event only when its whole text, trimmed and after an optional "/start ", is 43
// characters of base64url; any other is ignored. Events are kept at least 24 hours. pollEvents answers the events
// after the cursor at once, or waits up to wait seconds (25 by default, at most 60) for one; a cursor the bridge did
// not give is invalid. answerEvent is gone for an event the bridge does not hold, and otherwise gives the Messenger the
// [ReportedEvent] it answers, with the Chat the bridge reported.
//
// # Push
//
// With a [PushTarget] for a bot, in Config.Bots or later with [Bridge.SetPush], its events are posted to the callback
// URL one at a time and in order, signed as Standard Webhooks specifies with the push secret: webhook-id (the event's
// id), webhook-timestamp and webhook-signature. A 2xx answer acknowledges an event and 400 drops it; any other answer,
// a timeout of 10 seconds or a network error is retried with exponential back-off from 1 second up to 5 minutes under
// the same webhook-id, for an hour, after which the event stays queued for polling only. Redirects are not followed.
// [Bridge.Close] stops the pushers.
//
// # Store
//
// A [Store] keeps the Idempotency-Keys and the queued events; [NewMemoryStore], the default, keeps them in memory, so
// a restart forgets them and Muster polls again without a cursor. A bridge that must keep them across restarts
// implements Store over its own database. Long polls and pushers are woken by the process that queued the event: with
// several processes over one shared Store, run each bot in one process only.
package bridge
