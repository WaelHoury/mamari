# Contributing to Mamari

Thanks for helping improve Mamari.

## Prerequisites

- Go 1.26.5, as declared in `go.mod`.
- A C and C++ compiler for tree-sitter grammars.
- Git for tests that exercise committed-index workflows.
- macOS with Xcode for release snapshots; the repository installs its pinned
  Zig release toolchain with `scripts/install-zig.sh`.

## Development

```bash
go build ./cmd/mamari
go test ./...
go test -race ./...
go vet ./...
test -z "$(gofmt -l .)"
python3 scripts/regression_gate.py
```

Run `staticcheck ./...` when changing parser, graph, persistence, concurrency,
or caching code. Changes to release or workflow files should also pass:

```bash
goreleaser check
actionlint .github/workflows/*.yml
sh -n scripts/*.sh
```

Before changing release packaging, run the same artifact gate as CI:

```bash
zig_dir="$(./scripts/install-zig.sh)"
PATH="${zig_dir}:${PATH}" goreleaser release --snapshot --clean --skip=publish
./scripts/verify-release.sh dist
```

## Pull Requests

- Keep changes scoped and include regression coverage for behavior changes.
- Preserve stable symbol IDs and deterministic output ordering.
- Treat unresolved calls conservatively; do not increase confidence without
  structural evidence.
- Describe token, latency, memory, or accuracy effects when changing response
  shaping or ranking.
- Update `CHANGELOG.md` under `[Unreleased]`.

For substantial parser or ranking work, include the repository, commit, query,
expected evidence, and measurement method used to validate the change.

## Security

Do not open a public issue for a suspected vulnerability. Follow
[`SECURITY.md`](SECURITY.md).
