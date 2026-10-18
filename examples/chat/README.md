# The example bridge

A bridge with a messenger of its own: a small chat that lives in memory, with a web page, served on one listener
beside the Bridge protocol. It offers every feature of the protocol, so that Muster can be tried, and tested, with a
messenger that runs anywhere. It is built on the package `bridge` of this module and the standard library only, and is
MIT licensed like the rest of the module. It is a reference, not a product: it keeps everything in memory and its page
has no authentication of its own.

## What the page shows

- **Channels** (`-channels`, `#alerts` and `#ops` by default): the Root messages of the bot with their title, link,
  status, body and buttons, each with its Thread, where you write as the person you picked. A Root message shows its
  status colour on its edge, or the status emoji before its title when `status_color` is off; a message that notifies
  no one is marked `quiet`.
- **Buttons** in their style hints (primary, success, neutral, danger), three to a row. A press is confirmed on the
  page at once and reported to Muster; an answer only one person sees appears under the message, for that person, as
  "Only you can see this".
- **Direct**: the direct chat between the person and the bot, where Account link tokens are pasted and the bot's
  direct messages, link codes included, arrive.
- **You are**: the person (`-users`) you act as; presses, messages and deletions are theirs.
- **Features**: what the example offers, after `BRIDGE_FEATURES`.
- **Event log**: every protocol call with its status, and every report of the page (a press, a Thread message, a
  deletion, a direct message) with its outcome: `reported`, or `not_offered` when the feature it needs is off. Equal
  lines in a row are counted in one, and the text of a direct message is never written to it.

How each feature changes the page:

| Feature | On | Off |
|---|---|---|
| `buttons` | Muster sends buttons, shown in rows of three | no buttons; people use Text commands in Threads |
| `thread_messages` | a person's message in a Thread is reported | it is posted on the page only |
| `direct_messages` | Muster finds people by login and writes to them; Account links by code work | `user_lookup` is `none`: accounts link by token only |
| `private_answers` | the answer to a press appears under the message for the person who pressed | it appears in the Thread |
| `native_mentions` | a Mention marker becomes a highlighted name | it stays plain text |
| `quiet_messages` | a message that notifies no one is marked `quiet` | every message notifies |
| `status_color` | the status colour on the message's edge | the status emoji before the title |
| `deletion_events` | a deletion on the page is reported | it is not |
| `delete` | Muster can delete the bot's messages; they stay as "This message was deleted." | Muster cannot |

## Run it

From source, with Go 1.26 or newer, in the module's root:

```sh
printf example-token > /tmp/bt
go run ./examples/chat -token-file /tmp/bt
```

It listens on `127.0.0.1:18095`: the page is at `http://127.0.0.1:18095/`, the Bridge protocol under `/v1/`.

From the image, published for linux/amd64 and linux/arm64 as `vX.Y.Z` for each release, and as `edge` and
`sha-<7 hex digits>` for each commit on `master`:

```sh
docker run --rm -p 8095:8095 -e BRIDGE_TOKEN=example-token ghcr.io/muster-io/muster-example-bridge:<version>
```

The image listens on `:8095` and runs as the user 65532, so a token or push secret file must be readable by that user,
for example with mode 0644. Arguments are appended to the image's `-listen :8095`, and the last `-listen` wins:

```sh
chmod 0644 /tmp/bt
docker run --rm --network host -v /tmp/bt:/token:ro ghcr.io/muster-io/muster-example-bridge:<version> \
  -listen 127.0.0.1:18095 -token-file /token
```

`docker run --rm ghcr.io/muster-io/muster-example-bridge:<version> -help` lists the flags and the environment. The
version shown there and in `info` is the image's tag; built from source, the version Go derives from the module or its
checkout, else `devel`.

## Next to `muster dev`

1. Start `muster dev` from a Muster checkout (`make dev`) and sign in.
2. Start the example on the host's loopback at `127.0.0.1:18095`: from source as above, which listens there by
   default, or with the image, `--network host` (Linux) and `-listen 127.0.0.1:18095` as above; without that flag
   the image listens on `:8095` instead. `muster dev` lets Muster call loopback addresses. Elsewhere, the
   Organization's Outbound address policy blocks loopback, and the strict policy private addresses too: allow the
   example's address there.
3. In Muster, create a Bridge Connection with the base URL `http://127.0.0.1:18095`, the token `example-token` and the
   event delivery Polling. "Check connection" shows "Messenger: Example chat" and the features it offers.
4. Create a Destination on that Connection for the channel `alerts`; Muster shows it as `#alerts`. "Send test message"
   makes "🧪 Test message" appear in `#alerts` on the page.
5. Alert Groups routed to the Destination appear in `#alerts`, one Root message each, edited as they change. A press
   acts for the Muster User whose account is linked to the person who pressed, so link an account first.

**Push.** For the event delivery Push, choose a push secret, `whsec_` followed by base64 (for example
`printf 'whsec_%s\n' "$(head -c 32 /dev/urandom | base64)"`), enter it in the Bridge Connection and save it, and
restart the example with `BRIDGE_PUSH_URL` set to the callback address that the Connection then shows, and the secret
in `BRIDGE_PUSH_SECRET` or in the file of `-push-secret-file`. The example must reach that address; with the image,
`--network host` gives it Muster's loopback.

**Account links.** In Muster, under Account links, link through the Bridge Connection:

- **By token**: copy the token, pick the person under "You are" on the page, open their direct chat with the bot under
  "Direct", paste the token and send it. The bot answers in that chat.
- **By code** (with `direct_messages`): enter the person's login, such as `alice`, as the handle and send the code;
  it arrives in that person's direct chat on the page, and you enter it in Muster.

## Flags and environment

| Flag | Default | Does |
|---|---|---|
| `-listen` | `127.0.0.1:18095`; `:8095` in the image | the address of the page and the Bridge protocol |
| `-token-file` | | the file that holds the bot's token, instead of `BRIDGE_TOKEN` |
| `-push-secret-file` | | the file that holds the push secret, instead of `BRIDGE_PUSH_SECRET` |
| `-channels` | `alerts,ops` | the channels the bot is in, comma-separated; any other channel is forbidden |
| `-users` | `alice:Alice Smith,bob:Bob Brown` | the people, as `login:display name`, comma-separated |
| `-edit-max-age` | `0` | seconds after posting during which a message can be edited (`limits.edit_max_age_seconds`): 0 for no limit, else at least 300 |
| `-max-body-length` | `4000` | the longest body in code points (`limits.max_body_length`), 500 to 100000 |

| Variable | Does |
|---|---|
| `BRIDGE_TOKEN` | the bot's token, which Muster sends; this or `-token-file` is required |
| `BRIDGE_PUSH_URL` | the callback address of the Bridge Connection, to push the events to; unset, Muster polls |
| `BRIDGE_PUSH_SECRET` | the push secret of the Bridge Connection, `whsec_…`; this or `-push-secret-file` is required with `BRIDGE_PUSH_URL` |
| `BRIDGE_FEATURES` | the features to offer, comma-separated: `buttons`, `thread_messages`, `direct_messages`, `private_answers`, `native_mentions`, `quiet_messages`, `status_color`, `deletion_events`, `delete`. All when unset, none when empty. `BRIDGE_FEATURES=thread_messages` tries Text commands without buttons |

`user_lookup` is `login` while `direct_messages` is on, and `none` without it. Wrong settings end the example with the
exit status 2 and a message that names the setting.

## The page's JSON API

The page uses this API, and scripts and tests use it in place of a person. Every `POST` takes a JSON body with
`Content-Type: application/json` and refuses anything else with 415. A refused request is answered with a 4xx status, such as 400 for a malformed body,
404 for an unknown message and 409 for a press while `buttons` is off, and a report the library could not take with
500. Every failure has the body `{"error": "<what went wrong>"}`.

| Request | Does |
|---|---|
| `GET /chat/api/state` | the bridge, its features and limits, the channels, the people, a version that changes whenever a message, an answer under it or a direct chat changes (not with the event log), and the newest lines of the event log, newest first |
| `GET /chat/api/messages?channel=<id>` | the Root messages of the channel, oldest first, each with `id`, `title`, `buttons` (`label`, `style`), `thread`, `notes`, `deleted` and the rest of what the page shows |
| `GET /chat/api/direct-messages?user=<login>` | the direct chat between the person and the bot, oldest first |
| `POST /chat/api/press` `{message_id, user, button}` | presses a button as the person; `button` is the button's zero-based index, the first when left out |
| `POST /chat/api/thread` `{message_id, user, text}` | writes in the Thread of the message as the person |
| `POST /chat/api/direct-message` `{user, text}` | writes to the bot in the person's direct chat |
| `POST /chat/api/delete` `{message_id, user}` | deletes a message on the page, which stays in the lists with `deleted: true`; the deletion of a message of the bot is reported |

```sh
# The Root messages of #alerts with their buttons
curl -s '127.0.0.1:18095/chat/api/messages?channel=alerts' | jq -c '.[] | {id, title, buttons: [.buttons[].label]}'

# Press the button labelled Ack of a message as alice
id=<message id>
button=$(curl -s '127.0.0.1:18095/chat/api/messages?channel=alerts' \
  | jq --arg id "$id" '.[] | select(.id == $id) | [.buttons[].label] | index("Ack")')
curl -s -X POST -H 'Content-Type: application/json' \
  -d "{\"message_id\":\"$id\",\"user\":\"alice\",\"button\":$button}" 127.0.0.1:18095/chat/api/press

# Write ack in its Thread as bob, and paste an Account link token in alice's direct chat
curl -s -X POST -H 'Content-Type: application/json' \
  -d "{\"message_id\":\"$id\",\"user\":\"bob\",\"text\":\"ack\"}" 127.0.0.1:18095/chat/api/thread
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"user":"alice","text":"<Account link token>"}' 127.0.0.1:18095/chat/api/direct-message
```

A press answers `200` with the confirmation the page shows, such as `"confirmation": "Pressed Ack"`, and `reported`,
the outcome of the report to the library. A message in a Thread or a direct chat answers `201`.

## In memory

The example keeps everything in memory: a restart loses its messages, Threads, direct chats, event log, queued events
and Idempotency-Keys. Message ids and update ids start with a prefix that is random for each start, so that after a
restart Muster finds its old messages gone and posts them again, and never takes a new press for an old one. The chat
keeps at most 1000 Root messages per channel, 500 messages per Thread, 100 private answers per message, 500 messages
per direct chat and 500 lines of the event log, and forgets the oldest past those.

## No authentication

The Bridge protocol under `/v1/` needs the token. The page and its JSON API under `/chat/api/` have **no
authentication**: whoever reaches them reads every message and acts as any person. Put them behind some, such as an
authenticating proxy, before the example is reachable from beyond your machine; from source it listens on loopback
only. The page's Content-Security-Policy guards it against injected scripts, and the JSON-only `POST`s keep other web
sites from posting through a visitor's browser, but neither is authentication.

`GET /` answers 200 with the page and needs no token: use it as the HTTP probe of a deployment.

## Conformance

`make conformance`, in the module's root on Linux with Docker, runs `muster bridge check` against this example three
times — every feature, only `thread_messages`, and interactively with the example pushing to the check's listener —
after the same runs against `examples/minimal`. In the interactive run it plays the person by pressing, through
`POST /chat/api/press`, the message that the check's `WAIT press` line names.
