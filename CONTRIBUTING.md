# Contributing to muster-bridge-go

Thank you for helping. This guide covers how work is proposed, how a pull request is built and what it must prove.
[`AGENTS.md`](AGENTS.md) is the detailed guide for everyone who changes the code, people and coding agents alike; read
it before your first pull request.

## Licence of contributions

The library is under the [MIT licence](LICENSE), and contributions are accepted under the same licence: by opening a
pull request you agree that your contribution is licensed under MIT. There is no Contributor License Agreement.

## Proposing work

Open an issue for a bug or a change you would like to see. Planned work is described as story contracts in Muster's
repository, in [`design/stories/`](https://github.com/muster-io/muster/tree/master/design/stories), with
`repository: muster-io/muster-bridge-go` in their frontmatter; their issues, titled `S-NNN: <title>`, live in
[muster-io/muster](https://github.com/muster-io/muster/issues). A change to the protocol itself belongs in Muster's
repository: [`api/bridge.yaml`](api/bridge.yaml) here is only its copy.

## Development setup

You need Go 1.26 or 1.27, GNU make, curl and git; `make conformance` also needs Docker on Linux, since it reaches the
example through the host's network. The Makefile builds its pinned tools (golangci-lint, go-licenses, govulncheck,
oapi-codegen) into `bin/tools/` on first use; after switching Go releases, `make clean` rebuilds them with the new one.

The Makefile is the only entry point:

| Target | Does |
|---|---|
| `make help` | lists the targets (the default goal) |
| `make fmt` | formats the code (gofmt and goimports through golangci-lint) |
| `make lint` | checks the licence headers, `go.mod` and `go.sum`, and the code with golangci-lint |
| `make test` | runs the tests with the race detector and the coverage gate: 90 % of the package's statements, `models.gen.go` excluded |
| `make generate` | generates `models.gen.go` from `api/bridge.yaml` with oapi-codegen |
| `make generate-check` | regenerates and fails when `models.gen.go` was not current |
| `make spec-check` | compares `api/bridge.yaml` byte for byte with the one on Muster's `master` (`MUSTER_SPEC_URL`) and prints the difference |
| `make spec-sync` | replaces `api/bridge.yaml` with Muster's and runs `make generate` |
| `make licenses` | fails on a dependency linked into the library or the example whose licence is not allowed |
| `make vulncheck` | finds vulnerable Go code that is called (govulncheck) |
| `make build` | builds the package and the example into `bin/minimal` |
| `make ci` | runs `lint`, `generate-check`, `spec-check`, `licenses`, `test` and `build` |
| `make conformance` | runs `muster bridge check` from Muster's development image against `examples/minimal` three times |
| `make clean` | removes `bin/` |

In CI, the `ci` workflow runs the jobs `lint`, `test` (on Go 1.26 and 1.27), `generate`, `spec`, `licenses`,
`vulncheck` and `conformance` on every pull request, on every push to `master`, daily and by hand; the job `ci` passes
only when all of them passed, and the ruleset of `master` requires it. The `codeql` and `scorecard` workflows analyse
the code and the repository's practices.

## Branches, commits and pull requests

- Branch from `master`; a story's branch is `s-NNN-<slug>`, with the slug of its story file name.
- Commit messages are [conventional commits](https://www.conventionalcommits.org/) on one line:
  `<type>(<scope>): <description>`, with `feat`, `fix`, `docs`, `build`, `ci`, `test`, `refactor`, `perf` or `chore`.
  Say what changed; leave out story and phase IDs.
- No AI attribution: no `Co-Authored-By` trailers for tools and no "Generated with" lines, in commits or pull requests.
- Pull requests are squash-merged, so the title becomes the commit message, and release-please writes the changelog
  from it.
- The [pull request template](.github/pull_request_template.md) asks for the issue it closes
  (`Closes muster-io/muster#<issue>`), the story ID, a table with every acceptance statement and its evidence, the
  output of the story's `verify`, every change to the story file with its reason, the dependencies added and the
  maintainer's steps.

## Licence header

Every Go file, workflow, YAML file, the Renovate configuration and the Makefile start with these two lines in their own
comment syntax:

```go
// SPDX-License-Identifier: MIT
// Copyright The Muster Authors
```

Markdown, JSON, `go.mod`, `go.sum`, generated files (marked `Code generated … DO NOT EDIT.`) and `api/bridge.yaml`,
which states its licence in `info.license`, are exempt. `make lint` names every file that lacks the header.

## Dependencies and third-party code

- The library and the example may depend only on code under MIT, MIT-0, BSD-2-Clause, BSD-3-Clause, Apache-2.0, ISC,
  0BSD, Unlicense or CC0-1.0; `make licenses` checks it. Build tools may use any OSI-approved licence.
- A pull request that adds a dependency states its **licence**, its **latest release date** and its **maintenance
  status**. Prefer the standard library: the library uses `net/http` and needs no web framework, router or ORM.
- Never copy code from copyleft projects — Muster included, which is under the AGPL — into this repository; read them
  for behaviour only.

## Releases

release-please keeps a release pull request open, with the changelog written from the conventional commits on
`master`. Merging it creates the release and its `vX.Y.Z` tag, which Go modules use directly; the workflow then asks
the Go module proxy for the new version. Versions stay `0.x` until Muster's first release.
