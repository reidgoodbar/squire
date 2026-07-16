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
from concurrent.futures import ThreadPoolExecutor
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
    for rel in ("src", "src/nested", "lib", "docs", "tests", "logs", "assets", "node_modules"):
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
    (repo / "docs" / "guide.rst").write_text(
        "Guide\n=====\n\n"
        + "\n".join(f"guide line {i} token_{rng.randrange(23)} marker_{rng.randrange(19)}" for i in range(1, 240))
        + "\n",
        encoding="utf-8",
    )
    (repo / "docs" / "current-file.md").write_text(
        "\n".join(f"current file line {i:03d} marker_xx" for i in range(1, 100)) + "\n",
        encoding="utf-8",
    )
    (repo / ".hidden-search.txt").write_text("hidden_generalized_marker\n", encoding="utf-8")
    (repo / "docs" / "crlf.txt").write_bytes(b"crlf_marker one\r\ncrlf_marker two\r\n")
    (repo / "docs" / "no-newline.txt").write_bytes(b"no_newline_marker")
    (repo / "docs" / "space name.md").write_text("space_name_marker\n", encoding="utf-8")
    (repo / "docs" / "unicode.txt").write_text(
        "unicode_marker caf\u00e9\n"
        "unicode_space_marker\u00a0value\n"
        "unicode_space_marker\u2003value\n",
        encoding="utf-8",
    )
    (repo / "assets" / "sample.bin").write_bytes(b"binary_marker before\n\x00binary_marker after\n")
    (repo / "node_modules" / "ignored.js").write_text("ignored_repo_marker\n", encoding="utf-8")
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
        "docs/guide.rst",
        "src/nested/config.json",
        "logs/sample.log",
    ]


def generate_cases(seed: int, count: int, selected_buckets: list[str] | None = None) -> list[Case]:
    rng = random.Random(seed)
    paths = path_cases()
    buckets = [
        "git_metadata",
        "repo_state",
        "git_history",
        "file_read",
        "line_window",
        "numbered_window",
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
                ("git", "diff", "--check"),
                ("git", "diff", "--", path),
            ]
            argv = rng.choice(repo_state_choices)
        elif bucket == "git_history":
            limit = rng.choice([1, 2, 3, 5, 10, 20])
            selected = rng.choice(
                [
                    "README.md",
                    "docs",
                    "src/module_00.js",
                    "missing-history-path.txt",
                ]
            )
            argv = rng.choice(
                [
                    ("git", "log", f"-{limit}", "--oneline", "--", selected),
                    ("git", "log", "--oneline", f"-{limit}", "--", selected),
                    ("git", "log", "-n", str(limit), "--oneline", "--", selected),
                    ("git", "log", f"--max-count={limit}", "--oneline", "--", selected),
                    (
                        "/bin/sh",
                        "-c",
                        f"git log -{limit} --oneline -- {selected} | head -n {min(limit, 5)}",
                    ),
                ]
            )
        elif bucket == "file_read":
            argv = rng.choice([("cat", path), ("file", path)])
        elif bucket == "line_window":
            n = str(rng.choice([1, 2, 3, 5, 10, 20, 40, 80, 160]))
            start = rng.randint(1, 180)
            end = start + rng.randint(0, 80)
            second_start = rng.randint(1, 180)
            second_end = second_start + rng.randint(0, 80)
            selection = f"{start},{end}p;{second_start},{second_end}p"
            argv = rng.choice(
                [
                    ("sed", "-n", f"{start},{end}p", path),
                    ("sed", "-n", f"{start}p", path),
                    ("sed", "-n", selection, path),
                    ("head", "-n", n, path),
                    ("head", f"-{n}", path),
                    ("head", path),
                    ("tail", "-n", n, path),
                    ("tail", f"-{n}", path),
                    ("tail", path),
                ]
            )
        elif bucket == "numbered_window":
            start = rng.randint(1, 180)
            end = start + rng.randint(0, 80)
            second_start = rng.randint(1, 180)
            second_end = second_start + rng.randint(0, 80)
            if rng.random() < 0.25:
                argv = ("nl", "-ba", path)
            elif rng.random() < 0.5:
                argv = (
                    "/bin/sh",
                    "-c",
                    f"nl -ba {path} | sed -n '{start},{end}p;{second_start},{second_end}p'",
                )
            else:
                argv = ("/bin/sh", "-c", f"nl -ba {path} | sed -n {start},{end}p")
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
            second_start = rng.randint(1, 140)
            second_end = second_start + rng.randint(0, 40)
            pattern = rng.choice(["token_1", "value", "marker", "missing_literal"])
            head = f"head -{n}" if rng.random() < 0.5 else f"head -n {n}"
            script = rng.choice(
                [
                    "git rev-parse HEAD | cat",
                    f"git status --short | {head}",
                    "git ls-files | grep -F src",
                    "git ls-files src | wc -l",
                    "git ls-files src | sort",
                    f"cat {p1} | grep -F {pattern} | {head}",
                    f"sed -n {start},{end}p {p2} | tail -n {n}",
                    f"sed -n '{start},{end}p;{second_start},{second_end}p' {p2} | tail -n {n}",
                    f"head -n {n} {p1} | tail -n {max(1, n // 2)}",
                    f"nl -ba {p1} | sed -n {start},{end}p",
                    f"nl -ba {p1} | sed -n '{start},{end}p;{second_start},{second_end}p'",
                ]
            )
            if shutil.which("rg") and rng.random() < 0.25:
                script = f"rg -F {pattern} {p1} | {head}"
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
                    ("nl", "-bt", path),
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
        cpu_start = time.thread_time_ns()
        decision = self.lib.squire_runtime_try_execute(
            str(cwd).encode(),
            len(argv_bytes),
            argv_arr,
            len(env_items),
            env_arr,
            ctypes.byref(result),
        )
        if decision != 1 or not result.handle:
            cpu_us = (time.thread_time_ns() - cpu_start) / 1000
            elapsed_us = (time.perf_counter_ns() - start) / 1000
            return {"hit": False, "decision": int(decision), "elapsed_us": elapsed_us, "cpu_us": cpu_us}
        stdout = bytes_from_ptr(result.stdout_data, result.stdout_len)
        stderr = bytes_from_ptr(result.stderr_data, result.stderr_len)
        exit_code = int(result.exit_code)
        native_wall_ms = int(result.native_wall_ms)
        self.lib.squire_runtime_record_hit(ctypes.byref(result))
        self.lib.squire_runtime_release(ctypes.byref(result))
        cpu_us = (time.thread_time_ns() - cpu_start) / 1000
        elapsed_us = (time.perf_counter_ns() - start) / 1000
        return {
            "hit": True,
            "stdout": stdout,
            "stderr": stderr,
            "exit_code": exit_code,
            "native_wall_ms": native_wall_ms,
            "elapsed_us": elapsed_us,
            "cpu_us": cpu_us,
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
        if not command_outputs_equivalent(case.argv, hot, native):
            mismatch = "byte_mismatch"
    return {"case": case, "hot": hot, "native": native, "mismatch": mismatch}


def command_outputs_equivalent(argv: tuple[str, ...], hot: dict[str, Any], native: dict[str, Any]) -> bool:
    if hot["exit_code"] != native["exit_code"] or hot["stderr"] != native["stderr"]:
        return False
    if hot["stdout"] == native["stdout"]:
        return True
    # ripgrep deliberately does not guarantee cross-file output order unless
    # --sort is requested. Preserve duplicate lines while ignoring only that
    # unspecified ordering; shell compositions remain byte-exact because
    # downstream filters can make order observable.
    if is_plain_unordered_rg_argv(argv):
        return Counter(hot["stdout"].splitlines(keepends=True)) == Counter(
            native["stdout"].splitlines(keepends=True)
        )
    return False


def is_plain_unordered_rg_argv(argv: tuple[str, ...]) -> bool:
    if not argv:
        return False
    if Path(argv[0]).name == "rg":
        tokens = list(argv)
    elif (
        len(argv) == 3
        and Path(argv[0]).name in {"sh", "bash", "zsh"}
        and argv[1] in {"-c", "-lc"}
        and "\n" not in argv[2]
        and "\r" not in argv[2]
    ):
        try:
            lexer = shlex.shlex(argv[2], posix=True, punctuation_chars="|&;<>")
            lexer.whitespace_split = True
            tokens = list(lexer)
        except ValueError:
            return False
        if len(tokens) >= 3 and tokens[-3:] == ["2", ">", "/dev/null"]:
            tokens = tokens[:-3]
    else:
        return False
    if not tokens or Path(tokens[0]).name != "rg":
        return False
    if any(token in {"|", "||", "&", "&&", ";", "<", ">", ">>"} for token in tokens):
        return False
    return "--sort" not in tokens and not any(
        token.startswith("--sort=") for token in tokens
    )


def replay_is_current(hot: dict[str, Any], native: dict[str, Any]) -> bool:
    return (not hot["hit"]) or (
        hot.get("exit_code") == native["exit_code"]
        and hot.get("stdout") == native["stdout"]
        and hot.get("stderr") == native["stderr"]
    )


def warm_short(squire: Path, repo: Path, env: dict[str, str]) -> None:
    run([str(squire), "runtime", "warm", "--short"], repo, env=env, timeout=120)


def maintain_once(squire: Path, repo: Path, env: dict[str, str]) -> dict[str, Any]:
    proc = run(
        [str(squire), "runtime", "maintain", "--once", "--json"],
        repo,
        env=env,
        timeout=120,
    )
    return json.loads(proc.stdout.decode("utf-8"))


def run_generalized_repo_search_probe(
    api: HotAPI,
    squire: Path,
    repo: Path,
    env: dict[str, str],
) -> dict[str, Any]:
    rg_available = shutil.which("rg") is not None
    cases = [
        Case(
            "repo_regex_roots",
            "generalized_repo_search",
            ("rg", "-n", "value_03_(10|11)|deep_config", "src", "lib"),
        ),
        Case(
            "repo_regex_smart_case",
            "generalized_repo_search",
            ("rg", "-n", "-S", "token_3|Hot API", ".", "--glob", "!logs/**"),
        ),
        Case(
            "repo_regex_ignore_case_glob",
            "generalized_repo_search",
            ("rg", "-n", "-i", "GUIDE|deep_config", "docs", "src", "--glob", "*.{rst,json}"),
        ),
        Case(
            "repo_fixed_glob",
            "generalized_repo_search",
            ("rg", "-n", "-F", "marker_7", "docs", "--glob", "*.md"),
        ),
        Case(
            "repo_hidden_guarded",
            "generalized_repo_search",
            ("rg", "-n", "--hidden", "hidden_generalized_marker", ".", "--glob", "!**/.git/**"),
        ),
        Case(
            "repo_hidden_guarded_root_anchored",
            "generalized_repo_search",
            ("rg", "-n", "--hidden", "hidden_generalized_marker", ".", "--glob", "!/.git/**"),
        ),
        Case(
            "repo_regex_unicode_space",
            "generalized_repo_search",
            ("rg", "-n", r"unicode_space_marker\s+value", "docs"),
        ),
        Case(
            "repo_no_match",
            "generalized_repo_search",
            ("rg", "-n", "generalized_absent_(alpha|omega)", "."),
        ),
        Case(
            "repo_composed_head",
            "generalized_repo_search",
            ("sh", "-c", "rg -n 'token_(3|7)' docs --glob '*.md' | head -n 6"),
        ),
        Case(
            "repo_composed_short_head",
            "generalized_repo_search",
            ("sh", "-c", "rg -n 'token_(3|7)' docs --glob '*.md' | head -6"),
        ),
        Case(
            "repo_composed_pwd",
            "generalized_repo_search",
            ("sh", "-c", "pwd && rg -n 'token_(3|7)' docs --glob '*.md' | head -6"),
        ),
        Case(
            "repo_composed_escaped_quote",
            "generalized_repo_search",
            ("sh", "-c", 'rg -n "marker_0[1-9]|arg\\(\\"[A-Z]" docs --glob \'*.md\' | head -8'),
        ),
        Case(
            "repo_missing_paths_stderr_discarded",
            "generalized_repo_search",
            (
                "sh",
                "-c",
                'rg -n "token_1|marker" README.md missing-one docs missing-two 2>/dev/null',
            ),
        ),
        Case(
            "repo_overlapping_explicit_paths",
            "generalized_repo_search",
            ("rg", "-n", "token_3|marker_7", "docs", "docs/notes.md"),
        ),
        Case(
            "repo_shell_path_glob",
            "generalized_repo_search",
            ("sh", "-c", 'rg -n "token_3|marker_7" docs/*.md'),
        ),
        Case(
            "repo_composed_sequence",
            "generalized_repo_search",
            (
                "sh",
                "-c",
                "sed -n '1,4p' README.md; rg -n 'value_02_(10|11)' src --glob '*.js'",
            ),
        ),
    ]
    results = [run_case(api, repo, case, env) for case in cases]
    target = repo / "src" / "module_03.js"
    original = target.read_bytes()
    before = target.stat()
    target.write_bytes(original + b"export const generalized_mutation = 1;\n")
    os.utime(target, ns=(before.st_atime_ns, before.st_mtime_ns))
    stale = api.try_replay(repo, cases[0].argv, env)
    target.write_bytes(original)
    os.utime(target, ns=(before.st_atime_ns, before.st_mtime_ns))
    warm_short(squire, repo, env)
    if rg_available:
        safe = all(item["hot"]["hit"] and item["mismatch"] is None for item in results) and not stale["hit"]
    else:
        safe = all(not item["hot"]["hit"] and item["mismatch"] is None for item in results) and not stale["hit"]
    return {
        "safe": safe,
        "rg_available": rg_available,
        "cases": [
            {
                "name": item["case"].name,
                "argv": list(item["case"].argv),
                "hit": bool(item["hot"]["hit"]),
                "exact": item["mismatch"] is None,
                "byte_exact": bool(
                    item["hot"]["hit"]
                    and item["native"] is not None
                    and item["hot"]["exit_code"] == item["native"]["exit_code"]
                    and item["hot"]["stdout"] == item["native"]["stdout"]
                    and item["hot"]["stderr"] == item["native"]["stderr"]
                ),
                "hot_us": round(item["hot"]["elapsed_us"], 3),
                "native_us": round(item["native"]["elapsed_us"], 3) if item["native"] else None,
            }
            for item in results
        ],
        "same_mtime_mutation_missed": not stale["hit"],
    }


def generate_repo_search_fuzz_cases(seed: int, count: int) -> list[Case]:
    rng = random.Random(seed ^ 0x524750)
    patterns = [
        ("token_3", True, ()),
        ("marker_7", True, ()),
        ("crlf_marker", True, ()),
        ("no_newline_marker", True, ()),
        ("space_name_marker", True, ()),
        ("unicode_marker", True, ()),
        ("ignored_repo_marker", True, ()),
        ("binary_marker", True, ()),
        ("token_(3|7)", False, ()),
        ("marker_[0-9]+", False, ()),
        ("^export const value_[0-9]+_[0-9]+", False, ()),
        ("deep_config|nested", False, ()),
        ("GUIDE|deep_config", False, ("-i",)),
        ("token_3|Hot API", False, ("-S",)),
        ("return [0-9]+$", False, ()),
        ("generalized_absent_(alpha|omega)", False, ()),
        ("value_0[0-9]_(10|11)", False, ()),
        (r"value_03_10; // token_", False, ()),
    ]
    roots = [
        ((), ()),
        ((".",), ()),
        (("src",), ("!logs/**",)),
        (("lib",), ("*.py",)),
        (("docs",), ("*.md",)),
        (("docs", "src"), ("*.{md,rst,json,js,txt}",)),
        (("src", "lib"), ("!*.pyc",)),
        (("README.md",), ()),
        (("docs/crlf.txt",), ()),
        (("docs/no-newline.txt",), ()),
        (("docs/space name.md",), ()),
        (("src/nested/config.json",), ()),
    ]
    cases: list[Case] = []
    for index in range(count):
        pattern, fixed, pattern_flags = rng.choice(patterns)
        paths, globs = rng.choice(roots)
        argv = ["rg", "-n"]
        if fixed:
            argv.append("-F")
        argv.extend(pattern_flags)
        output_mode = rng.randrange(12)
        if output_mode == 0:
            argv.append("-q")
        elif output_mode == 1:
            argv.append("-l")
        elif output_mode == 2:
            argv.append("--with-filename")
        elif output_mode == 3:
            argv.append("--no-filename")
        argv.append(pattern)
        argv.extend(paths)
        for glob in globs:
            argv.extend(("--glob", glob))
        cases.append(
            Case(
                name=f"repo_search_fuzz_{index:04d}",
                bucket="repo_search_fuzz",
                argv=tuple(argv),
            )
        )

    hidden = Case(
        name="repo_search_fuzz_hidden",
        bucket="repo_search_fuzz",
        argv=("rg", "-n", "--hidden", "hidden_generalized_marker", ".", "--glob", "!**/.git/**"),
    )
    cases[0] = hidden
    return cases


def run_repo_search_differential_fuzz(
    api: HotAPI,
    repo: Path,
    env: dict[str, str],
    seed: int,
    count: int,
) -> dict[str, Any]:
    rg_available = shutil.which("rg") is not None
    cases = generate_repo_search_fuzz_cases(seed, count)
    results = [run_case(api, repo, case, env) for case in cases]
    hit_us = [item["hot"]["elapsed_us"] for item in results if item["hot"]["hit"]]
    native_us = [item["native"]["elapsed_us"] for item in results if item["native"] is not None]
    byte_exact = sum(
        item["hot"]["hit"]
        and item["native"] is not None
        and item["hot"]["exit_code"] == item["native"]["exit_code"]
        and item["hot"]["stdout"] == item["native"]["stdout"]
        and item["hot"]["stderr"] == item["native"]["stderr"]
        for item in results
    )
    mismatches = [item for item in results if item["mismatch"] is not None]
    misses = [item for item in results if not item["hot"]["hit"]]
    safe = not mismatches and (
        (rg_available and not misses)
        or (not rg_available and len(misses) == len(results))
    )
    return {
        "safe": safe,
        "rg_available": rg_available,
        "expected_decision": "hit" if rg_available else "safe_miss",
        "cases": len(results),
        "hits": len(results) - len(misses),
        "byte_exact": byte_exact,
        "semantic_mismatches": len(mismatches),
        "misses": len(misses),
        "hit_us": stats(hit_us),
        "native_us": stats(native_us),
        "mismatch_examples": [
            {
                "argv": list(item["case"].argv),
                "hot_exit": item["hot"].get("exit_code"),
                "native_exit": item["native"].get("exit_code") if item["native"] else None,
                "hot_stdout_sha256": sha256_bytes(item["hot"].get("stdout", b"")),
                "native_stdout_sha256": sha256_bytes(item["native"]["stdout"]) if item["native"] else None,
            }
            for item in mismatches[:20]
        ],
        "miss_examples": [list(item["case"].argv) for item in misses[:20]],
    }


def run_missing_search_tool_regression(
    api: HotAPI,
    repo: Path,
    env: dict[str, str],
    work: Path,
) -> dict[str, Any]:
    """A supported command must still miss when its native tool is absent."""
    empty_path = work / "empty-path"
    empty_path.mkdir(exist_ok=True)
    tool_keys = {
        "PATH",
        "SQUIRE_SHIM_REAL_PATH",
        "SQUIRE_REAL_RG",
        "SQUIRE_REAL_RG_PATH_HASH",
        "SQUIRE_REAL_RG_FILE_HASH",
        "SQUIRE_REAL_RG_STAT_SIGNAL",
    }
    saved = {key: os.environ.get(key) for key in tool_keys}
    try:
        for key in tool_keys:
            os.environ.pop(key, None)
        os.environ["PATH"] = str(empty_path)
        missing_env = dict(env)
        for key in tool_keys:
            missing_env.pop(key, None)
        missing_env["PATH"] = str(empty_path)
        commands = [
            ("rg", "-n", "missing_tool_probe", "."),
            ("rg", "-F", "token_3", "README.md"),
            ("sh", "-c", "rg -n missing_tool_probe . | head -n 1"),
        ]
        probes = []
        for argv in commands:
            hot = api.try_replay(repo, argv, missing_env)
            probes.append(
                {
                    "argv": list(argv),
                    "hit": bool(hot["hit"]),
                    "decision": hot.get("decision"),
                    "passed": not hot["hit"] and hot.get("decision") == 0,
                }
            )
        return {
            "safe": shutil.which("rg", path=str(empty_path)) is None
            and all(probe["passed"] for probe in probes),
            "probes": probes,
        }
    finally:
        for key in tool_keys:
            os.environ.pop(key, None)
        for key, value in saved.items():
            if value is not None:
                os.environ[key] = value


def run_git_history_regression(
    api: HotAPI,
    squire: Path,
    repo: Path,
    env: dict[str, str],
) -> dict[str, Any]:
    argv = ("git", "log", "-5", "--oneline", "--", "README.md", "docs")
    baseline = run_case(api, repo, Case("git_history_baseline", "git_history", argv), env)
    probes: list[dict[str, Any]] = [
        {
            "name": "baseline",
            "hit": bool(baseline["hot"]["hit"]),
            "exact": baseline["mismatch"] is None,
            "hot_us": round(baseline["hot"]["elapsed_us"], 3),
            "native_us": round(baseline["native"]["elapsed_us"], 3),
        }
    ]

    pathspec_env = dict(env)
    pathspec_env["GIT_ICASE_PATHSPECS"] = "1"
    pathspec_override = api.try_replay(repo, argv, pathspec_env)
    probes.append(
        {
            "name": "child_pathspec_environment",
            "hit": bool(pathspec_override["hit"]),
            "safe": not pathspec_override["hit"],
        }
    )

    payload = repo.parent / f"{repo.name}.unreachable-object"
    payload.write_text("object outside every ref\n", encoding="utf-8")
    run(["git", "hash-object", "-w", str(payload)], repo, env=env)
    loose_object = api.try_replay(repo, argv, env)
    probes.append(
        {
            "name": "unreachable_loose_object_namespace",
            "hit": bool(loose_object["hit"]),
            "safe": not loose_object["hit"],
        }
    )
    maintain_once(squire, repo, env)
    loose_rewarm = run_case(api, repo, Case("git_history_loose_rewarm", "git_history", argv), env)
    probes[-1]["rewarm_hit"] = bool(loose_rewarm["hot"]["hit"])
    probes[-1]["rewarm_exact"] = loose_rewarm["mismatch"] is None

    run(["git", "gc", "--quiet"], repo, env=env)
    packed_object = api.try_replay(repo, argv, env)
    probes.append(
        {
            "name": "packed_object_namespace",
            "hit": bool(packed_object["hit"]),
            "safe": not packed_object["hit"],
        }
    )
    maintain_once(squire, repo, env)
    packed_rewarm = run_case(api, repo, Case("git_history_packed_rewarm", "git_history", argv), env)
    probes[-1]["rewarm_hit"] = bool(packed_rewarm["hot"]["hit"])
    probes[-1]["rewarm_exact"] = packed_rewarm["mismatch"] is None

    run(["git", "update-ref", "refs/tags/squire-history-fuzz", "HEAD"], repo, env=env)
    loose_ref = api.try_replay(repo, argv, env)
    probes.append(
        {
            "name": "loose_ref_namespace",
            "hit": bool(loose_ref["hit"]),
            "safe": not loose_ref["hit"],
        }
    )
    run(["git", "update-ref", "-d", "refs/tags/squire-history-fuzz"], repo, env=env)
    maintain_once(squire, repo, env)
    ref_rewarm = run_case(api, repo, Case("git_history_ref_rewarm", "git_history", argv), env)
    probes[-1]["rewarm_hit"] = bool(ref_rewarm["hot"]["hit"])
    probes[-1]["rewarm_exact"] = ref_rewarm["mismatch"] is None

    old_abbrev = run(["git", "config", "--local", "--get", "core.abbrev"], repo, env=env, check=False)
    run(["git", "config", "--local", "core.abbrev", "12"], repo, env=env)
    config = api.try_replay(repo, argv, env)
    probes.append(
        {
            "name": "core_abbrev_config",
            "hit": bool(config["hit"]),
            "safe": not config["hit"],
        }
    )
    if old_abbrev.returncode == 0:
        run(
            ["git", "config", "--local", "core.abbrev", old_abbrev.stdout.decode().strip()],
            repo,
            env=env,
        )
    else:
        run(["git", "config", "--local", "--unset", "core.abbrev"], repo, env=env, check=False)
    maintain_once(squire, repo, env)
    config_rewarm = run_case(api, repo, Case("git_history_config_rewarm", "git_history", argv), env)
    probes[-1]["rewarm_hit"] = bool(config_rewarm["hot"]["hit"])
    probes[-1]["rewarm_exact"] = config_rewarm["mismatch"] is None

    merge_repo = Path(tempfile.mkdtemp(prefix="squire-history-merge.", dir=os.environ.get("TMPDIR") or default_tmp_dir()))
    try:
        run(["git", "init", "-b", "main"], merge_repo, env=env)
        run(["git", "config", "user.email", "history@example.com"], merge_repo, env=env)
        run(["git", "config", "user.name", "Squire History Fuzz"], merge_repo, env=env)
        (merge_repo / "base.txt").write_text("base\n", encoding="utf-8")
        run(["git", "add", "base.txt"], merge_repo, env=env)
        run(["git", "commit", "-m", "base"], merge_repo, env=env)
        run(["git", "checkout", "-b", "feature"], merge_repo, env=env)
        (merge_repo / "feature.txt").write_text("feature\n", encoding="utf-8")
        run(["git", "add", "feature.txt"], merge_repo, env=env)
        run(["git", "commit", "-m", "feature"], merge_repo, env=env)
        run(["git", "checkout", "main"], merge_repo, env=env)
        (merge_repo / "main.txt").write_text("main\n", encoding="utf-8")
        run(["git", "add", "main.txt"], merge_repo, env=env)
        run(["git", "commit", "-m", "main"], merge_repo, env=env)
        run(["git", "merge", "--no-ff", "feature", "-m", "merge feature"], merge_repo, env=env)
        run([str(squire), "setup"], merge_repo, env=env)
        warm_short(squire, merge_repo, env)
        merge_argv = ("git", "log", "-5", "--oneline", "--", ".")
        merge_result = run_case(
            api,
            merge_repo,
            Case("git_history_merge_must_miss", "git_history", merge_argv, must_miss=True),
            env,
        )
        probes.append(
            {
                "name": "merge_topology_declines",
                "hit": bool(merge_result["hot"]["hit"]),
                "safe": not merge_result["hot"]["hit"],
                "native_exit": merge_result["native"]["exit_code"],
            }
        )
    finally:
        shutil.rmtree(merge_repo, ignore_errors=True)
        payload.unlink(missing_ok=True)

    safe = bool(baseline["hot"]["hit"] and baseline["mismatch"] is None)
    for probe in probes[1:]:
        safe = safe and bool(probe.get("safe", True))
        if "rewarm_hit" in probe:
            safe = safe and bool(probe["rewarm_hit"] and probe["rewarm_exact"])
    return {"safe": safe, "argv": list(argv), "probes": probes}


def run_demand_preparation_probe(
    api: HotAPI,
    squire: Path,
    repo: Path,
    env: dict[str, str],
) -> dict[str, Any]:
    target = repo / "docs" / "demand-preparation.txt"
    target.write_text("demand_probe_alpha\n", encoding="utf-8")
    argv = ("rg", "-n", "demand_probe_(alpha|omega)", ".", "--glob", "!ignored/**")
    shell_argv = (
        "/bin/sh",
        "-c",
        "rg -n \"demand_probe_(alpha|omega)\" . --glob '!ignored/**' | head -n 1",
    )
    first = api.try_replay(repo, argv, env)
    shell_first = api.try_replay(repo, shell_argv, env)
    readme = repo / "README.md"
    readme.write_text(readme.read_text(encoding="utf-8") + "demand preparation diff\n", encoding="utf-8")
    additional_argvs = [
        ("file", "logs/sample.log"),
        ("git", "diff", "--", "README.md"),
        ("ls", "-p", "docs"),
    ]
    additional_first = {command: api.try_replay(repo, command, env) for command in additional_argvs}
    first_maintain = maintain_once(squire, repo, env)
    reference = native_reference(repo, argv, env)
    prepared = api.try_replay(repo, argv, env)
    shell_reference = native_reference(repo, shell_argv, env)
    shell_prepared = api.try_replay(repo, shell_argv, env)
    previous_no_color = os.environ.get("NO_COLOR")
    changed_env = dict(env)
    changed_env["NO_COLOR"] = "squire-proof-change"
    os.environ["NO_COLOR"] = changed_env["NO_COLOR"]
    try:
        environment_stale = api.try_replay(repo, argv, changed_env)
    finally:
        if previous_no_color is None:
            os.environ.pop("NO_COLOR", None)
        else:
            os.environ["NO_COLOR"] = previous_no_color
    additional_results: list[dict[str, Any]] = []
    for command in additional_argvs:
        command_reference = native_reference(repo, command, env)
        command_prepared = api.try_replay(repo, command, env)
        additional_results.append(
            {
                "argv": list(command),
                "initial_miss": not additional_first[command]["hit"],
                "prepared_hit": bool(command_prepared["hit"]),
                "prepared_exact": replay_is_current(command_prepared, command_reference),
            }
        )

    target_stat = target.stat()
    target.write_text("demand_probe_omega\n", encoding="utf-8")
    os.utime(target, ns=(target_stat.st_atime_ns, target_stat.st_mtime_ns))
    stale = api.try_replay(repo, argv, env)
    shell_stale = api.try_replay(repo, shell_argv, env)
    second_maintain = maintain_once(squire, repo, env)
    changed_reference = native_reference(repo, argv, env)
    refreshed = api.try_replay(repo, argv, env)
    shell_changed_reference = native_reference(repo, shell_argv, env)
    shell_refreshed = api.try_replay(repo, shell_argv, env)

    missing_argv = ("rg", "-n", "demand_probe_(missing)", ".")
    missing_first = api.try_replay(repo, missing_argv, env)
    missing_maintain = maintain_once(squire, repo, env)
    missing_reference = native_reference(repo, missing_argv, env)
    missing_prepared = api.try_replay(repo, missing_argv, env)
    missing_initial_current = replay_is_current(missing_first, missing_reference)

    safe = (
        not first["hit"]
        and not shell_first["hit"]
        and prepared["hit"]
        and replay_is_current(prepared, reference)
        and shell_prepared["hit"]
        and replay_is_current(shell_prepared, shell_reference)
        and not environment_stale["hit"]
        and not stale["hit"]
        and not shell_stale["hit"]
        and refreshed["hit"]
        and replay_is_current(refreshed, changed_reference)
        and shell_refreshed["hit"]
        and replay_is_current(shell_refreshed, shell_changed_reference)
        and missing_initial_current
        and missing_prepared["hit"]
        and missing_prepared.get("exit_code") == 1
        and replay_is_current(missing_prepared, missing_reference)
        and all(
            item["initial_miss"] and item["prepared_hit"] and item["prepared_exact"]
            for item in additional_results
        )
    )
    return {
        "safe": safe,
        "initial_miss": not first["hit"],
        "prepared_hit": bool(prepared["hit"]),
        "prepared_exact": replay_is_current(prepared, reference),
        "shell_initial_miss": not shell_first["hit"],
        "shell_prepared_hit": bool(shell_prepared["hit"]),
        "shell_prepared_exact": replay_is_current(shell_prepared, shell_reference),
        "environment_change_miss": not environment_stale["hit"],
        "mutation_miss": not stale["hit"],
        "shell_mutation_miss": not shell_stale["hit"],
        "refreshed_hit": bool(refreshed["hit"]),
        "refreshed_exact": replay_is_current(refreshed, changed_reference),
        "shell_refreshed_hit": bool(shell_refreshed["hit"]),
        "shell_refreshed_exact": replay_is_current(shell_refreshed, shell_changed_reference),
        "no_match_initial_hit": bool(missing_first["hit"]),
        "no_match_initial_current": missing_initial_current,
        "no_match_prepared_hit": bool(missing_prepared["hit"]),
        "no_match_exit_code": missing_prepared.get("exit_code"),
        "additional_operations": additional_results,
        "first_maintainer": first_maintain,
        "second_maintainer": second_maintain,
        "no_match_maintainer": missing_maintain,
    }


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
    maintain_once(squire, repo, env)
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

    numbered_case = Case(
        "invalidation_numbered_window_before",
        "invalidation",
        ("/bin/sh", "-c", "nl -ba src/module_02.js | sed -n 20,45p"),
    )
    numbered_before = run_case(api, repo, numbered_case, env)
    numbered_target = repo / "src" / "module_02.js"
    numbered_target.write_text(
        numbered_target.read_text(encoding="utf-8") + "export const numbered_invalidated = true;\n",
        encoding="utf-8",
    )
    append_invalidation_probe(
        probes,
        api,
        repo,
        env,
        name="numbered_window_file_epoch",
        argv=numbered_case.argv,
        before_hit=numbered_before["hot"]["hit"],
    )
    rewarm_probe(probes, api, squire, repo, env, numbered_case.argv)

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

    diff_check_case = Case("invalidation_diff_check_before", "invalidation", ("git", "diff", "--check"))
    diff_check_before = run_case(api, repo, diff_check_case, env)
    diff_check_target = repo / "docs" / "notes.md"
    diff_check_original = diff_check_target.read_bytes()
    diff_check_target.write_bytes(diff_check_original + b"whitespace error  \n")
    append_invalidation_probe(
        probes,
        api,
        repo,
        env,
        name="git_diff_check_workspace_epoch",
        argv=diff_check_case.argv,
        before_hit=diff_check_before["hot"]["hit"],
    )
    diff_check_target.write_bytes(diff_check_original)
    maintain_once(squire, repo, env)
    diff_check_restored = run_case(api, repo, Case("invalidation_diff_check_restored", "invalidation", diff_check_case.argv), env)
    probes[-1]["restored_hit"] = diff_check_restored["hot"]["hit"]
    probes[-1]["restored_exact"] = diff_check_restored["mismatch"] is None and diff_check_restored["hot"]["hit"]
    probes[-1]["safe"] = bool(probes[-1]["safe"] and probes[-1]["restored_exact"])

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


def run_current_file_execution_probes(api: HotAPI, repo: Path, env: dict[str, str]) -> dict[str, Any]:
    target = repo / "docs" / "current-file.md"
    original_stat = target.stat()
    commands: list[tuple[str, ...]] = [
        ("cat", "docs/current-file.md"),
        ("sed", "-n", "12,34p", "docs/current-file.md"),
        ("sed", "-n", "1,8p;20,28p;24,31p", "docs/current-file.md"),
        ("head", "-n", "17", "docs/current-file.md"),
        ("tail", "-n", "13", "docs/current-file.md"),
        ("grep", "-F", "marker_04", "docs/current-file.md"),
        ("grep", "-q", "-F", "marker_05", "docs/current-file.md"),
        ("nl", "-ba", "docs/current-file.md"),
        ("sh", "-c", "cat docs/current-file.md | head -n 9"),
        ("sh", "-c", "sed -n 10,40p docs/current-file.md | tail -n 7"),
        ("sh", "-c", "sed -n '1,12p;20,30p' docs/current-file.md | tail -n 9"),
        ("sh", "-c", "nl -ba docs/current-file.md | sed -n 22,31p"),
        ("sh", "-c", "nl -ba docs/current-file.md | sed -n '2,8p;20,27p;24p'"),
    ]
    if shutil.which("rg"):
        commands.insert(6, ("rg", "-n", "-F", "marker_06", "docs/current-file.md"))

    probes: list[dict[str, Any]] = []
    hot_values: list[float] = []
    native_values: list[float] = []
    safe = True
    for index, argv in enumerate(commands):
        marker = f"marker_{index:02d}"
        payload = "\n".join(f"current file line {line:03d} {marker}" for line in range(1, 100)) + "\n"
        if argv[:2] == ("nl", "-ba"):
            payload = payload.removesuffix("\n")
        target.write_text(payload, encoding="utf-8")
        os.utime(target, ns=(original_stat.st_atime_ns, original_stat.st_mtime_ns))
        result = run_case(api, repo, Case(f"current_file_{index}", "current_file", argv), env)
        passed = bool(result["hot"]["hit"]) and result["mismatch"] is None
        safe = safe and passed
        hot_values.append(result["hot"]["elapsed_us"])
        if result["native"] is not None:
            native_values.append(result["native"]["elapsed_us"])
        probes.append(
            {
                "argv": list(argv),
                "hit": bool(result["hot"]["hit"]),
                "decision": result["hot"].get("decision"),
                "exact": result["mismatch"] is None,
                "hot_us": round(result["hot"]["elapsed_us"], 3),
                "native_us": round(result["native"]["elapsed_us"], 3) if result["native"] is not None else None,
            }
        )

    log_target = repo / "logs" / "sample.log"
    log_stat = log_target.stat()
    log_target.write_text(
        "\n".join(f"updated log line {line:03d} direct_log_marker" for line in range(1, 180)) + "\n",
        encoding="utf-8",
    )
    os.utime(log_target, ns=(log_stat.st_atime_ns, log_stat.st_mtime_ns))
    log_argv = ("tail", "-n", "12", "logs/sample.log")
    log_result = run_case(api, repo, Case("current_log_file", "current_file", log_argv), env)
    log_passed = bool(log_result["hot"]["hit"]) and log_result["mismatch"] is None
    safe = safe and log_passed
    hot_values.append(log_result["hot"]["elapsed_us"])
    if log_result["native"] is not None:
        native_values.append(log_result["native"]["elapsed_us"])
    probes.append(
        {
            "argv": list(log_argv),
            "hit": bool(log_result["hot"]["hit"]),
            "decision": log_result["hot"].get("decision"),
            "exact": log_result["mismatch"] is None,
            "hot_us": round(log_result["hot"]["elapsed_us"], 3),
            "native_us": round(log_result["native"]["elapsed_us"], 3) if log_result["native"] is not None else None,
        }
    )
    return {"safe": safe, "hot_us": stats(hot_values), "native_us": stats(native_values), "probes": probes}


def run_parallel_replay_probe(
    api: HotAPI,
    squire: Path,
    repo: Path,
    env: dict[str, str],
    workers: int,
    iterations: int,
) -> dict[str, Any]:
    commands: list[tuple[str, ...]] = [
        ("git", "rev-parse", "HEAD"),
        ("git", "status", "--short"),
        ("git", "ls-files"),
        ("git", "diff", "--check"),
        ("git", "log", "-5", "--oneline", "--", "README.md", "docs"),
        ("cat", "README.md"),
        ("sed", "-n", "25,75p", "README.md"),
        ("sed", "-n", "1,12p;20,35p;30,45p", "README.md"),
        ("sh", "-c", "git ls-files | grep -F src | head -n 8"),
        ("sh", "-c", "sed -n 1,120p README.md | tail -n 12"),
        ("sh", "-c", "nl -ba README.md | sed -n '1,8p;20,28p;24p'"),
    ]
    warm_short(squire, repo, env)
    references = {command: native_reference(repo, command, env) for command in commands}
    primed: list[dict[str, Any]] = []
    for command in commands:
        result = api.try_replay(repo, command, env)
        reference = references[command]
        primed.append(
            {
                "argv": list(command),
                "hit": bool(result["hit"]),
                "exact": replay_is_current(result, reference),
                "hot_us": round(result["elapsed_us"], 3),
                "cpu_us": round(result["cpu_us"], 3),
            }
        )

    def exercise(worker: int) -> list[tuple[tuple[str, ...], dict[str, Any], bool]]:
        rows: list[tuple[tuple[str, ...], dict[str, Any], bool]] = []
        for index in range(iterations):
            command = commands[(worker + index) % len(commands)]
            result = api.try_replay(repo, command, env)
            rows.append((command, result, replay_is_current(result, references[command])))
        return rows

    rows: list[tuple[tuple[str, ...], dict[str, Any], bool]] = []
    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="squire-hot-api") as pool:
        for batch in pool.map(exercise, range(workers)):
            rows.extend(batch)

    by_command: dict[str, dict[str, Any]] = {}
    all_hit_us: list[float] = []
    all_hit_cpu_us: list[float] = []
    hits = 0
    exact = 0
    for command, result, current in rows:
        label = shlex.join(command)
        item = by_command.setdefault(label, {"calls": 0, "hits": 0, "exact": 0, "hit_us": [], "hit_cpu_us": []})
        item["calls"] += 1
        if result["hit"]:
            hits += 1
            item["hits"] += 1
            item["hit_us"].append(result["elapsed_us"])
            item["hit_cpu_us"].append(result["cpu_us"])
            all_hit_us.append(result["elapsed_us"])
            all_hit_cpu_us.append(result["cpu_us"])
        if current:
            exact += 1
            item["exact"] += 1
    for item in by_command.values():
        item["hit_us"] = stats(item["hit_us"])
        item["hit_cpu_us"] = stats(item["hit_cpu_us"])

    distribution = stats(all_hit_us)
    cpu_distribution = stats(all_hit_cpu_us)
    total = len(rows)
    safe = total > 0 and hits == total and exact == total and all(
        item["hit"] and item["exact"] for item in primed
    )
    return {
        "safe": safe,
        "workers": workers,
        "iterations_per_worker": iterations,
        "calls": total,
        "hits": hits,
        "exact": exact,
        "sub_ms_p99": bool(distribution.get("p99", float("inf")) < 1000),
        "sub_ms_cpu_p99": bool(cpu_distribution.get("p99", float("inf")) < 1000),
        "hit_us": distribution,
        "hit_cpu_us": cpu_distribution,
        "primed": primed,
        "by_command": by_command,
    }


def run_steady_state_replay_probe(
    api: HotAPI,
    squire: Path,
    repo: Path,
    env: dict[str, str],
    iterations: int,
) -> dict[str, Any]:
    commands: list[tuple[str, ...]] = [
        ("git", "rev-parse", "HEAD"),
        ("git", "rev-parse", "--abbrev-ref", "HEAD"),
        ("git", "status", "--short"),
        ("git", "ls-files"),
        ("git", "diff", "--check"),
        ("git", "log", "-1", "--format=%H%n%s"),
        ("git", "log", "-5", "--oneline", "--", "README.md", "docs"),
        ("git", "--version"),
        ("which", "python3"),
        ("whoami",),
        ("hostname",),
        ("id",),
        ("uname", "-m"),
        ("printenv", "PATH"),
        ("ls",),
        ("ls", "-p", "src"),
        ("file", "README.md"),
        ("cat", "README.md"),
        ("head", "-n", "20", "README.md"),
        ("tail", "-n", "20", "README.md"),
        ("sed", "-n", "25,75p", "README.md"),
        ("sed", "-n", "1,12p;20,35p;30,45p", "README.md"),
        ("grep", "-F", "token_1", "README.md"),
        ("sh", "-c", "git status --short | head -n 5"),
        ("sh", "-c", "git ls-files | grep -F src | head -n 8"),
        ("sh", "-c", "sed -n 1,120p README.md | tail -n 12"),
        ("sh", "-c", "sed -n '1,20p;40,55p' README.md | tail -n 10"),
        ("sh", "-c", "nl -ba README.md | sed -n '1,8p;20,28p;24p'"),
        (
            "sh",
            "-c",
            "git rev-parse HEAD >/dev/null && git status --short >/dev/null && cat README.md >/dev/null",
        ),
    ]
    if shutil.which("rg"):
        commands.extend(
            [
                ("rg", "--version"),
                ("rg", "-n", "-F", "token_1", "README.md"),
                ("rg", "-n", "demand_probe_(alpha|omega)", ".", "--glob", "!ignored/**"),
                (
                    "sh",
                    "-c",
                    "rg -n \"demand_probe_(alpha|omega)\" . --glob '!ignored/**' | head -n 1",
                ),
            ]
        )

    references = {command: native_reference(repo, command, env) for command in commands}
    warm_short(squire, repo, env)
    primed: list[dict[str, Any]] = []
    for command in commands:
        result = api.try_replay(repo, command, env)
        primed.append(
            {
                "argv": list(command),
                "hit": bool(result["hit"]),
                "exact": replay_is_current(result, references[command]),
                "elapsed_us": round(result["elapsed_us"], 3),
                "cpu_us": round(result["cpu_us"], 3),
            }
        )

    by_command: dict[str, dict[str, Any]] = {}
    all_hit_us: list[float] = []
    all_hit_cpu_us: list[float] = []
    hits = 0
    exact = 0
    for index in range(iterations):
        command = commands[index % len(commands)]
        result = api.try_replay(repo, command, env)
        current = replay_is_current(result, references[command])
        label = shlex.join(command)
        item = by_command.setdefault(label, {"calls": 0, "hits": 0, "exact": 0, "hit_us": [], "hit_cpu_us": []})
        item["calls"] += 1
        if result["hit"]:
            hits += 1
            item["hits"] += 1
            item["hit_us"].append(result["elapsed_us"])
            item["hit_cpu_us"].append(result["cpu_us"])
            all_hit_us.append(result["elapsed_us"])
            all_hit_cpu_us.append(result["cpu_us"])
        if current:
            exact += 1
            item["exact"] += 1
    for item in by_command.values():
        item["hit_us"] = stats(item["hit_us"])
        item["hit_cpu_us"] = stats(item["hit_cpu_us"])
    distribution = stats(all_hit_us)
    cpu_distribution = stats(all_hit_cpu_us)
    safe = (
        hits == iterations
        and exact == iterations
        and all(item["hit"] and item["exact"] for item in primed)
    )
    return {
        "safe": safe,
        "iterations": iterations,
        "commands": len(commands),
        "hits": hits,
        "exact": exact,
        "sub_ms_p99": bool(distribution.get("p99", float("inf")) < 1000),
        "sub_ms_cpu_p99": bool(cpu_distribution.get("p99", float("inf")) < 1000),
        "hit_us": distribution,
        "hit_cpu_us": cpu_distribution,
        "primed": primed,
        "by_command": by_command,
    }


def run_codex_user_shell_regression(
    api: HotAPI,
    squire: Path,
    repo: Path,
    env: dict[str, str],
) -> dict[str, Any]:
    """Cover the real squire-codex typed-command path shape.

    Codex user-shell commands arrive at the hot API as `sh -c <command>` with a
    per-command environment map. Git metadata can ignore proven-irrelevant
    locale/pager additions. Environment-sensitive compositions and printenv
    must reject any child environment that differs from the process which
    prepared the snapshot.
    """
    rg_available = shutil.which("rg") is not None
    keys = ("LC_ALL", "LC_CTYPE", "GIT_PAGER", "CODEX_PERMISSION_PROFILE")
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

        os.environ["CODEX_PERMISSION_PROFILE"] = "managed"
        parent_env = os.environ.copy()
        parent_env["GIT_OPTIONAL_LOCKS"] = env.get("GIT_OPTIONAL_LOCKS", "0")
        warm_short(squire, repo, parent_env)
        irrelevant_parent_env = dict(parent_env)
        irrelevant_parent_env.pop("CODEX_PERMISSION_PROFILE", None)
        irrelevant_parent_direct_case = Case(
            "codex_user_shell_irrelevant_parent_env_direct",
            "codex_user_shell",
            ("sh", "-c", "rg -n 'token_(3|7)' docs --glob '*.md'"),
        )
        irrelevant_parent_direct_result = run_case(
            api,
            repo,
            irrelevant_parent_direct_case,
            irrelevant_parent_env,
        )
        irrelevant_parent_direct_hot = irrelevant_parent_direct_result["hot"]
        irrelevant_parent_case = Case(
            "codex_user_shell_irrelevant_parent_env",
            "codex_user_shell",
            ("sh", "-c", "rg -n 'token_(3|7)' docs --glob '*.md' | head -n 2"),
        )
        irrelevant_parent_result = run_case(
            api,
            repo,
            irrelevant_parent_case,
            irrelevant_parent_env,
        )
        irrelevant_parent_hot = irrelevant_parent_result["hot"]
        irrelevant_parent_native = irrelevant_parent_result["native"]
        env_probe = api.try_replay(repo, ("sh", "-c", "printenv LC_ALL"), command_env)
        printenv_case = Case(
            "codex_user_shell_printenv_baseline",
            "codex_user_shell",
            ("printenv", "PATH"),
        )
        baseline_env = os.environ.copy()
        printenv_baseline = run_case(api, repo, printenv_case, baseline_env)
        reordered_env = dict(reversed(list(baseline_env.items())))
        printenv_reordered = run_case(api, repo, printenv_case, reordered_env)
        mismatched_path_env = dict(baseline_env)
        mismatched_path_env["PATH"] = "/squire/child/path-mismatch"
        printenv_mismatch = api.try_replay(repo, printenv_case.argv, mismatched_path_env)
        safe = (
            bool(hot["hit"])
            and result["mismatch"] is None
            and not bool(composed_hot["hit"])
            and bool(irrelevant_parent_direct_hot["hit"]) == rg_available
            and irrelevant_parent_direct_result["mismatch"] is None
            and bool(irrelevant_parent_hot["hit"]) == rg_available
            and irrelevant_parent_result["mismatch"] is None
            and not bool(env_probe["hit"])
            and bool(printenv_baseline["hot"]["hit"])
            and printenv_baseline["mismatch"] is None
            and bool(printenv_reordered["hot"]["hit"])
            and printenv_reordered["mismatch"] is None
            and not bool(printenv_mismatch["hit"])
        )
        return {
            "name": case.name,
            "argv": list(case.argv),
            "hit": bool(hot["hit"]),
            "mismatch": result["mismatch"],
            "safe": safe,
            "rg_available": rg_available,
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
            "irrelevant_parent_env": {
                "direct": {
                    "name": irrelevant_parent_direct_case.name,
                    "argv": list(irrelevant_parent_direct_case.argv),
                    "hit": bool(irrelevant_parent_direct_hot["hit"]),
                    "mismatch": irrelevant_parent_direct_result["mismatch"],
                    "hot_us": round(irrelevant_parent_direct_hot["elapsed_us"], 3),
                },
                "name": irrelevant_parent_case.name,
                "argv": list(irrelevant_parent_case.argv),
                "hit": bool(irrelevant_parent_hot["hit"]),
                "mismatch": irrelevant_parent_result["mismatch"],
                "hot_us": round(irrelevant_parent_hot["elapsed_us"], 3),
                "native_us": round(irrelevant_parent_native["elapsed_us"], 3)
                if irrelevant_parent_native is not None
                else None,
                "hot_exit": irrelevant_parent_hot.get("exit_code"),
                "native_exit": irrelevant_parent_native.get("exit_code")
                if irrelevant_parent_native is not None
                else None,
            },
            "env_probe_missed": not bool(env_probe["hit"]),
            "printenv_baseline_hit": bool(printenv_baseline["hot"]["hit"]),
            "printenv_reordered_hit": bool(printenv_reordered["hot"]["hit"]),
            "printenv_mismatch_missed": not bool(printenv_mismatch["hit"]),
        }
    finally:
        for key, value in saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
        restore_env = os.environ.copy()
        restore_env["GIT_OPTIONAL_LOCKS"] = env.get("GIT_OPTIONAL_LOCKS", "0")
        warm_short(squire, repo, restore_env)


def run_runtime_policy_regression(api: HotAPI, repo: Path, env: dict[str, str]) -> dict[str, Any]:
    hit_cases = [
        ("git", "rev-parse", "HEAD"),
        ("sh", "-c", "git rev-parse HEAD"),
        ("sh", "-c", "git ls-files | grep -F src"),
        ("sh", "-c", "sed -n 1000,1010p pyproject.toml | tail -n 5"),
        ("sed", "-n", "1,10p;20,30p;25,35p", "README.md"),
        ("sh", "-c", "sed -n '1,10p;20,30p' README.md | tail -n 6"),
        ("sh", "-c", "nl -ba docs/guide.rst | sed -n 20,45p"),
        ("sh", "-c", "nl -ba docs/guide.rst | sed -n '2,8p;20,25p'"),
        ("git", "diff", "--check"),
        ("git", "--version"),
        ("which", "python3"),
        ("file", "README.md"),
        ("whoami",),
        ("id",),
    ]
    rg_cases = [
        ("rg", "-n", "runtime_policy_(alpha|omega)", ".", "--glob", "!ignored/**"),
        (
            "sh",
            "-c",
            "rg -n \"runtime_policy_(alpha|omega)\" . --glob '!ignored/**' | head -n 1",
        ),
        (
            "sh",
            "-c",
            "rg -n 'runtime_policy_(alpha|omega)' .\ngit rev-parse HEAD",
        ),
    ]
    rg_available = shutil.which("rg") is not None
    eligible_miss_cases: list[tuple[str, ...]] = []
    if rg_available:
        hit_cases.extend(rg_cases)
    else:
        eligible_miss_cases.extend(rg_cases)
    unsupported_cases = [
        ("go", "test", "./..."),
        ("rg", "--files"),
        ("sh", "-c", "go test ./..."),
        ("sh", "-c", "rg --files | head -n 5"),
        ("sh", "-c", "git status --short | head -n 0"),
        ("sh", "-c", "git ls-files | python3 -c pass"),
        ("sh", "-c", "nl -bt docs/guide.rst | sed -n 20,45p"),
        ("sed", "-n", "1p;2p;3p;4p;5p;6p;7p;8p;9p", "README.md"),
        ("sed", "-n", "1,2p;touch must-not-exist", "README.md"),
        ("rg", "--pre", "touch must-not-exist", "needle", "."),
        ("rg", "-f", "../patterns.txt", "."),
        ("sh", "-c", "rg --pre 'touch must-not-exist' needle ."),
        ("sh", "-c", "rg -n \"$PATTERN\" ."),
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
    for argv in eligible_miss_cases:
        hot = api.try_replay(repo, argv, env)
        passed = not hot["hit"] and hot.get("decision") == 0
        safe = safe and passed
        results.append(
            {
                "argv": list(argv),
                "expected": "eligible_miss",
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
    return {"safe": safe, "rg_available": rg_available, "cases": results}


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
        f"- current-file execution probes safe: `{report['current_file_execution']['safe']}`",
        f"- demand preparation safe: `{report['demand_preparation']['safe']}`",
        f"- bounded Git history regression safe: `{report['git_history_regression']['safe']}`",
        f"- steady-state replay safe: `{report['steady_state_replay']['safe']}`",
        f"- steady-state replay p99 below 1 ms: `{report['steady_state_replay']['sub_ms_p99']}`",
        f"- parallel replay safe: `{report['parallel_replay']['safe']}`",
        f"- parallel replay p99 below 1 ms: `{report['parallel_replay']['sub_ms_p99']}`",
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
    lines.extend(["", "## Current-File Execution", "", "| command | hit | exact | hot us | native us |", "| --- | --- | --- | ---: | ---: |"])
    for probe in report["current_file_execution"]["probes"]:
        command = " ".join(shlex.quote(arg) for arg in probe["argv"])
        lines.append(
            f"| `{command}` | {probe['hit']} | {probe['exact']} | {probe['hot_us']} | {probe['native_us']} |"
        )
    lines.extend(["", "## Demand Preparation", ""])
    lines.append("```json")
    lines.append(json.dumps(report["demand_preparation"], indent=2))
    lines.append("```")
    lines.extend(["", "## Steady-State Replay", "", "| command | calls | hits | exact | p50 us | p95 us | p99 us | max us |", "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"])
    for command, item in sorted(report["steady_state_replay"]["by_command"].items()):
        latency = item["hit_us"]
        lines.append(
            f"| `{command}` | {item['calls']} | {item['hits']} | {item['exact']} | {latency.get('p50', 0)} | {latency.get('p95', 0)} | {latency.get('p99', 0)} | {latency.get('max', 0)} |"
        )
    lines.extend(["", "## Parallel Replay", "", "| command | calls | hits | exact | p50 us | p95 us | p99 us | max us |", "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"])
    for command, item in sorted(report["parallel_replay"]["by_command"].items()):
        latency = item["hit_us"]
        lines.append(
            f"| `{command}` | {item['calls']} | {item['hits']} | {item['exact']} | {latency.get('p50', 0)} | {latency.get('p95', 0)} | {latency.get('p99', 0)} | {latency.get('max', 0)} |"
        )
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
    parser.add_argument("--parallel-workers", type=int, default=8)
    parser.add_argument("--parallel-iterations", type=int, default=32)
    parser.add_argument("--steady-iterations", type=int, default=4096)
    parser.add_argument("--repo-search-cases", type=int, default=500)
    parser.add_argument("--json-out")
    parser.add_argument("--md-out")
    parser.add_argument("--keep-tmp", action="store_true")
    args = parser.parse_args()
    if args.cases < 1:
        raise ValueError("--cases must be positive")
    if args.parallel_workers < 1 or args.parallel_iterations < 1:
        raise ValueError("parallel probe dimensions must be positive")
    if args.steady_iterations < 1:
        raise ValueError("--steady-iterations must be positive")
    if args.repo_search_cases < 1:
        raise ValueError("--repo-search-cases must be positive")
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
    run([str(squire), "runtime", "warm", "--short"], repo, env=env, timeout=120)

    api = HotAPI(lib)
    missing_search_tool_regression = run_missing_search_tool_regression(api, repo, env, work)
    generalized_repo_search = run_generalized_repo_search_probe(api, squire, repo, env)
    repo_search_differential_fuzz = run_repo_search_differential_fuzz(
        api,
        repo,
        env,
        args.seed,
        args.repo_search_cases,
    )
    codex_user_shell_regression = run_codex_user_shell_regression(api, squire, repo, env)
    runtime_policy_regression = run_runtime_policy_regression(api, repo, env)
    current_file_execution = run_current_file_execution_probes(api, repo, env)
    demand_preparation = run_demand_preparation_probe(api, squire, repo, env)
    git_history_regression = run_git_history_regression(api, squire, repo, env)
    warm_short(squire, repo, env)
    parallel_replay = run_parallel_replay_probe(
        api,
        squire,
        repo,
        env,
        args.parallel_workers,
        args.parallel_iterations,
    )
    steady_state_replay = run_steady_state_replay_probe(
        api,
        squire,
        repo,
        env,
        args.steady_iterations,
    )
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
                miss_examples.append(
                    {
                        "bucket": case.bucket,
                        "argv": list(case.argv),
                        "must_miss": case.must_miss,
                        "decision": hot.get("decision"),
                    }
                )
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
        and current_file_execution["safe"]
        and missing_search_tool_regression["safe"]
        and generalized_repo_search["safe"]
        and repo_search_differential_fuzz["safe"]
        and demand_preparation["safe"]
        and git_history_regression["safe"]
        and steady_state_replay["safe"]
        and parallel_replay["safe"]
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
        "current_file_execution": current_file_execution,
        "missing_search_tool_regression": missing_search_tool_regression,
        "generalized_repo_search": generalized_repo_search,
        "repo_search_differential_fuzz": repo_search_differential_fuzz,
        "demand_preparation": demand_preparation,
        "git_history_regression": git_history_regression,
        "steady_state_replay": steady_state_replay,
        "parallel_replay": parallel_replay,
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
