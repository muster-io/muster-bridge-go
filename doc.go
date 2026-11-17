// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

// Package bridge serves Muster's Bridge protocol, so that a bridge between Muster and a messenger that Muster does not
// support natively is only the calls to that messenger.
//
// A bridge is a small HTTP service. Muster calls it to post, edit and delete messages, reply in their Threads, check
// channels, look up people, send direct messages and answer people; the bridge reports what people do in the messenger
// — button presses, messages in Threads, deleted messages, Account link tokens — as events that Muster polls or that
// the bridge pushes. The protocol is api/bridge.yaml of Muster's repository; this package embeds a byte-for-byte copy
// of it, generates its Go types from it (Info, Capabilities, Limits, MessageInput, MessageContent, Layout, ThreadReplyInput,
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
// Serve it with an [net/http.Server] that has no WriteTimeout, or one above 75 seconds: pollEvents holds a request for
// up to 60 seconds (Muster asks for 25 and waits 15 more), and a shorter WriteTimeout cuts such a poll off without an
// answer, so that its events reach Muster only on a later poll. ReadHeaderTimeout and IdleTimeout are safe to set.
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
// Before it calls the Messenger, the library refuses as 400 invalid a request that does not match the specification
// (except for a layout that alone does not match: see Layout),
// a request body over 4 MiB, a body longer than Limits.MaxBodyLength characters, buttons sent to a bridge without
// buttons, more buttons than MaxPerMessage and button data longer than DataMaxBytes. It checks every answer of the
// Messenger against the specification too; one that does not match is logged and answered as transient.
//
// # Roles
//
// A bridge is a messenger, a gateway, or both: [Config.Roles] lists [RoleMessenger] and [RoleGateway], and nil is the
// messenger alone, which info leaves out. A gateway reaches a person outside a messenger, by phone call or SMS for
// example. The role messenger needs the [Messenger] given to [New]; the role gateway needs [Config.Gateway], and a
// bridge whose only role is gateway passes a nil Messenger; [New] refuses either role without its implementation, a
// Gateway or a Messenger without its role, and names the reason. A bridge without the role messenger answers 404 to
// the operations of the messenger and its reports ([Bridge.Press] and the others) are [ErrNotOffered].
//
//   - sendNotification calls [Gateway.SendNotification] with the contact, the text, the language and the optional
//     action of the notification. It carries an Idempotency-Key like postMessage: a repeated key answers the first
//     notification_id with replayed true without calling the Gateway. A text over 1,000 code points, action data over
//     64 bytes and a call on a bridge without the role are 400 invalid before the Gateway is called. The library keeps
//     the action's data of each notification, per bot, for 24 hours in the [Store], as an entry of its own: a key
//     of 78 characters that starts with a space, so that it is never an Idempotency-Key, which a Store keeps as it is.
//     The notification_id must be unique per bot. When the Store fails to keep the entry, the call still succeeds, the
//     failure is logged, and the notification can no longer be acknowledged.
//   - [Bridge.Acknowledge] reports that the person acknowledged a notification: it queues notification_acknowledged
//     with that data, for polling and push like any other event. A notification the bridge did not send to the bot, or
//     one sent without an action, is refused ([ErrUnknownNotification], [ErrNoAction]).
//   - answerEvent for that event calls [Gateway.Answer] with a [ReportedEvent] that has the NotificationID and the
//     Data. During a phone call Muster answers a pushed event before it answers the push, so a gateway that wants to
//     say the answer to the person still on the line uses the push mode and reads the answer inside Answer, while the
//     push request is open; give the bot a [PushTarget] for it.
//
// examples/gateway in the repository is a complete gateway bridge in one file that prints each notification and
// acknowledges it from a page.
//
// # Idempotency
//
// postMessage, replyInThread, sendDirectMessage, sendNotification and answerEvent carry an Idempotency-Key. The first successful answer
// of a key is kept for 24 hours per bot and answered again, with replayed true, without calling the Messenger,
// whatever the repeated body. A request whose key is still running is transient; a failed request keeps nothing, so
// its retry runs again. A request holds its key for two minutes; a Messenger method should return when its ctx ends.
// If one runs longer, a retry may claim the key and run as well, but the late request neither frees nor overwrites
// the retry's claim. [IdempotencyKeyFrom] gives the key to a Messenger whose messenger has idempotency of its own.
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
// # Layout
//
// A [MessageInput] and a [MessageContent] carry the message twice: as CommonMark in Body, and, for a Root message and a
// test message, as structured data in Layout, a [*Layout] that is nil when Muster sends none. A bridge renders either
// Body or Layout; one that ignores Layout is unaffected, and one that renders Layout renders Body when Layout is nil,
// as it is for a Storm summary, a Thread reply, a Root message that a template of the Route renders, and a message of
// an earlier version of Muster. The library passes Layout to [Messenger.PostMessage] and [Messenger.EditMessage]
// unchanged, after it has checked it against the specification. A Layout that does not match it, while the rest of the
// request does, is dropped: the message is accepted, the Messenger gets a nil Layout and renders Body, and the library
// logs one warning with where and why, never the content. Its values are plain text, never CommonMark: the
// bridge escapes them for its messenger. [Layout.Assignees] and [Layout.Owner] name people by the Ref of an entry of
// the envelope's Mentions, never by a marker in the text; Ref is nil where the message carries no Mention of the
// person, and always in an edit, and the bridge then writes the Name. The test mark of a test message is the first of
// its Notices. A [LayoutLink]'s Url may be an IRI.
//
// # Mentions
//
// [ReplaceMentions] and [Bridge.ReplaceMentions] turn the Mention markers of a body into what the bridge's messenger
// writes. Each entry of the envelope's Mentions has a Kind: [MentionKindMentionKindUser], which is also an entry
// without kind; [MentionKindMentionKindGroup], with the group's name in ExternalId and DisplayName; or
// [MentionKindMentionKindEveryone], with the symbolic name "channel", "here", "all" or "everyone" in both. A bridge
// declares them in info with [Config.GroupMentions] and [Config.EveryoneMentions], and Muster sends only what it
// declared. Muster decides from a copy of info that may be older than the bridge's own, so the library never refuses a
// Mention entry for its kind: [Bridge.ReplaceMentions] shows a group or everyone entry that the bridge did not declare
// as its marker's link text, without calling render. Kind is nil on the raw entries of a message and is set only on
// the entries that render gets, so a bridge that declares groups or everyone renders through [Bridge.ReplaceMentions].
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
