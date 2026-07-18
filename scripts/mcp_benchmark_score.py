#!/usr/bin/env python3
"""Score mcp_stdio_bench reports against explicit evidence assertions."""

from __future__ import annotations

import argparse
import json
import re
import statistics
from pathlib import Path
from typing import Any


def visible_text(result: dict[str, Any]) -> str:
    pieces: list[str] = []
    for item in result.get("content", []):
        if item.get("type") == "text":
            pieces.append(str(item.get("text", "")))
        else:
            pieces.append(json.dumps(item, ensure_ascii=False))
    if "structuredContent" in result:
        pieces.append(json.dumps(result["structuredContent"], ensure_ascii=False))
    return "\n".join(pieces)


def symbol_present(text: str, symbol: str) -> bool:
    return re.search(rf"(?<![\w$]){re.escape(symbol)}(?![\w$])", text) is not None


def file_present(text: str, file: str) -> bool:
    variants = {
        file,
        file.replace("/", "."),
        Path(file).name,
    }
    return any(variant in text for variant in variants)


def score_report(report: dict[str, Any], tasks: list[dict[str, Any]]) -> dict[str, Any]:
    calls_by_label: dict[str, dict[str, Any]] = {}
    for call in report.get("calls", []):
        calls_by_label.setdefault(str(call.get("label")), call)

    scored_tasks: list[dict[str, Any]] = []
    total_hits = 0
    total_assertions = 0
    total_tokens = 0
    for task in tasks:
        label = str(task["label"])
        call = calls_by_label.get(label)
        if call is None:
            scored_tasks.append({"label": label, "missing": True, "recall": 0.0})
            total_assertions += sum(
                len(task.get(key, []))
                for key in ("symbols", "files", "terms")
            )
            continue
        text = visible_text(call.get("result", {}))
        checks: list[dict[str, Any]] = []
        for symbol in task.get("symbols", []):
            checks.append(
                {"kind": "symbol", "expected": symbol, "hit": symbol_present(text, symbol)}
            )
        for file in task.get("files", []):
            checks.append(
                {"kind": "file", "expected": file, "hit": file_present(text, file)}
            )
        lower = text.lower()
        for term in task.get("terms", []):
            checks.append(
                {"kind": "term", "expected": term, "hit": term.lower() in lower}
            )
        hits = sum(1 for check in checks if check["hit"])
        assertions = len(checks)
        output_tokens = int(call.get("model_tokens", 0))
        scored_tasks.append(
            {
                "label": label,
                "hits": hits,
                "assertions": assertions,
                "recall": hits / assertions if assertions else 1.0,
                "elapsed_ms": call.get("elapsed_ms", 0),
                "output_tokens": output_tokens,
                "hits_per_1k_tokens": hits * 1000 / output_tokens if output_tokens else 0,
                "misses": [check for check in checks if not check["hit"]],
            }
        )
        total_hits += hits
        total_assertions += assertions
        total_tokens += output_tokens

    latencies = [float(task["elapsed_ms"]) for task in scored_tasks if "elapsed_ms" in task]
    fixed_tokens = int(report.get("initialize", {}).get("instruction_tokens", 0)) + int(
        report.get("tools_list", {}).get("model_tokens", 0)
    )
    return {
        "fixed_session_tokens": fixed_tokens,
        "hits": total_hits,
        "assertions": total_assertions,
        "recall": total_hits / total_assertions if total_assertions else 1.0,
        "output_tokens": total_tokens,
        "total_model_tokens": fixed_tokens + total_tokens,
        "hits_per_1k_output_tokens": total_hits * 1000 / total_tokens if total_tokens else 0,
        "median_latency_ms": statistics.median(latencies) if latencies else 0,
        "peak_rss_kib": report.get("peak_rss_kib", 0),
        "rss_before_shutdown_kib": report.get("rss_before_shutdown_kib", 0),
        "tasks": scored_tasks,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument(
        "--report",
        action="append",
        required=True,
        help="NAME=path-to-mcp-stdio-bench-report.json",
    )
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()

    manifest = json.loads(args.manifest.read_text(encoding="utf-8"))
    tasks = manifest["tasks"]
    output: dict[str, Any] = {"manifest": str(args.manifest), "tools": {}}
    for value in args.report:
        name, separator, path = value.partition("=")
        if not separator or not name or not path:
            parser.error(f"invalid --report: {value!r}")
        report = json.loads(Path(path).read_text(encoding="utf-8"))
        output["tools"][name] = score_report(report, tasks)

    rendered = json.dumps(output, indent=2) + "\n"
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(rendered, encoding="utf-8")
    print(rendered, end="")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
