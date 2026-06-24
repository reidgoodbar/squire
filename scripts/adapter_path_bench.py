#!/usr/bin/env python3
"""Exercise the three foreground adapter performance paths.

Cases:
- replay_hit: warmed replayable command returns from the mmap hot snapshot.
- invalid_or_miss: first post-mutation request falls back native; later exact
  rewarm replays are allowed.
- never_direct: never-replay command bypasses cache/maintainer/kernel work.
"""

from __future__ import annotations

import argparse
import base64
import json
import math
import os
from pathlib import Path
import shutil
import statistics
import subprocess
import sys
import tempfile
import time
from typing import Any


def run(argv: list[str], cwd: Path, *, check: bool = True, timeout: float = 30) -> tuple[subprocess.CompletedProcess[bytes], float]:
    start = time.perf_counter()
    proc = subprocess.run(argv, cwd=str(cwd), stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    wall_ms = (time.perf_counter() - start) * 1000
    if check and proc.returncode != 0:
        raise RuntimeError(
            f"command failed ({proc.returncode}): {' '.join(argv)}\n"
            f"stdout={proc.stdout.decode(errors='replace')}\n"
            f"stderr={proc.stderr.decode(errors='replace')}"
        )
    return proc, wall_ms


def resolve_squire(value: str | None) -> Path:
    if value:
        path = Path(value)
        if path.exists():
            return path.resolve()
        found = shutil.which(value)
        if found:
            return Path(found)
        raise RuntimeError(f"squire binary not found: {value}")
    found = shutil.which("squire")
    if found:
        return Path(found)
    raise RuntimeError("usage: scripts/adapter_path_bench.py /path/to/squire")


def percentile(values: list[float], p: float) -> float:
    if not values:
        return 0
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, math.ceil(p * len(ordered)) - 1))
    return ordered[index]


def make_repo() -> Path:
    repo = Path(tempfile.mkdtemp(prefix="squire-adapter-path-bench.", dir=os.environ.get("TMPDIR") or None))
    (repo / "README.md").write_text("# adapter path bench\n", encoding="utf-8")
    (repo / "tests").mkdir()
    (repo / "tests" / "test_help.py").write_text("def test_placeholder():\n    assert True\n", encoding="utf-8")
    for argv in (
        ["git", "init", "-b", "main"],
        ["git", "config", "user.email", "squire@example.invalid"],
        ["git", "config", "user.name", "Squire Adapter Bench"],
        ["git", "add", "."],
        ["git", "commit", "-m", "init"],
    ):
        run(argv, repo)
    return repo


class Adapter:
    def __init__(self, squire: Path, repo: Path):
        self.repo = repo
        self.proc = subprocess.Popen(
            [str(squire), "kernel", "adapter", "--stdio"],
            cwd=str(repo),
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
        )
        self.seq = 0

    def request(self, argv: list[str]) -> tuple[dict[str, Any], float]:
        assert self.proc.stdin is not None
        assert self.proc.stdout is not None
        self.seq += 1
        payload = {"id": str(self.seq), "cwd": str(self.repo), "argv": argv, "session_id": "adapter-path-bench"}
        start = time.perf_counter()
        self.proc.stdin.write(json.dumps(payload) + "\n")
        self.proc.stdin.flush()
        line = self.proc.stdout.readline()
        wall_ms = (time.perf_counter() - start) * 1000
        if not line:
            stderr = self.proc.stderr.read() if self.proc.stderr else ""
            raise RuntimeError(f"adapter closed early: {stderr}")
        return json.loads(line), wall_ms

    def close(self) -> None:
        if self.proc.stdin:
            self.proc.stdin.close()
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.kill()
            self.proc.wait(timeout=5)


def decoded(resp: dict[str, Any]) -> tuple[int, bytes, bytes]:
    return (
        int(resp.get("exit_code", 0)),
        base64.b64decode(resp.get("stdout_b64", "")),
        base64.b64decode(resp.get("stderr_b64", "")),
    )


def exact(label: str, resp: dict[str, Any], native: subprocess.CompletedProcess[bytes]) -> None:
    got = decoded(resp)
    want = (native.returncode, native.stdout, native.stderr)
    if got != want:
        raise AssertionError(f"{label}: adapter/native mismatch\nresp={resp}\nwant_rc={native.returncode}")


def summarize(name: str, values: list[float], native_values: list[float] | None = None, overhead_values: list[float] | None = None) -> dict[str, Any]:
    row: dict[str, Any] = {
        "count": len(values),
        "avg_ms": round(statistics.mean(values), 3),
        "p95_ms": round(percentile(values, 0.95), 3),
        "max_ms": round(max(values), 3),
    }
    if native_values:
        row["native_avg_ms"] = round(statistics.mean(native_values), 3)
        row["native_p95_ms"] = round(percentile(native_values, 0.95), 3)
    if overhead_values:
        row["overhead_avg_ms"] = round(statistics.mean(overhead_values), 3)
        row["overhead_p95_ms"] = round(percentile(overhead_values, 0.95), 3)
    return {name: row}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("squire_bin", nargs="?")
    parser.add_argument("--rounds", type=int, default=20)
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args()

    squire = resolve_squire(args.squire_bin)
    repo = make_repo()
    try:
        run([str(squire), "setup"], repo)
        run([str(squire), "kernel", "maintain", "--background", "--short"], repo)
        run([str(squire), "kernel", "warm", "--short"], repo, timeout=60)

        adapter = Adapter(squire, repo)
        try:
            hit_cmd = ["git", "status", "--short"]
            never_cmd = ["git", "add", "-h"]

            native_hit: list[float] = []
            native_invalid: list[float] = []
            native_never: list[float] = []
            replay_hit: list[float] = []
            invalid_or_miss: list[float] = []
            never_direct: list[float] = []
            replay_hit_overhead: list[float] = []
            invalid_or_miss_overhead: list[float] = []
            never_direct_overhead: list[float] = []
            modes: dict[str, dict[str, int]] = {"replay_hit": {}, "invalid_or_miss": {}, "never_direct": {}}

            for _ in range(args.rounds):
                native, ms = run(hit_cmd, repo)
                native_hit.append(ms)
                resp, wall = adapter.request(hit_cmd)
                replay_hit.append(wall)
                replay_hit_overhead.append(wall - ms)
                modes["replay_hit"][resp.get("mode", "")] = modes["replay_hit"].get(resp.get("mode", ""), 0) + 1
                exact("replay_hit", resp, native)
                if resp.get("mode") != "replay":
                    raise AssertionError(f"replay_hit did not replay: {resp}")

            (repo / "README.md").write_text("# adapter path bench\n\nmutated\n", encoding="utf-8")
            invalid_first_mode = ""
            for i in range(args.rounds):
                native, ms = run(hit_cmd, repo)
                native_invalid.append(ms)
                resp, wall = adapter.request(hit_cmd)
                invalid_or_miss.append(wall)
                invalid_or_miss_overhead.append(wall - ms)
                modes["invalid_or_miss"][resp.get("mode", "")] = modes["invalid_or_miss"].get(resp.get("mode", ""), 0) + 1
                exact("invalid_or_miss", resp, native)
                if i == 0:
                    invalid_first_mode = str(resp.get("mode", ""))
                    if resp.get("mode") == "replay":
                        raise AssertionError(f"first invalid_or_miss request replayed stale status: {resp}")

            for _ in range(args.rounds):
                native, ms = run(never_cmd, repo, check=False)
                native_never.append(ms)
                resp, wall = adapter.request(never_cmd)
                never_direct.append(wall)
                never_direct_overhead.append(wall - ms)
                modes["never_direct"][resp.get("mode", "")] = modes["never_direct"].get(resp.get("mode", ""), 0) + 1
                exact("never_direct", resp, native)
                if resp.get("mode") != "never":
                    raise AssertionError(f"never_direct mode = {resp.get('mode')}, want never")
        finally:
            adapter.close()
            run([str(squire), "kernel", "maintain", "--stop", "--short"], repo, check=False, timeout=10)

        report: dict[str, Any] = {
            "repo": str(repo),
            "rounds": args.rounds,
            "ux": {
                "mode": "invisible_terminal_adapter",
                "agent_visible_squire_command": False,
                "hidden_backend": "squire kernel adapter --stdio",
                "visible_commands": [" ".join(hit_cmd), " ".join(never_cmd)],
                "measured_command_stream_contains_squire": False,
            },
            "modes": modes,
            "invalid_first_mode": invalid_first_mode,
            "budgets": {
                "replay_hit_p95_ms": 1,
                "invalid_or_miss_overhead_p95_ms": 1,
                "never_direct_overhead_p95_ms": 1,
            },
        }
        report.update(summarize("replay_hit", replay_hit, native_hit, replay_hit_overhead))
        report.update(summarize("invalid_or_miss", invalid_or_miss, native_invalid, invalid_or_miss_overhead))
        report.update(summarize("never_direct", never_direct, native_never, never_direct_overhead))
        violations = []
        if report["replay_hit"]["p95_ms"] > report["budgets"]["replay_hit_p95_ms"]:
            violations.append("replay_hit p95 over budget")
        if report["invalid_or_miss"]["overhead_p95_ms"] > report["budgets"]["invalid_or_miss_overhead_p95_ms"]:
            violations.append("invalid_or_miss overhead p95 over budget")
        if report["never_direct"]["overhead_p95_ms"] > report["budgets"]["never_direct_overhead_p95_ms"]:
            violations.append("never_direct overhead p95 over budget")
        report["violations"] = violations
        if args.json:
            print(json.dumps(report, indent=2, sort_keys=True))
        else:
            print("adapter_path_bench: " + ("pass" if not violations else "needs_optimization"))
            for key in ("replay_hit", "invalid_or_miss", "never_direct"):
                row = report[key]
                print(f"{key}: p95_ms={row['p95_ms']} overhead_p95_ms={row.get('overhead_p95_ms', 0)} modes={modes[key]}")
            for violation in violations:
                print(f"performance_violation: {violation}")
        return 1 if violations else 0
    except Exception as exc:
        print(f"adapter_path_bench: fail: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
