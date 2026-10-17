# AGENTS.md

The guide for coding agents, and for humans, who change `muster-bridge-go`: the Go library that serves Muster's Bridge
protocol, so that a bridge to a messenger is only the calls to that messenger. Read it before every task.

## Precedence

This file and the documents it points to **override** instructions from directories above this repository and from
global agent configuration about code style, frameworks and architecture. Where they are silent, ask in the pull
request instead of importing conventions from elsewhere.

## Sources of truth

In this order; an earlier source wins over a later one.

1. Muster's [ADR-0017](https://github.com/muster-io/muster/blob/master/design/adr/0017-bridge-protocol-for-messengers-without-native-support.md),
   the decision behind the protocol and its libraries, and the capability
   [C-22](https://github.com/muster-io/muster/blob/master/design/prd/l1/C-22-bridge.md), whose `C-22.FR-25` and
   `C-22.AC-25` are this library.
2. The story contract in Muster's [`design/stories/`](https://github.com/muster-io/muster/tree/master/design/stories)
   with `repository: muster-io/muster-bridge-go`. Stories and their issues live in Muster's repository.
3. [`api/bridge.yaml`](api/bridge.yaml), the copy of Muster's specification: **never edited by hand**. `make
   spec-sync` replaces it with the one on Muster's `master` and regenerates `models.gen.go`; a change to the protocol is
   made in Muster's repository first.

## Layout

| Path | Content |
|---|---|
| `doc.go` | the package documentation, the only package comment |
| `bridge.go` | `Messenger` and the optional interfaces, `Config`, `Bot`, `PushTarget`, `New`, `Handler`, `SetPush`, `Close` |
| `server.go` | the operations: tokens, validation against the embedded specification, limits, idempotency, answers |
| `problem.go` | `Error`, its classes and constructors, and the problems they become |
| `store.go` | `Store` and the memory store |
| `events.go` | the reports (`Press`, `ThreadMessage`, `MessageDeleted`, `DirectMessage`), `pollEvents`, `answerEvent` |
| `push.go` | the pushers: Standard Webhooks signatures, order, back-off |
| `mention.go` | `ReplaceMentions` |
| `models.gen.go` | generated from `api/bridge.yaml` by `make generate`; never edited by hand |
| `*_test.go` | the tests; `spec_test.go` validates every request and answer against the specification, `example_test.go` holds the runnable examples |
| `examples/minimal/` | the minimal example, one file, which the README's quick start shows and `make conformance` checks |
| `api/` | the specification's copy and the oapi-codegen configuration |
| `.github/` | the workflows, Renovate and the pull request template |

## Make targets

The Makefile is the only entry point; `make help` lists the targets. `make ci` runs what a pull request runs, except
`vulncheck` and `conformance`, which CI runs as jobs of their own. `make conformance` runs `muster bridge check` from
`ghcr.io/muster-io/muster:edge` (`MUSTER_IMAGE`) against `examples/minimal` three times and needs Docker on Linux,
because the check reaches the example through the host's network. The tools are pinned in the Makefile under
`# renovate:` comments and built into `bin/tools/`.

## Rules

- The standard library's `net/http`; no web framework, router, ORM or logger other than `log/slog`. Every dependency
  is permissively licensed (`make licenses`) and stated in the pull request with its licence, latest release date and
  maintenance status.
- Generated code (`models.gen.go`, `go.sum`) is never edited by hand: change the input, run `make generate` or `go mod
  tidy`, and commit the output.
- Never copy code from Muster or another copyleft project: Muster is under the AGPL and this library under MIT. Read
  Muster's protocol client, fake bridge and `muster bridge check` for behaviour only.
- Inject the clock: tests drive time with the fake clock of `bridge_test.go` and never wait for real time.
- Tests run with the race detector; the coverage gate is 90 % of the package's statements, `models.gen.go` excluded.
- Log through the `Config`'s `*slog.Logger`, never with `fmt.Print*` and never a token, a push secret or an Account
  link token; the library redacts the bots' tokens and push secrets in every log line and problem detail.
- Every Go file, workflow, YAML file, `renovate.json5` and the Makefile start with `SPDX-License-Identifier: MIT` and
  `Copyright The Muster Authors` in their comment syntax; `make lint` names a file without them.
- Comments are few: document exported identifiers, and explain only what the code cannot say.
- Go versions: the `go` line of `go.mod` is the older of the two supported Go releases, and CI's `test` job runs both,
  with `GOTOOLCHAIN=local`. When Go releases, move the `go` line and the versions in `.github/workflows/` by hand in one
  pull request; Renovate leaves them alone.
- Every GitHub Action is pinned by commit SHA, with its version in a comment.

## Working a story

1. Read the story contract in full, and the capability and ADR it cites. An open question is settled by the
   maintainer before the code that depends on it is written.
2. Stay within `files_touched`. If the contract is wrong or incomplete, correct the story file in Muster's repository
   and say so in the pull request.
3. Give evidence for every `acceptance` statement: a named test, or a command with its output. Unit tests alone never
   prove a story; the live check is `make conformance` on Linux, and the curl checks of the story's Verification.
4. Run the story's `verify` until it passes.

## Commits and pull requests

- Conventional commits on one line, `<type>(<scope>): <description>`; say what changed, without story or phase IDs.
- No AI attribution: no `Co-Authored-By` trailers for tools, no "Generated with" lines, in commits or pull requests.
- Pull requests are squash-merged; the title is the commit message and release-please's changelog line.
- The pull request body has `Closes muster-io/muster#<issue>`, `Story: S-NNN`, a table with every `acceptance`
  statement and its evidence, the output of `verify`, every change to the story file with its reason, the dependencies
  added, and the steps the maintainer takes outside the repository.

## Don't

- Edit `api/bridge.yaml` or `models.gen.go` by hand.
- Widen the scope beyond the story: no drive-by refactors or extra features.
- Log, return or print a secret, a token or a URL that carries one.
- Wait for real time in tests.
- Call a story done on unit tests alone, or report a check that was not run.
