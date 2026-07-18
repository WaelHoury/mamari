#!/usr/bin/env python3
"""Token benchmark for mamari evidence lookup versus grep-style baselines."""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from collections import defaultdict
from dataclasses import dataclass
from pathlib import Path
from typing import Any

try:
    import tiktoken
except ImportError as exc:
    raise SystemExit(
        "This benchmark requires tiktoken. Install it with: python3 -m pip install tiktoken"
    ) from exc


COMPACT_RESULT_LIMIT = 50
DEFAULT_GLOBS = ["*.ttl", "*.ts", "*.tsx", "*.js", "*.jsx", "*.mjs", "*.cjs", "*.vue"]
DEFAULT_EXCLUDES = [".mamari/**", "node_modules/**"]
BASELINE_METRICS = [
    "grepExactOutput",
    "grepLocalOutput",
    "grepExactOutputPlusFullFiles",
    "grepLocalOutputPlusFullFiles",
]


@dataclass(frozen=True)
class Metric:
    tokens: int
    bytes: int
    chars: int
    files: int = 0
    lines: int = 0


def token_count(encoding: Any, text: str) -> int:
    return len(encoding.encode(text))


def metric(encoding: Any, text: str, files: int = 0, lines: int = 0) -> Metric:
    return Metric(
        tokens=token_count(encoding, text),
        bytes=len(text.encode("utf-8")),
        chars=len(text),
        files=files,
        lines=lines,
    )


def split_term(term: str) -> tuple[str, str]:
    if ":" not in term:
        return "", ""
    prefix, local = term.split(":", 1)
    if not prefix or not local:
        return "", ""
    return prefix, local


def term_summary(term: dict[str, Any]) -> dict[str, Any]:
    out = {
        "id": term.get("id", ""),
        "term": term.get("term", ""),
        "iri": term.get("iri", ""),
    }
    if term.get("prefix"):
        out["prefix"] = term["prefix"]
    out["localName"] = term.get("localName", "")
    return out


def resolve_query(index: dict[str, Any], query: str) -> tuple[str, list[dict[str, Any]], list[dict[str, Any]]]:
    query = query.strip()
    terms = list(index.get("terms", {}).values())
    if not query:
        return "invalid", [], []
    if query.startswith(("http://", "https://")):
        matches = sorted([term for term in terms if term.get("iri") == query], key=lambda t: t.get("term", ""))
        if len(matches) > 1:
            return "ambiguous", [], matches
        if len(matches) == 1:
            return "found", matches, []
        return "not_found", [], []
    prefix, local = split_term(query)
    if prefix and local:
        matches = sorted([term for term in terms if term.get("term") == query], key=lambda t: t.get("iri", ""))
        if len(matches) > 1:
            return "ambiguous", [], matches
        if len(matches) == 1:
            return "found", matches, []
        iri = index.get("prefixes", {}).get(prefix, {}).get("iri", "")
        if iri:
            synthetic = {
                "id": "term:" + query,
                "term": query,
                "iri": iri + local,
                "prefix": prefix,
                "localName": local,
                "locations": [],
            }
            return "not_found", [synthetic], []
        return "not_found", [], []
    matches = sorted([term for term in terms if term.get("localName") == query], key=lambda t: t.get("term", ""))
    if len(matches) > 1:
        return "ambiguous", [], matches
    if len(matches) == 1:
        return "found", matches, []
    return "not_found", [], []


def dedupe_locations(locations: list[dict[str, Any]]) -> list[dict[str, Any]]:
    seen: set[tuple[str, int, int]] = set()
    out = []
    for loc in locations:
        key = (loc.get("file", ""), int(loc.get("startLine", 0)), int(loc.get("startColumn", 0)))
        if key in seen:
            continue
        seen.add(key)
        out.append(loc)
    return sorted(out, key=lambda loc: (loc.get("file", ""), loc.get("startLine", 0), loc.get("startColumn", 0)))


def trace_compact(index: dict[str, Any], query: str) -> dict[str, Any]:
    status, terms, candidates = resolve_query(index, query)
    response: dict[str, Any] = {
        "status": status,
        "query": query,
        "candidates": [term_summary(term) for term in candidates],
        "ttlUsageCount": 0,
        "codeReferenceCount": 0,
        "edgeCount": 0,
        "ttlUsages": [],
        "codeReferences": [],
    }
    if candidates:
        return response
    if not terms:
        return response

    term = terms[0]
    response["term"] = term_summary(term)
    iri = term.get("iri", "")
    related_terms = [t for t in index.get("terms", {}).values() if iri and t.get("iri") == iri] or [term]

    ttl_usages = []
    for related in related_terms:
        ttl_usages.extend(related.get("locations", []))
    ttl_usages = dedupe_locations(ttl_usages)

    code_references = [
        ref
        for ref in index.get("references", [])
        if ref.get("termId") == term.get("id") or (iri and ref.get("iri") == iri)
    ]
    code_references = sorted(code_references, key=lambda ref: (ref.get("file", ""), ref.get("startLine", 0), ref.get("startColumn", 0)))

    edges = [
        edge
        for edge in index.get("edges", [])
        if edge.get("from") == term.get("id")
        or edge.get("to") == term.get("id")
        or term.get("id", "") in edge.get("to", "")
    ]

    if not ttl_usages and not code_references and not edges:
        response["status"] = "not_found"
    else:
        response["status"] = "found"
    response["ttlUsageCount"] = len(ttl_usages)
    response["codeReferenceCount"] = len(code_references)
    response["edgeCount"] = len(edges)
    response["ttlUsages"] = [
        {
            "file": loc.get("file", ""),
            "startLine": loc.get("startLine", 0),
            "startColumn": loc.get("startColumn", 0),
            "kind": loc.get("kind", ""),
        }
        for loc in ttl_usages[:COMPACT_RESULT_LIMIT]
    ]
    response["codeReferences"] = [
        {
            "file": ref.get("file", ""),
            "startLine": ref.get("startLine", 0),
            "startColumn": ref.get("startColumn", 0),
            "confidence": ref.get("confidence", ""),
            "kind": ref.get("kind", ""),
        }
        for ref in code_references[:COMPACT_RESULT_LIMIT]
    ]
    return response


def group_trace(trace: dict[str, Any]) -> dict[str, Any]:
    grouped = {
        "status": trace.get("status"),
        "query": trace.get("query"),
        "ttlUsageCount": trace.get("ttlUsageCount", 0),
        "codeReferenceCount": trace.get("codeReferenceCount", 0),
        "edgeCount": trace.get("edgeCount", 0),
        "ttlUsages": {},
        "codeReferences": {},
    }
    if trace.get("term"):
        grouped["term"] = trace["term"]
    if trace.get("candidates"):
        grouped["candidates"] = trace["candidates"]
    for loc in trace.get("ttlUsages", []):
        grouped["ttlUsages"].setdefault(loc["file"], []).append(
            {"line": loc["startLine"], "column": loc["startColumn"], "kind": loc["kind"]}
        )
    for ref in trace.get("codeReferences", []):
        grouped["codeReferences"].setdefault(ref["file"], []).append(
            {
                "line": ref["startLine"],
                "column": ref["startColumn"],
                "confidence": ref["confidence"],
                "kind": ref["kind"],
            }
        )
    return grouped


def merge_ranges(ranges: list[tuple[int, int]]) -> list[tuple[int, int]]:
    if not ranges:
        return []
    ranges = sorted(ranges)
    merged = [ranges[0]]
    for start, end in ranges[1:]:
        old_start, old_end = merged[-1]
        if start <= old_end + 1:
            merged[-1] = (old_start, max(old_end, end))
        else:
            merged.append((start, end))
    return merged


def source_slices(repo: Path, file_line_counts: dict[str, int], locations: list[dict[str, Any]], context_lines: int) -> tuple[str, int, int]:
    ranges_by_file: dict[str, list[tuple[int, int]]] = defaultdict(list)
    for loc in locations:
        rel = loc.get("file", "")
        line = int(loc.get("startLine", 0))
        if not rel or line < 1 or rel not in file_line_counts:
            continue
        start = max(1, line - context_lines)
        end = min(file_line_counts[rel], line + context_lines)
        ranges_by_file[rel].append((start, end))

    chunks = []
    merged_count = 0
    line_count = 0
    for rel in sorted(ranges_by_file):
        path = repo / rel
        try:
            lines = path.read_text(encoding="utf-8", errors="replace").splitlines(keepends=True)
        except OSError:
            continue
        for start, end in merge_ranges(ranges_by_file[rel]):
            merged_count += 1
            line_count += end - start + 1
            text = "".join(lines[start - 1 : end])
            chunks.append(f"--- {rel}:{start}:{end} ---\n{text}")
    return "\n".join(chunks), merged_count, line_count


def adaptive_context_lines(location_count: int, max_context_lines: int) -> int:
    if max_context_lines <= 0 or location_count <= 0:
        return 0
    if location_count <= 10:
        return min(max_context_lines, 1)
    if location_count <= 50:
        return min(max_context_lines, 3)
    if location_count <= 200:
        return min(max_context_lines, 5)
    return max_context_lines


def rg_output(repo: Path, pattern: str, includes: list[str], excludes: list[str]) -> str:
    cmd = ["rg", "-n", "--hidden"]
    for item in excludes:
        cmd.extend(["-g", "!" + item])
    for item in includes:
        cmd.extend(["-g", item])
    cmd.extend([pattern, "."])
    proc = subprocess.run(cmd, cwd=repo, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
    if proc.returncode not in (0, 1):
        raise RuntimeError(proc.stderr.strip() or f"rg failed with code {proc.returncode}")
    return proc.stdout


def rg_files(output: str) -> set[str]:
    files = set()
    for line in output.splitlines():
        match = re.match(r"^\./([^:]+):\d+:", line)
        if match:
            files.add(match.group(1))
    return files


def full_file_payload(repo: Path, files: set[str]) -> tuple[str, int]:
    chunks = []
    line_count = 0
    for rel in sorted(files):
        path = repo / rel
        try:
            text = path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            continue
        line_count += len(text.splitlines())
        chunks.append(f"--- {rel} ---\n{text}")
    return "\n".join(chunks), line_count


def evidence_keys(trace: dict[str, Any]) -> set[tuple[str, int]]:
    keys = set()
    for loc in trace.get("ttlUsages", []):
        keys.add((loc["file"], int(loc["startLine"])))
    for ref in trace.get("codeReferences", []):
        keys.add((ref["file"], int(ref["startLine"])))
    return keys


def rg_keys(output: str) -> set[tuple[str, int]]:
    keys = set()
    for line in output.splitlines():
        match = re.match(r"^\./([^:]+):(\d+):", line)
        if match:
            keys.add((match.group(1), int(match.group(2))))
    return keys


def parse_location_key(value: Any) -> tuple[str, int]:
    if isinstance(value, dict):
        file = value.get("file", "")
        line = int(value.get("line", value.get("startLine", 0)))
        return file, line
    if not isinstance(value, str):
        raise ValueError(f"gold location must be a string or object, got {value!r}")
    parts = value.rsplit(":", 2)
    if len(parts) >= 2 and parts[-1].isdigit():
        if len(parts) == 3 and parts[-2].isdigit():
            return parts[0], int(parts[-2])
        return parts[0], int(parts[-1])
    raise ValueError(f"gold location must look like file:line or file:line:column: {value!r}")


def load_gold(path: Path | None) -> dict[str, Any]:
    if path is None:
        return {}
    raw = json.loads(path.read_text(encoding="utf-8"))
    if "terms" in raw:
        return raw["terms"]
    return raw


def gold_for_query(gold: dict[str, Any], query: str) -> dict[str, Any] | None:
    entry = gold.get(query)
    if entry is None:
        return None
    return {
        "exhaustive": bool(entry.get("exhaustive", False)),
        "mustFind": {parse_location_key(item) for item in entry.get("must_find", entry.get("mustFind", []))},
        "mustNotFind": {parse_location_key(item) for item in entry.get("must_not_find", entry.get("mustNotFind", []))},
    }


def evaluate_gold(keys: set[tuple[str, int]], spec: dict[str, Any], tokens: int) -> dict[str, Any]:
    must_find = spec["mustFind"]
    must_not_find = spec["mustNotFind"]
    true_hits = keys & must_find
    missed = must_find - keys
    must_not_hits = keys & must_not_find
    recall = 1.0 if not must_find else len(true_hits) / len(must_find)
    out: dict[str, Any] = {
        "returnedLines": len(keys),
        "trueHits": len(true_hits),
        "missed": len(missed),
        "mustNotViolations": len(must_not_hits),
        "recall": round(recall * 100, 2),
        "tokensPerTrueHit": round(tokens / len(true_hits), 2) if true_hits else None,
    }
    if spec["exhaustive"]:
        false_positives = keys - must_find
        precision = 1.0 if not keys else len(true_hits) / len(keys)
        out["falsePositives"] = len(false_positives)
        out["precision"] = round(precision * 100, 2)
    return out


def pct(saved: int, baseline: int) -> float:
    if baseline <= 0:
        return 0.0
    return round((saved / baseline) * 100, 2)


def benchmark_term(
    encoding: Any,
    index: dict[str, Any],
    repo: Path,
    query: str,
    includes: list[str],
    excludes: list[str],
    context_lines: int,
    adaptive_context: bool,
    gold: dict[str, Any],
) -> dict[str, Any]:
    trace = trace_compact(index, query)
    compact_payload = json.dumps(trace, indent=2, ensure_ascii=False) + "\n"
    grouped_payload = json.dumps(group_trace(trace), indent=2, ensure_ascii=False) + "\n"
    all_locations = trace.get("ttlUsages", []) + trace.get("codeReferences", [])
    line_counts = {rel: info.get("lineCount", 0) for rel, info in index.get("files", {}).items()}
    effective_context_lines = (
        adaptive_context_lines(len(all_locations), context_lines) if adaptive_context else context_lines
    )
    slices_payload, slice_count, slice_lines = source_slices(repo, line_counts, all_locations, effective_context_lines)
    mamari_payload = compact_payload + ("\n" + slices_payload if slices_payload else "")
    mamari_grouped_payload = grouped_payload + ("\n" + slices_payload if slices_payload else "")

    local = trace.get("term", {}).get("localName") or query.split(":")[-1]
    iri = trace.get("term", {}).get("iri", "")
    compact = trace.get("term", {}).get("term", query)
    exact_parts = [re.escape(compact)]
    if iri:
        exact_parts.append(re.escape(iri))
    exact_pattern = "|".join(exact_parts)
    local_pattern = r"\b" + re.escape(local) + r"\b"

    exact_rg = rg_output(repo, exact_pattern, includes, excludes)
    local_rg = rg_output(repo, local_pattern, includes, excludes)
    exact_files = rg_files(exact_rg)
    local_files = rg_files(local_rg)
    exact_full_payload, exact_full_lines = full_file_payload(repo, exact_files)
    local_full_payload, local_full_lines = full_file_payload(repo, local_files)

    mamari_keys = evidence_keys(trace)
    exact_keys = rg_keys(exact_rg)
    local_keys = rg_keys(local_rg)

    metrics = {
        "mamariCompactOnly": metric(encoding, compact_payload),
        "mamariCompactPlusSlices": metric(encoding, mamari_payload, files=len({loc.get("file", "") for loc in all_locations}), lines=slice_lines),
        "mamariGroupedCompactOnly": metric(encoding, grouped_payload),
        "mamariGroupedCompactPlusSlices": metric(encoding, mamari_grouped_payload, files=len({loc.get("file", "") for loc in all_locations}), lines=slice_lines),
        "grepExactOutput": metric(encoding, exact_rg, files=len(exact_files), lines=len(exact_rg.splitlines())),
        "grepLocalOutput": metric(encoding, local_rg, files=len(local_files), lines=len(local_rg.splitlines())),
        "grepExactOutputPlusFullFiles": metric(
            encoding,
            exact_rg + ("\n" + exact_full_payload if exact_full_payload else ""),
            files=len(exact_files),
            lines=len(exact_rg.splitlines()) + exact_full_lines,
        ),
        "grepLocalOutputPlusFullFiles": metric(
            encoding,
            local_rg + ("\n" + local_full_payload if local_full_payload else ""),
            files=len(local_files),
            lines=len(local_rg.splitlines()) + local_full_lines,
        ),
    }

    mamari_tokens = metrics["mamariCompactPlusSlices"].tokens
    savings = {}
    for name, value in metrics.items():
        if name.startswith("mamari"):
            continue
        savings[name] = {
            "savedTokens": value.tokens - mamari_tokens,
            "savedPercent": pct(value.tokens - mamari_tokens, value.tokens),
        }

    coverage_total = max(1, len(mamari_keys))
    strategy_keys = {
        "mamariCompactOnly": mamari_keys,
        "mamariCompactPlusSlices": mamari_keys,
        "mamariGroupedCompactOnly": mamari_keys,
        "mamariGroupedCompactPlusSlices": mamari_keys,
        "grepExactOutput": exact_keys,
        "grepLocalOutput": local_keys,
        "grepExactOutputPlusFullFiles": exact_keys,
        "grepLocalOutputPlusFullFiles": local_keys,
    }
    term_gold = gold_for_query(gold, query)
    gold_evaluation = None
    fair_comparison = {
        "basis": "mamariEvidenceLines",
        "target": "mamariGroupedCompactPlusSlices",
        "cheapestBaselineAtMamariRecall": None,
    }
    if term_gold:
        gold_evaluation = {
            name: evaluate_gold(keys, term_gold, metrics[name].tokens)
            for name, keys in strategy_keys.items()
        }
        target_recall = gold_evaluation["mamariCompactPlusSlices"]["recall"]
        eligible = [
            name
            for name in BASELINE_METRICS
            if gold_evaluation[name]["recall"] >= target_recall
            and gold_evaluation[name]["mustNotViolations"] == 0
        ]
        fair_comparison["basis"] = "gold"
        if eligible:
            best = min(eligible, key=lambda name: metrics[name].tokens)
            fair_comparison["cheapestBaselineAtMamariRecall"] = {
                "name": best,
                "tokens": metrics[best].tokens,
                "mamariTokens": metrics[fair_comparison["target"]].tokens,
                "savedTokens": metrics[best].tokens - metrics[fair_comparison["target"]].tokens,
                "savedPercent": pct(metrics[best].tokens - metrics[fair_comparison["target"]].tokens, metrics[best].tokens),
                "recall": gold_evaluation[best]["recall"],
                "precision": gold_evaluation[best].get("precision"),
            }
    else:
        eligible = [
            name
            for name, keys in {
                "grepExactOutput": exact_keys,
                "grepLocalOutput": local_keys,
                "grepExactOutputPlusFullFiles": exact_keys,
                "grepLocalOutputPlusFullFiles": local_keys,
            }.items()
            if mamari_keys <= keys
        ]
        if eligible:
            best = min(eligible, key=lambda name: metrics[name].tokens)
            fair_comparison["cheapestBaselineAtMamariRecall"] = {
                "name": best,
                "tokens": metrics[best].tokens,
                "mamariTokens": metrics[fair_comparison["target"]].tokens,
                "savedTokens": metrics[best].tokens - metrics[fair_comparison["target"]].tokens,
                "savedPercent": pct(metrics[best].tokens - metrics[fair_comparison["target"]].tokens, metrics[best].tokens),
            }

    result = {
        "query": query,
        "status": trace.get("status"),
        "ttlUsageCount": trace.get("ttlUsageCount", 0),
        "codeReferenceCount": trace.get("codeReferenceCount", 0),
        "metrics": {name: value.__dict__ for name, value in metrics.items()},
        "savingsVsMamariCompactPlusSlices": savings,
        "coverageOfMamariEvidenceLines": {
            "grepExact": {
                "matched": len(mamari_keys & exact_keys),
                "total": len(mamari_keys),
                "percent": round((len(mamari_keys & exact_keys) / coverage_total) * 100, 2),
            },
            "grepLocal": {
                "matched": len(mamari_keys & local_keys),
                "total": len(mamari_keys),
                "percent": round((len(mamari_keys & local_keys) / coverage_total) * 100, 2),
            },
        },
        "grepHitFiles": {
            "exact": sorted(exact_files),
            "local": sorted(local_files),
        },
        "mamariEvidenceFiles": sorted({loc.get("file", "") for loc in all_locations if loc.get("file", "")}),
        "sourceSliceCount": slice_count,
        "sourceSliceContextLines": effective_context_lines,
        "sourceSliceMaxContextLines": context_lines,
        "sourceSliceAdaptiveContext": adaptive_context,
        "fairComparison": fair_comparison,
    }
    if gold_evaluation is not None:
        result["gold"] = {
            "exhaustive": term_gold["exhaustive"],
            "mustFindCount": len(term_gold["mustFind"]),
            "mustNotFindCount": len(term_gold["mustNotFind"]),
            "evaluation": gold_evaluation,
        }
    return result


def print_summary(result: dict[str, Any]) -> None:
    print(f"Encoding: {result['encoding']}")
    print(f"Repo: {result['repo']}")
    print(f"Index: {result['index']}")
    print()
    for term in result["terms"]:
        print(f"== {term['query']} ({term['status']}) ==")
        print(f"Evidence: {term['ttlUsageCount']} TTL usages, {term['codeReferenceCount']} code refs")
        adaptive_note = "adaptive" if term.get("sourceSliceAdaptiveContext") else "fixed"
        print(
            f"Source slices: {term['sourceSliceCount']} merged ranges, "
            f"{term['sourceSliceContextLines']} context lines ({adaptive_note}, max {term['sourceSliceMaxContextLines']})"
        )
        coverage = term["coverageOfMamariEvidenceLines"]
        print(
            "Coverage vs mamari lines: "
            f"exact grep {coverage['grepExact']['matched']}/{coverage['grepExact']['total']} "
            f"({coverage['grepExact']['percent']}%), "
            f"local grep {coverage['grepLocal']['matched']}/{coverage['grepLocal']['total']} "
            f"({coverage['grepLocal']['percent']}%)"
        )
        print("Tokens:")
        for name, data in term["metrics"].items():
            extra = ""
            if data["files"] or data["lines"]:
                extra = f" ({data['files']} files, {data['lines']} lines)"
            print(f"  {name}: {data['tokens']:,}{extra}")
        print("Savings using mamariCompactPlusSlices:")
        for name, data in term["savingsVsMamariCompactPlusSlices"].items():
            print(f"  vs {name}: {data['savedTokens']:,} tokens ({data['savedPercent']}%)")
        if "gold" in term:
            print("Gold:")
            for name in ["mamariGroupedCompactPlusSlices", "mamariCompactPlusSlices", "grepExactOutput", "grepLocalOutput"]:
                data = term["gold"]["evaluation"][name]
                precision = ""
                if "precision" in data:
                    precision = f", precision {data['precision']}%"
                print(
                    f"  {name}: recall {data['recall']}%{precision}, "
                    f"true hits {data['trueHits']}, returned lines {data['returnedLines']}, "
                    f"tokens/true hit {data['tokensPerTrueHit']}"
                )
        fair = term["fairComparison"]["cheapestBaselineAtMamariRecall"]
        if fair:
            precision = ""
            if fair.get("precision") is not None:
                precision = f", precision {fair['precision']}%"
            print(
                f"Cheapest baseline at Mamari recall ({term['fairComparison']['basis']}): "
                f"{fair['name']} with {fair['tokens']:,} tokens{precision}"
            )
        else:
            print(f"Cheapest baseline at Mamari recall ({term['fairComparison']['basis']}): none")
        print()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", required=True, type=Path)
    parser.add_argument("--index", type=Path, help="Defaults to <repo>/.mamari/index.json")
    parser.add_argument("--term", action="append", dest="terms", required=True, help="Term/IRI/local name to benchmark. Repeatable.")
    parser.add_argument("--encoding", default="o200k_base", help="tiktoken encoding name")
    parser.add_argument("--context-lines", type=int, default=8, help="Maximum source lines to fetch around each mamari location")
    parser.add_argument("--fixed-context-lines", action="store_true", help="Disable adaptive source-slice context sizing")
    parser.add_argument("--include", action="append", default=[], help="rg include glob. Repeatable.")
    parser.add_argument("--exclude", action="append", default=[], help="rg exclude glob. Repeatable.")
    parser.add_argument("--gold", type=Path, help="Optional gold fixture JSON with must_find/must_not_find per term")
    parser.add_argument("--json-out", type=Path, help="Optional path for detailed JSON results")
    args = parser.parse_args()

    repo = args.repo.expanduser().resolve()
    index_path = (args.index or repo / ".mamari" / "index.json").expanduser().resolve()
    if not repo.is_dir():
        raise SystemExit(f"Repo does not exist: {repo}")
    if not index_path.is_file():
        raise SystemExit(f"Index does not exist: {index_path}")

    encoding = tiktoken.get_encoding(args.encoding)
    index = json.loads(index_path.read_text(encoding="utf-8"))
    includes = args.include or DEFAULT_GLOBS
    excludes = args.exclude or DEFAULT_EXCLUDES
    gold = load_gold(args.gold.expanduser().resolve() if args.gold else None)
    result = {
        "repo": str(repo),
        "index": str(index_path),
        "encoding": args.encoding,
        "contextLines": args.context_lines,
        "adaptiveContextLines": not args.fixed_context_lines,
        "includes": includes,
        "excludes": excludes,
        "terms": [
            benchmark_term(encoding, index, repo, term, includes, excludes, args.context_lines, not args.fixed_context_lines, gold)
            for term in args.terms
        ],
    }
    print_summary(result)
    if args.json_out:
        args.json_out.parent.mkdir(parents=True, exist_ok=True)
        args.json_out.write_text(json.dumps(result, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
        print(f"Wrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
