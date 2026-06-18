#!/usr/bin/env python3
"""Process-level Squire Kernel edge stress tests.

This script intentionally uses fresh temporary Git repositories and the public
Squire CLI. It is not a unit test harness: it stresses process boundaries,
signals, environment-keyed replay, and stale-proof behavior that are hard to
exercise inside a single Go test process.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import math
import os
from pathlib import Path
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
from typing import Any


class StressFailure(Exception):
    pass


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


def scenario_echo(sq: list[str], args: argparse.Namespace) -> dict[str, Any]:
    repo = make_repo("squire-edge-echo.", args.keep_tmp)
    try:
        setup_kernel(sq, repo)
        expected = run(["git", "rev-parse", "--show-toplevel"], cwd=repo, check=True).stdout
        before = len(replay_us(repo))

        def one() -> tuple[int, subprocess.CompletedProcess[bytes]]:
            start = time.perf_counter_ns()
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


SCENARIOS = {
    "echo": scenario_echo,
    "race": scenario_warm_race,
    "sigterm": scenario_sigterm,
    "gitignore": scenario_gitignore,
    "env": scenario_env,
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
    parser.add_argument("--hot-p95-budget-us", type=int, default=2000)
    parser.add_argument("--strict-performance", action="store_true")
    parser.add_argument("--race-files", type=int, default=800)
    parser.add_argument("--race-mutation-window-s", type=float, default=1.5)
    parser.add_argument("--sigterm-clients", type=int, default=30)
    parser.add_argument("--fd-delta-budget", type=int, default=2)
    parser.add_argument("--json", action="store_true", dest="as_json")
    parser.add_argument("--keep-tmp", action="store_true")
    args = parser.parse_args()

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


if __name__ == "__main__":
    raise SystemExit(main())
