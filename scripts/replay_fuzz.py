#!/usr/bin/env python3
"""Fuzz native/Squire equivalence for read-only command replay surfaces.

The fuzzer generates a deterministic random command plan over the current
proof-gated read-only operator set plus safe native-fallback controls. It then
runs the exact same plan natively and through `squire session --preload`,
recording stdout/stderr/exit-code hashes for every command.

This is intentionally not a semantic test. Any byte difference is a failure.
"""

from __future__ import annotations

import argparse
from collections import Counter, defaultdict
import json
import os
from pathlib import Path
import random
import shlex
import shutil
import statistics
import subprocess
import tempfile
import textwrap
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
PRELOAD_SOURCE = ROOT / "shims" / "squire_preload.c"
HELPER_SOURCE = ROOT / "shims" / "squire_preload_helper.c"


DRIVER_PREAMBLE = r"""#!/usr/bin/env zsh
set +e
zmodload zsh/datetime 2>/dev/null || true
work="${TMPDIR:-/tmp}/squire-replay-fuzz.$$"
mkdir -p "$work" || exit 97
cleanup() {
  rm -rf "$work"
}
trap cleanup EXIT

now_us() {
  printf '%.0f' $(( EPOCHREALTIME * 1000000 ))
}

hash_file() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
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
  local start end code elapsed
  start=$(now_us)
  eval "$command" >"$out" 2>"$err"
  code=$?
  end=$(now_us)
  elapsed=$(( end - start ))
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$idx" "$bucket" "$code" "$(hash_file "$out")" "$(hash_file "$err")" "$(size_file "$out")" "$(size_file "$err")" "$elapsed"
}
"""


def run(
    argv: list[str],
    cwd: Path,
    *,
    env: dict[str, str] | None = None,
    check: bool = True,
    timeout: int = 240,
) -> subprocess.CompletedProcess[bytes]:
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
        raise RuntimeError(f"squire binary not found: {explicit}")
    found = shutil.which("squire")
    if found:
        return Path(found).resolve()
    raise RuntimeError("usage: scripts/replay_fuzz.py /path/to/squire")


def compiler() -> str:
    cc = shutil.which("cc") or shutil.which("clang") or shutil.which("gcc")
    if not cc:
        raise RuntimeError("cc/clang/gcc is required")
    return cc


def compile_preload(out: Path) -> None:
    cc = compiler()
    if os.uname().sysname == "Darwin":
        argv = [cc, "-O3", "-DNDEBUG", "-dynamiclib", "-o", str(out), str(PRELOAD_SOURCE)]
    else:
        argv = [cc, "-O3", "-DNDEBUG", "-shared", "-fPIC", "-o", str(out), str(PRELOAD_SOURCE), "-ldl", "-lcrypto"]
    run(argv, ROOT, timeout=60)


def compile_helper(out: Path) -> None:
    cc = compiler()
    if os.uname().sysname == "Darwin":
        argv = [cc, "-O3", "-DNDEBUG", "-o", str(out), str(HELPER_SOURCE)]
    else:
        argv = [cc, "-O3", "-DNDEBUG", "-o", str(out), str(HELPER_SOURCE), "-lcrypto"]
    run(argv, ROOT, timeout=60)


def select_shell() -> Path:
    if os.uname().sysname == "Darwin":
        homebrew_zsh = Path("/opt/homebrew/bin/zsh")
        if homebrew_zsh.exists():
            return homebrew_zsh
    found = shutil.which("zsh") or shutil.which("bash") or shutil.which("sh")
    if found:
        return Path(found).resolve()
    raise RuntimeError("zsh, bash, or sh is required")


def make_repo(rng: random.Random) -> Path:
    base = os.environ.get("TMPDIR") or "/private/tmp"
    repo = Path(tempfile.mkdtemp(prefix="squire-replay-fuzz.", dir=base))
    for rel in ("src", "lib", "docs", "tests", "nested/deep"):
        (repo / rel).mkdir(parents=True, exist_ok=True)
    (repo / ".gitignore").write_text("node_modules/\n__pycache__/\n*.pyc\n.tmp/\n", encoding="utf-8")
    (repo / "README.md").write_text(
        "# Replay Fuzz\n\n"
        + "\n".join(f"- README line {i} token_{i % 9}" for i in range(1, 220))
        + "\n",
        encoding="utf-8",
    )
    (repo / "package.json").write_text(
        json.dumps({"name": "squire-replay-fuzz", "type": "module", "private": True}, indent=2) + "\n",
        encoding="utf-8",
    )
    (repo / "pyproject.toml").write_text(
        "[project]\nname = \"squire-replay-fuzz\"\nversion = \"0.0.0\"\n",
        encoding="utf-8",
    )
    for i in range(12):
        lines = [
            f"export const value_{i}_{j} = {i * 100 + j}; // token_{(i + j) % 13}"
            for j in range(1, 160)
        ]
        (repo / "src" / f"module_{i:02d}.js").write_text("\n".join(lines) + "\n", encoding="utf-8")
    for i in range(5):
        lines = [f"def value_{i}_{j}():\n    return {i * 100 + j}\n" for j in range(1, 80)]
        (repo / "lib" / f"mod_{i:02d}.py").write_text("\n".join(lines), encoding="utf-8")
    (repo / "docs" / "notes.md").write_text(
        "\n".join(f"note {i} marker_{rng.randrange(17)}" for i in range(1, 180)) + "\n",
        encoding="utf-8",
    )
    (repo / "nested" / "deep" / "config.json").write_text(
        json.dumps({"alpha": True, "items": list(range(20)), "marker": "deep_config"}, indent=2) + "\n",
        encoding="utf-8",
    )
    for argv in (
        ["git", "init", "-b", "main"],
        ["git", "config", "user.email", "fuzz@example.com"],
        ["git", "config", "user.name", "Squire Replay Fuzz"],
        ["git", "add", "."],
        ["git", "commit", "-m", "init"],
    ):
        run(argv, repo)
    # Add deterministic dirty state to exercise status and diff replay.
    with (repo / "src" / "module_00.js").open("a", encoding="utf-8") as f:
        f.write("export const dirty_marker = 1;\n")
    (repo / "docs" / "scratch.md").write_text("untracked scratch\n", encoding="utf-8")
    return repo


def sq(command: str) -> str:
    return shlex.quote(command)


def command_paths() -> list[str]:
    return [
        "README.md",
        "package.json",
        "pyproject.toml",
        "src/module_00.js",
        "src/module_01.js",
        "src/module_07.js",
        "lib/mod_00.py",
        "lib/mod_03.py",
        "docs/notes.md",
        "nested/deep/config.json",
    ]


def generate_cases(rng: random.Random, count: int) -> list[dict[str, str]]:
    paths = command_paths()
    tools = ["python3", "git"]
    if shutil.which("node"):
        tools.append("node")
    if shutil.which("go"):
        tools.append("go")
    if shutil.which("rg"):
        tools.append("rg")

    generators = [
        "git_metadata",
        "repo_state",
        "file_read",
        "line_window",
        "fixed_search",
        "dir_env",
        "tool_probe",
        "composed",
        "native_fallback_readonly",
    ]
    cases: list[dict[str, str]] = []
    for _ in range(count):
        bucket = rng.choice(generators)
        path = rng.choice(paths)
        if bucket == "git_metadata":
            command = rng.choice(
                [
                    "git rev-parse HEAD",
                    "git rev-parse --git-dir",
                    "git rev-parse --abbrev-ref HEAD",
                    "git rev-parse --show-toplevel",
                    "git rev-parse --is-inside-work-tree",
                ]
            )
        elif bucket == "repo_state":
            command = rng.choice(
                [
                    "git status --short",
                    "git status --porcelain",
                    "git ls-files",
                    "git diff",
                    "git diff --stat",
                    f"git diff -- {sq(path)}",
                ]
            )
        elif bucket == "file_read":
            command = rng.choice([f"cat {sq(path)}", f"file {sq(path)}"])
        elif bucket == "line_window":
            n = rng.choice([1, 2, 3, 5, 10, 20, 40, 80, 160, 300])
            start = rng.randint(1, 220)
            end = start + rng.randint(0, 80)
            command = rng.choice(
                [
                    f"sed -n {start},{end}p {sq(path)}",
                    f"sed -n {start}p {sq(path)}",
                    f"head -n {n} {sq(path)}",
                    f"head -{n} {sq(path)}",
                    f"head {sq(path)}",
                    f"tail -n {n} {sq(path)}",
                    f"tail -{n} {sq(path)}",
                    f"tail {sq(path)}",
                ]
            )
        elif bucket == "fixed_search":
            pattern = rng.choice(["token_1", "value", "marker", "deep_config", "missing_literal"])
            if shutil.which("rg") and rng.random() < 0.45:
                flag = rng.choice(["-F", "-n -F", "-q -F", "--line-number --fixed-strings"])
                command = f"rg {flag} {sq(pattern)} {sq(path)}"
            else:
                flag = rng.choice(["-F", "-q -F"])
                command = f"grep {flag} {sq(pattern)} {sq(path)}"
        elif bucket == "dir_env":
            command = rng.choice(
                [
                    "ls",
                    "ls -p",
                    "ls src",
                    "ls -p src",
                    "printenv PATH",
                    "uname -m",
                    "whoami",
                    "hostname",
                    "id",
                ]
            )
        elif bucket == "tool_probe":
            tool = rng.choice(tools)
            if tool == "git":
                command = "git --version"
            elif tool == "rg":
                command = "rg --version"
            else:
                command = rng.choice([f"{tool} --version", f"which {tool}", f"command -v {tool}"])
        elif bucket == "composed":
            p1 = rng.choice(paths)
            p2 = rng.choice(paths)
            n = rng.choice([1, 2, 3, 5, 10, 25, 50])
            start = rng.randint(1, 140)
            end = start + rng.randint(0, 40)
            pattern = rng.choice(["token_1", "value", "marker", "missing_literal"])
            command = rng.choice(
                [
                    f"sh -c {sq(f'git rev-parse HEAD | cat')}",
                    f"sh -c {sq(f'git status --short | head -n {n}')}",
                    f"sh -c {sq(f'git ls-files | grep -F src')}",
                    f"sh -c {sq(f'cat {p1} | grep -F {pattern} | head -n {n}')}",
                    f"sh -c {sq(f'sed -n {start},{end}p {p2} | tail -n {n}')}",
                    f"sh -c {sq(f'head -n {n} {p1} | tail -n {max(1, n // 2)}')}",
                    f"sh -c {sq(f'git rev-parse HEAD >/dev/null && git status --short >/dev/null && cat {p1} >/dev/null')}",
                ]
            )
            if shutil.which("rg") and rng.random() < 0.3:
                command = f"sh -c {sq(f'rg -F {pattern} {p1} | head -n {n}')}"
        else:
            # Safe controls that should fault open to native when unsupported.
            command = rng.choice(
                [
                    "pwd",
                    "printf 'fallback-control\\n'",
                    f"wc -l {sq(path)}",
                    f"python3 -c {sq('print(\"fallback-python\")')}",
                    "git add -h >/dev/null",
                ]
            )
        cases.append({"bucket": bucket, "command": command})
    return cases


def write_driver(path: Path, cases: list[dict[str, str]]) -> None:
    lines = [DRIVER_PREAMBLE]
    for i, case in enumerate(cases):
        lines.append(
            "run_one "
            + shlex.quote(str(i))
            + " "
            + shlex.quote(case["bucket"])
            + " "
            + shlex.quote(case["command"])
        )
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    path.chmod(0o755)


def parse_records(stdout: bytes, cases: list[dict[str, str]]) -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []
    for raw in stdout.decode("utf-8", errors="replace").splitlines():
        parts = raw.split("\t")
        if len(parts) != 8:
            continue
        idx = int(parts[0])
        case = cases[idx]
        records.append(
            {
                "bucket": case["bucket"],
                "command": case["command"],
                "exit_code": int(parts[2]),
                "stdout_sha256": parts[3],
                "stderr_sha256": parts[4],
                "stdout_size": int(parts[5]),
                "stderr_size": int(parts[6]),
                "elapsed_us": int(parts[7]),
            }
        )
    if len(records) != len(cases):
        raise RuntimeError(f"driver emitted {len(records)} records, expected {len(cases)}")
    return records


def compare(native: list[dict[str, Any]], squire: list[dict[str, Any]]) -> list[dict[str, Any]]:
    mismatches: list[dict[str, Any]] = []
    for i, (left, right) in enumerate(zip(native, squire)):
        for key in ["bucket", "command", "exit_code", "stdout_sha256", "stderr_sha256", "stdout_size", "stderr_size"]:
            if left[key] != right[key]:
                mismatches.append(
                    {
                        "index": i,
                        "bucket": left["bucket"],
                        "command": left["command"],
                        "field": key,
                        "native": left[key],
                        "squire": right[key],
                    }
                )
                break
    if len(native) != len(squire):
        mismatches.append({"field": "record_count", "native": len(native), "squire": len(squire)})
    return mismatches


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
        "max_us": ordered[-1],
        "under_1ms": sum(1 for value in ordered if value < 1000),
    }


def bucket_report(records: list[dict[str, Any]]) -> dict[str, dict[str, float | int]]:
    by_bucket: dict[str, list[float]] = defaultdict(list)
    for record in records:
        by_bucket[record["bucket"]].append(record["elapsed_us"] / 1000.0)
    return {bucket: stats(values) for bucket, values in sorted(by_bucket.items())}


def hot_events(store_root: Path) -> tuple[Counter[str], list[int]]:
    log = store_root / "hot_client_events.log"
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


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("squire_bin", nargs="?")
    parser.add_argument("--cases", type=int, default=2000)
    parser.add_argument("--seed", type=int, default=1)
    parser.add_argument("--json", action="store_true")
    parser.add_argument("--keep-tmp", action="store_true")
    args = parser.parse_args()
    if args.cases < 1:
        raise ValueError("--cases must be positive")

    rng = random.Random(args.seed)
    squire = resolve_squire(args.squire_bin)
    repo = make_repo(rng)
    work = Path(tempfile.mkdtemp(prefix="squire-replay-fuzz-work.", dir=os.environ.get("TMPDIR") or "/private/tmp"))
    store_root = work / "store"
    preload = work / ("squire-preload.dylib" if os.uname().sysname == "Darwin" else "squire-preload.so")
    helper = work / "squire-preload-helper"
    driver = work / "driver.zsh"
    compile_preload(preload)
    compile_helper(helper)

    cases = generate_cases(rng, args.cases)
    write_driver(driver, cases)
    shell = select_shell()

    kernel_env = os.environ.copy()
    kernel_env["SQUIRE_KERNEL_STORE_ROOT"] = str(store_root)
    kernel_env["SQUIRE_STORE_ROOT"] = str(store_root)
    kernel_env["GIT_OPTIONAL_LOCKS"] = "0"
    run([str(squire), "setup"], repo, env=kernel_env)
    run([str(squire), "kernel", "warm", "--short"], repo, env=kernel_env, timeout=120)
    # Run both sides after setup/warm so Squire's local store and Git optional
    # lock behavior are part of the same stable workspace state.
    native_records = parse_records(run([str(shell), str(driver)], repo, env=kernel_env, timeout=600).stdout, cases)
    before_counts, before_us = hot_events(store_root)
    session_args = [
        str(squire),
        "session",
        "--quiet",
        "--preload",
        "--preload-lib",
        str(preload),
        "--enable-warm-file-replay",
        "--no-warm",
        "--no-maintainer",
        "--",
        str(shell),
        str(driver),
    ]
    squire_records = parse_records(run(session_args, repo, env=kernel_env, timeout=600).stdout, cases)
    after_counts, after_us = hot_events(store_root)

    mismatches = compare(native_records, squire_records)
    native_total = sum(record["elapsed_us"] for record in native_records) / 1000.0
    squire_total = sum(record["elapsed_us"] for record in squire_records) / 1000.0
    event_counts = after_counts - before_counts
    replay_us = after_us[len(before_us) :]
    bucket_counts = Counter(case["bucket"] for case in cases)
    report: dict[str, Any] = {
        "replay_fuzz": "pass" if not mismatches else "fail",
        "seed": args.seed,
        "cases": len(cases),
        "repo": str(repo),
        "work": str(work),
        "store_root": str(store_root),
        "exactness": not mismatches,
        "mismatches": len(mismatches),
        "mismatch_examples": mismatches[:20],
        "bucket_counts": dict(sorted(bucket_counts.items())),
        "native_total_ms": round(native_total, 3),
        "squire_total_ms": round(squire_total, 3),
        "delta_ms": round(native_total - squire_total, 3),
        "speedup": round(native_total / squire_total, 3) if squire_total > 0 else None,
        "native_by_bucket": bucket_report(native_records),
        "squire_by_bucket": bucket_report(squire_records),
        "hot_client_event_counts": dict(sorted(event_counts.items())),
        "hot_client_replay_us": stats_us(replay_us),
    }
    if args.json:
        print(json.dumps(report, indent=2, sort_keys=True))
    else:
        print(f"replay_fuzz: {report['replay_fuzz']}")
        print(f"seed: {args.seed}")
        print(f"cases: {len(cases)}")
        print(f"exactness: {report['exactness']}")
        print(f"mismatches: {report['mismatches']}")
        print(f"native_total_ms: {report['native_total_ms']}")
        print(f"squire_total_ms: {report['squire_total_ms']}")
        print(f"delta_ms: {report['delta_ms']}")
        print(f"speedup: {report['speedup']}")
        print(f"hot_client_event_counts: {report['hot_client_event_counts']}")
    if not args.keep_tmp:
        shutil.rmtree(repo, ignore_errors=True)
        shutil.rmtree(work, ignore_errors=True)
    return 0 if not mismatches else 1


if __name__ == "__main__":
    raise SystemExit(main())
