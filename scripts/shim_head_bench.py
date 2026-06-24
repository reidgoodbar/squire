#!/usr/bin/env python3
"""Compare native `git rev-parse HEAD` with Squire's scoped C mmap shim.

The shim is session-scoped: this script creates a temporary PATH directory with
a `git` symlink to the compiled generic mmap shim. The shim serves proven hot
snapshot hits and execs the real binary for everything else.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import statistics
import subprocess
import tempfile
import time
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
SHIM_SOURCE = ROOT / "shims" / "squire_mmap_shim.c"
SOCKET_SHIM_SOURCE = ROOT / "experiments" / "squire_socket_git_shim.c"
LEGACY_ADAPTER_SHIM_SOURCE = r'''#!/usr/bin/env python3
import base64
import json
import os
import subprocess
import sys

argv = ["git"] + sys.argv[1:]
real_git = os.environ.get("SQUIRE_REAL_GIT", "git")
cwd = os.getcwd()
adapter = os.environ.get("SQUIRE_ADAPTER_BIN")
if not adapter:
    os.execv(real_git, [real_git] + sys.argv[1:])

payload = {
    "id": "shim",
    "cwd": cwd,
    "argv": argv,
    "session_id": os.environ.get("SQUIRE_KERNEL_SESSION_ID", "adapter-shim-bench"),
}
try:
    proc = subprocess.run(
        [adapter, "kernel", "adapter", "--stdio"],
        input=json.dumps(payload) + "\n",
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=5,
    )
    if proc.returncode != 0 or not proc.stdout:
        raise RuntimeError(proc.stderr)
    resp = json.loads(proc.stdout.splitlines()[0])
    if not resp.get("ok"):
        raise RuntimeError(resp.get("error", "adapter failed"))
    stdout = base64.b64decode(resp.get("stdout_b64", ""))
    stderr = base64.b64decode(resp.get("stderr_b64", ""))
    os.write(1, stdout)
    os.write(2, stderr)
    raise SystemExit(int(resp.get("exit_code", 0)))
except BaseException:
    os.execv(real_git, [real_git] + sys.argv[1:])
'''


def bench_tmpdir() -> str:
    return os.environ.get("SQUIRE_SHIM_BENCH_TMPDIR") or "/private/tmp"


def run(argv: list[str], cwd: Path, env: dict[str, str] | None = None, check: bool = True) -> subprocess.CompletedProcess[bytes]:
    proc = subprocess.run(argv, cwd=str(cwd), env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if check and proc.returncode != 0:
        raise RuntimeError(
            f"{argv} failed with {proc.returncode}\nstdout={proc.stdout.decode(errors='replace')}\nstderr={proc.stderr.decode(errors='replace')}"
        )
    return proc


def percentile(values: list[float], q: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    idx = min(len(ordered) - 1, max(0, int(round((len(ordered) - 1) * q))))
    return ordered[idx]


def summarize(values: list[float]) -> dict[str, float]:
    return {
        "count": len(values),
        "avg_ms": round(statistics.mean(values), 4),
        "p50_ms": round(percentile(values, 0.50), 4),
        "p95_ms": round(percentile(values, 0.95), 4),
        "min_ms": round(min(values), 4),
        "max_ms": round(max(values), 4),
    }


def timed(argv: list[str], cwd: Path, env: dict[str, str]) -> tuple[subprocess.CompletedProcess[bytes], float]:
    start = time.perf_counter_ns()
    proc = run(argv, cwd, env=env, check=False)
    elapsed_ms = (time.perf_counter_ns() - start) / 1_000_000
    return proc, elapsed_ms


def make_repo() -> Path:
    repo = Path(tempfile.mkdtemp(prefix="squire-shim-head.", dir=bench_tmpdir()))
    run(["git", "init", "-b", "main"], repo)
    run(["git", "config", "user.email", "squire@example.invalid"], repo)
    run(["git", "config", "user.name", "Squire Shim Bench"], repo)
    (repo / "README.md").write_text("# shim bench\n", encoding="utf-8")
    run(["git", "add", "README.md"], repo)
    run(["git", "commit", "-m", "init"], repo)
    return repo


def compile_c_shim(source: Path, out: Path) -> None:
    cc = shutil.which("cc") or shutil.which("clang") or shutil.which("gcc")
    if not cc:
        raise RuntimeError("cc/clang/gcc is required for shim benchmark")
    argv = [cc, "-O3", "-DNDEBUG", "-o", str(out), str(source)]
    if os.uname().sysname != "Darwin":
        argv.append("-lcrypto")
    run(argv, ROOT)


def write_adapter_shim(out: Path) -> None:
    out.write_text(LEGACY_ADAPTER_SHIM_SOURCE, encoding="utf-8")
    out.chmod(0o755)


def build_env(
    repo: Path,
    real_git: str,
    shim_dir: Path,
    squire: Path | None = None,
    socket_path: Path | None = None,
) -> tuple[dict[str, str], dict[str, str]]:
    base_env = os.environ.copy()
    native_env = base_env.copy()
    repo_root = run([real_git, "rev-parse", "--show-toplevel"], repo, env=native_env).stdout.decode().strip()
    git_dir_out = run([real_git, "rev-parse", "--git-dir"], repo, env=native_env).stdout.decode().strip()
    git_dir = Path(git_dir_out)
    if not git_dir.is_absolute():
        git_dir = (Path(repo_root) / git_dir).resolve()
    shim_env = base_env.copy()
    shim_env.update(
        {
            "PATH": str(shim_dir) + os.pathsep + base_env.get("PATH", ""),
            "SQUIRE_REAL_GIT": real_git,
            "SQUIRE_REPO_ROOT": repo_root,
            "SQUIRE_GIT_DIR": str(git_dir),
            "SQUIRE_STORE_ROOT": str(git_dir / "squire" / "kernel"),
            "SQUIRE_SHIM_REAL_PATH": base_env.get("PATH", ""),
        }
    )
    if squire is not None:
        shim_env["SQUIRE_ADAPTER_BIN"] = str(squire)
    if socket_path is not None:
        shim_env["SQUIRE_SHIM_SOCKET"] = str(socket_path)
    return native_env, shim_env


class ShimHelper:
    def __init__(self, squire: Path, repo: Path, socket_path: Path, env: dict[str, str]):
        self.socket_path = socket_path
        self.proc = subprocess.Popen(
            [str(squire), "kernel", "shim-helper", "--socket", str(socket_path)],
            cwd=str(repo),
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        deadline = time.time() + 5
        while time.time() < deadline:
            if socket_path.exists():
                return
            if self.proc.poll() is not None:
                stderr = self.proc.stderr.read().decode(errors="replace") if self.proc.stderr else ""
                raise RuntimeError(f"shim helper exited early: {stderr}")
            time.sleep(0.01)
        self.close()
        raise RuntimeError("shim helper socket did not appear")

    def close(self) -> None:
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=5)
        try:
            self.socket_path.unlink()
        except FileNotFoundError:
            pass


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("squire_bin", nargs="?", default="./squire")
    parser.add_argument("--repo", type=Path)
    parser.add_argument("--rounds", type=int, default=500)
    parser.add_argument("--warmups", type=int, default=30)
    parser.add_argument("--legacy-adapter-shim", "--adapter-shim", dest="adapter_shim", action="store_true")
    parser.add_argument("--socket-shim", action="store_true")
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args()

    squire = Path(args.squire_bin).resolve()
    if not squire.exists():
        raise SystemExit(f"squire binary not found: {squire}")

    real_git = shutil.which("git")
    if not real_git:
        raise SystemExit("git not found")
    if args.adapter_shim and args.socket_shim:
        raise SystemExit("--adapter-shim and --socket-shim are mutually exclusive")

    owned_repo = args.repo is None
    repo = args.repo.resolve() if args.repo else make_repo()
    work = Path(tempfile.mkdtemp(prefix="squire-shim-head-build.", dir=bench_tmpdir()))
    shim = work / ("squire-adapter-shim" if args.adapter_shim else "squire-socket-shim" if args.socket_shim else "squire-head-shim")
    shim_dir = work / "path"
    shim_dir.mkdir()
    if args.adapter_shim:
        write_adapter_shim(shim)
    elif args.socket_shim:
        compile_c_shim(SOCKET_SHIM_SOURCE, shim)
    else:
        compile_c_shim(SHIM_SOURCE, shim)
    os.symlink(shim, shim_dir / "git")

    run([str(squire), "setup"], repo)
    run([str(squire), "kernel", "warm", "--metadata-only", "--short"], repo)

    socket_path = work / "shim-helper.sock"
    helper: ShimHelper | None = None
    native_env, shim_env = build_env(
        repo,
        real_git,
        shim_dir,
        squire if args.adapter_shim else None,
        socket_path if args.socket_shim else None,
    )
    try:
        if args.socket_shim:
            helper_env = native_env.copy()
            helper_env["SQUIRE_SHIM_REAL_PATH"] = native_env.get("PATH", "")
            helper = ShimHelper(squire, repo, socket_path, helper_env)
        expected = run([real_git, "rev-parse", "HEAD"], repo, env=native_env).stdout

        require_env = shim_env.copy()
        if not args.adapter_shim:
            require_env["SQUIRE_SHIM_REQUIRE_HIT"] = "1"
        probe = run(["git", "rev-parse", "HEAD"], repo, env=require_env, check=False)
        if probe.returncode != 0:
            raise RuntimeError(
                "shim did not return successfully\n"
                f"stdout={probe.stdout.decode(errors='replace')}\n"
                f"stderr={probe.stderr.decode(errors='replace')}"
            )
        if probe.stdout != expected or probe.stderr:
            raise RuntimeError("shim hot replay did not match native output exactly")

        for _ in range(args.warmups):
            run(["git", "rev-parse", "HEAD"], repo, env=native_env)
            run([real_git, "rev-parse", "HEAD"], repo, env=native_env)
            run(["git", "rev-parse", "HEAD"], repo, env=shim_env)

        native_path: list[float] = []
        native_abs: list[float] = []
        shim_replay: list[float] = []
        mismatches = 0
        for _ in range(args.rounds):
            proc, ms = timed(["git", "rev-parse", "HEAD"], repo, native_env)
            native_path.append(ms)
            if proc.returncode != 0 or proc.stdout != expected or proc.stderr:
                mismatches += 1

            proc, ms = timed([real_git, "rev-parse", "HEAD"], repo, native_env)
            native_abs.append(ms)
            if proc.returncode != 0 or proc.stdout != expected or proc.stderr:
                mismatches += 1

            proc, ms = timed(["git", "rev-parse", "HEAD"], repo, shim_env)
            shim_replay.append(ms)
            if proc.returncode != 0 or proc.stdout != expected or proc.stderr:
                mismatches += 1

        if args.adapter_shim:
            shim_mode = "adapter_subprocess"
        elif args.socket_shim:
            shim_mode = "socket_helper"
        else:
            shim_mode = "direct_mmap"
        report: dict[str, Any] = {
            "repo": str(repo),
            "rounds": args.rounds,
            "warmups": args.warmups,
            "real_git": real_git,
            "shim_path": str(shim),
            "mismatches": mismatches,
            "target": "git rev-parse HEAD",
            "native_path": summarize(native_path),
            "native_absolute": summarize(native_abs),
            "shim_mode": shim_mode,
            "shim": summarize(shim_replay),
        }
        report["delta_vs_native_path_avg_ms"] = round(report["native_path"]["avg_ms"] - report["shim"]["avg_ms"], 4)
        report["delta_vs_native_path_p95_ms"] = round(report["native_path"]["p95_ms"] - report["shim"]["p95_ms"], 4)
        report["delta_vs_native_absolute_avg_ms"] = round(report["native_absolute"]["avg_ms"] - report["shim"]["avg_ms"], 4)
        report["delta_vs_native_absolute_p95_ms"] = round(report["native_absolute"]["p95_ms"] - report["shim"]["p95_ms"], 4)
        report["shim_faster_than_native_path_p95"] = report["shim"]["p95_ms"] < report["native_path"]["p95_ms"]
        report["shim_faster_than_native_absolute_p95"] = report["shim"]["p95_ms"] < report["native_absolute"]["p95_ms"]
        report["post_commit_old_snapshot_replayed"] = None
        report["post_commit_fallback_exact"] = None

        if owned_repo:
            (repo / "after.txt").write_text("after\n", encoding="utf-8")
            run([real_git, "add", "after.txt"], repo, env=native_env)
            run([real_git, "commit", "-m", "after"], repo, env=native_env)
            after_expected = run([real_git, "rev-parse", "HEAD"], repo, env=native_env).stdout
            after_require = run(["git", "rev-parse", "HEAD"], repo, env=require_env, check=False)
            after_fallback = run(["git", "rev-parse", "HEAD"], repo, env=shim_env, check=False)
            report["post_commit_old_snapshot_replayed"] = (not args.adapter_shim) and after_require.returncode == 0
            report["post_commit_fallback_exact"] = (
                after_fallback.returncode == 0 and after_fallback.stdout == after_expected and after_fallback.stderr == b""
            )
            if report["post_commit_old_snapshot_replayed"] or not report["post_commit_fallback_exact"]:
                mismatches += 1
                report["mismatches"] = mismatches

        if args.json:
            print(json.dumps(report, indent=2, sort_keys=True))
        else:
            print("shim_head_bench:")
            print(f"  target: {report['target']}")
            print(f"  mismatches: {mismatches}")
            print(f"  native_path: {report['native_path']}")
            print(f"  native_absolute: {report['native_absolute']}")
            print(f"  shim_mode: {report['shim_mode']}")
            print(f"  shim: {report['shim']}")
            print(f"  delta_vs_native_path_avg_ms: {report['delta_vs_native_path_avg_ms']}")
            print(f"  delta_vs_native_path_p95_ms: {report['delta_vs_native_path_p95_ms']}")
            print(f"  delta_vs_native_absolute_avg_ms: {report['delta_vs_native_absolute_avg_ms']}")
            print(f"  delta_vs_native_absolute_p95_ms: {report['delta_vs_native_absolute_p95_ms']}")
        return 0 if mismatches == 0 else 1
    finally:
        if helper is not None:
            helper.close()


if __name__ == "__main__":
    raise SystemExit(main())
