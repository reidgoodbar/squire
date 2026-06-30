#!/usr/bin/env python3
"""Process-level Squire edge stress tests.

This script intentionally uses fresh temporary Git repositories and the public
Squire CLI. It is not a unit test harness: it stresses process boundaries,
signals, environment-keyed replay, and stale-proof behavior that are hard to
exercise inside a single Go test process.
"""

from __future__ import annotations

import argparse
import base64
import concurrent.futures
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import threading
import time
from typing import Any

try:
    import resource
except ImportError:  # pragma: no cover - Windows fallback.
    resource = None


class StressFailure(Exception):
    pass


USE_NORMAL_UX = False
ADAPTERS: dict[str, "AdapterSession"] = {}


class AdapterSession:
    def __init__(self, sq: list[str], repo: Path):
        self.repo = repo
        self.proc = subprocess.Popen(
            sq + ["kernel", "adapter", "--stdio"],
            cwd=str(repo),
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
            env=os.environ.copy(),
        )
        self.seq = 0
        self.lock = threading.Lock()

    def request(
        self,
        command: list[str],
        *,
        cwd: Path,
        env: dict[str, str] | None = None,
        debug: bool = False,
    ) -> tuple[subprocess.CompletedProcess[bytes], dict[str, Any]]:
        with self.lock:
            if self.proc.stdin is None or self.proc.stdout is None:
                raise StressFailure("normal UX adapter pipes unavailable")
            self.seq += 1
            payload: dict[str, Any] = {
                "id": str(self.seq),
                "cwd": str(cwd),
                "argv": command,
                "session_id": "edge-stress-normal-ux",
            }
            if env:
                payload["env"] = env
            if debug:
                payload["debug"] = True
            self.proc.stdin.write(json.dumps(payload) + "\n")
            self.proc.stdin.flush()
            line = self.proc.stdout.readline()
        if not line:
            stderr = self.proc.stderr.read() if self.proc.stderr else ""
            raise StressFailure(f"normal UX adapter closed early: {stderr}")
        resp = json.loads(line)
        stdout = base64.b64decode(resp.get("stdout_b64", ""))
        stderr = base64.b64decode(resp.get("stderr_b64", ""))
        proc = subprocess.CompletedProcess(command, int(resp.get("exit_code", 0)), stdout, stderr)
        return proc, resp

    def close(self) -> None:
        if self.proc.stdin:
            self.proc.stdin.close()
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.kill()
            self.proc.wait(timeout=5)


def adapter_key(repo: Path) -> str:
    return str(repo.resolve())


def adapter_for(sq: list[str], repo: Path) -> AdapterSession:
    key = adapter_key(repo)
    adapter = ADAPTERS.get(key)
    if adapter is None or adapter.proc.poll() is not None:
        adapter = AdapterSession(sq, repo)
        ADAPTERS[key] = adapter
    return adapter


def close_adapter(repo: Path) -> None:
    adapter = ADAPTERS.pop(adapter_key(repo), None)
    if adapter is not None:
        adapter.close()


def close_all_adapters() -> None:
    for adapter in list(ADAPTERS.values()):
        adapter.close()
    ADAPTERS.clear()


def run(
    argv: list[str],
    *,
    cwd: Path,
    env: dict[str, str] | None = None,
    timeout: float = 30,
    check: bool = False,
) -> subprocess.CompletedProcess[bytes]:
    merged_env = os.environ.copy()
    if env:
        merged_env.update(env)
    proc = subprocess.run(
        argv,
        cwd=str(cwd),
        env=merged_env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=timeout,
    )
    if check and proc.returncode != 0:
        raise StressFailure(
            f"command failed ({proc.returncode}): {' '.join(argv)}\n"
            f"stdout:\n{proc.stdout.decode(errors='replace')}\n"
            f"stderr:\n{proc.stderr.decode(errors='replace')}"
        )
    return proc


def resolve_squire_bin(value: str | None) -> list[str]:
    if value:
        p = Path(value)
        if p.exists():
            return [str(p.resolve())]
        found = shutil.which(value)
        if found:
            return [found]
        raise StressFailure(f"squire binary not found: {value}")
    found = shutil.which("squire")
    if found:
        return [found]
    repo_root = Path(__file__).resolve().parents[1]
    if (repo_root / "cmd" / "squire").exists():
        return ["go", "run", str(repo_root / "cmd" / "squire")]
    raise StressFailure("usage: scripts/edge_stress.py /path/to/squire or set SQUIRE_BIN")


def squire(
    sq: list[str],
    repo: Path,
    args: list[str],
    *,
    env: dict[str, str] | None = None,
    timeout: float = 30,
    check: bool = False,
) -> subprocess.CompletedProcess[bytes]:
    return run(sq + args, cwd=repo, env=env, timeout=timeout, check=check)


def squire_debug(
    sq: list[str],
    repo: Path,
    command: list[str],
    *,
    env: dict[str, str] | None = None,
    timeout: float = 30,
) -> tuple[subprocess.CompletedProcess[bytes], dict[str, Any] | None]:
    if USE_NORMAL_UX:
        return adapter_for(sq, repo).request(command, cwd=repo, env=env, debug=True)
    debug_env = {"SQUIRE_KERNEL_DEBUG_RESULT": "1"}
    if env:
        debug_env.update(env)
    proc = squire(sq, repo, ["kernel", "run", "--"] + command, env=debug_env, timeout=timeout)
    debug = None
    real_stderr: list[str] = []
    for line in proc.stderr.decode(errors="replace").splitlines(keepends=True):
        prefix = "SQUIRE_KERNEL_RESULT "
        stripped = line.rstrip("\r\n")
        if stripped.startswith(prefix):
            debug = json.loads(stripped[len(prefix) :])
        else:
            real_stderr.append(line)
    proc.stderr = "".join(real_stderr).encode()
    return proc, debug


def require_exact(
    label: str,
    squire_proc: subprocess.CompletedProcess[bytes],
    native_proc: subprocess.CompletedProcess[bytes],
) -> None:
    if (
        squire_proc.returncode != native_proc.returncode
        or squire_proc.stdout != native_proc.stdout
        or squire_proc.stderr != native_proc.stderr
    ):
        raise StressFailure(
            f"{label}: squire/native mismatch\n"
            f"squire rc={squire_proc.returncode} stdout={squire_proc.stdout!r} stderr={squire_proc.stderr!r}\n"
            f"native rc={native_proc.returncode} stdout={native_proc.stdout!r} stderr={native_proc.stderr!r}"
        )


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def make_repo(prefix: str, keep_tmp: bool) -> Path:
    repo = Path(tempfile.mkdtemp(prefix=prefix, dir=os.environ.get("TMPDIR") or None))
    run(["git", "init"], cwd=repo, check=True)
    run(["git", "config", "user.email", "squire@example.invalid"], cwd=repo, check=True)
    run(["git", "config", "user.name", "Squire Edge Stress"], cwd=repo, check=True)
    (repo / "src").mkdir()
    (repo / "src" / "app.py").write_text("def main():\n    return 'ready'\n", encoding="utf-8")
    (repo / "README.md").write_text("# edge stress\n", encoding="utf-8")
    (repo / "package.json").write_text('{"name":"edge-stress","private":true}\n', encoding="utf-8")
    run(["git", "add", "."], cwd=repo, check=True)
    run(["git", "commit", "-m", "init"], cwd=repo, check=True)
    if keep_tmp:
        print(f"keep_tmp_repo: {repo}")
    return repo


def setup_kernel(sq: list[str], repo: Path, *, background: bool = True, env: dict[str, str] | None = None) -> int | None:
    squire(sq, repo, ["setup"], env=env, check=True)
    if background:
        start = squire(sq, repo, ["kernel", "maintain", "--background", "--short"], env=env, check=True)
        pid = parse_pid(start.stdout.decode(errors="replace"))
        squire(sq, repo, ["kernel", "warm", "--short"], env=env, check=True, timeout=60)
        return pid
    squire(sq, repo, ["kernel", "warm", "--short"], env=env, check=True, timeout=60)
    return None


def stop_kernel(sq: list[str], repo: Path) -> None:
    close_adapter(repo)
    squire(sq, repo, ["kernel", "maintain", "--stop", "--short"], timeout=10)


def parse_pid(text: str) -> int | None:
    for line in text.splitlines():
        if line.startswith("pid:"):
            try:
                return int(line.split(":", 1)[1].strip())
            except ValueError:
                return None
        if " pid=" in line:
            try:
                return int(line.rsplit(" pid=", 1)[1].split()[0])
            except ValueError:
                return None
    return None


def event_path(repo: Path) -> Path:
    return repo / ".git" / "squire" / "kernel" / "hot_client_events.log"


def replay_us(repo: Path) -> list[int]:
    path = event_path(repo)
    if not path.exists():
        return []
    values: list[int] = []
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        parts = line.split()
        if len(parts) >= 5 and parts[1] == "replay":
            try:
                values.append(int(parts[4]))
            except ValueError:
                pass
    return values


def pct(values: list[int], percentile: float) -> int:
    if not values:
        return 0
    ordered = sorted(values)
    index = max(0, min(len(ordered) - 1, math.ceil(percentile * len(ordered)) - 1))
    return ordered[index]


def timing_stats(values: list[int]) -> dict[str, int]:
    if not values:
        return {"count": 0, "p50_us": 0, "p95_us": 0, "max_us": 0}
    return {
        "count": len(values),
        "p50_us": pct(values, 0.50),
        "p95_us": pct(values, 0.95),
        "max_us": max(values),
    }


def fd_count(pid: int | None) -> int | None:
    if not pid:
        return None
    proc_fd = Path("/proc") / str(pid) / "fd"
    if proc_fd.is_dir():
        try:
            return len(list(proc_fd.iterdir()))
        except OSError:
            return None
    lsof = shutil.which("lsof")
    if lsof:
        try:
            proc = subprocess.run([lsof, "-p", str(pid)], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        except OSError:
            return None
        if proc.returncode == 0:
            lines = proc.stdout.splitlines()
            return max(0, len(lines) - 1)
    return None


def rss_kb(pid: int | None) -> int | None:
    if not pid:
        return None
    ps = shutil.which("ps")
    if not ps:
        return None
    try:
        proc = subprocess.run([ps, "-o", "rss=", "-p", str(pid)], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    except OSError:
        return None
    if proc.returncode != 0:
        return None
    text = proc.stdout.decode(errors="replace").strip()
    if not text:
        return None
    try:
        return int(text.splitlines()[-1].strip())
    except ValueError:
        return None


def pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def terminate_pid(pid: int) -> None:
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    except PermissionError:
        return
    deadline = time.time() + 2
    while time.time() < deadline:
        if not pid_alive(pid):
            return
        time.sleep(0.05)
    try:
        os.kill(pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError, AttributeError):
        pass


def low_fd_preexec(limit: int):
    if resource is None:
        return None

    def apply_limit() -> None:
        resource.setrlimit(resource.RLIMIT_NOFILE, (limit, limit))

    return apply_limit


def scenario_echo(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    repo = make_repo("squire-edge-echo.", args.keep_tmp)
    try:
        setup_kernel(sq, repo)
        expected = run(["git", "rev-parse", "--show-toplevel"], cwd=repo, check=True).stdout
        before = len(replay_us(repo))

        def one() -> tuple[int, subprocess.CompletedProcess[bytes]]:
            start = time.perf_counter_ns()
            if USE_NORMAL_UX:
                proc, _ = adapter_for(sq, repo).request(["git", "rev-parse", "--show-toplevel"], cwd=repo)
            else:
                proc = squire(sq, repo, ["kernel", "run", "--", "git", "rev-parse", "--show-toplevel"])
            elapsed = (time.perf_counter_ns() - start) // 1000
            return elapsed, proc

        with concurrent.futures.ThreadPoolExecutor(max_workers=args.echo_requests) as pool:
            results = list(pool.map(lambda _: one(), range(args.echo_requests)))

        external_us = [int(elapsed) for elapsed, _ in results]
        for _, proc in results:
            if proc.returncode != 0 or proc.stdout != expected or proc.stderr:
                raise StressFailure(
                    "token-stream echo: concurrent output mismatch "
                    f"rc={proc.returncode} stdout={proc.stdout!r} stderr={proc.stderr!r}"
                )
        hot = replay_us(repo)[before:]
        if len(hot) != args.echo_requests:
            raise StressFailure(f"token-stream echo: hot replay events {len(hot)} != requests {args.echo_requests}")
        return {
            "scenario": "token_stream_echo",
            "status": "pass",
            "command_serving_ux": "adapter" if USE_NORMAL_UX else "process_cli",
            "requests": args.echo_requests,
            "hot_replay": timing_stats(hot),
            "external_process_wall": timing_stats(external_us),
            "hot_p95_budget_us": args.hot_p95_budget_us,
            "hot_p95_budget_pass": timing_stats(hot)["p95_us"] <= args.hot_p95_budget_us,
            "strict_performance": args.strict_performance,
        }
    finally:
        stop_kernel(sq, repo)
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


def scenario_warm_race(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    repo = make_repo("squire-edge-race.", args.keep_tmp)
    try:
        target = repo / "src" / "target.py"
        target.write_text("VALUE = 'old'\n", encoding="utf-8")
        for i in range(args.race_files):
            (repo / "src" / f"warm_{i:04d}.py").write_text(f"VALUE = {i}\n", encoding="utf-8")
        run(["git", "add", "."], cwd=repo, check=True)
        run(["git", "commit", "-m", "race seed"], cwd=repo, check=True)

        setup_kernel(sq, repo, background=False)
        old, old_debug = squire_debug(sq, repo, ["cat", "src/target.py"])
        if old.returncode != 0 or old.stdout != b"VALUE = 'old'\n":
            raise StressFailure(f"warm race setup failed: {old.returncode} {old.stdout!r} {old.stderr!r}")

        warm = subprocess.Popen(
            sq + ["kernel", "warm", "--short"],
            cwd=str(repo),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=os.environ.copy(),
        )
        mutations = 0
        deadline = time.time() + args.race_mutation_window_s
        time.sleep(0.01)
        while warm.poll() is None and time.time() < deadline:
            mutations += 1
            target.write_text(f"VALUE = 'new-{mutations}'\n", encoding="utf-8")
            time.sleep(0.005)
        if mutations == 0:
            mutations = 1
            target.write_text("VALUE = 'new-1'\n", encoding="utf-8")
        target.write_text("VALUE = 'new-final'\n", encoding="utf-8")
        stdout, stderr = warm.communicate(timeout=60)
        if warm.returncode != 0:
            raise StressFailure(
                f"warm race: warm failed rc={warm.returncode}\n"
                f"stdout:\n{stdout.decode(errors='replace')}\n"
                f"stderr:\n{stderr.decode(errors='replace')}"
            )

        after, after_debug = squire_debug(sq, repo, ["cat", "src/target.py"])
        native = run(["cat", "src/target.py"], cwd=repo, check=True)
        require_exact("warm race post-mutation cat", after, native)
        if after.stdout == b"VALUE = 'old'\n":
            raise StressFailure("warm race: stale old file bytes were replayed")
        return {
            "scenario": "warm_race_invalidation",
            "status": "pass",
            "mutations_during_warm": mutations,
            "old_mode": (old_debug or {}).get("mode"),
            "after_mode": (after_debug or {}).get("mode"),
            "stale_output_replayed": False,
        }
    finally:
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


def scenario_sigterm(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    repo = make_repo("squire-edge-sigterm.", args.keep_tmp)
    try:
        pid = setup_kernel(sq, repo)
        baseline_fd = fd_count(pid)
        baseline_rss = rss_kb(pid)
        hot_children: list[subprocess.Popen[bytes]] = []
        for _ in range(args.sigterm_clients):
            hot_children.append(
                subprocess.Popen(
                    sq + ["kernel", "run", "--", "git", "rev-parse", "--show-toplevel"],
                    cwd=str(repo),
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    env=os.environ.copy(),
                )
            )
        time.sleep(0.002)
        hot_interrupted = 0
        for child in hot_children:
            if child.poll() is None:
                child.terminate()
                hot_interrupted += 1
        for child in hot_children:
            try:
                child.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                child.kill()
                child.communicate(timeout=5)

        slow_children: list[subprocess.Popen[bytes]] = []
        for _ in range(args.sigterm_clients):
            slow_children.append(
                subprocess.Popen(
                    sq + ["kernel", "run", "--", "sh", "-c", "sleep 5; git status --short"],
                    cwd=str(repo),
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    env=os.environ.copy(),
                )
            )
        time.sleep(0.05)
        slow_interrupted = 0
        for i, child in enumerate(slow_children):
            if child.poll() is None:
                child.send_signal(signal.SIGINT if i % 2 == 0 else signal.SIGTERM)
                slow_interrupted += 1
        for child in slow_children:
            try:
                child.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                child.kill()
                child.communicate(timeout=5)
        if slow_interrupted == 0:
            raise StressFailure("sigterm: no slow foreground clients were interrupted")

        time.sleep(0.25)
        final_fd = fd_count(pid)
        final_rss = rss_kb(pid)
        follow = squire(sq, repo, ["kernel", "status", "--short"], check=True)
        if b"native_fallback: available" not in follow.stdout:
            raise StressFailure(f"sigterm: follow-up status unhealthy\n{follow.stdout.decode(errors='replace')}")
        native = run(["git", "rev-parse", "HEAD"], cwd=repo, check=True)
        squire_head = squire(sq, repo, ["kernel", "run", "--", "git", "rev-parse", "HEAD"], check=True)
        require_exact("sigterm follow-up HEAD", squire_head, native)
        fd_delta = None if baseline_fd is None or final_fd is None else final_fd - baseline_fd
        fd_pass = fd_delta is None or fd_delta <= args.fd_delta_budget
        if not fd_pass:
            raise StressFailure(f"sigterm: fd delta {fd_delta} exceeds budget {args.fd_delta_budget}")
        return {
            "scenario": "sigterm_bombardment",
            "status": "pass",
            "hot_clients": args.sigterm_clients,
            "hot_clients_interrupted": hot_interrupted,
            "slow_clients": args.sigterm_clients,
            "slow_clients_interrupted": slow_interrupted,
            "daemon_pid": pid,
            "fd_baseline": baseline_fd,
            "fd_final": final_fd,
            "fd_delta": fd_delta,
            "fd_delta_budget": args.fd_delta_budget,
            "rss_kb_baseline": baseline_rss,
            "rss_kb_final": final_rss,
        }
    finally:
        stop_kernel(sq, repo)
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


def scenario_gitignore(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    repo = make_repo("squire-edge-gitignore.", args.keep_tmp)
    try:
        setup_kernel(sq, repo)
        scratch = repo / "scratch.tmp"
        scratch.write_text("scratch\n", encoding="utf-8")
        native_before = run(["git", "status", "--porcelain"], cwd=repo, check=True)
        squire_before, before_debug = squire_debug(sq, repo, ["git", "status", "--porcelain"])
        require_exact("gitignore before", squire_before, native_before)

        (repo / ".gitignore").write_text("scratch.tmp\n", encoding="utf-8")
        native_after = run(["git", "status", "--porcelain"], cwd=repo, check=True)
        squire_after, after_debug = squire_debug(sq, repo, ["git", "status", "--porcelain"])
        require_exact("gitignore after", squire_after, native_after)
        if b"scratch.tmp" in squire_after.stdout:
            raise StressFailure("gitignore: stale status exposed ignored scratch.tmp")

        squire(sq, repo, ["kernel", "warm", "--short"], check=True, timeout=60)
        squire_warmed, warmed_debug = squire_debug(sq, repo, ["git", "status", "--porcelain"])
        require_exact("gitignore warmed after", squire_warmed, native_after)
        return {
            "scenario": "gitignore_cloaking",
            "status": "pass",
            "before_mode": (before_debug or {}).get("mode"),
            "after_mode": (after_debug or {}).get("mode"),
            "warmed_mode": (warmed_debug or {}).get("mode"),
            "ignored_file_exposed_after": False,
        }
    finally:
        stop_kernel(sq, repo)
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


def make_fake_python(path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("#!/bin/sh\nprintf 'fake-python\\n'\n", encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


def scenario_env(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    repo = make_repo("squire-edge-env.", args.keep_tmp)
    try:
        bin_a = repo / "env-a" / "python3"
        bin_b = repo / "env-b" / "python3"
        make_fake_python(bin_a)
        make_fake_python(bin_b)
        base_path = os.environ.get("PATH", "")
        env_a = {"PATH": f"{bin_a.parent}{os.pathsep}{base_path}"}
        env_b = {"PATH": f"{bin_b.parent}{os.pathsep}{base_path}"}

        setup_kernel(sq, repo, background=False, env=env_a)
        native_a = run(["which", "python3"], cwd=repo, env=env_a, check=True)
        squire_a, debug_a = squire_debug(sq, repo, ["which", "python3"], env=env_a)
        require_exact("env A which python3", squire_a, native_a)

        native_b_cold = run(["which", "python3"], cwd=repo, env=env_b, check=True)
        squire_b_cold, debug_b_cold = squire_debug(sq, repo, ["which", "python3"], env=env_b)
        require_exact("env B cold which python3", squire_b_cold, native_b_cold)
        if squire_b_cold.stdout == squire_a.stdout:
            raise StressFailure("env variants: PATH B received PATH A result")

        squire(sq, repo, ["kernel", "warm", "--short"], env=env_b, check=True, timeout=60)
        squire_b_warm, debug_b_warm = squire_debug(sq, repo, ["which", "python3"], env=env_b)
        require_exact("env B warmed which python3", squire_b_warm, native_b_cold)
        return {
            "scenario": "environment_variants",
            "status": "pass",
            "env_a_mode": (debug_a or {}).get("mode"),
            "env_b_cold_mode": (debug_b_cold or {}).get("mode"),
            "env_b_warmed_mode": (debug_b_warm or {}).get("mode"),
            "env_a_path": squire_a.stdout.decode(errors="replace").strip(),
            "env_b_path": squire_b_warm.stdout.decode(errors="replace").strip(),
        }
    finally:
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


def scenario_cwd(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    repo = make_repo("squire-edge-cwd.", args.keep_tmp)
    try:
        core = repo / "src" / "core"
        integration = repo / "tests" / "integration"
        core.mkdir(parents=True)
        integration.mkdir(parents=True)
        (core / "package.json").write_text('{"scope":"core"}\n', encoding="utf-8")
        (integration / "package.json").write_text('{"scope":"integration"}\n', encoding="utf-8")
        run(["git", "add", "."], cwd=repo, check=True)
        run(["git", "commit", "-m", "cwd seed"], cwd=repo, check=True)

        setup_kernel(sq, repo)
        for cwd in (repo, core, integration):
            squire(sq, cwd, ["kernel", "warm", "--short"], check=True, timeout=60)

        commands: list[tuple[str, Path, list[str]]] = [
            ("root_git_dir", repo, ["git", "rev-parse", "--git-dir"]),
            ("core_git_dir", core, ["git", "rev-parse", "--git-dir"]),
            ("integration_git_dir", integration, ["git", "rev-parse", "--git-dir"]),
            ("root_toplevel", repo, ["git", "rev-parse", "--show-toplevel"]),
            ("core_toplevel", core, ["git", "rev-parse", "--show-toplevel"]),
            ("integration_toplevel", integration, ["git", "rev-parse", "--show-toplevel"]),
            ("root_manifest", repo, ["cat", "package.json"]),
            ("core_manifest", core, ["cat", "package.json"]),
            ("integration_manifest", integration, ["cat", "package.json"]),
        ]

        mismatches = 0
        modes: dict[str, str] = {}

        def one(item: tuple[str, Path, list[str]]) -> tuple[str, str]:
            label, cwd, command = item
            native = run(command, cwd=cwd, check=True)
            observed, debug = squire_debug(sq, cwd, command)
            require_exact(f"multi-cwd {label}", observed, native)
            return label, str((debug or {}).get("mode"))

        work = commands * args.cwd_rounds
        with concurrent.futures.ThreadPoolExecutor(max_workers=min(len(work), args.cwd_workers)) as pool:
            for label, mode in pool.map(one, work):
                modes[label] = mode

        if (core / "package.json").read_bytes() == (integration / "package.json").read_bytes():
            raise StressFailure("multi-cwd setup bug: manifests are identical")
        return {
            "scenario": "multi_cwd_race",
            "status": "pass",
            "operations": len(work),
            "cwd_count": 3,
            "diagnostic_mismatches": mismatches,
            "observed_modes": modes,
        }
    finally:
        stop_kernel(sq, repo)
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


def try_symlink(target: Path, link: Path, *, target_is_directory: bool = False) -> bool:
    try:
        link.symlink_to(target, target_is_directory=target_is_directory)
        return True
    except (OSError, NotImplementedError):
        return False


def scenario_symlink(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    repo = make_repo("squire-edge-symlink.", args.keep_tmp)
    outside = Path(tempfile.mkdtemp(prefix="squire-edge-symlink-outside.", dir=os.environ.get("TMPDIR") or None))
    try:
        (repo / "src" / "config.json").write_text('{"inside":"src"}\n', encoding="utf-8")
        (outside / "outside.json").write_text('{"outside":"secret-adjacent"}\n', encoding="utf-8")
        if not try_symlink(repo / "package.json", repo / "src" / "app.json"):
            return {"scenario": "symlink_labyrinth", "status": "skip", "reason": "symlink unavailable"}
        if not try_symlink(repo / "src", repo / "linked_src", target_is_directory=True):
            return {"scenario": "symlink_labyrinth", "status": "skip", "reason": "directory symlink unavailable"}
        if not try_symlink(outside / "outside.json", repo / "src" / "outside.json"):
            return {"scenario": "symlink_labyrinth", "status": "skip", "reason": "outside symlink unavailable"}
        if not try_symlink(outside, repo / "escape", target_is_directory=True):
            return {"scenario": "symlink_labyrinth", "status": "skip", "reason": "outside directory symlink unavailable"}
        run(["git", "add", "."], cwd=repo, check=True)
        run(["git", "commit", "-m", "symlink seed"], cwd=repo, check=True)

        setup_kernel(sq, repo)
        cases = [
            ("inside_file_symlink", ["cat", "src/app.json"], True),
            ("inside_dir_symlink", ["cat", "linked_src/config.json"], True),
            ("outside_file_symlink", ["cat", "src/outside.json"], False),
            ("outside_dir_symlink", ["sed", "-n", "1,1p", "escape/outside.json"], False),
        ]
        modes: dict[str, str] = {}
        for label, command, may_replay in cases:
            native = run(command, cwd=repo, check=True)
            observed, debug = squire_debug(sq, repo, command)
            require_exact(f"symlink {label}", observed, native)
            mode = str((debug or {}).get("mode"))
            modes[label] = mode
            if not may_replay and mode == "replay":
                raise StressFailure(f"symlink {label}: out-of-workspace symlink replayed")
        return {
            "scenario": "symlink_labyrinth",
            "status": "pass",
            "checked_paths": len(cases),
            "out_of_workspace_replays": 0,
            "observed_modes": modes,
        }
    finally:
        stop_kernel(sq, repo)
        shutil.rmtree(outside, ignore_errors=True)
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


def early_close_returncode(argv: list[str], cwd: Path) -> tuple[int | None, bytes]:
    proc = subprocess.Popen(argv, cwd=str(cwd), stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=os.environ.copy())
    first = b""
    if proc.stdout is not None:
        first = proc.stdout.read(1)
        proc.stdout.close()
    try:
        _, _ = proc.communicate(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
        _, _ = proc.communicate(timeout=5)
    return proc.returncode, first


def scenario_sigpipe(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    repo = make_repo("squire-edge-sigpipe.", args.keep_tmp)
    try:
        payload = b'{\n  "first": true,\n  "payload": "' + (b"x" * args.sigpipe_bytes) + b'"\n}\n'
        (repo / "long_file.json").write_bytes(payload)
        run(["git", "add", "."], cwd=repo, check=True)
        run(["git", "commit", "-m", "sigpipe seed"], cwd=repo, check=True)
        pid = setup_kernel(sq, repo)
        squire(sq, repo, ["kernel", "warm", "--short"], check=True, timeout=60)
        baseline_fd = fd_count(pid)

        native_rc, native_first = early_close_returncode(["cat", "long_file.json"], repo)
        squire_rc, squire_first = early_close_returncode(sq + ["kernel", "run", "--", "cat", "long_file.json"], repo)
        if native_first != squire_first:
            raise StressFailure(f"sigpipe: first byte mismatch native={native_first!r} squire={squire_first!r}")
        follow_native = run(["git", "rev-parse", "HEAD"], cwd=repo, check=True)
        follow_squire = squire(sq, repo, ["kernel", "run", "--", "git", "rev-parse", "HEAD"], check=True)
        require_exact("sigpipe follow-up HEAD", follow_squire, follow_native)
        final_fd = fd_count(pid)
        fd_delta = None if baseline_fd is None or final_fd is None else final_fd - baseline_fd
        if fd_delta is not None and fd_delta > args.fd_delta_budget:
            raise StressFailure(f"sigpipe: fd delta {fd_delta} exceeds budget {args.fd_delta_budget}")
        exact_rc = native_rc == squire_rc
        return {
            "scenario": "sigpipe_early_exit",
            "status": "pass",
            "native_returncode": native_rc,
            "squire_returncode": squire_rc,
            "returncode_exact": exact_rc,
            "first_byte_hash": sha256_bytes(squire_first),
            "fd_baseline": baseline_fd,
            "fd_final": final_fd,
            "fd_delta": fd_delta,
        }
    finally:
        stop_kernel(sq, repo)
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


def scenario_ansi(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    repo = make_repo("squire-edge-ansi.", args.keep_tmp)
    try:
        payload = b"\x1b[31mred\x1b[0m\nnul:\x00\nutf8:\xe2\x98\x83\nraw:\x01\x02\x7f\n"
        (repo / "ansi.json").write_bytes(payload)
        run(["git", "add", "."], cwd=repo, check=True)
        run(["git", "commit", "-m", "ansi seed"], cwd=repo, check=True)
        setup_kernel(sq, repo)
        squire(sq, repo, ["kernel", "warm", "--short"], check=True, timeout=60)
        native = run(["cat", "ansi.json"], cwd=repo, check=True)
        observed, debug = squire_debug(sq, repo, ["cat", "ansi.json"])
        require_exact("ansi byte fidelity", observed, native)
        if observed.stdout != payload:
            raise StressFailure("ansi: replay output did not match original payload bytes")
        return {
            "scenario": "ansi_byte_fidelity",
            "status": "pass",
            "mode": (debug or {}).get("mode"),
            "stdout_sha256": sha256_bytes(observed.stdout),
            "stdout_size": len(observed.stdout),
            "contains_nul": b"\x00" in observed.stdout,
            "contains_ansi_escape": b"\x1b[31m" in observed.stdout,
        }
    finally:
        stop_kernel(sq, repo)
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


def scenario_mtime(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    repo = make_repo("squire-edge-mtime.", args.keep_tmp)
    try:
        target = repo / "src" / "skew.py"
        old = b"VALUE = 'AAAA'\n"
        new = b"VALUE = 'BBBB'\n"
        if len(old) != len(new):
            raise StressFailure("mtime setup bug: payload sizes differ")
        target.write_bytes(old)
        run(["git", "add", "."], cwd=repo, check=True)
        run(["git", "commit", "-m", "mtime seed"], cwd=repo, check=True)
        setup_kernel(sq, repo)
        squire(sq, repo, ["kernel", "warm", "--short"], check=True, timeout=60)
        before, before_debug = squire_debug(sq, repo, ["cat", "src/skew.py"])
        if before.stdout != old:
            raise StressFailure(f"mtime setup read mismatch: {before.stdout!r}")

        stat_before = target.stat()
        target.write_bytes(new)
        skewed_mtime = stat_before.st_mtime - 86400
        os.utime(target, (stat_before.st_atime, skewed_mtime))
        native = run(["cat", "src/skew.py"], cwd=repo, check=True)
        observed, debug = squire_debug(sq, repo, ["cat", "src/skew.py"])
        require_exact("mtime skew file inspection", observed, native)
        if observed.stdout == old:
            raise StressFailure("mtime: stale old bytes replayed after content change")
        status_native = run(["git", "status", "--short"], cwd=repo, check=True)
        status_squire, status_debug = squire_debug(sq, repo, ["git", "status", "--short"])
        require_exact("mtime skew git status", status_squire, status_native)
        return {
            "scenario": "mtime_skew_invalidation",
            "status": "pass",
            "before_mode": (before_debug or {}).get("mode"),
            "after_mode": (debug or {}).get("mode"),
            "status_mode": (status_debug or {}).get("mode"),
            "same_size_change": True,
            "mtime_moved_back_seconds": 86400,
            "stale_output_replayed": False,
        }
    finally:
        stop_kernel(sq, repo)
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


def scenario_storage(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    repo = make_repo("squire-edge-storage.", args.keep_tmp)
    store = repo / ".git" / "squire" / "kernel"
    try:
        target = repo / "src" / "storage.py"
        target.write_text("VALUE = 'old'\n", encoding="utf-8")
        run(["git", "add", "."], cwd=repo, check=True)
        run(["git", "commit", "-m", "storage seed"], cwd=repo, check=True)
        setup_kernel(sq, repo, background=False)
        before, before_debug = squire_debug(sq, repo, ["cat", "src/storage.py"])
        if before.returncode != 0 or before.stdout != b"VALUE = 'old'\n":
            raise StressFailure(f"storage setup read mismatch: {before.returncode} {before.stdout!r}")

        old_modes: dict[Path, int] = {}
        for path in [store, store / "outputs", store / "warm_files"]:
            if path.exists():
                old_modes[path] = stat.S_IMODE(path.stat().st_mode)
                path.chmod(0o500)
        target.write_text("VALUE = 'new'\n", encoding="utf-8")
        warm = squire(sq, repo, ["kernel", "warm", "--short"], timeout=30)
        observed, debug = squire_debug(sq, repo, ["cat", "src/storage.py"])
        native = run(["cat", "src/storage.py"], cwd=repo, check=True)
        require_exact("storage write-denial fallback", observed, native)
        if observed.stdout == b"VALUE = 'old'\n":
            raise StressFailure("storage: stale old bytes replayed after store write denial")
        status = squire(sq, repo, ["kernel", "status", "--short"])
        if status.returncode != 0:
            raise StressFailure(f"storage: status failed after write denial: {status.stderr!r}")
        return {
            "scenario": "storage_write_failure_wall",
            "status": "pass",
            "write_failure_kind": "permission_denied_proxy_for_enospc",
            "warm_returncode": warm.returncode,
            "before_mode": (before_debug or {}).get("mode"),
            "after_mode": (debug or {}).get("mode"),
            "stale_output_replayed": False,
            "kernel_status_ok": True,
        }
    finally:
        for path, mode in sorted(old_modes.items(), key=lambda item: len(str(item[0]))):
            try:
                path.chmod(mode)
            except OSError:
                pass
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


def scenario_emfile(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    if resource is None or platform.system() == "Windows":
        return {"scenario": "emfile_fd_limit", "status": "skip", "reason": "resource limits unavailable"}
    repo = make_repo("squire-edge-emfile.", args.keep_tmp)
    try:
        setup_kernel(sq, repo)
        expected = run(["git", "rev-parse", "--show-toplevel"], cwd=repo, check=True).stdout
        preexec = low_fd_preexec(args.emfile_limit)

        def one() -> tuple[int, bytes, bytes]:
            proc = subprocess.run(
                sq + ["kernel", "run", "--", "git", "rev-parse", "--show-toplevel"],
                cwd=str(repo),
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                preexec_fn=preexec,
                timeout=10,
            )
            return proc.returncode, proc.stdout, proc.stderr

        failures = 0
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.emfile_clients) as pool:
            results = list(pool.map(lambda _: one(), range(args.emfile_clients)))
        for rc, stdout, stderr in results:
            if rc != 0 or stdout != expected or stderr:
                failures += 1
        if failures:
            raise StressFailure(f"emfile: {failures} clients failed exact output under fd limit {args.emfile_limit}")
        follow = squire(sq, repo, ["kernel", "status", "--short"], check=True)
        if b"native_fallback: available" not in follow.stdout:
            raise StressFailure("emfile: kernel status lost native fallback")
        return {
            "scenario": "emfile_fd_limit",
            "status": "pass",
            "clients": args.emfile_clients,
            "fd_limit": args.emfile_limit,
            "exact_failures": failures,
        }
    finally:
        stop_kernel(sq, repo)
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


def scenario_signalflood(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    if not hasattr(signal, "SIGWINCH"):
        return {"scenario": "signal_flood_eintr", "status": "skip", "reason": "SIGWINCH unavailable"}
    repo = make_repo("squire-edge-signalflood.", args.keep_tmp)
    try:
        payload = b"x" * args.signal_payload_bytes + b"\n"
        (repo / "signal.json").write_bytes(payload)
        run(["git", "add", "."], cwd=repo, check=True)
        run(["git", "commit", "-m", "signal seed"], cwd=repo, check=True)
        setup_kernel(sq, repo)
        squire(sq, repo, ["kernel", "warm", "--short"], check=True, timeout=60)

        children: list[subprocess.Popen[bytes]] = []
        signals_sent = 0
        for _ in range(args.signal_clients):
            child = subprocess.Popen(
                sq + ["kernel", "run", "--", "cat", "signal.json"],
                cwd=str(repo),
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                env=os.environ.copy(),
            )
            children.append(child)
            try:
                os.kill(child.pid, signal.SIGWINCH)
                signals_sent += 1
            except ProcessLookupError:
                pass
        deadline = time.time() + args.signal_duration_s
        while time.time() < deadline:
            alive = 0
            for child in children:
                if child.poll() is None:
                    alive += 1
                    try:
                        os.kill(child.pid, signal.SIGWINCH)
                        signals_sent += 1
                    except ProcessLookupError:
                        pass
            if alive == 0:
                break
            time.sleep(0.002)

        hung = 0
        mismatches = 0
        for child in children:
            try:
                stdout, stderr = child.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                hung += 1
                child.kill()
                stdout, stderr = child.communicate(timeout=5)
            if child.returncode != 0 or stdout != payload or stderr:
                mismatches += 1
        if signals_sent < args.signal_min_sent:
            raise StressFailure(f"signal flood: sent {signals_sent} signals, below minimum {args.signal_min_sent}")
        if hung or mismatches:
            raise StressFailure(f"signal flood: hung={hung} mismatches={mismatches}")
        return {
            "scenario": "signal_flood_eintr",
            "status": "pass",
            "clients": args.signal_clients,
            "signals_sent": signals_sent,
            "hung_processes": hung,
            "mismatches": mismatches,
        }
    finally:
        stop_kernel(sq, repo)
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


def scenario_giant(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    repo = make_repo("squire-edge-giant.", args.keep_tmp)
    try:
        payload = (b'{"line":"' + (b"x" * 120) + b'"}\n') * max(1, args.giant_bytes // 132)
        payload = payload[: args.giant_bytes]
        target = repo / "giant.json"
        target.write_bytes(payload)
        setup_kernel(sq, repo)
        squire(sq, repo, ["kernel", "warm", "--short"], check=True, timeout=60)
        observed, debug = squire_debug(sq, repo, ["cat", "giant.json"], timeout=60)
        if observed.returncode != 0:
            raise StressFailure(f"giant: squire returned {observed.returncode} stderr={observed.stderr!r}")
        if sha256_bytes(observed.stdout) != sha256_bytes(payload):
            raise StressFailure("giant: output hash mismatch")
        mode = str((debug or {}).get("mode"))
        if mode == "replay":
            raise StressFailure("giant: oversized payload replayed instead of native fallback")
        return {
            "scenario": "giant_payload_native_fallback",
            "status": "pass",
            "bytes": len(payload),
            "mode": mode,
            "stdout_sha256": sha256_bytes(observed.stdout),
            "oversized_payload_replayed": False,
        }
    finally:
        stop_kernel(sq, repo)
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


def make_fake_sleep_tool(path: Path, pid_file: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        "#!/bin/sh\n"
        f"echo $$ >> {pid_file}\n"
        "sleep 5\n"
        "printf 'fake-tool\\n'\n",
        encoding="utf-8",
    )
    path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


def scenario_zombie(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    if platform.system() == "Windows":
        return {"scenario": "zombie_process_cleanup", "status": "skip", "reason": "pid signaling unavailable"}
    repo = make_repo("squire-edge-zombie.", args.keep_tmp)
    try:
        fake_dir = repo / "fake-tools"
        pid_file = repo / "fake-tool-pids.txt"
        for name in ["node", "npm", "python3", "python", "pip3", "pip", "rg"]:
            make_fake_sleep_tool(fake_dir / name, pid_file)
        env = {"PATH": f"{fake_dir}{os.pathsep}{os.environ.get('PATH', '')}"}
        squire(sq, repo, ["setup"], env=env, check=True)
        proc = subprocess.Popen(
            sq + ["kernel", "warm", "--short"],
            cwd=str(repo),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env={**os.environ.copy(), **env},
        )
        deadline = time.time() + 3
        while time.time() < deadline and not pid_file.exists() and proc.poll() is None:
            time.sleep(0.05)
        pids: list[int] = []
        if pid_file.exists():
            for line in pid_file.read_text(encoding="utf-8", errors="replace").splitlines():
                try:
                    pids.append(int(line.strip()))
                except ValueError:
                    pass
        if proc.poll() is None:
            proc.terminate()
        try:
            proc.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.communicate(timeout=5)
        time.sleep(0.5)
        alive = [pid for pid in pids if pid_alive(pid)]
        for pid in alive:
            terminate_pid(pid)
        if alive:
            raise StressFailure(f"zombie cleanup: child tool processes still alive after warm termination: {alive}")
        status = squire(sq, repo, ["kernel", "status", "--short"], env=env)
        return {
            "scenario": "zombie_process_cleanup",
            "status": "pass",
            "fake_tool_processes_observed": len(pids),
            "alive_after_parent_termination": 0,
            "status_returncode_after": status.returncode,
        }
    finally:
        if not args.keep_tmp:
            shutil.rmtree(repo, ignore_errors=True)


SCENARIOS = {
    "echo": scenario_echo,
    "race": scenario_warm_race,
    "sigterm": scenario_sigterm,
    "gitignore": scenario_gitignore,
    "env": scenario_env,
    "cwd": scenario_cwd,
    "symlink": scenario_symlink,
    "sigpipe": scenario_sigpipe,
    "ansi": scenario_ansi,
    "mtime": scenario_mtime,
    "storage": scenario_storage,
    "emfile": scenario_emfile,
    "signalflood": scenario_signalflood,
    "giant": scenario_giant,
    "zombie": scenario_zombie,
}


def print_result(result: dict[str, Any]) -> None:
    print(f"{result['scenario']}: {result['status']}")
    for key, value in result.items():
        if key in {"scenario", "status"}:
            continue
        if isinstance(value, dict):
            print(f"  {key}:")
            for inner_key, inner_value in value.items():
                print(f"    {inner_key}: {inner_value}")
        else:
            print(f"  {key}: {value}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("squire_bin", nargs="?", default=os.environ.get("SQUIRE_BIN"))
    parser.add_argument(
        "--scenario",
        choices=["all", *SCENARIOS.keys()],
        default="all",
        help="scenario to run",
    )
    parser.add_argument("--echo-requests", type=int, default=40)
    parser.add_argument("--hot-p95-budget-us", type=int, default=1000)
    parser.add_argument("--strict-performance", action="store_true")
    parser.add_argument("--race-files", type=int, default=800)
    parser.add_argument("--race-mutation-window-s", type=float, default=1.5)
    parser.add_argument("--sigterm-clients", type=int, default=30)
    parser.add_argument("--fd-delta-budget", type=int, default=2)
    parser.add_argument("--cwd-rounds", type=int, default=6)
    parser.add_argument("--cwd-workers", type=int, default=12)
    parser.add_argument("--sigpipe-bytes", type=int, default=60000)
    parser.add_argument("--emfile-clients", type=int, default=16)
    parser.add_argument("--emfile-limit", type=int, default=32)
    parser.add_argument("--signal-clients", type=int, default=24)
    parser.add_argument("--signal-duration-s", type=float, default=0.25)
    parser.add_argument("--signal-payload-bytes", type=int, default=65520)
    parser.add_argument("--signal-min-sent", type=int, default=24)
    parser.add_argument("--giant-bytes", type=int, default=15 * 1024 * 1024)
    parser.add_argument("--json", action="store_true", dest="as_json")
    parser.add_argument("--keep-tmp", action="store_true")
    parser.add_argument(
        "--normal-ux",
        action="store_true",
        help="serve ordinary commands through long-lived adapter sessions where the scenario is command-serving focused",
    )
    args = parser.parse_args()

    global USE_NORMAL_UX
    USE_NORMAL_UX = args.normal_ux

    try:
        sq = resolve_squire_bin(args.squire_bin)
        names = list(SCENARIOS) if args.scenario == "all" else [args.scenario]
        results = []
        for name in names:
            result = SCENARIOS[name](sq, args)
            if args.strict_performance and result.get("hot_p95_budget_pass") is False:
                raise StressFailure(
                    f"{result['scenario']}: hot p95 {result['hot_replay']['p95_us']}us "
                    f"exceeds budget {result['hot_p95_budget_us']}us"
                )
            results.append(result)
        if args.as_json:
            print(json.dumps({"edge_stress": "pass", "results": results}, indent=2))
        else:
            for result in results:
                print_result(result)
            print("edge_stress: pass")
        return 0
    except StressFailure as exc:
        if args.as_json:
            print(json.dumps({"edge_stress": "fail", "error": str(exc)}, indent=2), file=sys.stderr)
        else:
            print(f"edge_stress: fail: {exc}", file=sys.stderr)
        return 1
    finally:
        close_all_adapters()


if __name__ == "__main__":
    raise SystemExit(main())
