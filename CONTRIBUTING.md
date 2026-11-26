# Contributing

Thanks for helping improve `agent-git-service`.

## Development setup

Use the Go version in [`.go-version`](.go-version) to match hosted CI. See the
[quick start](docs/quickstart.md) for local prerequisites. You do not need a
running AGS or a database to compile development code with `make build-dev`.
`make build` produces an attributable binary from an exact accepted Git commit.

Follow [fork governance](fork/README.md) for the active generation, branch
workflow, and upstream boundary. Open pull requests against the maintained fork,
not the upstream repository.

## Before opening a pull request

For documentation-only changes, run `python3 scripts/doc_lint.py` and check
links or anchors the lint does not cover. For code changes, start with `make check`
to compile and vet development code. Add focused tests beside the changed code.
Use the [test strategy](docs/test-strategy.md) to choose commands and provision an
isolated test database when needed. Never point tests at a deployed AGS database.
Use the [CI guide](docs/ci.md) to distinguish local checks from hosted gates.

Update the matching [implementation docs](docs/README.md) when you change an API,
Git transport, routing, authorization, CI, or test contract.

## Pull Request Expectations

- Keep each PR scoped to one behavior change or cleanup.
- Include a clear description of why the change is needed.
- Link a tracking issue or plan when available.
- List the exact commands you ran.
- Do not commit local databases, generated binaries, credentials, logs, or scan outputs.

## Vendored CLI

The `cli/` directory contains a vendored GitHub CLI tree used for gh CLI compatibility testing. Avoid broad edits there unless the change is specifically about that harness, gh CLI compatibility, or GitHub-compatible client behavior.
