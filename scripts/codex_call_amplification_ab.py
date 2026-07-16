#!/usr/bin/env python3
"""Measure whether Squire changes Codex terminal-call behavior.

The control and treatment use the same squire-codex binary. The control disables
the bridge explicitly; the treatment enters through the public ``squire codex``
path. Codex rollout traces preserve model-facing requests and nested tool calls.
"""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import hashlib
import json
import math
import os
from pathlib import Path
import random
import resource
import re
import shlex
import shutil
import statistics
import subprocess
import time
from typing import Any


EXPRESS_PROMPT = """Inspect this Express repository without modifying files.

Determine how `utils.normalizeType` parses MIME parameters and where its focused tests live.
Report the exact implementation function and the edge cases that matter for making parameter
names case-insensitive. Use normal repository exploration, do not use the web or network, do
not install anything, and do not run tests. Stop once you have enough evidence for a concise
implementation plan.
"""


FLASK_PROMPT = """Inspect this Flask repository without modifying files.

Determine how Flask selects `instance_path`, how `instance_relative_config` consumes it, and
where focused instance-configuration tests live. Report the exact implementation points and
edge cases for an opt-in `FLASK_INSTANCE_PATH` override. Use normal repository exploration,
do not use the web or network, do not install anything, and do not run tests. Stop once you
have enough evidence for a concise implementation plan.
"""


FMT_PROMPT = """Inspect this fmt repository without modifying files.

Determine how named format-argument identifiers are validated and where the focused named-arg
tests live. Report the exact implementation point and edge cases for accepting uppercase ASCII
letters at the start of a name. Use normal repository exploration, do not use the web or
network, do not install anything, and do not build or run tests. Stop once you have enough
evidence for a concise implementation plan.
"""


@dataclass(frozen=True)
class RepoSpec:
    name: str
    base: Path
    prompt: str


def run_capture(
    argv: list[str],
    cwd: Path,
    env: dict[str, str] | None = None,
    timeout: float | None = None,
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        argv,
        cwd=cwd,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
        timeout=timeout,
    )


def clean_environment() -> dict[str, str]:
    env = os.environ.copy()
    for key in list(env):
        if key.startswith("SQUIRE_") or key == "CODEX_ROLLOUT_TRACE_ROOT":
            env.pop(key, None)
    env.pop("DYLD_INSERT_LIBRARIES", None)
    env["TMPDIR"] = "/private/tmp"
    env["GIT_OPTIONAL_LOCKS"] = "0"
    return env


def setup_isolated_codex_home(out: Path, auth_file: Path) -> Path:
    if not auth_file.is_file():
        raise RuntimeError(f"missing Codex authentication file: {auth_file}")
    home = out / "codex-home"
    home.mkdir(mode=0o700)
    home.chmod(0o700)
    (home / "auth.json").symlink_to(auth_file.resolve())
    (home / "config.toml").write_text(
        "[features]\n"
        "apps = false\n"
        "plugins = false\n"
        "remote_plugin = false\n"
        "tool_suggest = false\n"
        "\n"
        "[skills.bundled]\n"
        "enabled = false\n"
    )
    return home


def clone_repo(spec: RepoSpec, destination: Path) -> str:
    proc = run_capture(
        ["git", "clone", "--quiet", "--no-hardlinks", str(spec.base), str(destination)],
        spec.base.parent,
    )
    if proc.returncode != 0:
        raise RuntimeError(proc.stderr.strip() or f"failed to clone {spec.base}")
    if spec.name == "express":
        dependencies = spec.base / "node_modules"
        if not dependencies.is_dir():
            raise RuntimeError(f"missing Express dependencies: {dependencies}")
        (destination / "node_modules").symlink_to(dependencies, target_is_directory=True)
    head = run_capture(["git", "rev-parse", "HEAD"], destination)
    if head.returncode != 0:
        raise RuntimeError(head.stderr.strip() or "failed to resolve HEAD")
    return head.stdout.strip()


def squire_status(squire: Path, repo: Path, env: dict[str, str]) -> dict[str, Any]:
    proc = run_capture([str(squire), "status", "--json"], repo, env, timeout=30)
    if proc.returncode != 0:
        return {}
    try:
        value = json.loads(proc.stdout)
    except json.JSONDecodeError:
        return {}
    return value if isinstance(value, dict) else {}


def status_metrics(status: dict[str, Any]) -> dict[str, Any]:
    value = status.get("metrics", {})
    return value if isinstance(value, dict) else {}


def metric_delta(before: dict[str, Any], after: dict[str, Any], key: str) -> int:
    return int(status_metrics(after).get(key, 0)) - int(status_metrics(before).get(key, 0))


def replay_coverage(
    arm: str,
    terminal_call_count: int,
    replay_count: int,
    minimum_hit_rate: float,
) -> dict[str, Any]:
    """Evaluate treatment coverage against every observed terminal call."""
    if arm != "treatment":
        return {
            "hit_rate": None,
            "accounting_valid": True,
            "passed": True,
        }

    accounting_valid = 0 <= replay_count <= terminal_call_count
    hit_rate = replay_count / terminal_call_count if terminal_call_count > 0 else None
    return {
        "hit_rate": hit_rate,
        "accounting_valid": accounting_valid,
        "passed": (
            accounting_valid
            and hit_rate is not None
            and hit_rate >= minimum_hit_rate
        ),
    }


def parse_codex_events(path: Path) -> dict[str, Any]:
    commands: list[dict[str, Any]] = []
    item_types: dict[str, int] = {}
    token_usage: dict[str, Any] = {}
    final_message = ""
    for raw in path.read_text(errors="replace").splitlines():
        try:
            event = json.loads(raw)
        except json.JSONDecodeError:
            continue
        item = event.get("item")
        if isinstance(item, dict) and event.get("type") == "item.completed":
            item_type = str(item.get("type", "unknown"))
            item_types[item_type] = item_types.get(item_type, 0) + 1
            if item_type == "command_execution":
                commands.append(
                    {
                        "command": item.get("command", ""),
                        "exit_code": item.get("exit_code"),
                        "output_sha256": hashlib.sha256(
                            str(item.get("aggregated_output", "")).encode()
                        ).hexdigest(),
                        "output_bytes": len(
                            str(item.get("aggregated_output", "")).encode()
                        ),
                    }
                )
            elif item_type == "agent_message":
                final_message = str(item.get("text", ""))
        if event.get("type") == "turn.completed" and isinstance(event.get("usage"), dict):
            token_usage = event["usage"]
    return {
        "commands": commands,
        "item_types": item_types,
        "token_usage": token_usage,
        "final_message_sha256": hashlib.sha256(final_message.encode()).hexdigest(),
    }


def trace_counts(stderr: str) -> dict[str, int]:
    return {
        "runtime_hits": stderr.count("squire runtime: hit"),
        "preparation_launches": stderr.count("squire runtime: preparation requested"),
        "runtime_loads": stderr.count("squire runtime: runtime loaded"),
        "runtime_unavailable": stderr.count("squire runtime: runtime unavailable"),
    }


def find_trace_bundles(root: Path) -> list[Path]:
    return sorted(path.parent for path in root.rglob("manifest.json"))


def payload_value(state: dict[str, Any], bundle: Path, payload_id: Any) -> Any:
    if not isinstance(payload_id, str):
        return None
    payloads = state.get("raw_payloads", {})
    if not isinstance(payloads, dict):
        return None
    record = payloads.get(payload_id)
    if not isinstance(record, dict) or not isinstance(record.get("path"), str):
        return None
    path = bundle / record["path"]
    if not path.is_file():
        return None
    return json.loads(path.read_text())


def command_from_invocation(value: Any) -> str:
    if not isinstance(value, dict):
        return ""
    payload = value.get("payload")
    if not isinstance(payload, dict):
        return ""
    arguments = payload.get("arguments")
    if not isinstance(arguments, str):
        return ""
    try:
        parsed = json.loads(arguments)
    except json.JSONDecodeError:
        return ""
    if not isinstance(parsed, dict):
        return ""
    for key in ("cmd", "command"):
        command = parsed.get(key)
        if isinstance(command, str):
            return command
        if isinstance(command, list) and all(isinstance(part, str) for part in command):
            return " ".join(command)
    return ""


def canonicalize_model_value(value: Any, repo: Path) -> Any:
    volatile_keys = {
        "call_id",
        "chunk_id",
        "prompt_cache_key",
        "wall_time_seconds",
        "session_id",
        "thread_id",
        "turn_id",
        "turn_started_at_unix_ms",
        "window_id",
        "process_id",
        "x-codex-window-id",
    }
    if isinstance(value, dict):
        canonical = {}
        for key, member in sorted(value.items()):
            if key in volatile_keys:
                continue
            if key == "x-codex-turn-metadata" and isinstance(member, str):
                try:
                    member = json.loads(member)
                except json.JSONDecodeError:
                    pass
            canonical_key = key.replace(str(repo), "<REPO>")
            canonical[canonical_key] = canonicalize_model_value(member, repo)
        return canonical
    if isinstance(value, list):
        return [canonicalize_model_value(member, repo) for member in value]
    if isinstance(value, str):
        text = value.replace(str(repo), "<REPO>")
        text = text.replace(
            "git: warning: confstr() failed with code 5: couldn't get path of "
            "DARWIN_USER_TEMP_DIR; using /tmp instead\n",
            "",
        )
        text = re.sub(r"(?m)^Chunk ID: [^\n]+$", "Chunk ID: <VOLATILE>", text)
        text = re.sub(
            r"(?m)^Wall time: [0-9]+(?:\.[0-9]+)? seconds$",
            "Wall time: <VOLATILE>",
            text,
        )
        text = re.sub(
            r"(?m)^Process running with session ID [0-9]+$",
            "Process running with session ID <VOLATILE>",
            text,
        )
        text = re.sub(
            r"(?m)^Original token count: [0-9]+$",
            "Original token count: <DERIVED>",
            text,
        )
        return text
    return value


def is_plain_unordered_rg_command(command: str) -> bool:
    """Return whether a direct rg result is an unordered line multiset.

    Ripgrep searches files in parallel unless ordering is requested explicitly.
    Two native runs can therefore emit the same matching lines in different
    orders. Keep pipelines and context-sensitive forms byte-strict because
    their consumers can attach meaning to adjacency or position.
    """
    if not command or "\n" in command or "\r" in command:
        return False
    try:
        lexer = shlex.shlex(command, posix=True, punctuation_chars="|&;<>")
        lexer.whitespace_split = True
        tokens = list(lexer)
    except ValueError:
        return False
    if len(tokens) >= 3 and tokens[-3:] == ["2", ">", "/dev/null"]:
        tokens = tokens[:-3]
    if not tokens or Path(tokens[0]).name != "rg":
        return False
    if any(token in {"|", "||", "&", "&&", ";", "<", ">", ">>"} for token in tokens):
        return False
    switches = {
        "-n",
        "--line-number",
        "-S",
        "--smart-case",
        "-i",
        "--ignore-case",
        "--hidden",
        "--no-heading",
        "--with-filename",
        "--no-filename",
        "-l",
        "--files-with-matches",
        "-q",
        "--quiet",
        "-F",
        "--fixed-strings",
    }
    pattern_seen = False
    index = 1
    while index < len(tokens):
        token = tokens[index]
        if token in switches:
            index += 1
            continue
        if token in {"-g", "--glob"}:
            index += 2
            if index > len(tokens):
                return False
            continue
        if token.startswith("--glob=") or (len(token) > 2 and token.startswith("-g")):
            index += 1
            continue
        if token.startswith("-"):
            return False
        pattern_seen = True
        index += 1
    return pattern_seen


def canonicalize_tool_result(command: str, result: Any, repo: Path) -> Any:
    canonical = canonicalize_model_value(result, repo)
    if not is_plain_unordered_rg_command(command) or not isinstance(canonical, dict):
        return canonical
    value = canonical.get("value")
    if not isinstance(value, dict) or not isinstance(value.get("output"), str):
        return canonical
    value["output"] = {"unordered_lines": sorted(value["output"].splitlines())}
    return canonical


def stable_json_hash(value: Any) -> str:
    return hashlib.sha256(
        json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()


def ordered_inference_calls(state: dict[str, Any]) -> list[dict[str, Any]]:
    calls = state.get("inference_calls", {})
    if not isinstance(calls, dict):
        return []
    return sorted(
        (call for call in calls.values() if isinstance(call, dict)),
        key=lambda call: int((call.get("execution") or {}).get("started_seq", 0)),
    )


def extract_inference_records(
    state: dict[str, Any], bundle: Path, repo: Path
) -> list[dict[str, Any]]:
    records = []
    for ordinal, call in enumerate(ordered_inference_calls(state)):
        request = payload_value(state, bundle, call.get("raw_request_payload_id"))
        canonical_request = canonicalize_model_value(request, repo)
        records.append(
            {
                "ordinal": ordinal,
                "started_seq": (call.get("execution") or {}).get("started_seq"),
                "request_sha256": stable_json_hash(request),
                "canonical_request_sha256": stable_json_hash(canonical_request),
                "tool_call_count": len(call.get("tool_call_ids_started_by_response") or []),
            }
        )
    return records


def extract_tool_records(state: dict[str, Any], bundle: Path, repo: Path) -> list[dict[str, Any]]:
    calls = state.get("tool_calls", {})
    if not isinstance(calls, dict):
        return []
    ordered = sorted(
        (call for call in calls.values() if isinstance(call, dict)),
        key=lambda call: int((call.get("execution") or {}).get("started_seq", 0)),
    )
    records = []
    occurrences: dict[str, int] = {}
    inference_starts = [
        int((call.get("execution") or {}).get("started_seq", 0))
        for call in ordered_inference_calls(state)
    ]
    for call in ordered:
        invocation = payload_value(state, bundle, call.get("raw_invocation_payload_id"))
        result = payload_value(state, bundle, call.get("raw_result_payload_id"))
        command = command_from_invocation(invocation)
        occurrence = occurrences.get(command, 0)
        occurrences[command] = occurrence + 1
        canonical = canonicalize_tool_result(command, result, repo)
        started_seq = int((call.get("execution") or {}).get("started_seq", 0))
        inference_ordinal = max(
            (index for index, start in enumerate(inference_starts) if start <= started_seq),
            default=-1,
        )
        records.append(
            {
                "tool_call_id": call.get("tool_call_id"),
                "requester": call.get("requester"),
                "kind": call.get("kind"),
                "started_seq": started_seq,
                "inference_ordinal": inference_ordinal,
                "command": command,
                "command_occurrence": occurrence,
                "result_sha256": stable_json_hash(result),
                "canonical_result_sha256": stable_json_hash(canonical),
                "canonical_result": canonical,
            }
        )
    return records


def reduce_rollout_traces(
    codex: Path,
    trace_root: Path,
    env: dict[str, str],
    repo: Path,
) -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []
    for index, bundle in enumerate(find_trace_bundles(trace_root)):
        output = trace_root / f"reduced-{index}.json"
        proc = run_capture(
            [str(codex), "debug", "trace-reduce", str(bundle), "--output", str(output)],
            trace_root,
            env,
            timeout=120,
        )
        record: dict[str, Any] = {
            "bundle": str(bundle),
            "reduce_exit_code": proc.returncode,
            "reduce_stderr": proc.stderr,
            "state": str(output),
        }
        if proc.returncode == 0 and output.is_file():
            value = json.loads(output.read_text())
            record["state_sha256"] = hashlib.sha256(
                json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
            ).hexdigest()
            record["state_summary"] = summarize_rollout_state(value)
            record["inference_records"] = extract_inference_records(value, bundle, repo)
            record["tool_records"] = extract_tool_records(value, bundle, repo)
        records.append(record)
    return records


def summarize_rollout_state(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        return {}
    summary: dict[str, Any] = {}
    for key in (
        "threads",
        "conversation_items",
        "inference_calls",
        "tool_calls",
        "code_cells",
        "terminal_operations",
        "interaction_edges",
    ):
        member = value.get(key)
        if isinstance(member, list):
            summary[key] = len(member)
        elif isinstance(member, dict):
            summary[key] = len(member)
    return summary


def run_arm(
    args: argparse.Namespace,
    spec: RepoSpec,
    pair: int,
    arm: str,
    repo: Path,
    base_head: str,
) -> dict[str, Any]:
    bundle = args.bundle.resolve()
    squire = bundle / "squire"
    codex = bundle / "squire-codex"
    run_id = f"{spec.name}-pair{pair:02d}-{arm}"
    env = clean_environment()
    path_parts = [str(bundle)]
    if spec.name == "flask":
        path_parts.append(str(args.flask_venv / "bin"))
    path_parts.append(env.get("PATH", ""))
    env["PATH"] = os.pathsep.join(path_parts)
    env["CODEX_HOME"] = str(args.codex_home)
    trace_root = args.out / "rollout-traces" / run_id
    trace_root.mkdir(parents=True)
    env["CODEX_ROLLOUT_TRACE_ROOT"] = str(trace_root)
    env["SQUIRE_CODEX_BRIDGE_TRACE"] = "1"
    prewarm = {
        "requested": False,
        "exit_code": None,
        "wall_seconds": 0.0,
        "stderr": "",
    }

    if arm == "control":
        prefix = [str(codex)]
        env["SQUIRE_CODEX_BRIDGE"] = "0"
        env["SQUIRE_AUTO_PREPARE"] = "0"
        env["SQUIRE_CODEX_AUTO_WARM"] = "0"
    elif arm == "treatment":
        prefix = [str(squire), "codex"]
        env["SQUIRE_CODEX_BRIDGE"] = "1"
        env["SQUIRE_CODEX_SQUIRE"] = str(squire)
        env["SQUIRE_CODEX_RUNTIME_LIB"] = str(bundle / "libsquire_runtime.dylib")
        if args.cohort == "warm":
            env["SQUIRE_AUTO_PREPARE"] = "0"
            env["SQUIRE_CODEX_AUTO_WARM"] = "0"
            prewarm["requested"] = True
            prewarm_started = time.perf_counter_ns()
            prepared = run_capture(
                [str(squire), "runtime", "warm", "--short"],
                repo,
                env,
                timeout=120,
            )
            prewarm["wall_seconds"] = (
                time.perf_counter_ns() - prewarm_started
            ) / 1_000_000_000
            prewarm["exit_code"] = prepared.returncode
            prewarm["stderr"] = prepared.stderr
    else:
        raise ValueError(arm)

    command = prefix + [
        "exec",
        "--ephemeral",
        "--json",
        "--color",
        "never",
        "--sandbox",
        "read-only",
        "--model",
        args.model,
        "--config",
        f'model_reasoning_effort="{args.effort}"',
        "--cd",
        str(repo),
        spec.prompt,
    ]
    event_path = args.out / f"{run_id}.jsonl"
    stderr_path = args.out / f"{run_id}.stderr"
    before = squire_status(squire, repo, env) if arm == "treatment" else {}
    usage_before = resource.getrusage(resource.RUSAGE_CHILDREN)
    started = time.perf_counter_ns()
    timed_out = False
    with event_path.open("w") as stdout, stderr_path.open("w") as stderr:
        try:
            proc = subprocess.run(
                command,
                cwd=repo,
                env=env,
                text=True,
                stdout=stdout,
                stderr=stderr,
                check=False,
                timeout=args.timeout,
            )
            exit_code = proc.returncode
        except subprocess.TimeoutExpired:
            timed_out = True
            exit_code = 124
    elapsed = (time.perf_counter_ns() - started) / 1_000_000_000
    usage_after = resource.getrusage(resource.RUSAGE_CHILDREN)
    after = squire_status(squire, repo, env) if arm == "treatment" else {}
    parsed = parse_codex_events(event_path)
    stderr_text = stderr_path.read_text(errors="replace")
    rollout = reduce_rollout_traces(codex, trace_root, env, repo)
    status = run_capture(["git", "status", "--short"], repo, env, timeout=30)
    terminal_call_count = len(parsed["commands"])
    replay_delta = metric_delta(before, after, "replays")
    coverage = replay_coverage(
        arm,
        terminal_call_count,
        replay_delta,
        args.min_treatment_hit_rate,
    )
    result = {
        "id": run_id,
        "repo": spec.name,
        "pair": pair,
        "arm": arm,
        "workspace": str(repo),
        "cohort": args.cohort,
        "binary": prefix,
        "base_head": base_head,
        "exit_code": exit_code,
        "timed_out": timed_out,
        "wall_seconds": elapsed,
        "child_user_seconds": usage_after.ru_utime - usage_before.ru_utime,
        "child_system_seconds": usage_after.ru_stime - usage_before.ru_stime,
        "terminal_call_count": terminal_call_count,
        "commands": parsed["commands"],
        "item_types": parsed["item_types"],
        "token_usage": parsed["token_usage"],
        "final_message_sha256": parsed["final_message_sha256"],
        "bridge_trace": trace_counts(stderr_text),
        "replay_delta": replay_delta,
        "replay_hit_rate": coverage["hit_rate"],
        "replay_accounting_valid": coverage["accounting_valid"],
        "replay_coverage_threshold": args.min_treatment_hit_rate,
        "replay_coverage_passed": coverage["passed"],
        "diagnostic_mismatch_delta": metric_delta(before, after, "diagnostic_mismatches"),
        "estimated_saved_ms_delta": metric_delta(before, after, "estimated_net_saved_ms"),
        "prewarm": prewarm,
        "git_status": status.stdout.splitlines(),
        "rollout": rollout,
    }
    (args.out / f"{run_id}.summary.json").write_text(json.dumps(result, indent=2) + "\n")
    return result


def bootstrap_ci(values: list[float], seed: int = 1729) -> tuple[float, float]:
    if not values:
        return 0.0, 0.0
    rng = random.Random(seed)
    means = []
    for _ in range(20_000):
        sample = [values[rng.randrange(len(values))] for _ in values]
        means.append(statistics.fmean(sample))
    means.sort()
    return means[math.floor(0.025 * len(means))], means[math.ceil(0.975 * len(means)) - 1]


def paired_analysis(results: list[dict[str, Any]]) -> dict[str, Any]:
    pairs: dict[tuple[str, int], dict[str, dict[str, Any]]] = {}
    for result in results:
        pairs.setdefault((result["repo"], int(result["pair"])), {})[result["arm"]] = result
    rows = []
    payload_rows = []
    for (repo, pair), arms in sorted(pairs.items()):
        if set(arms) != {"control", "treatment"}:
            continue
        control = arms["control"]
        treatment = arms["treatment"]
        control_inference_calls = sum(
            int(trace.get("state_summary", {}).get("inference_calls", 0))
            for trace in control.get("rollout", [])
        )
        treatment_inference_calls = sum(
            int(trace.get("state_summary", {}).get("inference_calls", 0))
            for trace in treatment.get("rollout", [])
        )
        control_initial_inferences = [
            inference
            for trace in control.get("rollout", [])
            for inference in trace.get("inference_records", [])
            if inference.get("ordinal") == 0
        ]
        treatment_initial_inferences = [
            inference
            for trace in treatment.get("rollout", [])
            for inference in trace.get("inference_records", [])
            if inference.get("ordinal") == 0
        ]
        initial_requests_equal = (
            len(control_initial_inferences) == 1
            and len(treatment_initial_inferences) == 1
            and control_initial_inferences[0]["canonical_request_sha256"]
            == treatment_initial_inferences[0]["canonical_request_sha256"]
        )
        control_initial_commands = [
            tool.get("command", "")
            for trace in control.get("rollout", [])
            for tool in trace.get("tool_records", [])
            if tool.get("inference_ordinal") == 0
        ]
        treatment_initial_commands = [
            tool.get("command", "")
            for trace in treatment.get("rollout", [])
            for tool in trace.get("tool_records", [])
            if tool.get("inference_ordinal") == 0
        ]
        rows.append(
            {
                "repo": repo,
                "pair": pair,
                "control_calls": control["terminal_call_count"],
                "treatment_calls": treatment["terminal_call_count"],
                "call_delta": treatment["terminal_call_count"] - control["terminal_call_count"],
                "control_inference_calls": control_inference_calls,
                "treatment_inference_calls": treatment_inference_calls,
                "inference_call_delta": treatment_inference_calls - control_inference_calls,
                "initial_requests_canonical_equal": initial_requests_equal,
                "control_initial_tool_calls": len(control_initial_commands),
                "treatment_initial_tool_calls": len(treatment_initial_commands),
                "initial_tool_call_delta": len(treatment_initial_commands)
                - len(control_initial_commands),
                "initial_command_sequence_equal": control_initial_commands
                == treatment_initial_commands,
                "control_initial_commands": control_initial_commands,
                "treatment_initial_commands": treatment_initial_commands,
                "control_wall_seconds": control["wall_seconds"],
                "treatment_wall_seconds": treatment["wall_seconds"],
                "wall_delta_seconds": treatment["wall_seconds"]
                - control["wall_seconds"],
                "control_child_cpu_seconds": control["child_user_seconds"]
                + control["child_system_seconds"],
                "treatment_child_cpu_seconds": treatment["child_user_seconds"]
                + treatment["child_system_seconds"],
                "child_cpu_delta_seconds": treatment["child_user_seconds"]
                + treatment["child_system_seconds"]
                - control["child_user_seconds"]
                - control["child_system_seconds"],
                "replays": treatment["replay_delta"],
                "preparation_launches": treatment["bridge_trace"]["preparation_launches"],
            }
        )
        control_tools = {
            (tool["command"], int(tool["command_occurrence"])): tool
            for trace in control.get("rollout", [])
            for tool in trace.get("tool_records", [])
            if tool.get("command")
        }
        treatment_tools = {
            (tool["command"], int(tool["command_occurrence"])): tool
            for trace in treatment.get("rollout", [])
            for tool in trace.get("tool_records", [])
            if tool.get("command")
        }
        for key in sorted(control_tools.keys() & treatment_tools.keys()):
            control_tool = control_tools[key]
            treatment_tool = treatment_tools[key]
            payload_rows.append(
                {
                    "repo": repo,
                    "pair": pair,
                    "command": key[0],
                    "occurrence": key[1],
                    "exact_equal": control_tool["result_sha256"]
                    == treatment_tool["result_sha256"],
                    "canonical_equal": control_tool["canonical_result_sha256"]
                    == treatment_tool["canonical_result_sha256"],
                    "control_result_sha256": control_tool["result_sha256"],
                    "treatment_result_sha256": treatment_tool["result_sha256"],
                    "control_canonical_sha256": control_tool["canonical_result_sha256"],
                    "treatment_canonical_sha256": treatment_tool[
                        "canonical_result_sha256"
                    ],
                }
            )

    def summarize(selected: list[dict[str, Any]]) -> dict[str, Any]:
        deltas = [float(row["call_delta"]) for row in selected]
        low, high = bootstrap_ci(deltas)
        control = [float(row["control_calls"]) for row in selected]
        treatment = [float(row["treatment_calls"]) for row in selected]
        inference_deltas = [float(row["inference_call_delta"]) for row in selected]
        initial_tool_deltas = [
            float(row["initial_tool_call_delta"]) for row in selected
        ]
        wall_deltas = [float(row["wall_delta_seconds"]) for row in selected]
        child_cpu_deltas = [
            float(row["child_cpu_delta_seconds"]) for row in selected
        ]
        wall_low, wall_high = bootstrap_ci(wall_deltas, seed=1733)
        cpu_low, cpu_high = bootstrap_ci(child_cpu_deltas, seed=1741)
        control_inference = [float(row["control_inference_calls"]) for row in selected]
        treatment_inference = [float(row["treatment_inference_calls"]) for row in selected]
        return {
            "pairs": len(selected),
            "control_calls_mean": statistics.fmean(control) if control else 0.0,
            "control_calls_stddev": statistics.stdev(control)
            if len(control) > 1
            else 0.0,
            "treatment_calls_mean": statistics.fmean(treatment) if treatment else 0.0,
            "treatment_calls_stddev": statistics.stdev(treatment)
            if len(treatment) > 1
            else 0.0,
            "mean_call_delta": statistics.fmean(deltas) if deltas else 0.0,
            "call_delta_stddev": statistics.stdev(deltas) if len(deltas) > 1 else 0.0,
            "median_call_delta": statistics.median(deltas) if deltas else 0.0,
            "mean_delta_bootstrap_95pct": [low, high],
            "control_inference_calls_mean": statistics.fmean(control_inference)
            if control_inference
            else 0.0,
            "treatment_inference_calls_mean": statistics.fmean(treatment_inference)
            if treatment_inference
            else 0.0,
            "mean_inference_call_delta": statistics.fmean(inference_deltas)
            if inference_deltas
            else 0.0,
            "initial_requests_canonical_equal": sum(
                bool(row["initial_requests_canonical_equal"]) for row in selected
            ),
            "initial_command_sequences_equal": sum(
                bool(row["initial_command_sequence_equal"]) for row in selected
            ),
            "mean_initial_tool_call_delta": statistics.fmean(initial_tool_deltas)
            if initial_tool_deltas
            else 0.0,
            "mean_wall_delta_seconds": statistics.fmean(wall_deltas)
            if wall_deltas
            else 0.0,
            "mean_wall_delta_bootstrap_95pct": [wall_low, wall_high],
            "mean_child_cpu_delta_seconds": statistics.fmean(child_cpu_deltas)
            if child_cpu_deltas
            else 0.0,
            "mean_child_cpu_delta_bootstrap_95pct": [cpu_low, cpu_high],
            "initial_model_divergences_before_results": sum(
                bool(row["initial_requests_canonical_equal"])
                and not bool(row["initial_command_sequence_equal"])
                for row in selected
            ),
            "treatment_more": sum(value > 0 for value in deltas),
            "equal": sum(value == 0 for value in deltas),
            "treatment_fewer": sum(value < 0 for value in deltas),
            "replays": sum(int(row["replays"]) for row in selected),
            "preparation_launches": sum(
                int(row["preparation_launches"]) for row in selected
            ),
        }

    repos = sorted({row["repo"] for row in rows})
    request_matched_rows = [
        row for row in rows if row["initial_requests_canonical_equal"]
    ]
    return {
        "pairs": rows,
        "matched_model_payloads": payload_rows,
        "model_payload_summary": {
            "matched": len(payload_rows),
            "exact_equal": sum(row["exact_equal"] for row in payload_rows),
            "canonical_equal": sum(row["canonical_equal"] for row in payload_rows),
            "canonical_mismatches": [
                row for row in payload_rows if not row["canonical_equal"]
            ],
        },
        "by_repo": {
            repo: summarize([row for row in rows if row["repo"] == repo]) for repo in repos
        },
        "by_repo_initial_request_matched": {
            repo: summarize(
                [
                    row
                    for row in request_matched_rows
                    if row["repo"] == repo
                ]
            )
            for repo in repos
        },
        "overall": summarize(rows),
        "overall_initial_request_matched": summarize(request_matched_rows),
    }


def repo_specs(args: argparse.Namespace) -> list[RepoSpec]:
    values = {
        "express": RepoSpec("express", args.sources / "express", EXPRESS_PROMPT),
        "flask": RepoSpec("flask", args.flask_base, FLASK_PROMPT),
        "fmt": RepoSpec("fmt", args.bases / "fmt", FMT_PROMPT),
    }
    return [values[name] for name in args.repos]


def benchmark_workspace(args: argparse.Namespace, spec: RepoSpec, pair: int) -> Path:
    suffix = "shared" if args.reuse_workspaces else f"pair{pair:02d}"
    return args.out / "repos" / f"{spec.name}-{suffix}"


def orchestrate(args: argparse.Namespace) -> int:
    args.out.mkdir(parents=True, exist_ok=False)
    args.codex_home = setup_isolated_codex_home(args.out, args.auth_file)
    specs = repo_specs(args)
    manifest = {
        "design": "same squire-codex binary; bridge disabled control vs squire codex treatment",
        "bundle": str(args.bundle.resolve()),
        "binary_sha256": hashlib.sha256(
            (args.bundle / "squire-codex").read_bytes()
        ).hexdigest(),
        "model": args.model,
        "reasoning_effort": args.effort,
        "cohort": args.cohort,
        "minimum_treatment_hit_rate": args.min_treatment_hit_rate,
        "workspace_design": (
            "one persistent read-only clone per repository across all pairs"
            if args.reuse_workspaces
            else "one shared read-only clone per paired control/treatment run"
        ),
        "codex_home_design": "one isolated shared home; auth symlink only; bundled skills disabled",
        "pairs_per_repo": args.pairs,
        "repos": [
            {
                "name": spec.name,
                "base": str(spec.base),
                "head": run_capture(["git", "rev-parse", "HEAD"], spec.base).stdout.strip(),
                "prompt": spec.prompt,
            }
            for spec in specs
        ],
    }
    (args.out / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")

    results: list[dict[str, Any]] = []
    for pair in range(1, args.pairs + 1):
        for repo_index, spec in enumerate(specs):
            repo = benchmark_workspace(args, spec, pair)
            repo.parent.mkdir(parents=True, exist_ok=True)
            if repo.exists():
                base_head = run_capture(["git", "rev-parse", "HEAD"], repo).stdout.strip()
            else:
                base_head = clone_repo(spec, repo)
            control_first = (pair + repo_index) % 2 == 1
            order = ["control", "treatment"] if control_first else ["treatment", "control"]
            for arm in order:
                run_id = f"{spec.name}-pair{pair:02d}-{arm}"
                print(f"[{run_id}] starting", flush=True)
                result = run_arm(args, spec, pair, arm, repo, base_head)
                results.append(result)
                analysis = paired_analysis(results)
                (args.out / "all-results.json").write_text(json.dumps(results, indent=2) + "\n")
                (args.out / "analysis.json").write_text(json.dumps(analysis, indent=2) + "\n")
                print(
                    f"[{run_id}] exit={result['exit_code']} wall={result['wall_seconds']:.2f}s "
                    f"calls={result['terminal_call_count']} hits={result['replay_delta']} "
                    f"hit_rate={result['replay_hit_rate'] if result['replay_hit_rate'] is not None else 'n/a'} "
                    f"prepare={result['bridge_trace']['preparation_launches']} "
                    f"prewarm={result['prewarm']['exit_code']}",
                    flush=True,
                )

    failed = any(
        result["exit_code"] != 0
        or result["timed_out"]
        or result["git_status"]
        or result["diagnostic_mismatch_delta"] != 0
        or not result["replay_accounting_valid"]
        or not result["replay_coverage_passed"]
        or (
            result["prewarm"]["requested"]
            and result["prewarm"]["exit_code"] != 0
        )
        for result in results
    )
    return 1 if failed else 0


def reanalyze_existing(args: argparse.Namespace) -> int:
    results_path = args.out / "all-results.json"
    if not results_path.is_file():
        raise RuntimeError(f"missing existing results: {results_path}")
    results = json.loads(results_path.read_text())
    for result in results:
        repo_value = result.get("workspace")
        if repo_value:
            repo = Path(repo_value)
        else:
            repo = args.out / "repos" / f"{result['repo']}-pair{int(result['pair']):02d}"
        for trace in result.get("rollout", []):
            state_path = Path(trace["state"])
            bundle = Path(trace["bundle"])
            state = json.loads(state_path.read_text())
            trace["state_summary"] = summarize_rollout_state(state)
            trace["inference_records"] = extract_inference_records(state, bundle, repo)
            trace["tool_records"] = extract_tool_records(state, bundle, repo)
        summary_path = args.out / f"{result['id']}.summary.json"
        summary_path.write_text(json.dumps(result, indent=2) + "\n")
    analysis = paired_analysis(results)
    results_path.write_text(json.dumps(results, indent=2) + "\n")
    (args.out / "analysis.json").write_text(json.dumps(analysis, indent=2) + "\n")
    return 0


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--bundle", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument(
        "--sources", type=Path, default=Path("/private/tmp/squire-crossrepo-sources")
    )
    parser.add_argument(
        "--bases", type=Path, default=Path("/private/tmp/squire-crossrepo-bases")
    )
    parser.add_argument(
        "--flask-base",
        type=Path,
        default=Path("/private/tmp/squire-flask-task-ab.fT2Oz7/base"),
    )
    parser.add_argument(
        "--flask-venv",
        type=Path,
        default=Path("/private/tmp/squire-flask-ab-venv"),
    )
    parser.add_argument("--pairs", type=int, default=10)
    parser.add_argument("--model", default="gpt-5.5")
    parser.add_argument("--effort", default="medium")
    parser.add_argument(
        "--min-treatment-hit-rate",
        type=float,
        default=0.50,
        help="minimum replay share required independently for every treatment run",
    )
    parser.add_argument("--timeout", type=float, default=600)
    parser.add_argument(
        "--auth-file",
        type=Path,
        default=Path.home() / ".codex" / "auth.json",
        help="authentication file linked into the isolated benchmark Codex home",
    )
    parser.add_argument(
        "--cohort",
        choices=["warm", "cold"],
        default="warm",
        help="warm prebuilds treatment state; cold exercises automatic preparation",
    )
    parser.add_argument(
        "--repos",
        nargs="+",
        choices=["express", "flask", "fmt"],
        default=["express", "flask", "fmt"],
    )
    parser.add_argument(
        "--reuse-workspaces",
        action="store_true",
        help="reuse one read-only clone per repository so treatment demand-prepared state persists across pairs",
    )
    parser.add_argument(
        "--analyze-existing",
        action="store_true",
        help="refresh analysis from the reduced traces in an existing output directory",
    )
    args = parser.parse_args()
    if not 0.0 <= args.min_treatment_hit_rate <= 1.0:
        parser.error("--min-treatment-hit-rate must be between 0 and 1")
    return args


if __name__ == "__main__":
    parsed_args = parse_args()
    if parsed_args.analyze_existing:
        raise SystemExit(reanalyze_existing(parsed_args))
    raise SystemExit(orchestrate(parsed_args))
