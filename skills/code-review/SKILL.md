---
name: code-review
description: Review a pull request, branch, or working-tree diff in ANY repository, grounded in mamari's real call graph. Use when asked to "review this PR", "review my changes/branch", "do a code review", or "review the diff against <branch>". Language- and stack-agnostic: it drives mamari's review/impact/trace/dead_code/duplicates flows and judges the diff against the repository's own coding standards (discovered automatically) plus a universal engineering rubric. Portable — drop this into any repo's .claude/skills/.
metadata:
  short-description: mamari-grounded PR/code review for any repo and language
---

# Code Review (mamari-grounded, generic)

Review a change the way a strong senior engineer would: grounded in the **real
call graph** (never guessed), measured against **this repository's own
standards** first and universal engineering principles second. mamari is the
ground-truth engine — it tells you what actually changed, what it reaches,
what's untested, where the risk is, and what's duplicated. You bring judgment.

**Golden rule of trust:** mamari never invents a call edge. When it says "N
proven callers," that is N it *resolved*; anything it can't prove is marked
`unresolved`. Report its numbers as facts, but treat "proven" as a **floor, not
a ceiling** — dynamically-dispatched calls (reflection, DI, event handlers,
string/table dispatch) are honestly unresolved and may add real callers.

This skill hardcodes no team's rules or tech stack. It **discovers** the repo's
conventions (§2 Step A) and applies only the stack lenses that fit the changed
files (§2 Step C).

---

## 0. Setup (once per repository)

```bash
mamari init -mcp claude   # index + validate + write project MCP config
```

In a session you call the single `mamari` MCP tool with an `action`. The CLI
(`mamari <cmd>`) is the same engine for CI/scripts.

---

## 1. The review workflow

### Step 1 — Make the index match what you're reviewing (non-negotiable)
`review` maps the git diff onto the **current index**, so it must reflect the
code on disk. If `mamari serve` (watch mode) is running it's already current;
otherwise:
```bash
mamari status -json     # compare "indexedCommit" vs "currentCommit" / check "stale"
mamari index            # if commits differ, or files were edited since indexing
```
Reviewing a colleague's PR by number: `gh pr checkout <n>` (or `git fetch && git
checkout <branch>`), then `mamari index`.

### Step 2 — Get the structural review (the spine)
Determine the **target branch** the work merges into (`main`/`master`/`develop`
— ask if unclear) and diff against the **merge-base**, not the branch tip
(`review` diffs against whatever ref you give it; if the target moved on, the
bare branch name blames its newer commits on this review):
```bash
BASE=$(git merge-base main HEAD)
mamari review -base "$BASE" -limit 100000        # CLI  (MCP: action "review", query "<BASE sha>")
```
If tests were run with coverage, add `-coverage <lcov file>` (MCP:
`args_json.coverage`) so "untested" reflects what actually executed, not just
static reachability. **`-limit 0` is NOT "all"** — it caps at 40; pass a large
limit for full coverage and check `truncated`.

Each changed symbol carries: `proven`/`possible` caller counts, `changeKind`
(`signature`/`body`/`new`), `untested` (+`untestedBy`), and `risk` with
`riskReasons` (complexity, hot-path/loop depth, O(n²) scan-in-loop).

### Step 3 — Triage; don't read everything
Review in order and **state where you stopped**:
1. `high` risk symbols;
2. `signature` changes with callers (interface changed → callers may break);
3. high proven-caller counts (wide blast radius);
4. `untested` + changed (the diff is the only safety net);
5. the rest — skim.

### Step 4 — Deep-review each selected symbol
Use its `file:name` from the review output (bare names are ambiguous):
```bash
mamari fetch-context "path/to/file:symbolName"   # MCP: action "context" — read the symbol
mamari impact -depth 2 "path/to/file:symbolName"  # MCP: action "impact"  — who breaks if it's wrong
mamari trace-symbol "path/to/file:symbolName"     # MCP: action "trace"   — callers + callees
```
(`fetch-source` / the `source` action take a `file:start:end` line range, not a
symbol name.) Then read the actual diff hunk (`git diff "$BASE" -- <file>`) and
judge it against §2. Anchor every finding to `file:line`.

### Step 5 — Dead code & duplication
```bash
mamari dead-code -limit 100000    # [dead] vs [uncertain] (held back by unresolved same-name calls)
mamari duplicates -limit 100000   # structural clone clusters (reusability)
```
**Verify before calling anything dead:** static analysis can't see dynamic
dispatch — background jobs/cron, route tables, event listeners, reflection, DI,
string-keyed lookups. Confirm with `mamari search-code "<name>"` (MCP: `search`)
before reporting a symbol dead. Use `duplicates` on new/changed code to catch
"this already exists as X."

### Step 6 — Write the review (§4)

---

## 2. The rubric

### Step A — Adopt the repository's OWN standards first
Before applying generic rules, discover what this project already mandates and
prefer it (a repo's explicit convention beats a universal default):
- Contributor/standards docs: `CONTRIBUTING*`, `CLAUDE.md`, `AGENTS.md`,
  `docs/**` (style/coding/architecture guides), `ADR`/`adr/` records.
- Lint/format config as machine-readable rules: `.eslintrc*`, `biome.json`,
  `.ruff.toml`/`ruff` in `pyproject.toml`, `.golangci.yml`, `.rubocop.yml`,
  `.editorconfig`, `checkstyle`, `clippy`.
- Existing code: match the surrounding file's style, naming, and idioms even if
  you'd personally do it differently.

Cite the repo's own rule when you flag something under it ("CONTRIBUTING.md
§Errors: …"). If the repo has no stated standard for a point, fall back to §B.

### Step B — Universal rubric (applies everywhere)
- **Correctness & logic:** off-by-one, nil/undefined, wrong operator/branch,
  boundary cases, error/edge paths, async/await misuse, mutation of shared or
  caller-owned data.
- **Error handling:** errors surfaced not swallowed; failures not masked by
  default/empty returns; caught at a sensible boundary, not everywhere.
- **Complexity & size:** deep nesting and high cyclomatic complexity (mamari's
  `complexity`/`risk` flag these) — prefer early returns and extraction. Very
  long functions doing several jobs → split.
- **Single responsibility & layering:** one unit = one job; respect the
  project's layering (don't reach across boundaries the codebase separates).
- **Naming & clarity:** intention-revealing names; no misleading names; comments
  explain *why*, not restate *what*.
- **Tests:** changed logic has a test that would fail without the change;
  `untested` changed symbols are called out (with coverage if provided).
- **Dead code & duplication:** newly-orphaned code removed; new code that
  duplicates existing structure (mamari `duplicates`) refactored or justified.
- **Security:** untrusted input validated; no injection (SQL/command/template);
  no secrets committed; authz/authn checks not bypassed; safe deserialization.
- **Performance & scalability:** work that grows with data volume — O(n²) over
  unbounded input (mamari flags scan-in-loop), N+1 data-access in a loop,
  per-item network/IO, unbounded in-memory accumulation, missing pagination.
- **Concurrency:** shared state without synchronization, races, deadlock-prone
  lock ordering, goroutine/thread/subscription leaks.
- **Public interface changes:** a `signature`/exported-API change (mamari
  `changeKind: signature`) with callers — the risk reason quotes the direct
  call sites that must pass new arguments (`provenCount` stays the transitive
  blast radius); breaking changes flagged and versioned/migrated appropriately.

### Step C — Stack lenses (apply only those matching the changed files)
- **Frontend components (React/Vue/Svelte/…):** reactivity/state pitfalls
  (stale closures, broken reactivity, effect deps), keys in lists, side effects
  cleaned up on unmount, no business logic in templates, no direct network calls
  from components.
- **Backend + database:** query correctness, N+1, missing/covering indexes,
  projection, transaction boundaries, migration safety.
- **Systems/low-level:** memory ownership/lifetime, bounds, resource cleanup,
  error propagation.
- **Infra/config (IaC, CI, Docker, k8s):** least privilege, no plaintext
  secrets, idempotency, blast radius of the change.
Apply a lens only when the diff touches that kind of code; skip the rest.

---

## 3. Concern → tool

| Concern | mamari | You judge |
|---|---|---|
| Coding standards | `context`/`fetch-context` reads exact code; `explore`/`map` for structure | §2A repo rules + §2B rubric |
| Blast radius / breaking change | `impact`, `trace`, review `changeKind` + proven/possible callers | is the interface change safe for all callers? |
| Loop complexity / scalability | review `risk`: complexity, loop/transitive-loop depth, O(n²) scan-in-loop | essential nesting? hot path doing expensive per-iteration work? |
| Reusability / duplication | `duplicates` clusters; `search`/`exact` for an existing helper | should this be extracted / reuse existing? |
| Dead code | `dead_code` (dead vs uncertain) | verify not dynamically dispatched before removing |
| Test coverage | review `untested` (+`coverage`); `tests_for`/`untested_symbols` | is the changed logic actually exercised? |
| Memory / resource leaks | `trace`/`impact` for lifetime; `search` for listener/interval patterns | uncleaned listeners, unbounded caches, retained large objects |

---

## 4. Output format
Lead with a verdict; findings ranked by severity, each anchored to `file:line`
and tied to a rule (repo rule or rubric item). Be specific and proportional —
flag what a changed line does wrong; don't demand rewrites of untouched code.

```
## Review: <branch> → <target>  (mamari-grounded)

Verdict: Approve / Approve with nits / Request changes
Scope: N changed symbols across M files. Reviewed: <what>. Skimmed/omitted: <what>.
Blast radius: X proven callers, Y possible (unresolved dynamic calls may add more).
Tests: Z changed symbols untested.

### Blocking
- `file:LINE` <rule> — <what's wrong> → <fix>. Impact: <who this breaks>.
### Should fix
- ...
### Nits
- ...
### Untested changes
- `file:LINE name` — <what a test should assert>
### Dead code / duplication (verify dynamic dispatch first)
- `file:LINE` — candidate; <evidence needed / existing equivalent>
```

**Honesty:** distinguish proven vs possible; never upgrade a possible caller to
a fact; say what you did NOT review. No false "looks complete."

---

## 5. Command reference

**MCP** — call the `mamari` tool with `action` + `query` (+ optional `args_json`):

| action | query | purpose |
|--------|-------|---------|
| `review` | base ref / merge-base sha | changed symbols + blast radius + `changeKind` + untested + risk. `args_json`: `{"limit":100000}`, `{"coverage":"<lcov>"}`, `{"callers":true}` |
| `dead_code` | — | unreferenced + uncertain symbols (`{"limit":100000}`) |
| `duplicates` | — | structural clone clusters |
| `impact` | `file:name` | reverse caller closure, confidence-tagged |
| `trace` | `file:name` | direct callers + callees |
| `context` | `file:name` | read a symbol's code + neighborhood |
| `source` | `file:start:end` | raw source for a line range |
| `search` | text | where a name/concept appears (dynamic-dispatch & reuse checks) |
| `explore`/`map` | path or question | structure / architecture |

**CLI:** `mamari status -json | index | serve | init -mcp claude`,
`mamari review -base <ref> -limit 100000 [-json] [-callers] [-coverage <lcov>]`,
`mamari dead-code -limit 100000 [-json]`, `mamari duplicates [-json]`,
`mamari impact -depth 2 <file:name>`, `mamari trace-symbol <file:name>`,
`mamari fetch-context <file:name>`, `mamari fetch-source <file:start:end>`,
`mamari search-code <text>`. (CLI flags use a single dash, before the positional.)

**Gotchas:** disambiguate symbols as `path/to/file:name` (bare names are
ambiguous). Re-index (or run `serve`) after switching branches or editing.
`limit: 0` is not "all" (review caps at 40, dead-code at 500) — pass a large
limit and check `truncated`. `review -base <branch>` diffs the branch **tip** —
resolve `git merge-base <branch> HEAD` first to review only this PR's changes.

---

## Deploy
Copy this directory into a repo's `.claude/skills/code-review/` (per-repo, shared
via git) or `~/.claude/skills/code-review/` (all your repos). To specialize it
for a team, add the team's concrete rules under §2 Step A (or point Step A at
the team's standards doc) — keep the mamari workflow as-is.
