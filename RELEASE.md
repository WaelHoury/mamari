# Release Process

## One-Time GitHub Setup

1. The canonical public repository is
   [`WaelHoury/mamari`](https://github.com/WaelHoury/mamari).
2. Keep GitHub Actions enabled and allow workflows to create releases using
   `GITHUB_TOKEN`.
3. Protect `main` and require the `test`, `release-snapshot`, and
   `windows-installer` checks from the `ci` workflow.
4. Enable GitHub private vulnerability reporting.
5. The `v0.1.x` release line is intentionally unsigned. Archives have SHA-256
   checksums and GitHub build-provenance attestations, but macOS notarization
   and Windows Authenticode require maintainer-owned certificates and are not
   configured. Do not describe these binaries as signed or notarized; retain
   the matching disclosure in `README.md`. Revisit signing before `v1.0.0`.

## Prepare a Release

1. Choose a semantic version, initially `v0.1.0`.
2. Replace `TBD` in the matching `CHANGELOG.md` heading with the release date.
3. Add a fresh empty `[Unreleased]` section for subsequent work.
4. Run the complete local gate:

   ```bash
   gofmt -w cmd internal
   test -z "$(gofmt -l .)"
   go mod tidy -diff
   go vet ./...
   staticcheck ./...
   go test ./...
   go test -race ./...
   go run github.com/google/go-licenses@v1.6.0 check ./... --disallowed_types=restricted,forbidden
   python3 scripts/regression_gate.py
   govulncheck ./...
   goreleaser check
   actionlint .github/workflows/*.yml
   ```

5. Build a snapshot from a macOS host with Xcode. Install the checksum-pinned
   Zig toolchain used by CI, then build and verify every archive:

   ```bash
   zig_dir="$(./scripts/install-zig.sh)"
   PATH="${zig_dir}:${PATH}" goreleaser release --snapshot --clean --skip=publish
   ./scripts/verify-release.sh dist
   ```

6. `verify-release.sh` confirms `dist/artifacts.json`, all six Darwin, Linux,
   and Windows archives, checksums, archive contents, binary architectures,
   macOS 12 deployment targets, the Linux glibc 2.17 ceiling, and execution of
   the native archive.
7. Commit the release preparation and wait for required CI checks.

## Publish

Create and push an annotated tag:

```bash
git tag -a v0.1.0 -m "mamari v0.1.0"
git push origin v0.1.0
```

The release workflow builds six cgo binaries, publishes fixed-name archives and
`checksums.txt`, and creates build-provenance attestations when the repository
is public. Never move or replace a published version tag.

## Verify the Published Release

1. Confirm these assets exist:

   - `mamari_darwin_amd64.tar.gz`
   - `mamari_darwin_arm64.tar.gz`
   - `mamari_linux_amd64.tar.gz`
   - `mamari_linux_arm64.tar.gz`
   - `mamari_windows_amd64.zip`
   - `mamari_windows_arm64.zip`
   - `checksums.txt`

2. Install on at least one clean macOS/Linux machine through
   `scripts/install.sh` and one Windows machine through `scripts/install.ps1`.
3. Run:

   ```bash
   mamari version
   mamari init --repo /path/to/test/repo --mcp codex
   mamari status --index /path/to/test/repo/.mamari/index.json
   ```

4. Restart the MCP client, verify that one `mamari` tool is exposed, and run
   `map`, `search`, `trace`, and `doctor`.
5. Verify provenance with GitHub CLI:

   ```bash
   gh attestation verify mamari_darwin_arm64.tar.gz --repo waelhoury/mamari
   ```

If any platform artifact or clean-machine MCP smoke test fails, delete the
release, fix the issue, and publish a new patch version rather than replacing
an immutable tagged artifact.
