# mamari

> Local code-intelligence MCP server for AI coding agents. Build a queryable symbol graph of any repo, then navigate it through one compact router — without bulk-reading files, and without a hallucinated call graph: calls Mamari can't prove are marked `unresolved`, never guessed. Structural, honest resolution for Go, Python, JS/TS, Java, C#, Kotlin, Rust, PHP, and more.

[![CI](https://github.com/waelhoury/mamari/actions/workflows/ci.yml/badge.svg)](https://github.com/waelhoury/mamari/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/waelhoury/mamari)](https://github.com/waelhoury/mamari/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go 1.26.5](https://img.shields.io/badge/go-1.26.5-00ADD8?logo=go&logoColor=white)](go.mod)
[![MCP](https://img.shields.io/badge/MCP-1_default_tool-7c3aed)](https://modelcontextprotocol.io)

Mamari indexes your codebase into `.mamari/index.json`, then runs a local stdio MCP server that Claude Code, Codex, VS Code, or any MCP-capable agent can talk to. Agents get precise symbol graphs, cross-file call traces, and budgeted source slices instead of spending tokens on full-file reads.

One compact MCP router keeps fixed session overhead small. Responses are
token-budgeted and byte-deterministic, and the implementation is tested with
`go test -race`.

**Local-first** — your source never leaves your machine. The MCP server is a process on your laptop.

---

## Contents

- [Features](#features)
- [Language Support](#language-support)
- [Quickstart](#quickstart)
- [Install](#install)
- [MCP Client Setup](#mcp-client-setup)
- [How Agents Use Mamari](#how-agents-use-mamari)
- [Team Rollout](#team-rollout)
- [MCP Tools](#mcp-tools)
- [Watch Mode](#watch-mode)
- [CLI Reference](#cli-reference)
- [How It Works](#how-it-works)
- [Operational characteristics](#operational-characteristics)

---

## Features

- **Never guesses the call graph** — every call edge is tagged `exact`, `scoped`, `heuristic`, or `unresolved`. When Mamari can't prove a target (dynamic dispatch, reflection, ambiguous overload), it says `unresolved` instead of inventing an edge. Agents get a graph they can act on without second-guessing it — and workflows that depend on this (impact, dead-code, review) separate what's proven from what needs a look.
- **Code-review & dead-code flows** — `review` turns a git diff into "what does my change break?": changed symbols with proven vs possible blast radius, untested-changed, hot-path risk, and co-change hints. `dead_code` finds unreferenced symbols and refuses to call anything dead that an unresolved edge might still reach.
- **Symbol graph** — functions, classes, methods, interfaces, types, enums, Vue components, RDF shapes, and their call/import edges
- **One-tool default MCP surface** — the slim `mamari` router exposes the main workflows at low startup cost; 34 named tools remain available through adaptive/full modes
- **Natural-language search** — `search_code` ranks evidence snippets by relevance, not just text match
- **Local semantic vectors** — `semantic_query` bridges vocabulary mismatch with corpus co-occurrence, software concepts, IDF weighting, and call-graph diffusion; no API key or external service
- **Architecture map** — `repo_map` summarizes languages, packages, entry points, routes, hotspots, connectivity-refined graph communities, typed coupling, and module boundaries
- **Built-in graph explorer** — `mamari ui --watch` serves a local, dependency-free visual graph that opens **hierarchically** (packages → files → symbols, with breadcrumb drill-down), plus search, language/kind/edge filters, confidence-aware weighted links (package-level import edges included), a **health overlay** (dead / untested / hot-path / high-complexity coloring), zoom/pan/drag, and source/caller/callee inspection
- **Deterministic workflows** — `inspect_flow` answers "how does X work?" in one round-trip; `inspect_symbol` combines trace + source in one packet and has a compact `format=node` symbol-read mode
- **RDF/SHACL/TTL** — full lexical indexing of Turtle files with prefix resolution and `sh:NodeShape` graphs
- **Token-budgeted responses** — high-volume discovery and context tools shape complete serialized responses to bounded budgets
- **Watch mode** — re-indexes only changed files; MCP server updates in-memory without restart
- **SCIP import** — additive ingestion from Sourcegraph SCIP indexes for compiler-backed cross-references
- **Stable symbol IDs** — IDs survive line-number shifts (e.g. adding an import); diffs reflect real renames, not whitespace churn
- **Race-safe** — worker pool + mutex-guarded index, tested with `go test -race`

---

## Language Support

| Language | Parser | Call resolution | Notes |
|---|---|---|---|
| JavaScript / TypeScript | Pure Go (no cgo) | Exact / heuristic / scoped | Typed parameters, fields, locals, aliases, imports, and constructed instances |
| Vue SFC | Pure Go (no cgo) | Heuristic / scoped | Template bindings, `renders-component`, `passes-prop`, `binds-model`, `listens-event` |
| Python | tree-sitter (cgo) | Exact / scoped | Annotations, constructor inference, imported aliases, and `self` fields |
| Java | tree-sitter (cgo) | Exact / scoped | Spring MVC `@RequestMapping` → `http-route` edges |
| Go | tree-sitter (cgo) | Exact / scoped | Struct-embedding promoted method calls |
| C# | tree-sitter (cgo) | Exact / scoped | ASP.NET Core `[Route]`/`[HttpGet]` → `http-route` edges |
| Dockerfile | Structural instruction scanner | Exact / scoped | Stages, base images, cross-stage copies, ports, commands, health checks |
| Kubernetes / Kustomize YAML | YAML AST | Exact / scoped | Multi-document resources, selectors, runtime dependencies, overlays, components, patches |
| Rust, Ruby, PHP, C, C++, Kotlin, Bash, Scala, Lua, Elixir, Dart, Haskell, Clojure, Swift | tree-sitter (cgo) | Exact / scoped | Real class/method nesting and call resolution via a shared generic tree-sitter engine — see the language-coverage rollout in `CHANGELOG.md` |
| R | tree-sitter (cgo) | Exact / scoped | `name <- function(...)` definitions; bare, `pkg::fn`, and `obj$method` call resolution |
| Julia | tree-sitter (cgo) | Exact / scoped | Long- and short-form functions, `struct`/`abstract`/`module`; bare and `Mod.fn` calls |
| Zig | tree-sitter (cgo) | Exact / scoped | `fn` declarations, `struct`/`enum`/`union` containers; bare and `recv.method` calls |
| OCaml | tree-sitter (cgo) | Exact / scoped | Function `let`-bindings, modules, module types, classes/methods, types; curried application + `Mod.fn` calls |
| Terraform / OpenTofu (`.tf`) | tree-sitter (cgo) | Exact / module-scoped | Canonical addresses for resources, data, modules, variables, locals, outputs, providers, and ephemeral resources, plus current Terraform action blocks; dependency edges and local-module source imports |
| Generic HCL (`.hcl`) | tree-sitter (cgo) | N/A (declarative) | Labeled blocks indexed structurally without assuming Terraform semantics |
| Turtle / SHACL | Lexical | N/A | File-scoped prefix resolution, named `sh:NodeShape` blocks |
| JSON | File/config recognition | N/A | Loaded for metadata and supported configuration discovery; no general JSON AST symbol graph |

---

## Quickstart

```bash
# 1. Install a published release on macOS or Linux
curl -fsSL https://raw.githubusercontent.com/waelhoury/mamari/main/scripts/install.sh | sh
# If the Releases page is still empty, use this instead:
# go install github.com/waelhoury/mamari/cmd/mamari@latest

# 2. Open the codebase
cd /path/to/your/project

# 3. Index it and configure your MCP client
mamari init --mcp codex
```

Replace `codex` with:

| Value | Use it for |
|---|---|
| `claude` | Claude Code, whether launched from a terminal, VS Code, or IntelliJ |
| `codex` | Codex CLI or the Codex IDE extension |
| `vscode` | GitHub Copilot in VS Code |
| `jetbrains` | GitHub Copilot or JetBrains AI Assistant in IntelliJ-based IDEs |
| `all` | Claude Code, Codex, and VS Code project files |

The command builds `.mamari/index.json`, resolves stable absolute paths,
validates that both the index and Mamari executable are usable, and configures
the selected client. It then prints the exact restart, trust, and verification
step for that client. Live filesystem updates are enabled by default.

---

## Install

### Release binary (recommended)

Release archives contain Mamari binaries for macOS, Linux, and Windows on amd64
and arm64; no Go installation is needed. The installers verify each archive
against the release's SHA-256 checksum before replacing the local binary.

The `v0.1.x` binaries are intentionally not Apple-notarized or Windows
Authenticode-signed, so the operating system may ask for confirmation before
the first run. SHA-256 checksums and GitHub build-provenance attestations are
published with each release; code signing is planned before `v1.0.0`.

The installers require at least one published
[GitHub release](https://github.com/waelhoury/mamari/releases). If the releases
page is still empty, use the source installation below until the first tag is
published.

Supported release targets are macOS 12 Monterey or newer, Linux with kernel
3.2 and glibc 2.17 or newer, and Windows 10 / Windows Server 2016 or newer.
These floors follow [Go 1.26's platform requirements](https://go.dev/wiki/MinimumRequirements);
the Linux glibc floor is also checked from the packaged binary during release
verification.

macOS or Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/waelhoury/mamari/main/scripts/install.sh | sh
```

The default destination is `~/.local/bin`. Override it with
`MAMARI_INSTALL_DIR`, or pin a release with `MAMARI_VERSION=v0.1.0`.

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/waelhoury/mamari/main/scripts/install.ps1 | iex
```

The Windows installer writes to
`%LOCALAPPDATA%\Programs\Mamari\bin` and adds that directory to the user PATH.

You can also download an archive and `checksums.txt` directly from
[GitHub Releases](https://github.com/waelhoury/mamari/releases/latest).

### From source

Source builds require Go 1.26.5 and a C/C++ compiler for the tree-sitter
grammars.

```bash
go install github.com/waelhoury/mamari/cmd/mamari@latest
```

Or clone the repository and run `go build -o mamari ./cmd/mamari`.

---

## MCP Client Setup

The normal setup is one command from the repository root:

```bash
mamari init --mcp <claude|codex|vscode|jetbrains|all>
```

This is intentionally explicit instead of guessing from the editor: VS Code or
IntelliJ may host several different agents, and configuring the wrong one is
worse than typing its name.

The setup is defensive:

- relative index paths are resolved against `--repo`, even when the command is
  run from another directory;
- written configs use absolute executable and index paths so GUI-launched
  clients do not depend on their inherited `PATH` or working directory;
- Mamari verifies that the executable is runnable and the index can be loaded
  before writing configuration;
- existing non-Mamari configuration is preserved;
- an existing `mamari` entry is not replaced unless `--mcp-force` is supplied;
- JSON and TOML configuration files are replaced atomically.

If the repository is already indexed, configure or repair a client without
re-indexing:

```bash
mamari setup-mcp --repo . --client codex --write
```

### Claude Code

```bash
mamari init --mcp claude
```

This writes project-root `.mcp.json`:

```json
{
  "mcpServers": {
    "mamari": {
      "command": "/absolute/path/to/mamari",
      "args": ["serve", "--index", "/absolute/path/to/project/.mamari/index.json"]
    }
  }
}
```

Restart Claude Code, approve the project MCP server, and run `/mcp` or
`claude mcp list`. Claude Code uses the same project configuration whether it
is launched in a standalone terminal, VS Code terminal, or IntelliJ terminal.
See the [Claude Code MCP documentation](https://code.claude.com/docs/en/mcp).

### Codex

```bash
mamari init --mcp codex
```

This writes project-root `.codex/config.toml`. Trust the project, restart
Codex, and run `/mcp` or `codex mcp list`. The Codex CLI and IDE extension
share this project configuration. See the
[Codex MCP documentation](https://learn.chatgpt.com/docs/extend/mcp).

### GitHub Copilot in VS Code

```bash
mamari init --mcp vscode
```

This writes `.vscode/mcp.json`. Reload the VS Code window, approve the server,
and run **MCP: List Servers** from the Command Palette. See the
[VS Code MCP documentation](https://code.visualstudio.com/docs/agent-customization/mcp-servers).

### IntelliJ and other JetBrains IDEs

JetBrains stores MCP configuration in IDE-managed settings, so Mamari does not
guess at or modify private IDE files. Instead, this command indexes the project,
validates both paths, and prints ready-to-paste JSON for both JetBrains hosts:

```bash
mamari init --mcp jetbrains
```

For GitHub Copilot:

1. Open Copilot Chat in Agent mode.
2. Open **MCP → Add MCP Tools**.
3. Paste the printed `servers` JSON and approve the local server.

For JetBrains AI Assistant, including Claude or Codex agents hosted there:

1. Open **Settings → Tools → AI Assistant → Model Context Protocol (MCP)**.
2. Add the printed `mcpServers` JSON at project level.
3. Open **Settings → Tools → AI Assistant → Agents** and enable
   **Pass custom MCP servers**.

See the [GitHub Copilot JetBrains MCP guide](https://docs.github.com/en/copilot/how-tos/provide-context/use-mcp-in-your-ide/extend-copilot-chat-with-mcp)
and [JetBrains MCP documentation](https://www.jetbrains.com/help/ai-assistant/mcp.html).

If Codex or Claude Code is merely running in IntelliJ's terminal rather than
inside JetBrains AI Assistant, use `--mcp codex` or `--mcp claude`; no
JetBrains-specific configuration is needed.

MCP over `stdio` is client-owned: JetBrains starts one Mamari child process for
each enabled server entry and cannot attach to a separately started
`mamari serve`. Configure Mamari at either the project level or the global IDE
level for a repository, not both, and do not keep a manual server running for
normal IDE use. The absolute `--index` generated by `mamari setup-mcp --repo .`
is the context-sensitive part of the setup; environment-variable or working-
directory substitution is not portable across JetBrains MCP hosts.

### Verify the setup

Mamari validates the runtime during setup. These client checks confirm that the
host loaded it:

| Client | Verification |
|---|---|
| Claude Code | `/mcp` or `claude mcp list` |
| Codex | `/mcp` or `codex mcp list` |
| VS Code / Copilot | **MCP: List Servers** |
| JetBrains | Confirm `mamari` in the MCP status/tools list |

You can also inspect the local index independently:

```bash
mamari version
type -a mamari                         # reveal duplicate PATH installations
go version -m "$(command -v mamari)"  # inspect the exact executable selected
mamari status --index .mamari/index.json
```

Then ask:

> Use Mamari to map this repository and identify its main entry points.

### Reconfigure safely

Mamari refuses to overwrite an existing `mamari` server entry. After inspecting
the existing config, replace only that entry with:

```bash
mamari init --mcp codex --mcp-force

# Or without rebuilding the index:
mamari setup-mcp --client codex --write --force
```

## How Agents Use Mamari

After setup, restart or reload your AI client. The client starts
`mamari serve` automatically for this repository — do not run a separate
`mamari serve` process for normal MCP use.

The generated configuration contains the absolute path to this repository's
`.mamari/index.json`, so each project gets its own local Mamari process and
code graph. By default, the agent sees one read-only MCP tool named `mamari`.
That compact tool routes actions such as:

| Agent task | Mamari action |
|---|---|
| Understand repository architecture | `map` |
| Follow how a feature works | `explore` |
| Find relevant code or an exact identifier | `search` / `exact` |
| Inspect callers and callees | `trace` |
| Read focused source around a symbol | `node` / `context` |
| Estimate the blast radius of a change | `impact` |
| Review current changes | `review` |
| Find unreferenced or duplicated code | `dead_code` / `duplicates` |

For example, when asked:

> Explain how authentication works in this repository.

an agent can call the `mamari` tool with `action: "explore"` and
`query: "authentication flow"`, then use `trace` or `context` on the symbols it
discovers. Mamari supplies local code intelligence; the AI client continues to
use its normal file-editing and terminal tools when making changes.

While the MCP client is running, Mamari watches the repository. Saved edits,
creates, renames, and deletes are incrementally rebaked into the in-memory
graph without restarting the client. The on-disk `.mamari/index.json` remains
the session's startup snapshot unless the server is deliberately configured
with `--persist`.

If files or branches changed while the MCP client was closed, refresh the
startup index before the next session:

```bash
mamari index --repo .
```

### Encourage consistent agent use

MCP makes Mamari available, but the model still decides when to call it. For
more consistent repository navigation, add the following to the project's
`AGENTS.md`, `CLAUDE.md`, or equivalent agent-instruction file:

```md
Use the Mamari MCP tool before broad repository searches or full-file reads.

- Start with `map` for architecture questions.
- Use `explore` for feature-flow questions and `search` for discovery.
- Use `trace`, `node`, or `context` after identifying a symbol.
- Use `impact` before changing shared code and `review` when reviewing changes.
- Treat exact/scoped edges as proven and heuristic/unresolved edges as uncertain.
```

These instructions are optional. Mamari remains available without them, and
explicit prompts such as “Use Mamari to trace this flow before editing” work
without any repository instruction file.

## Team Rollout

For each teammate and codebase:

1. Install Mamari from the release binary and run `mamari version`.
2. Run `mamari init --mcp <client>` at the codebase root.
3. Follow the printed restart/trust instruction and confirm `mamari` appears.
4. Run the printed map prompt before relying on deeper graph analysis.
5. (Optional) `mamari install-skill code-review` drops a generic,
   mamari-grounded code-review skill into `./.claude/skills/` (commit it to
   share with the team). The skill discovers the repo's own coding standards
   and drives the review/impact/dead_code/duplicates flows — see
   [Bundled skills](#bundled-skills).

### Bundled skills

Mamari ships Claude Code skills embedded in the binary. Install one into a repo
(or your user scope) in a single command — no separate file to copy:

```bash
mamari install-skill --list              # show bundled skills
mamari install-skill code-review         # into ./.claude/skills (per-repo; commit to share)
mamari install-skill code-review --user  # into ~/.claude/skills (all your repos)
```

An existing `SKILL.md` is never overwritten without `--force`, so local
customization is safe. `code-review` is stack- and language-agnostic: it reads
the repository's own standards (CONTRIBUTING/docs/lint configs) and applies only
the stack lenses that fit the changed files.

Absolute-path configs generated by `init --mcp` or `setup-mcp --write` are
machine-local and should normally remain uncommitted. The most reliable team
setup is for every clone to run the one-command initialization locally; this
avoids machine-specific binary paths and GUI `PATH` differences. Existing
shared MCP files are merged, and Mamari changes only its named server entry.

For a git-tracked index, use `mamari init --commit-index`, commit
`.mamari/committed/`, and have each clone run `mamari hooks install`. Local
indexes remain the recommended default because they always reflect each
developer's working tree.

---

## MCP Tools

`mamari serve` defaults to a slim one-tool MCP surface to minimize the fixed
`tools/list` schema cost every session pays. The single `mamari` tool routes actions
such as `explore`, `map`, `search`, `exact`, `trace`, `node`, `context`, `source`,
`impact`, `graph`, `review`, `dead_code`, and `doctor`; pass the main question/symbol/path
in `query` and optional workflow parameters as `args_json`.

If you prefer the older menu of named tools below, start the server with
`mamari serve --toolset adaptive`. Adaptive mode registers only named tools that are
useful for the loaded repo/session: RDF/Turtle tools appear when the index has TTL
terms or SHACL shapes, cross-repo tools appear once `--link` is passed, and
`changed_since` appears while watching, which is the default. Pass
`--watch=false` to run against an immutable loaded index. Pass
`mamari serve --toolset full` or
`mamari serve --full-toolset` to register every named tool unconditionally, including
rare admin tools such as `manage_notes`, `manage_adr`, and `diff_index`.

### Primary Router

| Tool | What it does |
|---|---|
| `mamari` | One compact MCP tool that dispatches to the main workflows by `action`, preserving Mamari's power with a much smaller schema footprint |

### Discovery

| Tool | What it does |
|---|---|
| `search_code` | Natural-language search — ranks evidence snippets by path, symbol names, and exact literals |
| `semantic_query` | Local vector search for vocabulary-mismatch questions, backed by a persistent quantized semantic index |
| `inspect_exact` | Exact-match workflow for known identifiers, route literals, MIME types, RDF predicates |
| `inspect_flow` | End-to-end "how does X work?" — runs discovery, anchors symbols, traces, fetches source in one call |
| `repo_map` | PageRank file/symbol overview plus packages, entry points, routes, hotspots, graph communities, typed coupling, and module boundaries |

### Code review & maintenance

| Tool / action | What it does |
|---|---|
| `review` | Reviews a git diff (`query` = base ref, default `HEAD`): maps changed lines to the symbols that own them, then reports each one's blast radius **split into proven (exact/scoped) and possible (heuristic/unresolved) callers**, whether a test reaches it, a **change classification** (`signature` / `body` / `new` — a signature change with callers is flagged as caller-breaking), hot-path risk, and "you usually also touch these" co-change hints. Pass `args_json.coverage` (an lcov path) to make the untested verdict reflect what actually ran under the suite. The daily "what does my change break?" flow — and it never presents an unproven caller as certain |
| `dead_code` | Finds symbols with no resolved reference, and **separately** lists symbols held back because an unresolved same-name call *might* reach them — Mamari reports the first as unreferenced and refuses to call the second dead. Honest by construction |
| `duplicates` | Structural (Type-2) clone clusters — functions with the same shape and different names/literals — for finding copy-paste and reusability opportunities. Fingerprinted at index time, so the query is cheap |
| `report` | The repo **report card**: parse health, edge-confidence breakdown (unresolved % stated, not hidden), dead code, static test reachability %, duplication clusters, complexity/hot-path hotspots, biggest blast radius (distinct proven callers), god files. With `--fail-on` it doubles as a CI quality gate |
| `tests_for` / `untested_symbols` | Which tests exercise a symbol (resolved vs possible), and which symbols no test reaches |

### Symbol graph

| Tool | What it does |
|---|---|
| `list_symbols` | List symbols with filtering by kind, source/test/story |
| `find_symbol` | Fuzzy-ranked symbol lookup — exact production symbols score ahead of tests and noise |
| `trace_symbol` | Cross-file callers, callees, and import edges for a symbol |
| `inspect_symbol` | Combined packet: trace + frontend edges + budgeted source for callers/callees; use `format: node` for compact source/docstring/return-type/caller/callee reads |
| `find_references` | All reference sites for a symbol or RDF term |
| `impact` | Downstream symbols affected by changing a given symbol |
| `query_graph` | Restricted, Cypher-like `MATCH/WHERE/RETURN/ORDER BY/LIMIT` queries over the symbol graph, including chained multi-hop patterns and variable-length traversal (`*`, `*N`, `*N..M`) for ad-hoc multi-hop/property/transitive-reachability questions (e.g. hot-path hotspots, "everything callable within 3 hops") — not full Cypher |

### Source fetching

| Tool | What it does |
|---|---|
| `fetch_context` | Budgeted source slices for a symbol, `file:line`, or RDF term |
| `fetch_source` | Raw source lines for an exact `file:start:end` range |

### RDF / Turtle

| Tool | What it does |
|---|---|
| `inspect_term` | Grouped term evidence + implementation identifiers + budgeted source |
| `trace_term` | Full trace of an RDF term from TTL shape to code references |
| `list_terms` | List all indexed RDF terms, optionally filtered by namespace prefix |
| `list_dynamic_iris` | Dynamic IRI call sites (namespace templates, computed IRIs) |
| `search_literal` | Exact-string search across all indexed literal values |
| `find_containing_shape` | Find the `sh:NodeShape` block that contains a given term |

### Cross-repo (requires `mamari serve --link <index.json,...>`)

| Tool | What it does |
|---|---|
| `find_route` | Resolve an HTTP route, or a bare event name, to its handler/listener and caller/emitter across the primary repo and every linked repo |
| `list_linked_repos` | List the primary repo and every linked repo with file/symbol counts |
| `cross_repo_architecture` | Every HTTP/event coupling edge across the full linked-repo set, plus community detection over the combined multi-repo graph — communities spanning >1 repo are real cross-repo architectural boundaries |

### Knowledge base

| Tool | What it does |
|---|---|
| `manage_notes` | Add/list/remove freeform notes attached to a symbol id |
| `manage_adr` | Get/list/update/remove named sections of a project-level Architecture Decision Record document — for durable decisions that don't attach to one symbol |

### Index management

| Tool | What it does |
|---|---|
| `doctor` | Index health report — parse coverage, edge confidence, unresolved call summary |
| `import_scip` | Additive ingestion from a Sourcegraph SCIP `index.scip` file |

---

## Watch Mode

`mamari serve` keeps the in-memory MCP index live by default, re-baking only changed files. Edits, creates, renames, and deletes are all handled — previous evidence for the changed file is dropped, the file is re-scanned, and cross-file resolution is recomputed.

```bash
# MCP server with live re-indexing (default)
mamari serve --index .mamari/index.json

# Also rewrite the on-disk index after each rebake
mamari serve --index .mamari/index.json --persist

# Deliberately keep the loaded index immutable
mamari serve --index .mamari/index.json --watch=false

# Standalone file watcher (no MCP server)
mamari watch --repo . --debounce-ms 200
```

`--debounce-ms` coalesces rapid save bursts from editors that emit multiple filesystem events per write.

---

## CLI Reference

Every MCP tool has a CLI equivalent with `--json` for the MCP response shape.

```bash
# Index & status
mamari init --mcp codex              # index + validated Codex project config
mamari init --repo .
mamari index --repo .
mamari status
mamari doctor --index .mamari/index.json --json

# Git-portable index (opt-in): commit the index so `git pull` alone is
# enough for teammates to use Mamari, no `mamari index` run required.
mamari init --commit-index           # writes .mamari/committed/index.json + installs the pre-commit hook
mamari hooks install                 # same hook install, for a repo already indexed
mamari index --commit                # refresh .mamari/committed/index.json by hand
mamari doctor --check-committed      # CI guard: fails if the committed index drifted from the working tree

# Discovery
mamari search-code "how are background jobs prevented from running twice"
mamari inspect-exact "POST /api/datasets application/json"
mamari inspect-flow "dataset permission scopes" --budget 1800

# Symbol graph
mamari list-symbols --json --limit 20
mamari find-symbol useDataset --source-only --json
mamari trace-symbol useDataset --json --with-edges
mamari inspect-symbol getAccessLevelIcon --budget 1800 --json
mamari inspect-symbol TraceSymbol --format node --budget 900 --json
mamari find-references useDataset --json
mamari impact useDataset --json
mamari repo-map --mentioned useDataset --architecture --budget 1800 --json

# Code review & maintenance
mamari review --base main --callers          # what does my branch change, and what does it affect?
mamari review --json                          # uncommitted changes vs HEAD
mamari review --base main --coverage coverage/lcov.info   # untested = what actually ran under tests
mamari dead-code --json                        # unreferenced symbols + honest "uncertain" list
mamari dead-code --include-exported --json
mamari duplicates --json                       # structural clone clusters (reusability)
mamari report                                  # repo report card: dead code, untested %, duplication, hotspots
mamari report --fail-on "dead<=150,untested-pct<=80"   # CI gate: exits non-zero when a threshold is exceeded
mamari query-graph "MATCH (a:function)-[:calls*1..3]->(b:function) WHERE a.name = 'handleRequest' RETURN b.name, b.file" --json

# Source fetching
mamari fetch-context useDataset --budget 1200 --callers --callees --json
mamari fetch-context src/composables/useDataset.ts:65 --budget 800 --json
mamari fetch-source src/composables/useDataset.ts:60:80

# RDF / Turtle
mamari trace-term dcterms:identifier --json
mamari trace-term dcterms:identifier --json --compact
mamari trace-term dcterms:identifier --json --grouped
mamari inspect-term sh:in --budget 900 --mode evidence --json
mamari find-references sh:path --json
mamari list-terms
mamari list-terms dcterms

# SCIP import
mamari import-scip --index .mamari/index.json --scip index.scip

# MCP server
mamari serve --index .mamari/index.json
mamari serve --index .mamari/index.json --full-toolset   # register every MCP tool unconditionally

# Local visual graph explorer
mamari ui --index .mamari/index.json --watch
mamari setup-mcp --client all --index .mamari/index.json   # preview all file-based client configs
mamari version
```

---

## How It Works

Mamari builds a **Code Graph Protocol (CGP)** index — a JSON symbol graph with stable IDs, typed edges, and confidence levels.

`mamari index` also writes sidecars next to the main index: `.mamari/literals.jsonl` for large literal payloads and, when `MAMARI_PERSIST_SEARCH=1` is set, a compact gob-encoded `.mamari/search.json` for the tokenized `search_code` cache. The search sidecar remains opt-in because rebuilding that cache is already highly parallel. `semantic_query` lazily builds `.mamari/semantic.gob` on first use; its int8 vectors are hash-validated, automatically invalidated by watch rebakes, and reused by later processes. Index and sidecar files are replaced atomically, so terminating a writer cannot expose a partially written target file.

By default `.mamari/` is gitignored — every developer indexes locally. Opting in with `mamari init --commit-index` (or `mamari hooks install` on an already-indexed repo) additionally writes a plain-JSON, diff-reviewable copy to `.mamari/committed/index.json`, un-ignores just that subdirectory, and installs a pre-commit hook that regenerates it before every commit. The hook script itself lives in `.git/hooks/`, which git does not track, so each fresh clone needs one local `mamari hooks install` run to keep the index *updating* going forward (the same one-time-per-clone tradeoff tools like husky make) — but the committed index data itself is usable immediately on `git clone`/`git pull` with no setup at all.

**JS / TS / Vue** files are parsed by a token-driven structural parser (pure Go, no cgo) that masks strings, template literals, regex literals, and comments before extracting calls. This removes prose/JSDoc false positives. Vue SFCs additionally produce `renders-component`, `passes-prop`, `binds-model`, and `listens-event` edges from template analysis, and lightweight `vue-prop`/`vue-model`/`vue-emit` symbols from `defineProps`/`defineModel`/`defineEmits` declarations.

**Python, Java, Go, C#, Rust, Ruby, PHP, C, C++, Kotlin, Bash, Scala, Lua, Elixir, Dart, Haskell, Clojure, and Swift** are parsed with tree-sitter (cgo), giving structural nesting, multi-line signatures, imports, and scoped call resolution where static type/import evidence permits it. Calls resolved this way are marked `exact` or `scoped`; dynamic receivers remain explicitly unresolved rather than being silently promoted. Java's Spring MVC annotations and C#'s ASP.NET Core route attributes produce `http-route` symbols with `handles-route` edges.

**Terraform and OpenTofu native `.tf` files** use the HCL tree-sitter grammar plus Terraform-aware indexing. Mamari preserves canonical addresses such as `var.region`, `local.name`, `data.aws_ami.ubuntu`, `aws_instance.web`, `module.network`, and `ephemeral.random_password.db`, plus `action.aws_lambda_invoke.restart` for current Terraform action blocks; resolves expression traversals to exact `depends-on` edges within the same module directory; recognizes aliased providers; and links local module `source` directories through `imports` edges. Plain `.hcl` files retain generic HCL block behavior.

**Turtle/SHACL** files are lexically indexed with file-scoped prefix resolution and named `sh:NodeShape` block extraction. Large literal payloads are written to a sidecar `.mamari/literals.jsonl` so symbol workflows skip the loading cost.

Known lock data and Turtle, YAML, or HCL files larger than 2 MiB are omitted
from the line-token search cache. They remain indexed for graph, symbol,
literal, and focused source operations; this prevents generated vocabularies,
state, or lock files from consuming most of a server's cold-search resources.

All source reads resolve symlinks and require the final target to remain inside
the indexed repository. A tracked file that points outside the repository, or
is replaced by such a symlink after indexing, is not exposed through Mamari.

**Unresolved calls** are represented as `unresolved:<name>` edges instead of being guessed — the graph never silently misattributes a call.

The build is parallelized with a worker pool sized to `runtime.NumCPU()`. All index mutations are guarded by an internal mutex and tested with `go test -race`.

---

## Operational characteristics

Identical queries return byte-identical responses across runs and fresh server processes. Rankings are deterministic, generation-checked caches are invalidated after file changes, and the test suite includes race detection.

By default, `mamari serve` installs an adaptive Go heap limit equal to 224 MiB plus the primary and linked index sizes. An existing `GOMEMLIMIT` takes precedence. Use `--memory-limit-mb N` for an explicit limit, or `--memory-limit-mb 0` to leave the runtime limit unchanged.

Repository-wide lexical and semantic search caches are built lazily when a
request first needs them. Starting an MCP server, leaving it idle, or changing
watched files before those features are used does not trigger speculative
whole-repository cache work. Once the lexical cache exists, watch rebakes
refresh only the files that changed.

### Current limitations

- Structural indexing is not compiler-equivalent resolution. Dynamic dispatch, reflection, dependency injection, proxies, macros, and metaprogramming can leave calls honestly `unresolved`.
- Syntax newer than a pinned tree-sitter grammar can mark an otherwise valid file as `partial`.
- XML, RDF/XML (`.rdf`), and Markdown are not currently indexed. RDF and SHACL support is provided through Turtle (`.ttl`).
- `semantic_query` is local and repository-adaptive; it does not ship a large pretrained embedding model, so broad paraphrases outside repository vocabulary can be weaker.
- `query_graph` supports chained and bounded variable-length patterns plus common aggregates, but intentionally omits full Cypher features such as `OPTIONAL MATCH`, `UNION`, `WITH`, and subqueries.

## Contributing

Bug reports and pull requests are welcome. See
[`CONTRIBUTING.md`](CONTRIBUTING.md) for the development and verification
workflow, and [`SECURITY.md`](SECURITY.md) for private vulnerability reports.
Maintainer release steps are in [`RELEASE.md`](RELEASE.md).

```bash
go test ./...
go test -race ./...
gofmt -l .
```

CI runs formatting, vet, tests, race tests, release-config validation, and
Unix/Windows installer smoke tests on every push.

---

## License

[MIT](LICENSE)
