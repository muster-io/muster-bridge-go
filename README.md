# muster-bridge-go

The Go library for writing a bridge between [Muster](https://github.com/muster-io/muster) and a messenger it does not
support natively: the library serves Muster's bridge protocol (`api/bridge.yaml`) — tokens, validation, idempotency,
events and signed push — and the bridge author writes only the calls to their messenger. MIT licensed.

A bridge is a small HTTP service that Muster calls to post, edit and reply to messages, and that reports what people do
in the messenger — button presses, messages in Threads, Account link tokens — as events. Why the protocol exists and
how it is versioned is Muster's
[ADR-0017](https://github.com/muster-io/muster/blob/master/design/adr/0017-bridge-protocol-for-messengers-without-native-support.md);
the protocol itself is [`api/bridge.yaml`](https://github.com/muster-io/muster/blob/master/api/bridge.yaml), of which
this repository keeps a byte-for-byte copy.

## Install

```sh
go get github.com/muster-io/muster-bridge-go@latest
```

Go 1.26 or newer. The package is `bridge`. Versions stay `0.x` until Muster's first release; then `1.x` speaks
protocol version 1.

## Quick start

[`examples/minimal/main.go`](examples/minimal/main.go) is a whole bridge in one file: its messenger lives in memory and
writes every message the bot posts to the log. Run it with a token, the one a Bridge Connection in Muster would hold:

```sh
printf example-token > /tmp/bt
go run ./examples/minimal -token-file /tmp/bt
```

It listens on `127.0.0.1:18095` (`-listen`), knows the channels `alerts` and `ops` (`-channels`) and the people
`alice` and `bob` (`-users`), and offers every feature of the protocol, or only those that `BRIDGE_FEATURES` lists:
`buttons`, `thread_messages`, `direct_messages` (which also turns on `user_lookup` `login`), `private_answers`,
`native_mentions`, `quiet_messages`, `status_color`, `deletion_events` and `delete`. With `-push-url` and
`-push-secret-file` it pushes its events as well. In another terminal:

```sh
curl -s -o /dev/null -w '%{http_code}\n' 127.0.0.1:18095/v1/info          # 401: no token
curl -s -H 'Authorization: Bearer example-token' 127.0.0.1:18095/v1/info \
  | jq -c '{protocol_version, user_lookup}'                                 # {"protocol_version":"1","user_lookup":"login"}
```

Then let Muster's conformance check run every step of the protocol against it, from Muster's development image. The
check reaches the example on the host's loopback, which `--network host` gives only on Linux:

```sh
docker run --rm --network host -v /tmp/bt:/token:ro ghcr.io/muster-io/muster:edge bridge check \
  http://127.0.0.1:18095 --token-file /token --channel alerts --forbidden-channel nobot --handle alice \
  --no-interactive
```

```text
PASS info
PASS channel_check
PASS post
PASS replayed_key
PASS buttons
PASS edit
PASS thread_reply
PASS thread_ref
PASS forbidden_channel
PASS user_lookup
PASS direct_message
PASS delete
PASS poll
SKIP press: --no-interactive
SKIP push: --no-interactive
PASS answer
```

The heart of the example is a `bridge.Config`, a messenger and the handler:

```go
cfg := bridge.Config{
	Name:          "muster-bridge-go minimal example",
	Version:       "0.0.0",
	MessengerName: "Minimal example",
	Bots:          []bridge.Bot{{Name: "main", Token: token}},
	Limits:        bridge.Limits{MaxBodyLength: 4000},
	Capabilities: bridge.Capabilities{
		Buttons: &bridge.Capabilities_Buttons{MaxPerMessage: 5, MaxPerRow: 5, DataMaxBytes: 64,
			Styles: []bridge.ButtonStyle{bridge.ButtonStylePrimary, bridge.ButtonStyleSuccess,
				bridge.ButtonStyleNeutral, bridge.ButtonStyleDanger}},
		ThreadMessages: new(bridge.CapabilitiesThreadMessagesAll),
		DirectMessages: &bridge.Capabilities_DirectMessages{BotCanStart: true},
		Delete:         &bridge.Capabilities_Delete{},
	},
	UserLookup: bridge.InfoUserLookupLogin,
}
b, err := bridge.New(cfg, c) // c implements Messenger, UserFinder, DirectMessenger and Deleter

mux := http.NewServeMux()
mux.Handle("/v1/", b.Handler())
// Outside the protocol: press the first button of a message as a person, for scripts.
mux.HandleFunc("POST /demo/press", func(w http.ResponseWriter, r *http.Request) { c.demoPress(w, r, b) })
```

`POST /demo/press` `{"message_id": "wl7ffy-m2", "user": "alice"}` presses the first button of message `wl7ffy-m2` as
`alice`, so that a script can play the person of an interactive check. Like the rest of the example it has no
authentication of its own: keep it on loopback. Message ids start with a prefix that is random for each start of the
example, so that a restarted example never reuses an id that Muster still holds.

## Writing a bridge

1. Implement [`bridge.Messenger`](https://pkg.go.dev/github.com/muster-io/muster-bridge-go#Messenger): `PostMessage`,
   `EditMessage`, `ReplyInThread`, `CheckChannel` and `AnswerEvent`, each given the name of the bot whose token Muster
   sent. For the features you declare, also `UserFinder` (a `UserLookup` other than `none`), `DirectMessenger`
   (`Capabilities.DirectMessages`) and `Deleter` (`Capabilities.Delete`); `bridge.New` refuses a configuration whose
   features lack their interface, or whose `info` would not match the protocol, and names the reason.
2. Answer the messenger's failures with `bridge.TransientError`, `RateLimitedError` (with the wait and the scope),
   `GoneError` (with `bridge.ReasonEditWindowClosed` for an edit the messenger refuses only for the message's age),
   `ForbiddenError` or `InvalidError`. Any other error, and a panic, is answered as `transient` with a generic detail
   and logged without the token.
3. Mount `b.Handler()` at the bridge's base URL, under a path prefix with `http.StripPrefix`. Serve it with no
   `WriteTimeout`, or one above 75 seconds: `pollEvents` holds a request for up to 60 seconds, and a shorter
   `WriteTimeout` cuts such a poll off without an answer, so that its events reach Muster late.
4. Report what the messenger tells you: `b.Press`, `b.ThreadMessage`, `b.MessageDeleted` and `b.DirectMessage`. Each
   returns at once, so you confirm a press to your messenger within its deadline whatever Muster does; give the
   messenger's own update id, so that an update it delivers twice is queued once. A direct message that is an Account
   link token becomes an `account_link` event; any other direct message is ignored.
5. Turn Mention markers into native mentions or plain names with `bridge.ReplaceMentions`.
6. For pushes, give a bot a `PushTarget` — the callback URL and the push secret that its Bridge Connection in Muster
   shows — in `Config.Bots` or later with `b.SetPush`; `b.Close` stops the pushers.
7. `bridge.NewMemoryStore`, the default `Store`, forgets Idempotency-Keys and queued events on a restart, and holds a
   day of events and answers in memory; implement `Store` over your database to keep them. Long polls and pushes are
   woken in the process that queued an event, so with several processes over one store, run each bot in one process
   only.

The package documentation, on [pkg.go.dev](https://pkg.go.dev/github.com/muster-io/muster-bridge-go) and in
[`doc.go`](doc.go), describes what the library does for every operation; [`example_test.go`](example_test.go) has
runnable examples.

## Checking a bridge

`muster bridge check <url> --token-file <file> --channel <id>` runs every step of the protocol against a bridge and
prints `PASS`, `FAIL` or `SKIP` for each; `--forbidden-channel` and `--handle` enable the steps that need them. Without
`--no-interactive` it also waits for a person to press a button (`press`) and, for a bridge that pushes, receives the
push (`push`): it prints `PUSH <url>` once its listener (`--push-listen`) is up — point the bridge's push target there,
with the secret of `--push-secret-file` — and `WAIT press: …, message_id <id>` before it waits. `make conformance`
plays that person through the example's `/demo/press`; do the same in your bridge's tests with your messenger's own
means.

## The protocol copy

[`api/bridge.yaml`](api/bridge.yaml) is a copy of Muster's, never edited here: the library embeds it to validate
requests and answers, and [`models.gen.go`](models.gen.go) is generated from it. `make spec-check` compares it with the
one on Muster's `master` byte for byte, every day in CI; `make spec-sync` takes the new one and regenerates the models.

## Development

The Makefile is the only entry point; `make help` lists the targets.

| Target | Does |
|---|---|
| `make fmt` | formats the code (gofmt and goimports through golangci-lint) |
| `make lint` | checks the licence headers, `go.mod` and `go.sum`, and the code with golangci-lint |
| `make test` | runs the tests with the race detector and the coverage gate of 90 % of the package (`models.gen.go` excluded) |
| `make generate`, `make generate-check` | generates `models.gen.go` from `api/bridge.yaml`; fails when it is stale |
| `make spec-check`, `make spec-sync` | compares `api/bridge.yaml` with Muster's; replaces it and regenerates |
| `make licenses` | fails on a linked dependency outside MIT, MIT-0, BSD-2-Clause, BSD-3-Clause, Apache-2.0, ISC, 0BSD, Unlicense and CC0-1.0 |
| `make vulncheck` | finds vulnerable Go code that is called (govulncheck) |
| `make build` | builds the package and `bin/minimal` |
| `make ci` | `lint`, `generate-check`, `spec-check`, `licenses`, `test` and `build`, as a pull request runs them |
| `make conformance` | runs `muster bridge check` from `ghcr.io/muster-io/muster:edge` (`MUSTER_IMAGE`) against the example three times: every feature, only `thread_messages`, and interactively with push; Linux with Docker only |
| `make clean` | removes `bin/`: the example, the coverage profiles and the installed tools |

The pinned tools (golangci-lint, go-licenses, govulncheck, oapi-codegen) are built into `bin/tools/` on first use.

## Contributing, security and licence

[CONTRIBUTING.md](CONTRIBUTING.md) says how to contribute, [AGENTS.md](AGENTS.md) is the guide for everyone who
changes the code, and [SECURITY.md](SECURITY.md) how to report a vulnerability privately. The library is under the
[MIT licence](LICENSE); contributions are accepted under it, without a CLA.
