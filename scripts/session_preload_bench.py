#!/usr/bin/env python3
"""Benchmark the scoped preload session UX with a non-protected launcher."""

from __future__ import annotations

import argparse
import json
import os
import shutil
import statistics
import subprocess
import tempfile
import textwrap
import time
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
PRELOAD_SOURCE = ROOT / "shims" / "squire_preload.c"
HELPER_SOURCE = ROOT / "shims" / "squire_preload_helper.c"

DRIVER_SOURCE = r"""
#include <errno.h>
#include <fcntl.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

int main(int argc, char **argv) {
  int n = argc > 1 ? atoi(argv[1]) : 100;
  if (n <= 0) {
    return 2;
  }
  int null_stdout = getenv("SQUIRE_DRIVER_NULL_STDOUT") != NULL;
  char *child_argv[] = {"git", "rev-parse", "HEAD", NULL};
  for (int i = 0; i < n; i++) {
    int pipefd[2];
    if (pipe(pipefd) != 0) {
      perror("pipe");
      return 3;
    }
    posix_spawn_file_actions_t actions;
    if (posix_spawn_file_actions_init(&actions) != 0 ||
        posix_spawn_file_actions_addclose(&actions, pipefd[0]) != 0 ||
        posix_spawn_file_actions_adddup2(&actions, pipefd[1], STDOUT_FILENO) != 0 ||
        posix_spawn_file_actions_addclose(&actions, pipefd[1]) != 0) {
      perror("file_actions");
      close(pipefd[0]);
      close(pipefd[1]);
      return 4;
    }
    pid_t pid;
    int rc = posix_spawnp(&pid, "git", &actions, NULL, child_argv, environ);
    posix_spawn_file_actions_destroy(&actions);
    close(pipefd[1]);
    if (rc != 0) {
      errno = rc;
      perror("posix_spawnp");
      close(pipefd[0]);
      return 5;
    }
    char buf[4096];
    for (;;) {
      ssize_t r = read(pipefd[0], buf, sizeof(buf));
      if (r < 0) {
        if (errno == EINTR) {
          continue;
        }
        perror("read");
        close(pipefd[0]);
        return 6;
      }
      if (r == 0) {
        break;
      }
      if (!null_stdout) {
        ssize_t off = 0;
        while (off < r) {
          ssize_t w = write(STDOUT_FILENO, buf + off, (size_t)(r - off));
          if (w < 0) {
            if (errno == EINTR) {
              continue;
            }
            perror("write");
            close(pipefd[0]);
            return 7;
          }
          off += w;
        }
      }
    }
    close(pipefd[0]);
    int status;
    if (waitpid(pid, &status, 0) < 0) {
      perror("waitpid");
      return 8;
    }
    if (!WIFEXITED(status) || WEXITSTATUS(status) != 0) {
      return 9;
    }
  }
  return 0;
}
"""

SHELL_DRIVER_SOURCE = r"""
#include <errno.h>
#include <fcntl.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

int main(int argc, char **argv) {
  int n = argc > 1 ? atoi(argv[1]) : 100;
  if (n <= 0) {
    return 2;
  }
  int null_stdout = getenv("SQUIRE_DRIVER_NULL_STDOUT") != NULL;
  char *child_argv[] = {"sh", "-c", "git rev-parse HEAD", NULL};
  for (int i = 0; i < n; i++) {
    int pipefd[2];
    if (pipe(pipefd) != 0) {
      perror("pipe");
      return 3;
    }
    posix_spawn_file_actions_t actions;
    if (posix_spawn_file_actions_init(&actions) != 0 ||
        posix_spawn_file_actions_addclose(&actions, pipefd[0]) != 0 ||
        posix_spawn_file_actions_adddup2(&actions, pipefd[1], STDOUT_FILENO) != 0 ||
        posix_spawn_file_actions_addclose(&actions, pipefd[1]) != 0) {
      perror("file_actions");
      close(pipefd[0]);
      close(pipefd[1]);
      return 4;
    }
    pid_t pid;
    int rc = posix_spawnp(&pid, "sh", &actions, NULL, child_argv, environ);
    posix_spawn_file_actions_destroy(&actions);
    close(pipefd[1]);
    if (rc != 0) {
      errno = rc;
      perror("posix_spawnp");
      close(pipefd[0]);
      return 5;
    }
    char buf[4096];
    for (;;) {
      ssize_t r = read(pipefd[0], buf, sizeof(buf));
      if (r < 0) {
        if (errno == EINTR) {
          continue;
        }
        perror("read");
        close(pipefd[0]);
        return 6;
      }
      if (r == 0) {
        break;
      }
      if (!null_stdout) {
        ssize_t off = 0;
        while (off < r) {
          ssize_t w = write(STDOUT_FILENO, buf + off, (size_t)(r - off));
          if (w < 0) {
            if (errno == EINTR) {
              continue;
            }
            perror("write");
            close(pipefd[0]);
            return 7;
          }
          off += w;
        }
      }
    }
    close(pipefd[0]);
    int status;
    if (waitpid(pid, &status, 0) < 0) {
      perror("waitpid");
      return 8;
    }
    if (!WIFEXITED(status) || WEXITSTATUS(status) != 0) {
      return 9;
    }
  }
  return 0;
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
    raise RuntimeError("usage: scripts/session_preload_bench.py /path/to/squire")


def cc() -> str:
    compiler = shutil.which("cc") or shutil.which("clang") or shutil.which("gcc")
    if not compiler:
        raise RuntimeError("cc/clang/gcc is required for preload benchmark")
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


def compile_driver(source: Path, out: Path, *, shell_launcher: bool = False) -> None:
    source.write_text(SHELL_DRIVER_SOURCE if shell_launcher else DRIVER_SOURCE, encoding="utf-8")
    run([cc(), "-O3", "-DNDEBUG", "-o", str(out), str(source)], ROOT, timeout=60)


def make_repo() -> Path:
    base = os.environ.get("TMPDIR") or "/private/tmp"
    repo = Path(tempfile.mkdtemp(prefix="squire-preload-bench.", dir=base))
    run(["git", "init", "-b", "main"], repo)
    run(["git", "config", "user.email", "bench@example.com"], repo)
    run(["git", "config", "user.name", "Bench"], repo)
    (repo / "README.md").write_text("# preload bench\n", encoding="utf-8")
    run(["git", "add", "README.md"], repo)
    run(["git", "commit", "-m", "init"], repo)
    return repo


def summary(values: list[float]) -> dict[str, float | int]:
    ordered = sorted(values)
    if not ordered:
        return {"count": 0}
    p95_index = min(len(ordered) - 1, int(round((len(ordered) - 1) * 0.95)))
    return {
        "count": len(ordered),
        "min_ms": round(ordered[0], 6),
        "p50_ms": round(statistics.median(ordered), 6),
        "p95_ms": round(ordered[p95_index], 6),
        "max_ms": round(ordered[-1], 6),
        "avg_ms": round(statistics.mean(ordered), 6),
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("squire_bin", nargs="?")
    parser.add_argument("--rounds", type=int, default=5)
    parser.add_argument("--commands", type=int, default=100)
    parser.add_argument("--require-hit-measurement", action="store_true", help="measure replay rounds with SQUIRE_SHIM_REQUIRE_HIT=1 instead of production fallback mode")
    parser.add_argument("--shell-launcher", action="store_true", help="measure a Codex-like sh -c child command instead of direct git spawn")
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args()

    squire = resolve_squire(args.squire_bin)
    repo = make_repo()
    work = Path(tempfile.mkdtemp(prefix="squire-preload-bench-work.", dir=os.environ.get("TMPDIR") or "/private/tmp"))
    preload = work / ("squire-preload.dylib" if os.uname().sysname == "Darwin" else "squire-preload.so")
    helper = work / "squire-preload-helper"
    driver = work / "squire-preload-driver"
    compile_preload(preload)
    compile_helper(helper)
    compile_driver(work / "driver.c", driver, shell_launcher=args.shell_launcher)

    run([str(squire), "setup"], repo)
    run([str(squire), "kernel", "warm", "--metadata-only", "--short"], repo)
    head = run(["git", "rev-parse", "HEAD"], repo).stdout.decode().strip()

    exact_env = os.environ.copy()
    exact_env["SQUIRE_SHIM_REQUIRE_HIT"] = "1"
    exact = run(
        [
            str(squire),
            "session",
            "--quiet",
            "--preload",
            "--preload-lib",
            str(preload),
            "--no-warm",
            "--no-maintainer",
            "--",
            str(driver),
            "3",
        ],
        repo,
        env=exact_env,
    )
    exact_lines = exact.stdout.decode().splitlines()
    exactness = exact_lines == [head, head, head]

    native_times: list[float] = []
    preload_times: list[float] = []
    native_env = os.environ.copy()
    native_env["SQUIRE_DRIVER_NULL_STDOUT"] = "1"
    replay_env = native_env.copy()
    if args.require_hit_measurement:
        replay_env["SQUIRE_SHIM_REQUIRE_HIT"] = "1"

    for _ in range(args.rounds):
        start = time.perf_counter_ns()
        run([str(driver), str(args.commands)], repo, env=native_env)
        native_times.append((time.perf_counter_ns() - start) / 1_000_000 / args.commands)

    for _ in range(args.rounds):
        start = time.perf_counter_ns()
        run(
            [
                str(squire),
                "session",
                "--quiet",
                "--preload",
                "--preload-lib",
                str(preload),
                "--no-warm",
                "--no-maintainer",
                "--",
                str(driver),
                str(args.commands),
            ],
            repo,
            env=replay_env,
        )
        preload_times.append((time.perf_counter_ns() - start) / 1_000_000 / args.commands)

    try:
        run([str(squire), "kernel", "maintain", "--stop", "--short"], repo, check=False, timeout=10)
    except Exception:
        pass

    native = summary(native_times)
    preload_summary = summary(preload_times)
    pass_gate = exactness and preload_summary["p50_ms"] < native["p50_ms"]
    report: dict[str, Any] = {
        "session_preload_bench": "pass" if pass_gate else "fail",
        "repo": str(repo),
        "rounds": args.rounds,
        "commands_per_round": args.commands,
        "exactness": exactness,
        "mismatches": 0 if exactness else 1,
        "native_ms_per_command": native,
        "preload_ms_per_command": preload_summary,
        "delta_p50_ms": round(native["p50_ms"] - preload_summary["p50_ms"], 6),
        "require_hit_measurement": args.require_hit_measurement,
        "ux": {
            "mode": "scoped_preload_session",
            "agent_visible_squire_command": False,
            "measured_commands_are_plain": True,
            "outer_launcher": "squire session -- <non-protected launcher>",
            "transport": "preload",
            "child_launcher": "sh -c git rev-parse HEAD" if args.shell_launcher else "git rev-parse HEAD",
        },
    }
    if args.json:
        print(json.dumps(report, indent=2, sort_keys=True))
    else:
        print(f"session_preload_bench: {report['session_preload_bench']}")
        print(f"native p50: {native['p50_ms']}ms")
        print(f"preload p50: {preload_summary['p50_ms']}ms")
        print(f"delta p50: {report['delta_p50_ms']}ms")
    return 0 if pass_gate else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"session_preload_bench: fail: {exc}", file=os.sys.stderr)
        raise SystemExit(1)
