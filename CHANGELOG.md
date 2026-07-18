# Changelog

All notable changes to Mamari are documented here. The project follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and uses semantic
versioning for public releases.

## [Unreleased]

## [0.1.0] - 2026-07-18

### Added

- One-command local MCP onboarding with `mamari init --mcp <client>`, including
  Claude Code, Codex, VS Code / GitHub Copilot, and guided JetBrains setup.
- Terraform and OpenTofu native `.tf` indexing now uses canonical declaration
  addresses, covers locals, aliased providers, ephemeral resources, and
  current Terraform action blocks, resolves module-scoped expression
  traversals into exact `depends-on` edges, and links local module sources to
  their child `.tf` files.
- Registry-driven tree-sitter conformance fixtures now cover every registered
  structural language, including cross-file symbols, calls, parents, graph
  integrity, and cross-language isolation.
- Generated mobile, JVM, .NET, and Terraform directories are excluded from
  repository walking by default.
- The generic `code-review` skill can be installed with
  `mamari install-skill code-review`.
- Reproducible release tooling now installs a checksum-pinned Zig 0.16.0
  toolchain and verifies the Go toolchain, architecture, platform floor, and
  contents of all six packaged platform archives.

### Changed

- MCP configuration now resolves paths against the target repository, validates
  the executable and index before writing, preflights conflicting server
  entries, and atomically replaces project config files.
- The default MCP surface uses one compact router with budgeted responses.
- Documentation now describes implementation behavior without depending on
  results or conventions from external repositories.
- Continuous integration now builds the real multi-platform release snapshot
  and installs its native macOS and Windows archives before a release can be
  tagged from a green commit.
- Tree-sitter partial-parse diagnostics now say "invalid or unsupported
  syntax" because parser grammar gaps do not necessarily mean source is
  invalid.

### Fixed

- The required Go toolchain is now 1.26.5, which includes the standard-library
  fix for GO-2026-5856 in `crypto/tls`.
- Parser metadata now covers every registered tree-sitter frontend, including
  R, Julia, Zig, OCaml, and HCL.
- Import-bound bare calls resolve within the imported file before falling back
  to repository-wide name matching.
- Ambiguous symbol candidates rank active source before backup or stale copies.
- Vue literal keywords are excluded from call targets while valid template
  handlers remain indexed.
- Weak search results are marked low-confidence when most distinctive query
  terms are absent from the corpus.
- The root binary ignore rule no longer hides source files under nested
  directories named `mamari`.
- Zig cross-compilation now uses a writable per-target local cache instead of
  trying to create `.zig-cache` directories in Go's read-only module cache.
- The Unix installer's local release override now accepts filesystem paths,
  including paths containing spaces, consistently with the Windows installer.
