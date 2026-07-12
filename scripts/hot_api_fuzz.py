#!/usr/bin/env python3
"""Fuzz the versioned Squire runtime API used by agent adapters.

This test calls `squire_runtime_try_execute(...)` directly. Native execution
is used only as the byte-for-byte reference for read-only commands. Mutations,
validation commands, package setup, out-of-workspace symlinks, and env-mismatch
probes must miss and are never executed natively by this script.
"""

from __future__ import annotations

import argparse
import ctypes
from collections import Counter, defaultdict
from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import random
import shlex
import shutil
import statistics
import subprocess
import tempfile
import time
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
HOT_API_SOURCE = ROOT / "shims" / "squire_hot_api.c"


def default_tmp_dir() -> str:
    if os.uname().sysname == "Darwin" and Path("/private/tmp").is_dir():
        return "/private/tmp"
    return tempfile.gettempdir()


@dataclass(frozen=True)
class Case:
    name: str
    bucket: str
    argv: tuple[str, ...]
    cwd_rel: str = "."
    compare_native: bool = True
    must_miss: bool = False


class SquireHotResult(ctypes.Structure):
    _fields_ = [
        ("handle", ctypes.c_void_p),
        ("stdout_data", ctypes.POINTER(ctypes.c_ubyte)),
        ("stdout_len", ctypes.c_uint32),
        ("stderr_data", ctypes.POINTER(ctypes.c_ubyte)),
        ("stderr_len", ctypes.c_uint32),
        ("exit_code", ctypes.c_int),
        ("native_wall_ms", ctypes.c_uint64),
    ]


def run(
    argv: list[str],
    cwd: Path,
    *,
    env: dict[str, str] | None = None,
    check: bool = True,
    timeout: int = 120,
) -> subprocess.CompletedProcess[bytes]:
    proc = subprocess.run(
        argv,
        cwd=str(cwd),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=timeout,
    )
    if check and proc.returncode != 0:
        raise RuntimeError(
            f"command failed ({proc.returncode}): {' '.join(argv)}\n"
            f"stdout={proc.stdout.decode(errors='replace')}\n"
            f"stderr={proc.stderr.decode(errors='replace')}"
        )
    return proc


def compiler() -> str:
    cc = shutil.which("cc") or shutil.which("clang") or shutil.which("gcc")
    if not cc:
        raise RuntimeError("cc/clang/gcc is required")
    return cc


def resolve_squire(explicit: str | None, work: Path) -> Path:
    if explicit:
        candidate = Path(explicit).expanduser()
        if not candidate.is_file():
            raise RuntimeError(f"squire binary not found: {candidate}")
        return candidate.resolve()

    go = shutil.which("go")
    if not go:
        raise RuntimeError("go is required to build the current Squire checkout")
    output = work / "squire"
    run([go, "build", "-o", str(output), "./cmd/squire"], ROOT, timeout=180)
    return output


def compile_hot_api(out: Path) -> None:
    cc = compiler()
    if os.uname().sysname == "Darwin":
        argv = [cc, "-O3", "-DNDEBUG", "-fPIC", "-shared", str(HOT_API_SOURCE), "-o", str(out)]
    else:
        argv = [
            cc,
            "-O3",
            "-DNDEBUG",
            "-fPIC",
            "-shared",
            str(HOT_API_SOURCE),
            "-o",
            str(out),
            "-ldl",
            "-lcrypto",
        ]
    compile_env = os.environ.copy()
    compile_env["TMPDIR"] = str(out.parent)
    run(argv, ROOT, env=compile_env, timeout=60)


def make_repo(seed: int) -> Path:
    rng = random.Random(seed)
    base = os.environ.get("TMPDIR") or default_tmp_dir()
    repo = Path(tempfile.mkdtemp(prefix="squire-hot-api-fuzz.", dir=base))
    for rel in ("src", "src/nested", "lib", "docs", "tests", "logs"):
        (repo / rel).mkdir(parents=True, exist_ok=True)
    (repo / ".gitignore").write_text("node_modules/\n__pycache__/\n*.pyc\n.tmp/\nlogs/*.tmp\n", encoding="utf-8")
    (repo / "README.md").write_text(
        "# Hot API Fuzz\n\n" + "\n".join(f"readme line {i} token_{i % 17}" for i in range(1, 260)) + "\n",
        encoding="utf-8",
    )
    (repo / "package.json").write_text(
        json.dumps({"name": "squire-hot-api-fuzz", "private": True, "type": "module"}, indent=2) + "\n",
        encoding="utf-8",
    )
    (repo / "pyproject.toml").write_text(
        "[project]\nname = \"squire-hot-api-fuzz\"\nversion = \"0.0.0\"\n",
        encoding="utf-8",
    )
    for i in range(16):
        lines = [
            f"export const value_{i}_{j} = {i * 1000 + j}; // token_{(i + j) % 23} marker_{j % 11}"
            for j in range(1, 220)
        ]
        (repo / "src" / f"module_{i:02d}.js").write_text("\n".join(lines) + "\n", encoding="utf-8")
    for i in range(8):
        lines = [f"def value_{i}_{j}():\n    return {i * 1000 + j}\n" for j in range(1, 120)]
        (repo / "lib" / f"mod_{i:02d}.py").write_text("\n".join(lines), encoding="utf-8")
    (repo / "docs" / "notes.md").write_text(
        "\n".join(f"note {i} token_{rng.randrange(23)} marker_{rng.randrange(19)}" for i in range(1, 240)) + "\n",
        encoding="utf-8",
    )
    (repo / "src" / "nested" / "config.json").write_text(
        json.dumps({"feature": "nested", "items": list(range(32)), "marker": "deep_config"}, indent=2) + "\n",
        encoding="utf-8",
    )
    (repo / "logs" / "sample.log").write_text(
        "\n".join(f"log line {i} token_{i % 13}" for i in range(1, 180)) + "\n",
        encoding="utf-8",
    )
    outside = repo.parent / f"{repo.name}.outside.txt"
    outside.write_text("outside workspace secret-ish bytes\n", encoding="utf-8")
    os.symlink(outside, repo / "src" / "outside-link.txt")
    run(["git", "init", "-b", "main"], repo)
    run(["git", "config", "user.email", "fuzz@example.com"], repo)
    run(["git", "config", "user.name", "Squire Hot API Fuzz"], repo)
    run(["git", "add", "."], repo)
    run(["git", "commit", "-m", "init"], repo)
    (repo / "src" / "module_00.js").write_text(
        (repo / "src" / "module_00.js").read_text(encoding="utf-8") + "export const dirty_marker = 1;\n",
        encoding="utf-8",
    )
    (repo / "docs" / "scratch.md").write_text("untracked scratch\n", encoding="utf-8")
    return repo


def path_cases() -> list[str]:
    return [
        "README.md",
        "package.json",
        "pyproject.toml",
        "src/module_00.js",
        "src/module_01.js",
        "src/module_07.js",
        "src/module_15.js",
        "lib/mod_00.py",
        "lib/mod_03.py",
        "docs/notes.md",
        "src/nested/config.json",
        "logs/sample.log",
    ]


def generate_cases(seed: int, count: int, selected_buckets: list[str] | None = None) -> list[Case]:
    rng = random.Random(seed)
    paths = path_cases()
    buckets = [
        "git_metadata",
        "repo_state",
        "file_read",
        "line_window",
        "fixed_search",
        "dir_env",
        "tool_probe",
        "composed_pipe",
        "composed_sequence",
        "safe_native_miss",
        "policy_must_miss",
    ]
    if selected_buckets is not None:
        unknown = sorted(set(selected_buckets) - set(buckets))
        if unknown:
            raise ValueError(f"unknown buckets: {', '.join(unknown)}")
        buckets = selected_buckets
    cases: list[Case] = []
    for i in range(count):
        bucket = rng.choice(buckets)
        path = rng.choice(paths)
        if bucket == "git_metadata":
            argv = rng.choice(
                [
                    ("git", "rev-parse", "HEAD"),
                    ("git", "rev-parse", "--git-dir"),
                    ("git", "rev-parse", "--abbrev-ref", "HEAD"),
                    ("git", "branch", "--show-current"),
                    ("git", "rev-parse", "--show-toplevel"),
                    ("git", "rev-parse", "--is-inside-work-tree"),
                ]
            )
        elif bucket == "repo_state":
            repo_state_choices = [
                ("git", "status", "--short"),
                ("git", "status", "--porcelain"),
                ("git", "ls-files"),
                ("git", "ls-files", "src"),
                ("git", "ls-files", "lib"),
                ("git", "log", "-1", "--format=%H%n%s"),
                ("git", "diff"),
                ("git", "diff", "--stat"),
                ("git", "diff", "--", path),
            ]
            argv = rng.choice(repo_state_choices)
        elif bucket == "file_read":
            argv = rng.choice([("cat", path), ("file", path)])
        elif bucket == "line_window":
            n = str(rng.choice([1, 2, 3, 5, 10, 20, 40, 80, 160]))
            start = rng.randint(1, 180)
            end = start + rng.randint(0, 80)
            argv = rng.choice(
                [
                    ("sed", "-n", f"{start},{end}p", path),
                    ("sed", "-n", f"{start}p", path),
                    ("head", "-n", n, path),
                    ("head", f"-{n}", path),
                    ("head", path),
                    ("tail", "-n", n, path),
                    ("tail", f"-{n}", path),
                    ("tail", path),
                ]
            )
        elif bucket == "fixed_search":
            pattern = rng.choice(["token_1", "value", "marker", "deep_config", "missing_literal"])
            if shutil.which("rg") and rng.random() < 0.45:
                argv = rng.choice(
                    [
                        ("rg", "-F", pattern, path),
                        ("rg", "-n", "-F", pattern, path),
                        ("rg", "-q", "-F", pattern, path),
                        ("rg", "--line-number", "--fixed-strings", pattern, path),
                    ]
                )
            else:
                argv = rng.choice([("grep", "-F", pattern, path), ("grep", "-q", "-F", pattern, path)])
        elif bucket == "dir_env":
            argv = rng.choice(
                [
                    ("ls",),
                    ("ls", "-p"),
                    ("ls", "src"),
                    ("ls", "-p", "src"),
                    ("printenv", "PATH"),
                    ("uname", "-m"),
                    ("whoami",),
                    ("hostname",),
                    ("id",),
                ]
            )
        elif bucket == "tool_probe":
            tool = rng.choice(["python3", "git", "go", "rg", "node"])
            if tool == "git":
                argv = ("git", "--version")
            elif tool == "rg":
                argv = ("rg", "--version")
            else:
                argv = rng.choice([(tool, "--version"), ("which", tool), ("command", "-v", tool)])
        elif bucket == "composed_pipe":
            p1 = rng.choice(paths)
            p2 = rng.choice(paths)
            n = rng.choice([1, 2, 3, 5, 10, 25, 50])
            start = rng.randint(1, 140)
            end = start + rng.randint(0, 40)
            pattern = rng.choice(["token_1", "value", "marker", "missing_literal"])
            script = rng.choice(
                [
                    "git rev-parse HEAD | cat",
                    f"git status --short | head -n {n}",
                    "git ls-files | grep -F src",
                    "git ls-files src | wc -l",
                    "git ls-files src | sort",
                    f"cat {p1} | grep -F {pattern} | head -n {n}",
                    f"sed -n {start},{end}p {p2} | tail -n {n}",
                    f"head -n {n} {p1} | tail -n {max(1, n // 2)}",
                ]
            )
            if shutil.which("rg") and rng.random() < 0.25:
                script = f"rg -F {pattern} {p1} | head -n {n}"
            argv = ("/bin/sh", "-c", script)
        elif bucket == "composed_sequence":
            p1 = rng.choice(paths)
            script = rng.choice(
                [
                    f"git rev-parse HEAD >/dev/null && git status --short >/dev/null && cat {p1} >/dev/null",
                    f"git ls-files >/dev/null; sed -n 1,4p {p1} >/dev/null; tail -n 2 {p1} >/dev/null",
                    f"(git ls-files | grep -F src >/dev/null) && (head -n 4 {p1} | tail -n 2 >/dev/null)",
                    "git branch --show-current >/dev/null; git ls-files src | wc -l >/dev/null; git ls-files src | sort >/dev/null",
                ]
            )
            argv = ("/bin/sh", "-c", script)
        elif bucket == "safe_native_miss":
            argv = rng.choice(
                [
                    ("pwd",),
                    ("printf", "fallback-control\n"),
                    ("wc", "-l", path),
                    ("python3", "-c", "print('fallback-python')"),
                    ("rg", "--files"),
                    ("/bin/sh", "-c", "echo hello | wc -c"),
                    ("/bin/sh", "-c", "rg --files | head -n 5"),
                    ("/bin/sh", "-c", "for i in 1 2; do git rev-parse HEAD; done"),
                ]
            )
        else:
            argv = rng.choice(
                [
                    ("git", "add", "README.md"),
                    ("git", "commit", "-m", "unsafe"),
                    ("git", "reset", "--hard"),
                    ("go", "test", "./..."),
                    ("npm", "install"),
                    ("touch", "should-not-exist"),
                    ("cat", "src/outside-link.txt"),
                ]
            )
            cases.append(Case(f"case_{i}", bucket, argv, must_miss=True, compare_native=False))
            continue
        cases.append(Case(f"case_{i}", bucket, tuple(argv)))
    return cases


def bytes_from_ptr(ptr: ctypes.POINTER(ctypes.c_ubyte), length: int) -> bytes:
    if length == 0:
        return b""
    if not ptr:
        raise RuntimeError("nonzero result length with null pointer")
    return bytes(ctypes.string_at(ptr, length))


class HotAPI:
    def __init__(self, library_path: Path) -> None:
        self.lib = ctypes.CDLL(str(library_path))
        self.lib.squire_runtime_abi_version.argtypes = []
        self.lib.squire_runtime_abi_version.restype = ctypes.c_uint32
        if self.lib.squire_runtime_abi_version() != 1:
            raise RuntimeError("unsupported Squire runtime ABI")
        self.lib.squire_runtime_try_execute.argtypes = [
            ctypes.c_char_p,
            ctypes.c_int,
            ctypes.POINTER(ctypes.c_char_p),
            ctypes.c_int,
            ctypes.POINTER(ctypes.c_char_p),
            ctypes.POINTER(SquireHotResult),
        ]
        self.lib.squire_runtime_try_execute.restype = ctypes.c_int
        self.lib.squire_runtime_record_hit.argtypes = [ctypes.POINTER(SquireHotResult)]
        self.lib.squire_runtime_release.argtypes = [ctypes.POINTER(SquireHotResult)]

    def try_replay(self, cwd: Path, argv: tuple[str, ...], env: dict[str, str]) -> dict[str, Any]:
        argv_bytes = [arg.encode() for arg in argv]
        argv_arr = (ctypes.c_char_p * len(argv_bytes))(*argv_bytes)
        env_items = [f"{key}={value}".encode() for key, value in sorted(env.items())]
        env_arr = (ctypes.c_char_p * len(env_items))(*env_items)
        result = SquireHotResult()
        start = time.perf_counter_ns()
        decision = self.lib.squire_runtime_try_execute(
            str(cwd).encode(),
            len(argv_bytes),
            argv_arr,
            len(env_items),
            env_arr,
            ctypes.byref(result),
        )
        if decision != 1 or not result.handle:
            elapsed_us = (time.perf_counter_ns() - start) / 1000
            return {"hit": False, "decision": int(decision), "elapsed_us": elapsed_us}
        stdout = bytes_from_ptr(result.stdout_data, result.stdout_len)
        stderr = bytes_from_ptr(result.stderr_data, result.stderr_len)
        exit_code = int(result.exit_code)
        native_wall_ms = int(result.native_wall_ms)
        self.lib.squire_runtime_record_hit(ctypes.byref(result))
        self.lib.squire_runtime_release(ctypes.byref(result))
        elapsed_us = (time.perf_counter_ns() - start) / 1000
        return {
            "hit": True,
            "stdout": stdout,
            "stderr": stderr,
            "exit_code": exit_code,
            "native_wall_ms": native_wall_ms,
            "elapsed_us": elapsed_us,
        }


def native_reference(cwd: Path, argv: tuple[str, ...], env: dict[str, str]) -> dict[str, Any]:
    if len(argv) == 3 and argv[0] == "command" and argv[1] == "-v":
        return native_reference(cwd, ("sh", "-c", "command -v " + shlex.quote(argv[2])), env)
    start = time.perf_counter_ns()
    try:
        proc = run(list(argv), cwd, env=env, check=False, timeout=20)
    except FileNotFoundError:
        elapsed_us = (time.perf_counter_ns() - start) / 1000
        return {
            "stdout": b"",
            "stderr": f"{argv[0]}: not found\n".encode(),
            "exit_code": 127,
            "elapsed_us": elapsed_us,
            "missing_executable": True,
        }
    elapsed_us = (time.perf_counter_ns() - start) / 1000
    return {
        "stdout": proc.stdout,
        "stderr": proc.stderr,
        "exit_code": proc.returncode,
        "elapsed_us": elapsed_us,
    }


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def stats(values: list[float]) -> dict[str, float | int]:
    if not values:
        return {"count": 0}
    ordered = sorted(values)

    def p(q: float) -> float:
        return ordered[min(len(ordered) - 1, max(0, int(len(ordered) * q + 0.999999) - 1))]

    return {
        "count": len(values),
        "avg": round(statistics.mean(values), 3),
        "p50": round(p(0.50), 3),
        "p95": round(p(0.95), 3),
        "p99": round(p(0.99), 3),
        "max": round(ordered[-1], 3),
    }


def record_bucket(summary: dict[str, Any], case: Case) -> dict[str, Any]:
    by_bucket = summary.setdefault("by_bucket", {})
    bucket = by_bucket.setdefault(
        case.bucket,
        {
            "cases": 0,
            "hits": 0,
            "misses": 0,
            "native_compared": 0,
            "mismatches": 0,
            "must_miss_hits": 0,
            "unsupported": 0,
            "eligible_misses": 0,
            "native_us": [],
            "native_hit_us": [],
            "hot_us": [],
            "hit_us": [],
            "miss_us": [],
        },
    )
    bucket["cases"] += 1
    return bucket


def run_case(api: HotAPI, repo: Path, case: Case, env: dict[str, str]) -> dict[str, Any]:
    cwd = (repo / case.cwd_rel).resolve()
    hot = api.try_replay(cwd, case.argv, env)
    native = None
    if case.compare_native:
        native = native_reference(cwd, case.argv, env)
    mismatch = None
    if hot["hit"] and case.must_miss:
        mismatch = "must_miss_hit"
    elif hot["hit"] and native is not None:
        if (
            hot["exit_code"] != native["exit_code"]
            or hot["stdout"] != native["stdout"]
            or hot["stderr"] != native["stderr"]
        ):
            mismatch = "byte_mismatch"
    return {"case": case, "hot": hot, "native": native, "mismatch": mismatch}


def replay_is_current(hot: dict[str, Any], native: dict[str, Any]) -> bool:
    return (not hot["hit"]) or (
        hot.get("exit_code") == native["exit_code"]
        and hot.get("stdout") == native["stdout"]
        and hot.get("stderr") == native["stderr"]
    )


def warm_short(squire: Path, repo: Path, env: dict[str, str]) -> None:
    run([str(squire), "kernel", "warm", "--short"], repo, env=env, timeout=120)


def append_invalidation_probe(
    probes: list[dict[str, Any]],
    api: HotAPI,
    repo: Path,
    env: dict[str, str],
    *,
    name: str,
    argv: tuple[str, ...],
    before_hit: bool,
) -> None:
    native = native_reference(repo, argv, env)
    hot = api.try_replay(repo, argv, env)
    probes.append(
        {
            "name": name,
            "before_hit": before_hit,
            "after_hit": hot["hit"],
            "safe": replay_is_current(hot, native),
        }
    )


def rewarm_probe(
    probes: list[dict[str, Any]],
    api: HotAPI,
    squire: Path,
    repo: Path,
    env: dict[str, str],
    argv: tuple[str, ...],
) -> None:
    warm_short(squire, repo, env)
    rewarm = run_case(api, repo, Case(f"{probes[-1]['name']}_after_rewarm", "invalidation", argv), env)
    probes[-1]["rewarm_hit"] = rewarm["hot"]["hit"]
    probes[-1]["rewarm_exact"] = rewarm["mismatch"] is None and rewarm["hot"]["hit"]


def run_invalidation_probes(api: HotAPI, squire: Path, repo: Path, env: dict[str, str]) -> list[dict[str, Any]]:
    probes: list[dict[str, Any]] = []
    file_case = Case("invalidation_file_before", "invalidation", ("cat", "src/module_01.js"))
    before = run_case(api, repo, file_case, env)
    target = repo / "src" / "module_01.js"
    target.write_text(target.read_text(encoding="utf-8") + "export const invalidated = true;\n", encoding="utf-8")
    append_invalidation_probe(
        probes,
        api,
        repo,
        env,
        name="file_content_epoch",
        argv=file_case.argv,
        before_hit=before["hot"]["hit"],
    )
    rewarm_probe(probes, api, squire, repo, env, file_case.argv)

    atomic_target = repo / "src" / "module_07.js"
    atomic_case = Case("invalidation_atomic_replace_before", "invalidation", ("cat", "src/module_07.js"))
    atomic_before = run_case(api, repo, atomic_case, env)
    original_stat = atomic_target.stat()
    original_bytes = atomic_target.read_bytes()
    replacement_bytes = original_bytes.replace(b"token_8", b"token_9", 1)
    if len(replacement_bytes) != len(original_bytes) or replacement_bytes == original_bytes:
        raise RuntimeError("failed to construct same-size replacement payload")
    replacement = atomic_target.with_name(f".{atomic_target.name}.replacement")
    replacement.write_bytes(replacement_bytes)
    os.chmod(replacement, original_stat.st_mode)
    os.utime(replacement, ns=(original_stat.st_atime_ns, original_stat.st_mtime_ns))
    os.replace(replacement, atomic_target)
    append_invalidation_probe(
        probes,
        api,
        repo,
        env,
        name="same_size_atomic_replace_restored_mtime",
        argv=atomic_case.argv,
        before_hit=atomic_before["hot"]["hit"],
    )
    rewarm_probe(probes, api, squire, repo, env, atomic_case.argv)

    index_case = Case("invalidation_index_before", "invalidation", ("git", "ls-files"))
    index_before = run_case(api, repo, index_case, env)
    (repo / "docs" / "index-only.md").write_text("index only boundary\n", encoding="utf-8")
    run(["git", "add", "docs/index-only.md"], repo, env=env)
    append_invalidation_probe(
        probes,
        api,
        repo,
        env,
        name="git_index_epoch",
        argv=index_case.argv,
        before_hit=index_before["hot"]["hit"],
    )
    rewarm_probe(probes, api, squire, repo, env, index_case.argv)

    status_case = Case("invalidation_untracked_before", "invalidation", ("git", "status", "--short"))
    status_before = run_case(api, repo, status_case, env)
    (repo / "docs" / "new-untracked.md").write_text("untracked boundary\n", encoding="utf-8")
    append_invalidation_probe(
        probes,
        api,
        repo,
        env,
        name="untracked_workspace_epoch",
        argv=status_case.argv,
        before_hit=status_before["hot"]["hit"],
    )
    rewarm_probe(probes, api, squire, repo, env, status_case.argv)

    diff_case = Case(
        "invalidation_same_size_diff_before",
        "invalidation",
        ("git", "diff", "--", "src/module_00.js"),
    )
    diff_before = run_case(api, repo, diff_case, env)
    diff_target = repo / "src" / "module_00.js"
    diff_stat = diff_target.stat()
    diff_bytes = diff_target.read_bytes()
    changed_diff_bytes = diff_bytes.replace(b"dirty_marker = 1", b"dirty_marker = 2", 1)
    if len(changed_diff_bytes) != len(diff_bytes) or changed_diff_bytes == diff_bytes:
        raise RuntimeError("failed to construct same-size diff payload")
    diff_target.write_bytes(changed_diff_bytes)
    os.utime(diff_target, ns=(diff_stat.st_atime_ns, diff_stat.st_mtime_ns))
    append_invalidation_probe(
        probes,
        api,
        repo,
        env,
        name="same_size_diff_restored_mtime",
        argv=diff_case.argv,
        before_hit=diff_before["hot"]["hit"],
    )
    rewarm_probe(probes, api, squire, repo, env, diff_case.argv)

    config_case = Case("invalidation_config_before", "invalidation", ("git", "status", "--short"))
    config_before = run_case(api, repo, config_case, env)
    run(["git", "config", "--local", "status.showUntrackedFiles", "no"], repo, env=env)
    append_invalidation_probe(
        probes,
        api,
        repo,
        env,
        name="git_config_epoch",
        argv=config_case.argv,
        before_hit=config_before["hot"]["hit"],
    )
    rewarm_probe(probes, api, squire, repo, env, config_case.argv)

    head_case = Case("invalidation_head_before", "invalidation", ("git", "rev-parse", "HEAD"))
    before_head = run_case(api, repo, head_case, env)
    (repo / "docs" / "commit-boundary.md").write_text("commit boundary\n", encoding="utf-8")
    run(["git", "add", "docs/commit-boundary.md"], repo, env=env)
    run(["git", "commit", "-m", "boundary"], repo, env=env)
    append_invalidation_probe(
        probes,
        api,
        repo,
        env,
        name="git_head_epoch",
        argv=head_case.argv,
        before_hit=before_head["hot"]["hit"],
    )
    rewarm_probe(probes, api, squire, repo, env, head_case.argv)

    branch_case = Case(
        "invalidation_branch_before",
        "invalidation",
        ("git", "rev-parse", "--abbrev-ref", "HEAD"),
    )
    branch_before = run_case(api, repo, branch_case, env)
    run(["git", "branch", "-m", "renamed-by-invalidation-probe"], repo, env=env)
    append_invalidation_probe(
        probes,
        api,
        repo,
        env,
        name="symbolic_head_epoch",
        argv=branch_case.argv,
        before_hit=branch_before["hot"]["hit"],
    )
    rewarm_probe(probes, api, squire, repo, env, branch_case.argv)

    bad_env = dict(env)
    bad_env["PATH"] = "/definitely/not/the/current/path"
    env_hot = api.try_replay(repo, ("git", "rev-parse", "HEAD"), bad_env)
    probes.append({"name": "env_path_mismatch", "after_hit": env_hot["hit"], "safe": not env_hot["hit"]})

    symlink_hot = api.try_replay(repo, ("cat", "src/outside-link.txt"), env)
    probes.append({"name": "out_of_workspace_symlink", "after_hit": symlink_hot["hit"], "safe": not symlink_hot["hit"]})
    return probes


def run_codex_user_shell_regression(api: HotAPI, repo: Path, env: dict[str, str]) -> dict[str, Any]:
    """Cover the real squire-codex typed-command path shape.

    Codex user-shell commands arrive at the hot API as `sh -c <command>` with a
    per-command environment map. Git metadata can ignore proven-irrelevant
    locale/pager additions. Environment-sensitive compositions and printenv
    must reject any child environment that differs from the process which
    prepared the snapshot.
    """
    keys = ("LC_ALL", "LC_CTYPE", "GIT_PAGER")
    saved = {key: os.environ.get(key) for key in keys}
    try:
        for key in keys:
            os.environ.pop(key, None)
        command_env = dict(env)
        command_env["LC_ALL"] = "C"
        command_env["LC_CTYPE"] = "C"
        command_env["GIT_PAGER"] = "cat"
        case = Case(
            "codex_user_shell_git_metadata_env_gap",
            "codex_user_shell",
            ("sh", "-c", "git rev-parse HEAD"),
        )
        result = run_case(api, repo, case, command_env)
        hot = result["hot"]
        native = result["native"]
        composed_case = Case(
            "codex_user_shell_composed_env_gap",
            "codex_user_shell",
            ("sh", "-c", "git ls-files | grep -F src"),
        )
        composed_result = run_case(api, repo, composed_case, command_env)
        composed_hot = composed_result["hot"]
        composed_native = composed_result["native"]
        env_probe = api.try_replay(repo, ("sh", "-c", "printenv LC_ALL"), command_env)
        printenv_case = Case(
            "codex_user_shell_printenv_baseline",
            "codex_user_shell",
            ("printenv", "PATH"),
        )
        baseline_env = os.environ.copy()
        printenv_baseline = run_case(api, repo, printenv_case, baseline_env)
        mismatched_path_env = dict(baseline_env)
        mismatched_path_env["PATH"] = "/squire/child/path-mismatch"
        printenv_mismatch = api.try_replay(repo, printenv_case.argv, mismatched_path_env)
        safe = (
            bool(hot["hit"])
            and result["mismatch"] is None
            and not bool(composed_hot["hit"])
            and not bool(env_probe["hit"])
            and bool(printenv_baseline["hot"]["hit"])
            and printenv_baseline["mismatch"] is None
            and not bool(printenv_mismatch["hit"])
        )
        return {
            "name": case.name,
            "argv": list(case.argv),
            "hit": bool(hot["hit"]),
            "mismatch": result["mismatch"],
            "safe": safe,
            "hot_us": round(hot["elapsed_us"], 3),
            "native_us": round(native["elapsed_us"], 3) if native is not None else None,
            "hot_exit": hot.get("exit_code"),
            "native_exit": native.get("exit_code") if native is not None else None,
            "composed": {
                "name": composed_case.name,
                "argv": list(composed_case.argv),
                "hit": bool(composed_hot["hit"]),
                "mismatch": composed_result["mismatch"],
                "hot_us": round(composed_hot["elapsed_us"], 3),
                "native_us": round(composed_native["elapsed_us"], 3)
                if composed_native is not None
                else None,
                "hot_exit": composed_hot.get("exit_code"),
                "native_exit": composed_native.get("exit_code")
                if composed_native is not None
                else None,
            },
            "env_probe_missed": not bool(env_probe["hit"]),
            "printenv_baseline_hit": bool(printenv_baseline["hot"]["hit"]),
            "printenv_mismatch_missed": not bool(printenv_mismatch["hit"]),
        }
    finally:
        for key, value in saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value


def run_runtime_policy_regression(api: HotAPI, repo: Path, env: dict[str, str]) -> dict[str, Any]:
    hit_cases = [
        ("git", "rev-parse", "HEAD"),
        ("sh", "-c", "git rev-parse HEAD"),
        ("sh", "-c", "git ls-files | grep -F src"),
    ]
    unsupported_cases = [
        ("go", "test", "./..."),
        ("git", "--version"),
        ("which", "python3"),
        ("rg", "--files"),
        ("file", "README.md"),
        ("whoami",),
        ("id",),
        ("sh", "-c", "go test ./..."),
        ("sh", "-c", "rg --files | head -n 5"),
        ("sh", "-c", "git ls-files | python3 -c pass"),
    ]
    results: list[dict[str, Any]] = []
    safe = True
    for argv in hit_cases:
        result = run_case(api, repo, Case("runtime_policy_hit", "runtime_policy", argv), env)
        passed = result["hot"]["hit"] and result["mismatch"] is None
        safe = safe and passed
        results.append(
            {
                "argv": list(argv),
                "expected": "hit",
                "decision": result["hot"].get("decision", 1 if result["hot"]["hit"] else None),
                "passed": passed,
            }
        )
    for argv in unsupported_cases:
        hot = api.try_replay(repo, argv, env)
        passed = not hot["hit"] and hot.get("decision") == -1
        safe = safe and passed
        results.append(
            {
                "argv": list(argv),
                "expected": "unsupported",
                "decision": hot.get("decision"),
                "passed": passed,
            }
        )
    mismatched_env = dict(env)
    mismatched_env["PATH"] = "/runtime/policy/mismatch"
    hot = api.try_replay(repo, ("git", "rev-parse", "HEAD"), mismatched_env)
    passed = not hot["hit"] and hot.get("decision") == 0
    safe = safe and passed
    results.append(
        {
            "argv": ["git", "rev-parse", "HEAD"],
            "expected": "eligible_miss",
            "decision": hot.get("decision"),
            "passed": passed,
        }
    )
    return {"safe": safe, "cases": results}


def render_markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Squire Hot API Fuzz Report",
        "",
        f"- status: `{report['status']}`",
        f"- seed: `{report['seed']}`",
        f"- generated cases: `{report['cases']}`",
        f"- hot hits: `{report['hot_hits']}`",
        f"- safe misses: `{report['safe_misses']}`",
        f"- unsupported decisions: `{report['unsupported']}`",
        f"- eligible cold/stale misses: `{report['eligible_misses']}`",
        f"- mismatches: `{report['mismatches']}`",
        f"- must-miss hits: `{report['must_miss_hits']}`",
        f"- compared native commands: `{report['native_compared']}`",
        f"- estimated native avoided on hits: `{report['estimated_native_avoided_ms']}` ms",
        f"- measured hot wall on hits: `{report['measured_hot_hit_wall_ms']}` ms",
        f"- Codex user-shell regression safe: `{report['codex_user_shell_regression']['safe']}`",
        f"- runtime policy regression safe: `{report['runtime_policy_regression']['safe']}`",
        f"- invalidation probes safe: `{report['invalidation_safe']}`",
        "",
        "## Latency",
        "",
        "| class | count | avg | p50 | p95 | p99 | max |",
        "| --- | ---: | ---: | ---: | ---: | ---: | ---: |",
    ]
    for label, values in [
        ("native_us_compared", report["native_us"]),
        ("native_us_matching_hits", report["native_hit_us"]),
        ("hot_us_all", report["hot_us_all"]),
        ("hot_us_hits", report["hot_us_hits"]),
        ("hot_us_misses", report["hot_us_misses"]),
    ]:
        lines.append(
            f"| {label} | {values.get('count', 0)} | {values.get('avg', 0)} | {values.get('p50', 0)} | {values.get('p95', 0)} | {values.get('p99', 0)} | {values.get('max', 0)} |"
        )
    lines.extend(["", "## Buckets", "", "| bucket | cases | hits | misses | unsupported | mismatches | hit p95 us | miss p95 us | matching native p95 us |", "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"])
    for bucket, data in sorted(report["by_bucket"].items()):
        lines.append(
            f"| {bucket} | {data['cases']} | {data['hits']} | {data['misses']} | {data['unsupported']} | {data['mismatches']} | {data['hit_us'].get('p95', 0)} | {data['miss_us'].get('p95', 0)} | {data['native_hit_us'].get('p95', 0)} |"
        )
    lines.extend(["", "## Codex User-Shell Regression", ""])
    lines.append("```json")
    lines.append(json.dumps(report["codex_user_shell_regression"], indent=2))
    lines.append("```")
    lines.extend(["", "## Invalidation Probes", "", "| probe | safe | before hit | after hit | rewarm hit | rewarm exact |", "| --- | --- | --- | --- | --- | --- |"])
    for probe in report["invalidation_probes"]:
        lines.append(
            f"| {probe['name']} | {probe.get('safe')} | {probe.get('before_hit', '')} | {probe.get('after_hit', '')} | {probe.get('rewarm_hit', '')} | {probe.get('rewarm_exact', '')} |"
        )
    if report["mismatch_examples"]:
        lines.extend(["", "## Mismatch Examples", ""])
        lines.append("```json")
        lines.append(json.dumps(report["mismatch_examples"], indent=2))
        lines.append("```")
    if report["miss_examples"]:
        lines.extend(["", "## Miss Examples", ""])
        lines.append("```json")
        lines.append(json.dumps(report["miss_examples"][:20], indent=2))
        lines.append("```")
    return "\n".join(lines) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("squire_bin", nargs="?")
    parser.add_argument("--cases", type=int, default=5000)
    parser.add_argument("--seed", type=int, default=20260702)
    parser.add_argument("--buckets", help="comma-separated generated bucket names")
    parser.add_argument("--json-out")
    parser.add_argument("--md-out")
    parser.add_argument("--keep-tmp", action="store_true")
    args = parser.parse_args()
    if args.cases < 1:
        raise ValueError("--cases must be positive")
    os.environ.setdefault("TMPDIR", default_tmp_dir())

    work = Path(tempfile.mkdtemp(prefix="squire-hot-api-fuzz-work.", dir=os.environ.get("TMPDIR") or default_tmp_dir()))
    squire = resolve_squire(args.squire_bin, work)
    lib = work / ("libsquire_runtime.dylib" if os.uname().sysname == "Darwin" else "libsquire_runtime.so")
    compile_hot_api(lib)
    repo = make_repo(args.seed)
    env = os.environ.copy()
    env["GIT_OPTIONAL_LOCKS"] = "0"
    os.environ["GIT_OPTIONAL_LOCKS"] = "0"
    run([str(squire), "setup"], repo, env=env)
    run([str(squire), "kernel", "warm", "--short"], repo, env=env, timeout=120)

    api = HotAPI(lib)
    codex_user_shell_regression = run_codex_user_shell_regression(api, repo, env)
    runtime_policy_regression = run_runtime_policy_regression(api, repo, env)
    selected_buckets = None
    if args.buckets:
        selected_buckets = [value.strip() for value in args.buckets.split(",") if value.strip()]
        if not selected_buckets:
            raise ValueError("--buckets requires at least one bucket")
    cases = generate_cases(args.seed, args.cases, selected_buckets)
    summary: dict[str, Any] = {"by_bucket": {}}
    mismatch_examples: list[dict[str, Any]] = []
    miss_examples: list[dict[str, Any]] = []
    hit_examples: list[dict[str, Any]] = []
    hot_us_all: list[float] = []
    hot_us_hits: list[float] = []
    hot_us_misses: list[float] = []
    native_us: list[float] = []
    native_hit_us: list[float] = []
    estimated_native_avoided_ms = 0.0
    measured_hot_hit_wall_ms = 0.0
    hot_hits = 0
    safe_misses = 0
    native_compared = 0
    mismatches = 0
    must_miss_hits = 0
    unsupported = 0
    eligible_misses = 0

    for case in cases:
        result = run_case(api, repo, case, env)
        hot = result["hot"]
        native = result["native"]
        bucket = record_bucket(summary, case)
        hot_us_all.append(hot["elapsed_us"])
        bucket["hot_us"].append(hot["elapsed_us"])
        if native is not None:
            native_compared += 1
            native_us.append(native["elapsed_us"])
            bucket["native_compared"] += 1
            bucket["native_us"].append(native["elapsed_us"])
        if hot["hit"]:
            hot_hits += 1
            bucket["hits"] += 1
            bucket["hit_us"].append(hot["elapsed_us"])
            hot_us_hits.append(hot["elapsed_us"])
            measured_hot_hit_wall_ms += hot["elapsed_us"] / 1000.0
            if native is not None:
                estimated_native_avoided_ms += native["elapsed_us"] / 1000.0
                native_hit_us.append(native["elapsed_us"])
                bucket["native_hit_us"].append(native["elapsed_us"])
            if len(hit_examples) < 20:
                hit_examples.append({"bucket": case.bucket, "argv": list(case.argv), "hot_us": round(hot["elapsed_us"], 3)})
        else:
            safe_misses += 1
            bucket["misses"] += 1
            bucket["miss_us"].append(hot["elapsed_us"])
            if hot.get("decision") == -1:
                unsupported += 1
                bucket["unsupported"] += 1
            else:
                eligible_misses += 1
                bucket["eligible_misses"] += 1
            hot_us_misses.append(hot["elapsed_us"])
            if len(miss_examples) < 40:
                miss_examples.append({"bucket": case.bucket, "argv": list(case.argv), "must_miss": case.must_miss})
        if result["mismatch"]:
            mismatches += 1
            bucket["mismatches"] += 1
            if result["mismatch"] == "must_miss_hit":
                must_miss_hits += 1
                bucket["must_miss_hits"] += 1
            if len(mismatch_examples) < 30:
                example: dict[str, Any] = {
                    "kind": result["mismatch"],
                    "bucket": case.bucket,
                    "argv": list(case.argv),
                    "hot_exit": hot.get("exit_code"),
                    "native_exit": native.get("exit_code") if native else None,
                    "hot_stdout_sha256": sha256_bytes(hot.get("stdout", b"")) if hot.get("hit") else None,
                    "native_stdout_sha256": sha256_bytes(native["stdout"]) if native else None,
                    "hot_stderr_sha256": sha256_bytes(hot.get("stderr", b"")) if hot.get("hit") else None,
                    "native_stderr_sha256": sha256_bytes(native["stderr"]) if native else None,
                }
                mismatch_examples.append(example)

    invalidation_probes = run_invalidation_probes(api, squire, repo, env)
    invalidation_safe = all(bool(probe.get("safe")) for probe in invalidation_probes)

    by_bucket = {}
    for bucket, data in summary["by_bucket"].items():
        by_bucket[bucket] = {
            key: value
            for key, value in data.items()
            if key not in {"native_us", "native_hit_us", "hot_us", "hit_us", "miss_us"}
        }
        by_bucket[bucket]["native_us"] = stats(data["native_us"])
        by_bucket[bucket]["native_hit_us"] = stats(data["native_hit_us"])
        by_bucket[bucket]["hot_us"] = stats(data["hot_us"])
        by_bucket[bucket]["hit_us"] = stats(data["hit_us"])
        by_bucket[bucket]["miss_us"] = stats(data["miss_us"])

    status = (
        "pass"
        if mismatches == 0
        and invalidation_safe
        and codex_user_shell_regression["safe"]
        and runtime_policy_regression["safe"]
        else "fail"
    )
    report = {
        "status": status,
        "seed": args.seed,
        "cases": len(cases),
        "repo": str(repo),
        "work": str(work),
        "library": str(lib),
        "warm_file_replay_enabled": True,
        "hot_hits": hot_hits,
        "safe_misses": safe_misses,
        "unsupported": unsupported,
        "eligible_misses": eligible_misses,
        "native_compared": native_compared,
        "mismatches": mismatches,
        "must_miss_hits": must_miss_hits,
        "estimated_native_avoided_ms": round(estimated_native_avoided_ms, 3),
        "measured_hot_hit_wall_ms": round(measured_hot_hit_wall_ms, 3),
        "net_hit_saved_ms": round(estimated_native_avoided_ms - measured_hot_hit_wall_ms, 3),
        "native_us": stats(native_us),
        "native_hit_us": stats(native_hit_us),
        "hot_us_all": stats(hot_us_all),
        "hot_us_hits": stats(hot_us_hits),
        "hot_us_misses": stats(hot_us_misses),
        "by_bucket": by_bucket,
        "invalidation_safe": invalidation_safe,
        "invalidation_probes": invalidation_probes,
        "codex_user_shell_regression": codex_user_shell_regression,
        "runtime_policy_regression": runtime_policy_regression,
        "hit_examples": hit_examples,
        "miss_examples": miss_examples,
        "mismatch_examples": mismatch_examples,
    }
    if args.json_out:
        Path(args.json_out).write_text(json.dumps(report, indent=2, sort_keys=True), encoding="utf-8")
    if args.md_out:
        Path(args.md_out).write_text(render_markdown(report), encoding="utf-8")
    print(json.dumps(report, indent=2, sort_keys=True))
    if not args.keep_tmp:
        shutil.rmtree(repo, ignore_errors=True)
        shutil.rmtree(work, ignore_errors=True)
    return 0 if status == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
