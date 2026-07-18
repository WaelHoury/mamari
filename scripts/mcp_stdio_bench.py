#!/usr/bin/env python3
"""Measure a line-delimited stdio MCP server without an LLM in the loop.

The calls file is JSON Lines. Each non-empty line must contain a tool ``name``
and ``arguments`` object; an optional ``label`` is copied into the report.
The report separates wire size from the model-visible token estimate:

* initialize tokens count only server instructions;
* tools/list tokens count the advertised tool definitions across every page;
* tool-result tokens count text and structured content returned to the model.
"""

from __future__ import annotations

import argparse
import json
import os
import select
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path
from typing import Any

try:
    import tiktoken
except ImportError as exc:  # pragma: no cover - exercised by CLI users
    raise SystemExit("mcp_stdio_bench.py requires tiktoken") from exc


ENCODING = tiktoken.get_encoding("cl100k_base")


def compact(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def tokens(value: str) -> int:
    return len(ENCODING.encode(value))


def process_tree_rss_kib(root_pid: int) -> int:
    """Return resident KiB for root_pid and every current descendant."""
    try:
        out = subprocess.check_output(
            ["ps", "-axo", "pid=,ppid=,rss="],
            text=True,
            stderr=subprocess.DEVNULL,
        )
    except (OSError, subprocess.CalledProcessError):
        return 0
    rows: dict[int, tuple[int, int]] = {}
    for line in out.splitlines():
        fields = line.split()
        if len(fields) != 3:
            continue
        try:
            pid, ppid, rss = map(int, fields)
        except ValueError:
            continue
        rows[pid] = (ppid, rss)
    wanted = {root_pid}
    changed = True
    while changed:
        changed = False
        for pid, (ppid, _) in rows.items():
            if ppid in wanted and pid not in wanted:
                wanted.add(pid)
                changed = True
    return sum(rows.get(pid, (0, 0))[1] for pid in wanted)


class RSSSampler:
    def __init__(self, pid: int, interval: float = 0.05) -> None:
        self.pid = pid
        self.interval = interval
        self.samples: list[tuple[float, int]] = []
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, daemon=True)

    def start(self) -> None:
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()
        self._thread.join(timeout=2)

    def _run(self) -> None:
        started = time.monotonic()
        while not self._stop.is_set():
            self.samples.append(
                (time.monotonic() - started, process_tree_rss_kib(self.pid))
            )
            self._stop.wait(self.interval)

    @property
    def peak_kib(self) -> int:
        return max((rss for _, rss in self.samples), default=0)

    @property
    def last_kib(self) -> int:
        return self.samples[-1][1] if self.samples else 0


class MCPClient:
    def __init__(
        self,
        command: list[str],
        cwd: Path,
        root: Path,
        env: dict[str, str],
        timeout: float,
    ) -> None:
        merged_env = os.environ.copy()
        merged_env.update(env)
        self.root = root.resolve()
        self.timeout = timeout
        self.notifications: list[dict[str, Any]] = []
        self.stderr: list[str] = []
        self._next_id = 1
        self.proc = subprocess.Popen(
            command,
            cwd=cwd,
            env=merged_env,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
            start_new_session=True,
        )
        assert self.proc.stdin is not None
        assert self.proc.stdout is not None
        assert self.proc.stderr is not None
        self._stderr_thread = threading.Thread(target=self._drain_stderr, daemon=True)
        self._stderr_thread.start()
        self.rss = RSSSampler(self.proc.pid)
        self.rss.start()

    def _drain_stderr(self) -> None:
        assert self.proc.stderr is not None
        for line in self.proc.stderr:
            self.stderr.append(line.rstrip("\n"))

    def _send(self, message: dict[str, Any]) -> int:
        payload = compact(message)
        assert self.proc.stdin is not None
        self.proc.stdin.write(payload + "\n")
        self.proc.stdin.flush()
        return len(payload.encode("utf-8")) + 1

    def _handle_server_request(self, message: dict[str, Any]) -> None:
        method = message.get("method")
        if method == "roots/list":
            result: dict[str, Any] = {
                "roots": [{"uri": self.root.as_uri(), "name": self.root.name}]
            }
        elif method == "ping":
            result = {}
        else:
            self._send(
                {
                    "jsonrpc": "2.0",
                    "id": message.get("id"),
                    "error": {"code": -32601, "message": f"unsupported: {method}"},
                }
            )
            return
        self._send({"jsonrpc": "2.0", "id": message.get("id"), "result": result})

    def request(self, method: str, params: dict[str, Any]) -> dict[str, Any]:
        request_id = self._next_id
        self._next_id += 1
        request = {
            "jsonrpc": "2.0",
            "id": request_id,
            "method": method,
            "params": params,
        }
        sent_bytes = self._send(request)
        started = time.perf_counter()
        deadline = time.monotonic() + self.timeout
        assert self.proc.stdout is not None
        while time.monotonic() < deadline:
            if self.proc.poll() is not None:
                raise RuntimeError(
                    f"MCP server exited {self.proc.returncode}: "
                    + "\n".join(self.stderr[-20:])
                )
            ready, _, _ = select.select([self.proc.stdout], [], [], 0.2)
            if not ready:
                continue
            line = self.proc.stdout.readline()
            if not line:
                continue
            try:
                message = json.loads(line)
            except json.JSONDecodeError:
                self.notifications.append({"non_json_stdout": line.rstrip("\n")})
                continue
            if "id" in message and "method" in message:
                self._handle_server_request(message)
                continue
            if message.get("id") != request_id:
                self.notifications.append(message)
                continue
            elapsed_ms = (time.perf_counter() - started) * 1000
            raw = compact(message)
            return {
                "elapsed_ms": elapsed_ms,
                "request_wire_bytes": sent_bytes,
                "response_wire_bytes": len(raw.encode("utf-8")) + 1,
                "message": message,
            }
        raise TimeoutError(f"timed out after {self.timeout}s waiting for {method}")

    def notify(self, method: str, params: dict[str, Any]) -> None:
        self._send({"jsonrpc": "2.0", "method": method, "params": params})

    def close(self) -> None:
        try:
            if self.proc.stdin is not None:
                self.proc.stdin.close()
        except OSError:
            pass
        try:
            self.proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(self.proc.pid, signal.SIGTERM)
                self.proc.wait(timeout=3)
            except (ProcessLookupError, subprocess.TimeoutExpired):
                try:
                    os.killpg(self.proc.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                self.proc.wait(timeout=3)
        self.rss.stop()
        self._stderr_thread.join(timeout=1)


def model_result_text(result: dict[str, Any]) -> str:
    pieces: list[str] = []
    for item in result.get("content", []):
        if item.get("type") == "text":
            pieces.append(str(item.get("text", "")))
        else:
            pieces.append(compact(item))
    if "structuredContent" in result:
        pieces.append(compact(result["structuredContent"]))
    return "\n".join(pieces)


def load_calls(path: Path) -> list[dict[str, Any]]:
    calls: list[dict[str, Any]] = []
    with path.open(encoding="utf-8") as handle:
        for number, line in enumerate(handle, 1):
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            item = json.loads(line)
            if not isinstance(item.get("name"), str) or not isinstance(
                item.get("arguments"), dict
            ):
                raise ValueError(f"{path}:{number}: need name and arguments")
            calls.append(item)
    return calls


def parse_env(values: list[str]) -> dict[str, str]:
    result: dict[str, str] = {}
    for value in values:
        key, separator, item = value.partition("=")
        if not separator or not key:
            raise ValueError(f"invalid --env value: {value!r}")
        result[key] = item
    return result


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--command-json", required=True)
    parser.add_argument("--cwd", type=Path, required=True)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--calls", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--env", action="append", default=[])
    parser.add_argument("--timeout", type=float, default=120.0)
    parser.add_argument(
        "--settle-seconds",
        type=float,
        default=0.0,
        help="wait after the last call before recording steady-state RSS",
    )
    parser.add_argument(
        "--repeat",
        type=int,
        default=1,
        help="repeat the calls file N times in the same MCP session",
    )
    parser.add_argument("--protocol-version", default="2024-11-05")
    args = parser.parse_args()

    command = json.loads(args.command_json)
    if not isinstance(command, list) or not all(isinstance(x, str) for x in command):
        parser.error("--command-json must be a JSON array of strings")
    if args.repeat < 1:
        parser.error("--repeat must be at least 1")
    calls = load_calls(args.calls) * args.repeat
    client = MCPClient(command, args.cwd.resolve(), args.root, parse_env(args.env), args.timeout)
    report: dict[str, Any] = {
        "command": command,
        "cwd": str(args.cwd.resolve()),
        "root": str(args.root.resolve()),
        "calls_file": str(args.calls.resolve()),
        "repeat": args.repeat,
        "protocol_version": args.protocol_version,
        "calls": [],
    }
    session_started = time.perf_counter()
    try:
        initialized = client.request(
            "initialize",
            {
                "protocolVersion": args.protocol_version,
                "capabilities": {"roots": {"listChanged": False}},
                "clientInfo": {"name": "mcp-stdio-bench", "version": "1"},
                "rootUri": args.root.resolve().as_uri(),
            },
        )
        init_result = initialized["message"].get("result", {})
        instructions = str(init_result.get("instructions", ""))
        report["initialize"] = {
            "elapsed_ms": initialized["elapsed_ms"],
            "wire_bytes": initialized["response_wire_bytes"],
            "instruction_bytes": len(instructions.encode("utf-8")),
            "instruction_tokens": tokens(instructions),
            "server_info": init_result.get("serverInfo"),
        }
        client.notify("notifications/initialized", {})

        listed_tools: list[dict[str, Any]] = []
        tool_pages: list[dict[str, Any]] = []
        cursor: str | None = None
        while True:
            params = {"cursor": cursor} if cursor is not None else {}
            listed = client.request("tools/list", params)
            result = listed["message"].get("result", {})
            page_tools = result.get("tools", [])
            listed_tools.extend(page_tools)
            tool_pages.append(
                {
                    "elapsed_ms": listed["elapsed_ms"],
                    "wire_bytes": listed["response_wire_bytes"],
                    "tools": len(page_tools),
                    "next_cursor": result.get("nextCursor"),
                }
            )
            cursor = result.get("nextCursor")
            if cursor is None:
                break
        schema_text = compact(listed_tools)
        report["tools_list"] = {
            "tool_count": len(listed_tools),
            "pages": tool_pages,
            "model_bytes": len(schema_text.encode("utf-8")),
            "model_tokens": tokens(schema_text),
            "tool_names": [tool.get("name") for tool in listed_tools],
        }
        report["rss_after_startup_kib"] = client.rss.last_kib

        for index, call in enumerate(calls):
            measured = client.request(
                "tools/call",
                {"name": call["name"], "arguments": call["arguments"]},
            )
            message = measured["message"]
            result = message.get("result", {})
            visible = model_result_text(result)
            report["calls"].append(
                {
                    "index": index,
                    "label": call.get("label", str(index)),
                    "name": call["name"],
                    "arguments": call["arguments"],
                    "elapsed_ms": measured["elapsed_ms"],
                    "request_wire_bytes": measured["request_wire_bytes"],
                    "response_wire_bytes": measured["response_wire_bytes"],
                    "model_bytes": len(visible.encode("utf-8")),
                    "model_tokens": tokens(visible),
                    "rss_after_call_kib": client.rss.last_kib,
                    "is_error": bool(result.get("isError") or "error" in message),
                    "result": result,
                }
            )
        if args.settle_seconds > 0:
            time.sleep(args.settle_seconds)
        report["settle_seconds"] = args.settle_seconds
    finally:
        report["rss_before_shutdown_kib"] = client.rss.last_kib
        client.close()
        report["session_elapsed_ms"] = (time.perf_counter() - session_started) * 1000
        report["peak_rss_kib"] = client.rss.peak_kib
        # Sampling after the process exits commonly returns zero. Preserve the
        # last live sample as the session's final/steady RSS instead.
        report["final_rss_kib"] = report["rss_before_shutdown_kib"]
        report["notifications"] = client.notifications
        report["stderr_tail"] = client.stderr[-100:]

    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(compact({key: report[key] for key in (
        "initialize", "tools_list", "rss_after_startup_kib",
        "session_elapsed_ms", "peak_rss_kib", "final_rss_kib",
    )}))
    return 0


if __name__ == "__main__":
    sys.exit(main())
