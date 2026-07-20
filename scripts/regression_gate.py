#!/usr/bin/env python3
"""Fast local regression gate for Mamari token/relevance guardrails.

This script is intentionally self-contained and competitor-free. It checks the
behaviors that past competitive benchmarks exposed as regressions, then builds
the CLI and verifies the same shapes through real commands.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import tempfile
import time
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
GO = os.environ.get("GO") or shutil.which("go")
if GO is None:
    raise SystemExit("Go toolchain not found: install Go or set GO to its executable path")

MAMARI_TEST_RE = "|".join(
    [
        "TestBuildIndexSkipsHugeGeneratedParserArtifacts",
        "TestCompactQueryGraphLiteResponseIsLosslessAndColumnOrdered",
        "TestInspectFlowPrefersCoreImplementationOverExamples",
        "TestInspectFlowRanksFrameworkLifecycleImplementationOverRequestWrappers",
        "TestInspectFlowBoundsLargeParserContext",
        "TestLimitDoctorParseFailuresPreservesTotalAndOriginal",
        "TestOverloadedMethodResolutionIsDeterministicAcrossBuilds",
        "TestQueryGraphLiteRefreshesDeterministicOrderAfterSymbolInsertion",
        "TestQueryGraphLiteWithoutOrderByIsDeterministic",
        "TestRepoMapCacheClonesResponsesAndInvalidatesOnGraphMutation",
        "TestSearchCodeBudgetIncludesSerializedMetadata",
        "TestSearchCodeBlastRadiusSurvivesSymbolCompaction",
        "TestSearchCodeSymbolDetailDefaultsCompact",
        "TestTemplateExpressionRangesFindsMultipleNestedExpressions",
        "TestTraceSymbolCompactOmitsVerboseTestGroupDetails",
        "TestVueComponentNameSkipsNonLiteralNameProperty",
        "TestWatchRebakesOnEdit",
    ]
)
MCP_TEST_RE = "|".join(
    [
        "TestApplyServerMemoryLimitHonorsModesAndRestores",
        "TestAutomaticServerMemoryLimitScalesWithAllIndexes",
        "TestDoctorDispatchBoundsParseFailureExamples",
        "TestInitializeReportsConfiguredServerVersion",
        "TestPrimaryGraphDefaultsToLosslessCompactTable",
        "TestPrimarySearchDefaultsToFocusedEvidence",
        "TestSearchCodeDispatchEnforcesSerializedBudget",
        "TestSlimToolsetDefaultExposesOnlyPrimaryRouter",
        "TestToolAnnotationsMatchActualSideEffects",
    ]
)
CMD_TEST_RE = "|".join(
    [
        "TestResolveMCPConfigCommandUsesStableAbsolutePathWhenWriting",
        "TestRunServeMissingIndexExplainsRecovery",
        "TestServeRejectsInvalidMemoryLimitsBeforeLoadingIndex",
        "TestServeWatchesByDefaultWithExplicitOptOut",
        "TestWriteMCPConfig",
    ]
)


def run(cmd: list[str], *, cwd: Path = ROOT, timeout: int = 120) -> subprocess.CompletedProcess[str]:
    proc = subprocess.run(
        cmd,
        cwd=str(cwd),
        text=True,
        errors="replace",
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=timeout,
        env={**os.environ, "NO_COLOR": "1", "FORCE_COLOR": "0"},
    )
    if proc.returncode != 0:
        raise SystemExit(
            "command failed: "
            + " ".join(cmd)
            + "\nstdout:\n"
            + proc.stdout
            + "\nstderr:\n"
            + proc.stderr
        )
    return proc


def token_count(text: str) -> int:
    try:
        import tiktoken  # type: ignore

        return len(tiktoken.get_encoding("cl100k_base").encode(text))
    except Exception:
        return max(1, len(text) // 4)


def write(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)


def build_binary(out: Path) -> Path:
    out.parent.mkdir(parents=True, exist_ok=True)
    run([GO, "build", "-o", str(out), "./cmd/mamari"], timeout=180)
    return out


def index_repo(mamari: Path, repo: Path) -> Path:
    index = repo / ".mamari" / "index.json"
    if index.parent.exists():
        shutil.rmtree(index.parent)
    run([str(mamari), "index", "-repo", str(repo), "-index", str(index)], timeout=120)
    return index


def inspect_flow(mamari: Path, index: Path, query: str, budget: int = 1800) -> dict[str, Any]:
    proc = run(
        [
            str(mamari),
            "inspect-flow",
            "--json",
            "-index",
            str(index),
            "--budget",
            str(budget),
            query,
        ],
        timeout=120,
    )
    return json.loads(proc.stdout)


def parser_fixture(root: Path) -> Path:
    repo = root / "parser-fixture"
    write(
        repo / "src/search/query-parser.ts",
        """export function parseQuery(raw: string): ParsedQuery {
  const filters: Filter[] = []
  const terms: string[] = []
  const tokens = raw.split(/\\s+/)
  for (const token of tokens) {
    if (!token) continue
    if (token.startsWith("file:")) {
      filters.push({ kind: "file", value: token.slice(5) })
      continue
    }
    if (token.startsWith("symbol:")) {
      filters.push({ kind: "symbol", value: token.slice(7) })
      continue
    }
    if (token.startsWith("kind:")) {
      filters.push({ kind: "kind", value: token.slice(5) })
      continue
    }
    terms.push(token)
  }
  const normalized = terms.map((term) => term.trim()).filter(Boolean).join(" ")
  const query: ParsedQuery = { raw, terms, filters, normalized }
  query.filterText = filters.map((filter) => filter.kind + ":" + filter.value).join(" ")
  query.searchText = [normalized, query.filterText].filter(Boolean).join(" ")
  query.extra = { rawLength: raw.length, tokenCount: tokens.length, filterCount: filters.length }
  query.auditTrail = []
  query.auditTrail.push("parse raw query")
  query.auditTrail.push("extract filters")
  query.auditTrail.push("normalize remaining terms")
  query.auditTrail.push("prepare search text")
  query.debug = [raw, normalized, String(filters.length), String(terms.length)]
  query.scoreHints = terms.map((term) => ({ term, weight: term.length > 3 ? 2 : 1 }))
  query.more = [
    "padding one", "padding two", "padding three", "padding four",
    "padding five", "padding six", "padding seven", "padding eight",
  ]
  query.finalText = query.searchText + query.debug.join(" ")
  return query
}
""",
    )
    write(
        repo / "src/db/queries.ts",
        """export function applyFilters(parsed: ParsedQuery, rows: Row[]): Row[] {
  return rows.filter((row) => parsed.filters.every((filter) => rowMatchesFilter(row, filter)))
}
""",
    )
    return repo


def lifecycle_fixture(root: Path) -> Path:
    repo = root / "lifecycle-fixture"
    write(
        repo / "src/webapp/wrappers.py",
        '''class Request:
    """Generic request wrapper docs mentioning before after teardown exceptions."""

    def close(self):
        pass
''',
    )
    write(
        repo / "src/webapp/app.py",
        """class Application:
    def full_dispatch_request(self, ctx):
        rv = self.preprocess_request(ctx)
        if rv is None:
            rv = self.dispatch_request(ctx)
        return self.finalize_request(ctx, rv)

    def handle_exception(self, ctx, exc):
        return self.finalize_request(ctx, exc, from_error_handler=True)

    def finalize_request(self, ctx, rv, from_error_handler=False):
        response = self.make_response(rv)
        return self.process_response(ctx, response)

    def process_response(self, ctx, response):
        return response

    def do_teardown_request(self, ctx, exc=None):
        for func in reversed(self.teardown_request_funcs):
            self.ensure_sync(func)(exc)
""",
    )
    write(
        repo / "src/webapp/ctx.py",
        """class RequestContext:
    def pop(self, exc=None):
        self.app.do_teardown_request(self, exc)
""",
    )
    return repo


def generated_fixture(root: Path) -> Path:
    repo = root / "generated-fixture"
    write(repo / "src/app.go", "package app\nfunc RealSymbol() {}\n")
    generated = "/* generated parser */\n" + ("static int generated_symbol(void) { return 0; }\n" * 70000)
    write(repo / "internal/mamari/treesitter/swiftgrammar/parser.c", generated)
    return repo


def assert_parser_gate(mamari: Path, root: Path) -> None:
    index = index_repo(mamari, parser_fixture(root))
    resp = inspect_flow(mamari, index, "parse raw query filters", budget=1800)
    slices = resp.get("context", {}).get("slices", [])
    parser_slices = [s for s in slices if s.get("file") == "src/search/query-parser.ts"]
    if not parser_slices:
        raise SystemExit("parser gate failed: no query-parser context returned")
    for item in parser_slices:
        span = int(item.get("endLine", 0)) - int(item.get("startLine", 0)) + 1
        if span > 48:
            raise SystemExit(f"parser gate failed: parser slice is {span} lines, want <= 48")
    if token_count(json.dumps(resp, separators=(",", ":"))) > 2200:
        raise SystemExit("parser gate failed: inspect-flow parser response exceeded 2200 tokens")


def assert_lifecycle_gate(mamari: Path, root: Path) -> None:
    index = index_repo(mamari, lifecycle_fixture(root))
    resp = inspect_flow(mamari, index, "request dispatch before after teardown exceptions", budget=1800)
    hits = resp.get("search", {}).get("hits", [])
    if not hits:
        raise SystemExit("lifecycle gate failed: no hits")
    first = hits[0].get("file")
    if first not in {"src/webapp/app.py", "src/webapp/ctx.py"}:
        raise SystemExit(f"lifecycle gate failed: first hit is {first!r}")
    if any(hit.get("file") == "src/webapp/wrappers.py" for hit in hits[:2]):
        raise SystemExit("lifecycle gate failed: wrapper docs outranked implementation")


def assert_generated_gate(mamari: Path, root: Path) -> None:
    repo = generated_fixture(root)
    index = index_repo(mamari, repo)
    proc = run([str(mamari), "trace-symbol", "--json", "-index", str(index), "generated_symbol"], timeout=120)
    resp = json.loads(proc.stdout)
    if resp.get("status") != "not_found":
        raise SystemExit("generated gate failed: huge generated parser artifact contributed generated_symbol")


def assert_default_watch_gate(mamari: Path, root: Path) -> None:
    repo = root / "default-watch-fixture"
    write(repo / "base.js", "export function base() { return 1 }\n")
    index = index_repo(mamari, repo)
    proc = subprocess.Popen(
        [str(mamari), "serve", "--index", str(index)],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        errors="replace",
        bufsize=1,
    )
    assert proc.stdin is not None
    assert proc.stdout is not None

    def send(message: dict[str, Any]) -> None:
        proc.stdin.write(json.dumps(message, separators=(",", ":")) + "\n")
        proc.stdin.flush()

    def receive(request_id: int) -> dict[str, Any]:
        while True:
            line = proc.stdout.readline()
            if not line:
                stderr = proc.stderr.read() if proc.stderr is not None else ""
                raise SystemExit(f"default-watch gate server exited early:\n{stderr}")
            message = json.loads(line)
            if message.get("id") == request_id:
                return message

    def search(request_id: int, query: str) -> list[str]:
        send(
            {
                "jsonrpc": "2.0",
                "id": request_id,
                "method": "tools/call",
                "params": {"name": "mamari", "arguments": {"action": "search", "query": query}},
            }
        )
        response = receive(request_id)
        payload = json.loads(response["result"]["content"][0]["text"])
        return [str(hit.get("file", "")) for hit in payload.get("hits", [])]

    try:
        send(
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {
                    "protocolVersion": "2025-03-26",
                    "capabilities": {},
                    "clientInfo": {"name": "regression-gate", "version": "1"},
                },
            }
        )
        receive(1)
        send({"jsonrpc": "2.0", "method": "notifications/initialized", "params": {}})

        marker = "mamari_default_watch_regression_marker"
        rel = "src/live-marker.js"
        if search(10, marker):
            raise SystemExit("default-watch gate fixture unexpectedly contained marker before edit")
        write(repo / rel, f'export const marker = "{marker}"\n')
        deadline = time.monotonic() + 3
        request_id = 11
        while time.monotonic() < deadline:
            time.sleep(0.1)
            if rel in search(request_id, marker):
                return
            request_id += 1
        raise SystemExit("default-watch gate failed: plain `mamari serve` missed a live edit")
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mamari-bin", type=Path, help="Existing Mamari binary to test")
    parser.add_argument("--skip-go-tests", action="store_true", help="Only run black-box CLI gates")
    args = parser.parse_args()

    if not args.skip_go_tests:
        run([GO, "test", "./cmd/mamari", "-run", CMD_TEST_RE], timeout=180)
        run([GO, "test", "./internal/mamari", "-run", MAMARI_TEST_RE], timeout=180)
        run([GO, "test", "./internal/mcpserver", "-run", MCP_TEST_RE], timeout=180)

    with tempfile.TemporaryDirectory(prefix="mamari-regression-gate-") as tmp:
        tmp_path = Path(tmp)
        mamari = args.mamari_bin or build_binary(tmp_path / "mamari")
        assert_parser_gate(mamari, tmp_path)
        assert_lifecycle_gate(mamari, tmp_path)
        assert_generated_gate(mamari, tmp_path)
        assert_default_watch_gate(mamari, tmp_path)

    print("mamari regression gate passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
