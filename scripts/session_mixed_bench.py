#!/usr/bin/env python3
"""Benchmark scoped preload UX across a mixed local command workload."""

from __future__ import annotations

import argparse
import json
import os
import shlex
import shutil
import statistics
import subprocess
import tempfile
import textwrap
from collections import Counter, defaultdict
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
PRELOAD_SOURCE = ROOT / "shims" / "squire_preload.c"
HELPER_SOURCE = ROOT / "shims" / "squire_preload_helper.c"


DRIVER_PREAMBLE = r"""#!/usr/bin/env zsh
set +e
zmodload zsh/datetime 2>/dev/null || true
work="${TMPDIR:-/tmp}/squire-mixed-driver.$$"
mkdir -p "$work" || exit 97
cleanup() {
  rm -rf "$work"
}
trap cleanup EXIT

now_us() {
  printf '%.0f' $(( EPOCHREALTIME * 1000000 ))
}

hash_file() {
  shasum -a 256 "$1" | awk '{print $1}'
}

size_file() {
  wc -c < "$1" | tr -d ' '
}

run_one() {
  local idx="$1"
  local bucket="$2"
  local command="$3"
  local out="$work/out.$idx"
  local err="$work/err.$idx"
  local start end code out_hash err_hash out_size err_size elapsed
  start=$(now_us)
  eval "$command" >"$out" 2>"$err"
  code=$?
  end=$(now_us)
  elapsed=$(( end - start ))
  out_hash=$(hash_file "$out")
  err_hash=$(hash_file "$err")
  out_size=$(size_file "$out")
  err_size=$(size_file "$err")
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$idx" "$bucket" "$code" "$out_hash" "$err_hash" "$out_size" "$err_size" "$elapsed"
}
"""


def run(argv: list[str], cwd: Path, *, env: dict[str, str] | None = None, check: bool = True, timeout: int = 120) -> subprocess.CompletedProcess[bytes]:
    proc = subprocess.run(argv, cwd=str(cwd), env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    if check and proc.returncode != 0:
        raise RuntimeError(
            f"command failed ({proc.returncode}): {' '.join(argv)}\n"
            f"stdout={proc.stdout.decode(errors='replace')}\n"
            f"stderr={proc.stderr.decode(errors='replace')}"
        )
    return proc


def resolve_squire(explicit: str | None) -> Path:
    if explicit:
        path = Path(explicit)
        if path.exists():
            return path.resolve()
        found = shutil.which(explicit)
        if found:
            return Path(found).resolve()
    found = shutil.which("squire")
    if found:
        return Path(found).resolve()
    raise RuntimeError("usage: scripts/session_mixed_bench.py /path/to/squire")


def cc() -> str:
    compiler = shutil.which("cc") or shutil.which("clang") or shutil.which("gcc")
    if not compiler:
        raise RuntimeError("cc/clang/gcc is required for mixed session benchmark")
    return compiler


def compile_preload(out: Path) -> None:
    compiler = cc()
    if os.uname().sysname == "Darwin":
        cmd = [compiler, "-O3", "-DNDEBUG", "-dynamiclib", "-o", str(out), str(PRELOAD_SOURCE)]
    else:
        cmd = [compiler, "-O3", "-DNDEBUG", "-shared", "-fPIC", "-o", str(out), str(PRELOAD_SOURCE), "-ldl", "-lcrypto"]
    run(cmd, ROOT, timeout=60)


def compile_helper(out: Path) -> None:
    compiler = cc()
    if os.uname().sysname == "Darwin":
        cmd = [compiler, "-O3", "-DNDEBUG", "-o", str(out), str(HELPER_SOURCE)]
    else:
        cmd = [compiler, "-O3", "-DNDEBUG", "-o", str(out), str(HELPER_SOURCE), "-lcrypto"]
    run(cmd, ROOT, timeout=60)


def select_session_shell() -> Path:
    if os.uname().sysname == "Darwin":
        homebrew_zsh = Path("/opt/homebrew/bin/zsh")
        if homebrew_zsh.exists():
            return homebrew_zsh
    found = shutil.which("zsh") or shutil.which("bash") or shutil.which("sh")
    if found:
        return Path(found).resolve()
    raise RuntimeError("zsh, bash, or sh is required for mixed session benchmark")


def make_repo() -> Path:
    base = os.environ.get("TMPDIR") or "/private/tmp"
    repo = Path(tempfile.mkdtemp(prefix="squire-mixed-bench.", dir=base))
    for path in ["src", "py", "tests", "docs"]:
        (repo / path).mkdir(parents=True, exist_ok=True)
    (repo / ".gitignore").write_text("node_modules/\n__pycache__/\n*.pyc\n.tmp/\n", encoding="utf-8")
    (repo / "package.json").write_text(
        json.dumps(
            {
                "name": "squire-mixed-bench",
                "type": "module",
                "scripts": {"test": "node --test"},
                "dependencies": {"@types/node": "^20.0.0"},
            },
            indent=2,
        )
        + "\n",
        encoding="utf-8",
    )
    (repo / "tsconfig.json").write_text(
        json.dumps(
            {
                "compilerOptions": {
                    "target": "ES2022",
                    "module": "NodeNext",
                    "moduleResolution": "NodeNext",
                    "strict": True,
                },
                "include": ["src/**/*.ts"],
            },
            indent=2,
        )
        + "\n",
        encoding="utf-8",
    )
    (repo / "src/app.ts").write_text(
        textwrap.dedent(
            """\
            import { normalizeName } from "./util";

            export interface Semaphore {
              id: string;
              permits: number;
              holders: string[];
            }

            export function acquire(sem: Semaphore, actor: string): Semaphore {
              if (sem.holders.includes(actor) || sem.holders.length >= sem.permits) {
                return sem;
              }
              return { ...sem, holders: [...sem.holders, normalizeName(actor)] };
            }
            """
        ),
        encoding="utf-8",
    )
    (repo / "src/util.ts").write_text(
        "export function normalizeName(value: string): string {\n  return value.trim().toLowerCase();\n}\n",
        encoding="utf-8",
    )
    (repo / "pyproject.toml").write_text(
        "[project]\nname = \"squire-mixed-bench\"\nversion = \"0.1.0\"\nrequires-python = \">=3.11\"\n",
        encoding="utf-8",
    )
    (repo / "py/service.py").write_text(
        textwrap.dedent(
            """\
            from dataclasses import dataclass


            @dataclass
            class Permit:
                owner: str
                expires_at: int


            def active(permit: Permit, now: int) -> bool:
                return permit.expires_at > now
            """
        ),
        encoding="utf-8",
    )
    (repo / "README.md").write_text(
        "# Squire Mixed Benchmark\n\nThis fixture resembles a small Python and TypeScript service.\n",
        encoding="utf-8",
    )
    run(["git", "init", "-b", "main"], repo)
    run(["git", "config", "user.email", "bench@example.com"], repo)
    run(["git", "config", "user.name", "Bench"], repo)
    run(["git", "add", "."], repo)
    run(["git", "commit", "-m", "init"], repo)
    with (repo / "src/app.ts").open("a", encoding="utf-8") as f:
        f.write("\nexport const BENCH_MARKER = \"dirty\";\n")
    (repo / "docs/notes.md").write_text("untracked local note\n", encoding="utf-8")
    return repo


def add_repeated(commands: list[dict[str, str]], bucket: str, command: str, repeats: int) -> None:
    for _ in range(repeats):
        commands.append({"bucket": bucket, "command": command})


def build_plan() -> dict[str, Any]:
    commands: list[dict[str, str]] = []
    metadata = [
        "git rev-parse HEAD",
        "git rev-parse --git-dir",
        "git rev-parse --abbrev-ref HEAD",
        "git rev-parse --show-toplevel",
        "git rev-parse --is-inside-work-tree",
    ]
    repo_summary = [
        "git status --short",
        "git status --porcelain",
        "git ls-files",
        "git diff --stat",
        "git diff -- src/app.ts",
    ]
    file_reads = [
        "cat package.json",
        "cat pyproject.toml",
        "sed -n 1,40p src/app.ts",
        "sed -n 1,40p README.md",
    ]
    tool_probes = [
        "git --version",
        "python3 --version",
        "which python3",
        "command -v python3",
    ]
    if shutil.which("node"):
        tool_probes.extend(["node --version", "which node"])
    native_controls = [
        "python3 -m py_compile py/service.py",
        "git diff --check",
    ]
    for command in metadata:
        add_repeated(commands, "git_metadata", command, 24)
    for command in repo_summary:
        add_repeated(commands, "repo_summary", command, 12)
    for command in file_reads:
        add_repeated(commands, "file_read", command, 8)
    for command in tool_probes:
        add_repeated(commands, "tool_probe", command, 8)
    for command in native_controls:
        add_repeated(commands, "native_control", command, 5)
    return {"commands": commands}


def stats(values: list[float]) -> dict[str, float | int]:
    ordered = sorted(values)
    if not ordered:
        return {"count": 0}
    def idx(p: float) -> int:
        return min(len(ordered) - 1, max(0, int(len(ordered) * p + 0.999999) - 1))
    return {
        "count": len(ordered),
        "total_ms": round(sum(ordered), 3),
        "avg_ms": round(statistics.mean(ordered), 3),
        "p50_ms": round(ordered[idx(0.50)], 3),
        "p95_ms": round(ordered[idx(0.95)], 3),
        "p99_ms": round(ordered[idx(0.99)], 3),
        "max_ms": round(ordered[-1], 3),
    }


def stats_us(values: list[int]) -> dict[str, float | int]:
    ordered = sorted(values)
    if not ordered:
        return {"count": 0}
    def idx(p: float) -> int:
        return min(len(ordered) - 1, max(0, int(len(ordered) * p + 0.999999) - 1))
    return {
        "count": len(ordered),
        "total_us": sum(ordered),
        "avg_us": round(statistics.mean(ordered), 1),
        "p50_us": ordered[idx(0.50)],
        "p95_us": ordered[idx(0.95)],
        "p99_us": ordered[idx(0.99)],
        "p999_us": ordered[idx(0.999)],
        "max_us": ordered[-1],
    }


def hot_event_counts(repo: Path) -> tuple[Counter[str], list[int]]:
    log = repo / ".git" / "squire" / "kernel" / "hot_client_events.log"
    counts: Counter[str] = Counter()
    replay_us: list[int] = []
    if not log.exists():
        return counts, replay_us
    for line in log.read_text(encoding="utf-8", errors="replace").splitlines():
        parts = line.split()
        if len(parts) < 5 or parts[1] != "replay":
            continue
        counts[parts[2]] += 1
        try:
            replay_us.append(int(parts[4]))
        except ValueError:
            pass
    return counts, replay_us


def compare(native: list[dict[str, Any]], squire: list[dict[str, Any]]) -> tuple[int, list[dict[str, Any]]]:
    mismatches: list[dict[str, Any]] = []
    for i, (left, right) in enumerate(zip(native, squire)):
        for key in ["bucket", "command", "exit_code", "stdout_sha256", "stderr_sha256", "stdout_size", "stderr_size"]:
            if left[key] != right[key]:
                mismatches.append({
                    "index": i,
                    "bucket": left["bucket"],
                    "command": left["command"],
                    "field": key,
                    "native": left[key],
                    "squire": right[key],
                })
                break
    if len(native) != len(squire):
        mismatches.append({"field": "record_count", "native": len(native), "squire": len(squire)})
    return len(mismatches), mismatches[:10]


def bucket_report(records: list[dict[str, Any]]) -> dict[str, dict[str, float | int]]:
    by_bucket: dict[str, list[float]] = defaultdict(list)
    for record in records:
        by_bucket[record["bucket"]].append(record["elapsed_us"] / 1000.0)
    return {bucket: stats(values) for bucket, values in sorted(by_bucket.items())}


def write_shell_driver(driver: Path, plan: dict[str, Any]) -> None:
    lines = [DRIVER_PREAMBLE]
    for idx, item in enumerate(plan["commands"]):
        lines.append(
            "run_one "
            + shlex.quote(str(idx))
            + " "
            + shlex.quote(item["bucket"])
            + " "
            + shlex.quote(item["command"])
        )
    driver.write_text("\n".join(lines) + "\n", encoding="utf-8")
    driver.chmod(0o755)


def parse_shell_records(stdout: bytes, plan: dict[str, Any]) -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []
    for raw in stdout.decode("utf-8", errors="replace").splitlines():
        parts = raw.split("\t")
        if len(parts) != 8:
            continue
        idx = int(parts[0])
        item = plan["commands"][idx]
        records.append({
            "bucket": item["bucket"],
            "command": item["command"],
            "exit_code": int(parts[2]),
            "stdout_sha256": parts[3],
            "stderr_sha256": parts[4],
            "stdout_size": int(parts[5]),
            "stderr_size": int(parts[6]),
            "elapsed_us": int(parts[7]),
        })
    if len(records) != len(plan["commands"]):
        raise RuntimeError(f"driver emitted {len(records)} records, expected {len(plan['commands'])}")
    return records


def run_driver(shell: Path, driver: Path, plan: dict[str, Any], cwd: Path, *, env: dict[str, str] | None = None) -> list[dict[str, Any]]:
    proc = run([str(shell), str(driver)], cwd, env=env, timeout=240)
    return parse_shell_records(proc.stdout, plan)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("squire_bin", nargs="?")
    parser.add_argument("--json", action="store_true")
    parser.add_argument("--enable-warm-file-replay", action="store_true")
    args = parser.parse_args()

    squire = resolve_squire(args.squire_bin)
    repo = make_repo()
    work = Path(tempfile.mkdtemp(prefix="squire-mixed-bench-work.", dir=os.environ.get("TMPDIR") or "/private/tmp"))
    preload = work / ("squire-preload.dylib" if os.uname().sysname == "Darwin" else "squire-preload.so")
    helper = work / "squire-preload-helper"
    shell = select_session_shell()
    driver = work / "driver.zsh"
    compile_preload(preload)
    compile_helper(helper)
    plan = build_plan()
    write_shell_driver(driver, plan)

    native_records = run_driver(shell, driver, plan, repo)

    run([str(squire), "setup"], repo)
    run([str(squire), "kernel", "warm", "--short"], repo)
    before_counts, before_us = hot_event_counts(repo)
    session_args = [
        str(squire),
        "session",
        "--quiet",
        "--preload",
        "--preload-lib",
        str(preload),
        "--no-warm",
        "--no-maintainer",
    ]
    if args.enable_warm_file_replay:
        session_args.append("--enable-warm-file-replay")
    session_args.extend(["--", str(shell), str(driver)])
    proc = run(session_args, repo, timeout=240)
    squire_records = parse_shell_records(proc.stdout, plan)
    after_counts, after_us = hot_event_counts(repo)
    event_counts = after_counts - before_counts
    replay_us = after_us[len(before_us):]

    mismatch_count, mismatch_examples = compare(native_records, squire_records)
    native_total = sum(record["elapsed_us"] for record in native_records) / 1000.0
    squire_total = sum(record["elapsed_us"] for record in squire_records) / 1000.0
    report: dict[str, Any] = {
        "session_mixed_bench": "pass" if mismatch_count == 0 and squire_total < native_total else "fail",
        "repo": str(repo),
        "commands": len(plan["commands"]),
        "exactness": mismatch_count == 0,
        "mismatches": mismatch_count,
        "mismatch_examples": mismatch_examples,
        "native_total_ms": round(native_total, 3),
        "squire_total_ms": round(squire_total, 3),
        "delta_ms": round(native_total - squire_total, 3),
        "speedup": round(native_total / squire_total, 3) if squire_total > 0 else None,
        "native_by_bucket": bucket_report(native_records),
        "squire_by_bucket": bucket_report(squire_records),
        "hot_client_event_counts": dict(sorted(event_counts.items())),
        "hot_client_replay_us": stats_us(replay_us),
        "ux": {
            "mode": "scoped_preload_session",
            "agent_visible_squire_command": False,
            "measured_commands_are_plain": True,
            "transport": "preload",
            "session_shell": str(shell),
            "warm_file_replay_enabled": args.enable_warm_file_replay,
        },
    }
    if args.json:
        print(json.dumps(report, indent=2, sort_keys=True))
    else:
        print(f"session_mixed_bench: {report['session_mixed_bench']}")
        print(f"commands: {report['commands']}")
        print(f"exactness: {report['exactness']}")
        print(f"native_total_ms: {report['native_total_ms']}")
        print(f"squire_total_ms: {report['squire_total_ms']}")
        print(f"delta_ms: {report['delta_ms']}")
        print(f"speedup: {report['speedup']}")
        print(f"hot_client_event_counts: {report['hot_client_event_counts']}")
    return 0 if report["session_mixed_bench"] == "pass" else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"session_mixed_bench: fail: {exc}", file=os.sys.stderr)
        raise SystemExit(1)
