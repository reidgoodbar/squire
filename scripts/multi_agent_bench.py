#!/usr/bin/env python3
"""Many-agent normal-UX A/B benchmark for Squire Kernel.

The measured Squire workload uses ordinary command argv sent through long-lived
terminal adapter sessions. The measured command stream does not contain
`squire kernel run -- ...`.
"""

from __future__ import annotations

import argparse
import base64
import concurrent.futures
import json
import math
import os
from pathlib import Path
import shutil
import statistics
import subprocess
import sys
import tempfile
import threading
import time
from typing import Any


class BenchFailure(Exception):
    pass


def run(
    argv: list[str],
    cwd: Path,
    *,
    check: bool = True,
    timeout: float = 30,
) -> tuple[subprocess.CompletedProcess[bytes], float]:
    start = time.perf_counter()
    proc = subprocess.run(argv, cwd=str(cwd), stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    wall_ms = (time.perf_counter() - start) * 1000
    if check and proc.returncode != 0:
        raise BenchFailure(
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
        raise BenchFailure(f"squire binary not found: {value}")
    found = shutil.which("squire")
    if found:
        return Path(found)
    raise BenchFailure("usage: scripts/multi_agent_bench.py /path/to/squire")


def percentile(values: list[float], p: float) -> float:
    if not values:
        return 0
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, math.ceil(p * len(ordered)) - 1))
    return ordered[index]


def make_repo() -> Path:
    repo = Path(tempfile.mkdtemp(prefix="squire-multi-agent-bench.", dir=os.environ.get("TMPDIR") or None))
    (repo / "src").mkdir()
    (repo / "tests").mkdir()
    (repo / "README.md").write_text(
        "# Multi-Agent Bench\n\n"
        "This repository is generated for Squire Kernel benchmarking.\n\n"
        + "\n".join(f"- item {i}" for i in range(120))
        + "\n",
        encoding="utf-8",
    )
    (repo / "package.json").write_text('{"name":"multi-agent-bench","private":true}\n', encoding="utf-8")
    (repo / "pyproject.toml").write_text("[project]\nname = \"multi-agent-bench\"\nversion = \"0.0.0\"\n", encoding="utf-8")
    for i in range(80):
        (repo / "src" / f"module_{i:03d}.py").write_text(
            f"def value_{i}():\n    return {i}\n",
            encoding="utf-8",
        )
    for argv in (
        ["git", "init", "-b", "main"],
        ["git", "config", "user.email", "squire@example.invalid"],
        ["git", "config", "user.name", "Squire Multi Agent Bench"],
        ["git", "add", "."],
        ["git", "commit", "-m", "init"],
    ):
        run(argv, repo)
    return repo


def available_tool_version_command() -> list[str]:
    for argv in (["python3", "--version"], ["node", "--version"], ["go", "version"]):
        if shutil.which(argv[0]):
            return argv
    return ["git", "--version"]


def workload() -> list[list[str]]:
    return [
        ["git", "rev-parse", "--show-toplevel"],
        ["git", "rev-parse", "HEAD"],
        ["git", "status", "--short"],
        ["git", "ls-files"],
        ["cat", "package.json"],
        ["sed", "-n", "1,80p", "README.md"],
        available_tool_version_command(),
        ["git", "add", "-h"],
    ]


class Adapter:
    def __init__(self, squire: Path, repo: Path, agent_id: int):
        self.repo = repo
        self.agent_id = agent_id
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
        self.lock = threading.Lock()

    def request(self, argv: list[str]) -> tuple[subprocess.CompletedProcess[bytes], dict[str, Any], float]:
        with self.lock:
            if self.proc.stdin is None or self.proc.stdout is None:
                raise BenchFailure("adapter pipes unavailable")
            self.seq += 1
            payload = {
                "id": f"{self.agent_id}:{self.seq}",
                "cwd": str(self.repo),
                "argv": argv,
                "session_id": f"multi-agent-{self.agent_id}",
            }
            start = time.perf_counter()
            self.proc.stdin.write(json.dumps(payload) + "\n")
            self.proc.stdin.flush()
            line = self.proc.stdout.readline()
            wall_ms = (time.perf_counter() - start) * 1000
        if not line:
            stderr = self.proc.stderr.read() if self.proc.stderr else ""
            raise BenchFailure(f"adapter closed early: {stderr}")
        resp = json.loads(line)
        stdout = base64.b64decode(resp.get("stdout_b64", ""))
        stderr = base64.b64decode(resp.get("stderr_b64", ""))
        proc = subprocess.CompletedProcess(argv, int(resp.get("exit_code", 0)), stdout, stderr)
        return proc, resp, wall_ms

    def close(self) -> None:
        if self.proc.stdin:
            self.proc.stdin.close()
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.kill()
            self.proc.wait(timeout=5)


def exact(label: str, got: subprocess.CompletedProcess[bytes], want: subprocess.CompletedProcess[bytes]) -> None:
    if got.returncode != want.returncode or got.stdout != want.stdout or got.stderr != want.stderr:
        raise BenchFailure(
            f"{label}: mismatch\n"
            f"got rc={got.returncode} stdout={got.stdout!r} stderr={got.stderr!r}\n"
            f"want rc={want.returncode} stdout={want.stdout!r} stderr={want.stderr!r}"
        )


def command_key(argv: list[str]) -> str:
    return " ".join(argv)


def expected_outputs(repo: Path, commands: list[list[str]]) -> dict[str, subprocess.CompletedProcess[bytes]]:
    return {command_key(argv): run(argv, repo, check=False)[0] for argv in commands}


def run_native_agents(
    repo: Path,
    commands: list[list[str]],
    expected: dict[str, subprocess.CompletedProcess[bytes]],
    agents: int,
    rounds: int,
) -> dict[str, Any]:
    latencies: list[float] = []
    per_command: dict[str, list[float]] = {command_key(argv): [] for argv in commands}
    mismatches = 0

    def one(agent_id: int) -> list[tuple[str, float, subprocess.CompletedProcess[bytes]]]:
        rows = []
        for _ in range(rounds):
            for argv in commands:
                proc, wall = run(argv, repo, check=False)
                exact(command_key(argv), proc, expected[command_key(argv)])
                rows.append((command_key(argv), wall, proc))
        return rows

    start = time.perf_counter()
    with concurrent.futures.ThreadPoolExecutor(max_workers=agents) as pool:
        for rows in pool.map(one, range(agents)):
            for key, wall, _ in rows:
                latencies.append(wall)
                per_command[key].append(wall)
    wall_ms = (time.perf_counter() - start) * 1000
    return {
        "wall_ms": wall_ms,
        "latencies": latencies,
        "per_command": per_command,
        "mismatches": mismatches,
    }


def run_squire_agents(
    repo: Path,
    squire: Path,
    commands: list[list[str]],
    expected: dict[str, subprocess.CompletedProcess[bytes]],
    agents: int,
    rounds: int,
) -> dict[str, Any]:
    adapters = [Adapter(squire, repo, i) for i in range(agents)]
    latencies: list[float] = []
    per_command: dict[str, list[float]] = {command_key(argv): [] for argv in commands}
    modes: dict[str, int] = {}
    mismatches = 0

    def one(adapter: Adapter) -> list[tuple[str, float, str, bool]]:
        rows = []
        for _ in range(rounds):
            for argv in commands:
                observed, resp, wall = adapter.request(argv)
                native = expected[command_key(argv)]
                ok = (
                    observed.returncode == native.returncode
                    and observed.stdout == native.stdout
                    and observed.stderr == native.stderr
                )
                rows.append((command_key(argv), wall, str(resp.get("mode", "")), ok))
        return rows

    try:
        start = time.perf_counter()
        with concurrent.futures.ThreadPoolExecutor(max_workers=agents) as pool:
            for rows in pool.map(one, adapters):
                for key, wall, mode, ok in rows:
                    latencies.append(wall)
                    per_command[key].append(wall)
                    modes[mode] = modes.get(mode, 0) + 1
                    if not ok:
                        mismatches += 1
        wall_ms = (time.perf_counter() - start) * 1000
    finally:
        for adapter in adapters:
            adapter.close()
    return {
        "wall_ms": wall_ms,
        "latencies": latencies,
        "per_command": per_command,
        "modes": modes,
        "mismatches": mismatches,
    }


def summarize_latencies(values: list[float]) -> dict[str, Any]:
    if not values:
        return {"count": 0, "avg_ms": 0, "p50_ms": 0, "p95_ms": 0, "max_ms": 0}
    return {
        "count": len(values),
        "avg_ms": round(statistics.mean(values), 3),
        "p50_ms": round(percentile(values, 0.50), 3),
        "p95_ms": round(percentile(values, 0.95), 3),
        "max_ms": round(max(values), 3),
    }


def summarize_per_command(values: dict[str, list[float]]) -> dict[str, Any]:
    return {key: summarize_latencies(rows) for key, rows in values.items()}


def bench_level(squire: Path, agents: int, rounds: int) -> dict[str, Any]:
    repo = make_repo()
    commands = workload()
    try:
        run([str(squire), "setup"], repo)
        run([str(squire), "kernel", "maintain", "--background", "--short"], repo)
        run([str(squire), "kernel", "warm", "--short"], repo, timeout=60)

        expected = expected_outputs(repo, commands)
        native = run_native_agents(repo, commands, expected, agents, rounds)
        squire_result = run_squire_agents(repo, squire, commands, expected, agents, rounds)
        run([str(squire), "kernel", "maintain", "--stop", "--short"], repo, check=False, timeout=10)

        native_wall = float(native["wall_ms"])
        squire_wall = float(squire_result["wall_ms"])
        speedup = native_wall / squire_wall if squire_wall > 0 else 0
        total_commands = agents * rounds * len(commands)
        return {
            "agents": agents,
            "rounds_per_agent": rounds,
            "total_commands": total_commands,
            "repo": str(repo),
            "ux": {
                "mode": "invisible_terminal_adapter",
                "agent_visible_squire_command": False,
                "hidden_backend": "squire kernel adapter --stdio",
                "measured_command_stream_contains_squire": False,
            },
            "native": {
                "wall_ms": round(native_wall, 3),
                "latency": summarize_latencies(native["latencies"]),
                "per_command": summarize_per_command(native["per_command"]),
                "mismatches": native["mismatches"],
            },
            "squire": {
                "wall_ms": round(squire_wall, 3),
                "latency": summarize_latencies(squire_result["latencies"]),
                "per_command": summarize_per_command(squire_result["per_command"]),
                "modes": squire_result["modes"],
                "mismatches": squire_result["mismatches"],
            },
            "speedup_ratio": round(speedup, 3),
            "wall_delta_ms": round(native_wall - squire_wall, 3),
            "no_broad_agent_speedup_claim": True,
        }
    finally:
        run([str(squire), "kernel", "maintain", "--stop", "--short"], repo, check=False, timeout=10)
        shutil.rmtree(repo, ignore_errors=True)


def parse_agent_counts(text: str) -> list[int]:
    values = []
    for part in text.split(","):
        part = part.strip()
        if not part:
            continue
        value = int(part)
        if value <= 0:
            raise argparse.ArgumentTypeError("agent counts must be positive")
        values.append(value)
    if not values:
        raise argparse.ArgumentTypeError("at least one agent count is required")
    return values


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("squire_bin", nargs="?")
    parser.add_argument("--agents", default="1,2,4,8", help="comma-separated agent counts")
    parser.add_argument("--rounds", type=int, default=10)
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args()

    try:
        squire = resolve_squire(args.squire_bin)
        agent_counts = parse_agent_counts(args.agents)
        results = [bench_level(squire, agents, args.rounds) for agents in agent_counts]
        report = {
            "multi_agent_bench": "pass",
            "agent_counts": agent_counts,
            "rounds": args.rounds,
            "workload": [" ".join(argv) for argv in workload()],
            "results": results,
        }
        if args.json:
            print(json.dumps(report, indent=2, sort_keys=True))
        else:
            for row in results:
                print(
                    "agents={agents} commands={total_commands} "
                    "native_wall_ms={native_wall} squire_wall_ms={squire_wall} "
                    "speedup={speedup} delta_ms={delta} modes={modes}".format(
                        agents=row["agents"],
                        total_commands=row["total_commands"],
                        native_wall=row["native"]["wall_ms"],
                        squire_wall=row["squire"]["wall_ms"],
                        speedup=row["speedup_ratio"],
                        delta=row["wall_delta_ms"],
                        modes=row["squire"]["modes"],
                    )
                )
            print("multi_agent_bench: pass")
        return 0
    except Exception as exc:
        if args.json:
            print(json.dumps({"multi_agent_bench": "fail", "error": str(exc)}, indent=2), file=sys.stderr)
        else:
            print(f"multi_agent_bench: fail: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
