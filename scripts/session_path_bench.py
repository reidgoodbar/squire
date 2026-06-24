#!/usr/bin/env python3
"""Benchmark the scoped `squire session` UX.

The measured commands are ordinary process invocations from inside one session.
The model-visible command stream would be `git`, `cat`, `sed`, etc.; Squire is
only the outer session launcher.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import shutil
import statistics
import subprocess
import sys
import tempfile
import textwrap
import time
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
SHIM_SOURCE = ROOT / "shims" / "squire_mmap_shim.c"
PRELOAD_SOURCE = ROOT / "shims" / "squire_preload.c"


def run(argv: list[str], cwd: Path, *, env: dict[str, str] | None = None, check: bool = True, timeout: float = 60) -> subprocess.CompletedProcess[bytes]:
    proc = subprocess.run(argv, cwd=str(cwd), env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    if check and proc.returncode != 0:
        raise RuntimeError(
            f"command failed ({proc.returncode}): {' '.join(argv)}\n"
            f"stdout={proc.stdout.decode(errors='replace')}\n"
            f"stderr={proc.stderr.decode(errors='replace')}"
        )
    return proc


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
    raise RuntimeError("usage: scripts/session_path_bench.py /path/to/squire")


def compile_c_shim(out: Path) -> None:
    cc = shutil.which("cc") or shutil.which("clang") or shutil.which("gcc")
    if not cc:
        raise RuntimeError("cc/clang/gcc is required for session benchmark")
    argv = [cc, "-O3", "-DNDEBUG", "-o", str(out), str(SHIM_SOURCE)]
    if os.uname().sysname != "Darwin":
        argv.append("-lcrypto")
    run(argv, ROOT)


def compile_preload(out: Path) -> None:
    cc = shutil.which("cc") or shutil.which("clang") or shutil.which("gcc")
    if not cc:
        raise RuntimeError("cc/clang/gcc is required for session benchmark")
    if os.uname().sysname == "Darwin":
        argv = [cc, "-O3", "-DNDEBUG", "-dynamiclib", "-o", str(out), str(PRELOAD_SOURCE)]
    else:
        argv = [cc, "-O3", "-DNDEBUG", "-shared", "-fPIC", "-o", str(out), str(PRELOAD_SOURCE), "-ldl", "-lcrypto"]
    run(argv, ROOT)


def percentile(values: list[float], q: float) -> float:
    if not values:
        return 0
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, round((len(ordered) - 1) * q)))
    return ordered[index]


def summarize(values: list[float]) -> dict[str, float]:
    return {
        "count": len(values),
        "avg_ms": round(statistics.mean(values), 4),
        "p50_ms": round(percentile(values, 0.50), 4),
        "p95_ms": round(percentile(values, 0.95), 4),
        "min_ms": round(min(values), 4),
        "max_ms": round(max(values), 4),
    }


def make_repo() -> Path:
    repo = Path(tempfile.mkdtemp(prefix="squire-session-bench.", dir=os.environ.get("TMPDIR") or "/private/tmp"))
    run(["git", "init", "-b", "main"], repo)
    run(["git", "config", "user.email", "squire@example.invalid"], repo)
    run(["git", "config", "user.name", "Squire Session Bench"], repo)
    (repo / "README.md").write_text("# session bench\n", encoding="utf-8")
    (repo / "src").mkdir()
    (repo / "src" / "app.py").write_text("print('hello')\nprint('world')\n", encoding="utf-8")
    run(["git", "add", "."], repo)
    run(["git", "commit", "-m", "init"], repo)
    (repo / "README.md").write_text("# session bench\n\nchanged\n", encoding="utf-8")
    return repo


WORKER = r'''
import json
import os
import shutil
import statistics
import subprocess
import sys
import time

repo = sys.argv[1]
rounds = int(sys.argv[2])
require_hits = sys.argv[3] == "1"

def pct(values, q):
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, round((len(ordered) - 1) * q)))
    return ordered[index]

def summary(values):
    return {
        "count": len(values),
        "avg_ms": round(statistics.mean(values), 4),
        "p50_ms": round(pct(values, 0.50), 4),
        "p95_ms": round(pct(values, 0.95), 4),
        "min_ms": round(min(values), 4),
        "max_ms": round(max(values), 4),
    }

def timed(argv, env=None):
    start = time.perf_counter_ns()
    proc = subprocess.run(argv, cwd=repo, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    return proc, (time.perf_counter_ns() - start) / 1_000_000

def real_tool(name):
    value = os.environ.get("SQUIRE_REAL_" + name.upper())
    if value:
        return value
    found = shutil.which(name)
    if found:
        return found
    raise RuntimeError("tool not found: " + name)

def command_env():
    env = os.environ.copy()
    if require_hits:
        env["SQUIRE_SHIM_REQUIRE_HIT"] = "1"
    return env

cases = [
    ("git_head", [real_tool("git"), "rev-parse", "HEAD"], ["git", "rev-parse", "HEAD"]),
    ("git_status_short", [real_tool("git"), "status", "--short"], ["git", "status", "--short"]),
    ("git_diff_stat", [real_tool("git"), "diff", "--stat"], ["git", "diff", "--stat"]),
    ("cat_readme", [real_tool("cat"), "README.md"], ["cat", "README.md"]),
    ("sed_readme", [real_tool("sed"), "-n", "1,1p", "README.md"], ["sed", "-n", "1,1p", "README.md"]),
]
if real_tool("python3"):
    cases.append(("python3_version", [real_tool("python3"), "--version"], ["python3", "--version"]))

report = {"cases": {}, "mismatches": 0}
for name, native_argv, shim_argv in cases:
    native_times = []
    shim_times = []
    for _ in range(rounds):
        native, native_ms = timed(native_argv)
        shim, shim_ms = timed(shim_argv, env=command_env())
        native_times.append(native_ms)
        shim_times.append(shim_ms)
        if (native.returncode, native.stdout, native.stderr) != (shim.returncode, shim.stdout, shim.stderr):
            report["mismatches"] += 1
    report["cases"][name] = {
        "native": summary(native_times),
        "session_shim": summary(shim_times),
        "delta_avg_ms": round(statistics.mean(native_times) - statistics.mean(shim_times), 4),
        "delta_p95_ms": round(pct(native_times, 0.95) - pct(shim_times, 0.95), 4),
    }
print(json.dumps(report, sort_keys=True))
'''


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("squire_bin", nargs="?")
    parser.add_argument("--rounds", type=int, default=50)
    parser.add_argument("--require-hit", action="store_true", help="force every measured shim command to replay or fail")
    parser.add_argument("--transport", choices=["auto", "preload", "path-shims"], default="auto")
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args()

    squire = resolve_squire(args.squire_bin)
    repo = make_repo()
    work = Path(tempfile.mkdtemp(prefix="squire-session-bench-work.", dir=os.environ.get("TMPDIR") or "/private/tmp"))
    shim = work / "squire-mmap-shim"
    preload = work / ("squire-preload.dylib" if os.uname().sysname == "Darwin" else "squire-preload.so")
    worker = work / "worker.py"
    compile_c_shim(shim)
    compile_preload(preload)
    worker.write_text(textwrap.dedent(WORKER), encoding="utf-8")

    try:
        session_argv = [
            str(squire),
            "session",
            "--quiet",
            "--shim",
            str(shim),
            "--preload-lib",
            str(preload),
        ]
        if args.transport == "preload":
            session_argv.append("--preload")
        if args.transport == "path-shims":
            session_argv.append("--path-shims")
        if args.require_hit:
            session_argv.append("--enable-warm-file-replay")
        session_argv.extend(
            [
                "--",
                sys.executable,
                str(worker),
                str(repo),
                str(args.rounds),
                "1" if args.require_hit else "0",
            ]
        )
        proc = run(
            session_argv,
            repo,
            check=False,
            timeout=120,
        )
        if proc.returncode != 0:
            raise RuntimeError(
                f"squire session benchmark failed ({proc.returncode})\n"
                f"stdout={proc.stdout.decode(errors='replace')}\n"
                f"stderr={proc.stderr.decode(errors='replace')}"
            )
    finally:
        run([str(squire), "kernel", "maintain", "--stop", "--short"], repo, check=False, timeout=10)
    report: dict[str, Any] = json.loads(proc.stdout.decode())
    report.update(
        {
            "session_path_bench": "pass" if report.get("mismatches") == 0 else "fail",
            "repo": str(repo),
            "rounds": args.rounds,
            "ux": {
                "mode": "scoped_c_mmap_session",
                "agent_visible_squire_command": False,
                "outer_launcher": "squire session -- <command>",
                "measured_commands_are_plain": True,
                "require_hit": args.require_hit,
                "transport": args.transport,
            },
        }
    )
    if args.json:
        print(json.dumps(report, indent=2, sort_keys=True))
    else:
        print(f"session_path_bench: {report['session_path_bench']}")
        for name, row in report["cases"].items():
            print(
                f"{name}: native_p95={row['native']['p95_ms']}ms "
                f"session_p95={row['session_shim']['p95_ms']}ms "
                f"delta_p95={row['delta_p95_ms']}ms"
            )
    return 0 if report["session_path_bench"] == "pass" else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"session_path_bench: fail: {exc}", file=sys.stderr)
        raise SystemExit(1)
