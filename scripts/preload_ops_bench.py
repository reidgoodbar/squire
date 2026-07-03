#!/usr/bin/env python3
"""Benchmark proof-gated operators through scoped preload, not PATH shims."""

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
PRELOAD_SOURCE = ROOT / "shims" / "squire_preload.c"
HELPER_SOURCE = ROOT / "shims" / "squire_preload_helper.c"


def default_tmp_dir() -> str:
    if os.uname().sysname == "Darwin" and Path("/private/tmp").is_dir():
        return "/private/tmp"
    return tempfile.gettempdir()


DRIVER_SOURCE = r"""
#include <errno.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

static char *const *command_for(const char *kind) {
  static char *git_head_argv[] = {"git", "rev-parse", "HEAD", NULL};
  static char *git_git_dir_argv[] = {"git", "rev-parse", "--git-dir", NULL};
  static char *git_branch_argv[] = {"git", "rev-parse", "--abbrev-ref", "HEAD", NULL};
  static char *git_top_argv[] = {"git", "rev-parse", "--show-toplevel", NULL};
  static char *git_inside_argv[] = {"git", "rev-parse", "--is-inside-work-tree", NULL};
  static char *git_status_short_argv[] = {"git", "status", "--short", NULL};
  static char *git_status_porcelain_argv[] = {"git", "status", "--porcelain", NULL};
  static char *git_ls_files_argv[] = {"git", "ls-files", NULL};
  static char *git_diff_argv[] = {"git", "diff", NULL};
  static char *git_diff_stat_argv[] = {"git", "diff", "--stat", NULL};
  static char *cat_argv[] = {"cat", "src/app.js", NULL};
  static char *sed_argv[] = {"sed", "-n", "1,2p", "src/app.js", NULL};
  static char *head_argv[] = {"head", "-n", "2", "src/app.js", NULL};
  static char *tail_argv[] = {"tail", "-n", "2", "src/app.js", NULL};
  static char *file_argv[] = {"file", "src/app.js", NULL};
  static char *grep_argv[] = {"grep", "-F", "two", "src/app.js", NULL};
  static char *grep_q_argv[] = {"grep", "-q", "-F", "two", "src/app.js", NULL};
  static char *rg_fixed_argv[] = {"rg", "-F", "two", "src/app.js", NULL};
  static char *rg_fixed_n_argv[] = {"rg", "-n", "-F", "two", "src/app.js", NULL};
  static char *rg_fixed_q_argv[] = {"rg", "-q", "-F", "two", "src/app.js", NULL};
  static char *ls_argv[] = {"ls", "src", NULL};
  static char *printenv_path_argv[] = {"printenv", "PATH", NULL};
  static char *uname_argv[] = {"uname", "-m", NULL};
  static char *whoami_argv[] = {"whoami", NULL};
  static char *hostname_argv[] = {"hostname", NULL};
  static char *id_argv[] = {"id", NULL};
  static char *shell_git_pipe_argv[] = {"sh", "-c", "git rev-parse HEAD | cat", NULL};
  static char *shell_sequence_argv[] = {"sh", "-c", "git rev-parse HEAD >/dev/null && git status --short >/dev/null && cat src/app.js >/dev/null", NULL};
  static char *shell_complex_argv[] = {"sh", "-c", "(git ls-files | grep -F app >/dev/null) && (sed -n '1,4p' src/app.js | tail -n 2 >/dev/null)", NULL};
  static char *shell_status_head_argv[] = {"sh", "-c", "git status --short | head -n 5", NULL};
  static char *shell_ls_files_grep_argv[] = {"sh", "-c", "git ls-files | grep -F src", NULL};
  static char *shell_cat_grep_head_argv[] = {"sh", "-c", "cat src/app.js | grep -F two | head -n 1", NULL};
  static char *shell_rg_head_argv[] = {"sh", "-c", "rg -F two src/app.js | head -n 1", NULL};
  static char *shell_sed_tail_argv[] = {"sh", "-c", "sed -n '1,4p' src/app.js | tail -n 2", NULL};
  static char *shell_mixed_semicolon_argv[] = {"sh", "-c", "git rev-parse HEAD >/dev/null; git ls-files >/dev/null; cat src/app.js >/dev/null", NULL};
  if (strcmp(kind, "git_rev_parse_head") == 0) return git_head_argv;
  if (strcmp(kind, "git_rev_parse_git_dir") == 0) return git_git_dir_argv;
  if (strcmp(kind, "git_rev_parse_branch") == 0) return git_branch_argv;
  if (strcmp(kind, "git_rev_parse_show_toplevel") == 0) return git_top_argv;
  if (strcmp(kind, "git_rev_parse_is_inside_work_tree") == 0) return git_inside_argv;
  if (strcmp(kind, "git_status_short") == 0) return git_status_short_argv;
  if (strcmp(kind, "git_status_porcelain") == 0) return git_status_porcelain_argv;
  if (strcmp(kind, "git_ls_files") == 0) return git_ls_files_argv;
  if (strcmp(kind, "git_diff") == 0) return git_diff_argv;
  if (strcmp(kind, "git_diff_stat") == 0) return git_diff_stat_argv;
  if (strcmp(kind, "cat") == 0) return cat_argv;
  if (strcmp(kind, "sed") == 0) return sed_argv;
  if (strcmp(kind, "head") == 0) return head_argv;
  if (strcmp(kind, "tail") == 0) return tail_argv;
  if (strcmp(kind, "file") == 0) return file_argv;
  if (strcmp(kind, "grep") == 0) return grep_argv;
  if (strcmp(kind, "grep_q") == 0) return grep_q_argv;
  if (strcmp(kind, "rg_fixed") == 0) return rg_fixed_argv;
  if (strcmp(kind, "rg_fixed_n") == 0) return rg_fixed_n_argv;
  if (strcmp(kind, "rg_fixed_q") == 0) return rg_fixed_q_argv;
  if (strcmp(kind, "ls") == 0) return ls_argv;
  if (strcmp(kind, "printenv_path") == 0) return printenv_path_argv;
  if (strcmp(kind, "uname") == 0) return uname_argv;
  if (strcmp(kind, "whoami") == 0) return whoami_argv;
  if (strcmp(kind, "hostname") == 0) return hostname_argv;
  if (strcmp(kind, "id") == 0) return id_argv;
  if (strcmp(kind, "shell_git_pipe") == 0) return shell_git_pipe_argv;
  if (strcmp(kind, "shell_sequence") == 0) return shell_sequence_argv;
  if (strcmp(kind, "shell_complex") == 0) return shell_complex_argv;
  if (strcmp(kind, "shell_status_head") == 0) return shell_status_head_argv;
  if (strcmp(kind, "shell_ls_files_grep") == 0) return shell_ls_files_grep_argv;
  if (strcmp(kind, "shell_cat_grep_head") == 0) return shell_cat_grep_head_argv;
  if (strcmp(kind, "shell_rg_head") == 0) return shell_rg_head_argv;
  if (strcmp(kind, "shell_sed_tail") == 0) return shell_sed_tail_argv;
  if (strcmp(kind, "shell_mixed_semicolon") == 0) return shell_mixed_semicolon_argv;
  return NULL;
}

int main(int argc, char **argv) {
  if (argc < 3) return 2;
  int n = atoi(argv[1]);
  char *const *child_argv = command_for(argv[2]);
  if (n <= 0 || child_argv == NULL) return 3;
  int null_stdout = getenv("SQUIRE_DRIVER_NULL_STDOUT") != NULL;
  for (int i = 0; i < n; i++) {
    int pipefd[2];
    if (pipe(pipefd) != 0) return 4;
    posix_spawn_file_actions_t actions;
    if (posix_spawn_file_actions_init(&actions) != 0 ||
        posix_spawn_file_actions_addclose(&actions, pipefd[0]) != 0 ||
        posix_spawn_file_actions_adddup2(&actions, pipefd[1], STDOUT_FILENO) != 0 ||
        posix_spawn_file_actions_addclose(&actions, pipefd[1]) != 0) return 5;
    pid_t pid;
    int rc = posix_spawnp(&pid, child_argv[0], &actions, NULL, (char *const *)child_argv, environ);
    posix_spawn_file_actions_destroy(&actions);
    close(pipefd[1]);
    if (rc != 0) {
      errno = rc;
      perror("posix_spawnp");
      close(pipefd[0]);
      return 6;
    }
    char buf[8192];
    for (;;) {
      ssize_t r = read(pipefd[0], buf, sizeof(buf));
      if (r < 0) {
        if (errno == EINTR) continue;
        close(pipefd[0]);
        return 7;
      }
      if (r == 0) break;
      if (!null_stdout) {
        ssize_t off = 0;
        while (off < r) {
          ssize_t w = write(STDOUT_FILENO, buf + off, (size_t)(r - off));
          if (w < 0) {
            if (errno == EINTR) continue;
            close(pipefd[0]);
            return 8;
          }
          off += w;
        }
      }
    }
    close(pipefd[0]);
    int status;
    if (waitpid(pid, &status, 0) < 0) return 9;
    if (!WIFEXITED(status) || WEXITSTATUS(status) != 0) return 10;
  }
  return 0;
}
"""


def run(
    argv: list[str],
    cwd: Path,
    *,
    env: dict[str, str] | None = None,
    check: bool = True,
    timeout: int = 120,
) -> subprocess.CompletedProcess[bytes]:
    proc = subprocess.run(argv, cwd=str(cwd), env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
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


def compile_driver(source: Path, out: Path) -> None:
    source.write_text(DRIVER_SOURCE, encoding="utf-8")
    run([compiler(), "-O3", "-DNDEBUG", "-o", str(out), str(source)], ROOT, timeout=60)


def make_repo() -> Path:
    repo = Path(tempfile.mkdtemp(prefix="squire-preload-ops.", dir=os.environ.get("TMPDIR") or default_tmp_dir()))
    run(["git", "init", "-b", "main"], repo)
    run(["git", "config", "user.email", "bench@example.com"], repo)
    run(["git", "config", "user.name", "Bench"], repo)
    (repo / "src").mkdir()
    (repo / "src" / "app.js").write_text("one\ntwo\nthree\nfour\nfive\n", encoding="utf-8")
    (repo / "package.json").write_text('{"name":"bench"}\n', encoding="utf-8")
    run(["git", "add", "."], repo)
    run(["git", "commit", "-m", "init"], repo)
    return repo


def percentile(values: list[float], q: float) -> float:
    ordered = sorted(values)
    return ordered[min(len(ordered) - 1, max(0, round((len(ordered) - 1) * q)))]


def summary(values: list[float]) -> dict[str, float | int]:
    return {
        "count": len(values),
        "avg_ms": round(statistics.mean(values), 6),
        "p50_ms": round(percentile(values, 0.50), 6),
        "p95_ms": round(percentile(values, 0.95), 6),
        "p99_ms": round(percentile(values, 0.99), 6),
        "min_ms": round(min(values), 6),
        "max_ms": round(max(values), 6),
    }


def summary_us(values: list[int]) -> dict[str, float | int]:
    if not values:
        return {"count": 0}
    ordered = sorted(values)
    def idx(q: float) -> int:
        return min(len(ordered) - 1, max(0, int(len(ordered) * q + 0.999999) - 1))

    return {
        "count": len(ordered),
        "avg_us": round(statistics.mean(ordered), 3),
        "p50_us": ordered[idx(0.50)],
        "p95_us": ordered[idx(0.95)],
        "p99_us": ordered[idx(0.99)],
        "max_us": ordered[-1],
        "under_1ms": sum(1 for value in ordered if value < 1000),
    }


def event_lines(repo: Path) -> list[str]:
    log = repo / ".git" / "squire" / "kernel" / "hot_client_events.log"
    if not log.exists():
        return []
    return log.read_text(encoding="utf-8", errors="replace").splitlines()


def clear_event_log(repo: Path) -> None:
    log = repo / ".git" / "squire" / "kernel" / "hot_client_events.log"
    if log.exists():
        log.unlink()


def event_replay_us(lines: list[str]) -> list[int]:
    out: list[int] = []
    for line in lines:
        parts = line.split()
        if len(parts) < 5 or parts[1] != "replay":
            continue
        try:
            out.append(int(parts[4]))
        except ValueError:
            pass
    return out


def native_direct_samples(argv: list[str], repo: Path, reference: subprocess.CompletedProcess[bytes], rounds: int) -> tuple[list[float], int]:
    times: list[float] = []
    mismatches = 0
    for _ in range(rounds):
        start = time.perf_counter_ns()
        proc = run(argv, repo, check=False)
        times.append((time.perf_counter_ns() - start) / 1_000_000)
        if (
            proc.returncode != reference.returncode
            or proc.stdout != reference.stdout
            or proc.stderr != reference.stderr
        ):
            mismatches += 1
    return times, mismatches


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("squire_bin")
    parser.add_argument("--rounds", type=int, default=200)
    parser.add_argument("--native-direct-rounds", type=int, default=None)
    parser.add_argument("--native-batch-rounds", type=int, default=None)
    parser.add_argument("--e2e-samples", type=int, default=1)
    parser.add_argument("--only", default="")
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args()
    native_direct_rounds = args.rounds if args.native_direct_rounds is None else args.native_direct_rounds
    native_batch_rounds = args.rounds if args.native_batch_rounds is None else args.native_batch_rounds
    if native_direct_rounds < 1:
        raise ValueError("--native-direct-rounds must be at least 1")
    if native_batch_rounds < 1:
        raise ValueError("--native-batch-rounds must be at least 1")
    if args.e2e_samples < 1:
        raise ValueError("--e2e-samples must be at least 1")

    squire = Path(args.squire_bin).resolve()
    repo = make_repo()
    work = Path(tempfile.mkdtemp(prefix="squire-preload-ops-build.", dir=os.environ.get("TMPDIR") or default_tmp_dir()))
    preload = work / ("squire-preload.dylib" if os.uname().sysname == "Darwin" else "squire-preload.so")
    helper = work / "squire-preload-helper"
    driver = work / "ops-driver"
    compile_preload(preload)
    compile_helper(helper)
    compile_driver(work / "driver.c", driver)

    run([str(squire), "setup"], repo)
    run([str(squire), "kernel", "warm", "--short"], repo)

    commands = {
        "git_rev_parse_head": ["git", "rev-parse", "HEAD"],
        "git_rev_parse_git_dir": ["git", "rev-parse", "--git-dir"],
        "git_rev_parse_branch": ["git", "rev-parse", "--abbrev-ref", "HEAD"],
        "git_rev_parse_show_toplevel": ["git", "rev-parse", "--show-toplevel"],
        "git_rev_parse_is_inside_work_tree": ["git", "rev-parse", "--is-inside-work-tree"],
        "git_status_short": ["git", "status", "--short"],
        "git_status_porcelain": ["git", "status", "--porcelain"],
        "git_ls_files": ["git", "ls-files"],
        "git_diff": ["git", "diff"],
        "git_diff_stat": ["git", "diff", "--stat"],
        "cat": ["cat", "src/app.js"],
        "sed": ["sed", "-n", "1,2p", "src/app.js"],
        "head": ["head", "-n", "2", "src/app.js"],
        "tail": ["tail", "-n", "2", "src/app.js"],
        "file": ["file", "src/app.js"],
        "grep": ["grep", "-F", "two", "src/app.js"],
        "grep_q": ["grep", "-q", "-F", "two", "src/app.js"],
        "ls": ["ls", "src"],
        "printenv_path": ["printenv", "PATH"],
        "uname": ["uname", "-m"],
        "whoami": ["whoami"],
        "hostname": ["hostname"],
        "id": ["id"],
        "shell_git_pipe": ["sh", "-c", "git rev-parse HEAD | cat"],
        "shell_sequence": ["sh", "-c", "git rev-parse HEAD >/dev/null && git status --short >/dev/null && cat src/app.js >/dev/null"],
        "shell_complex": ["sh", "-c", "(git ls-files | grep -F app >/dev/null) && (sed -n '1,4p' src/app.js | tail -n 2 >/dev/null)"],
        "shell_status_head": ["sh", "-c", "git status --short | head -n 5"],
        "shell_ls_files_grep": ["sh", "-c", "git ls-files | grep -F src"],
        "shell_cat_grep_head": ["sh", "-c", "cat src/app.js | grep -F two | head -n 1"],
        "shell_sed_tail": ["sh", "-c", "sed -n '1,4p' src/app.js | tail -n 2"],
        "shell_mixed_semicolon": ["sh", "-c", "git rev-parse HEAD >/dev/null; git ls-files >/dev/null; cat src/app.js >/dev/null"],
    }
    if shutil.which("rg"):
        commands.update(
            {
                "rg_fixed": ["rg", "-F", "two", "src/app.js"],
                "rg_fixed_n": ["rg", "-n", "-F", "two", "src/app.js"],
                "rg_fixed_q": ["rg", "-q", "-F", "two", "src/app.js"],
                "shell_rg_head": ["sh", "-c", "rg -F two src/app.js | head -n 1"],
            }
        )
    native_env = os.environ.copy()
    native_env["SQUIRE_DRIVER_NULL_STDOUT"] = "1"
    preload_env = native_env.copy()
    preload_env["SQUIRE_SHIM_REQUIRE_HIT"] = "1"
    exact_env = os.environ.copy()
    exact_env["SQUIRE_SHIM_REQUIRE_HIT"] = "1"

    results: dict[str, Any] = {}
    only = {value.strip() for value in args.only.split(",") if value.strip()}
    for kind, argv in commands.items():
        if only and kind not in only:
            continue
        print(f"preload_ops_bench: {kind}", file=os.sys.stderr, flush=True)
        clear_event_log(repo)
        native_ref = run(argv, repo, check=False)
        if native_ref.returncode != 0:
            raise RuntimeError(
                f"native reference failed for {kind}: {native_ref.returncode}\n"
                f"stdout={native_ref.stdout.decode(errors='replace')}\n"
                f"stderr={native_ref.stderr.decode(errors='replace')}"
            )
        exact = run(
            [
                str(squire), "session", "--quiet", "--preload", "--preload-lib", str(preload),
                "--enable-warm-file-replay", "--no-warm", "--no-maintainer", "--",
                str(driver), "3", kind,
            ],
            repo,
            env=exact_env,
        )
        exactness = exact.stdout == native_ref.stdout * 3
        for _ in range(5):
            run(argv, repo, check=False)
        native_direct_times, native_direct_mismatches = native_direct_samples(argv, repo, native_ref, native_direct_rounds)
        for _ in range(5):
            run(
                [
                    str(squire), "session", "--quiet", "--preload", "--preload-lib", str(preload),
                    "--enable-warm-file-replay", "--no-warm", "--no-maintainer", "--",
                    str(driver), "1", kind,
                ],
                repo,
                env=preload_env,
            )

        native_batch_times: list[float] = []
        preload_batch_times: list[float] = []
        before = len(event_lines(repo))

        for _ in range(args.e2e_samples):
            start = time.perf_counter_ns()
            run([str(driver), str(native_batch_rounds), kind], repo, env=native_env)
            native_batch_times.append((time.perf_counter_ns() - start) / 1_000_000 / native_batch_rounds)

            start = time.perf_counter_ns()
            run(
                [
                    str(squire), "session", "--quiet", "--preload", "--preload-lib", str(preload),
                    "--enable-warm-file-replay", "--no-warm", "--no-maintainer", "--",
                    str(driver), str(args.rounds), kind,
                ],
                repo,
                env=preload_env,
            )
            preload_batch_times.append((time.perf_counter_ns() - start) / 1_000_000 / args.rounds)

        events = event_replay_us(event_lines(repo)[before:])
        native_direct = summary(native_direct_times)
        native_batch = summary(native_batch_times)
        preload_summary = summary(preload_batch_times)
        hot_replay = summary_us(events)
        hot_avg_ms = float(hot_replay.get("avg_us", 0)) / 1000 if hot_replay.get("count", 0) else 0.0
        results[kind] = {
            "argv": argv,
            "exactness": exactness,
            "native_ms_per_command": native_direct,
            "native_direct_mismatches": native_direct_mismatches,
            "native_driver_batch_ms_per_command": native_batch,
            "preload_session_batch_ms_per_command": preload_summary,
            "hot_client_replay_us": hot_replay,
            "native_vs_preload_session_delta_avg_ms": round(native_batch["avg_ms"] - preload_summary["avg_ms"], 6),
            "native_direct_vs_hot_replay_delta_avg_ms": round(native_direct["avg_ms"] - hot_avg_ms, 6),
        }

    report = {
        "repo": str(repo),
        "rounds_per_command": args.rounds,
        "native_direct_rounds_per_command": native_direct_rounds,
        "native_batch_rounds_per_command": native_batch_rounds,
        "e2e_samples": args.e2e_samples,
        "transport": "scoped_preload_no_path_shim",
        "results": results,
    }
    print(json.dumps(report, indent=2, sort_keys=True) if args.json else json.dumps(report))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
