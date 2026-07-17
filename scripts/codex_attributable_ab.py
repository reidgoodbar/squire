#!/usr/bin/env python3
"""Measure command time attributable to Squire through the Codex boundary.

The benchmark replaces model inference with a local deterministic Responses API.
Both arms receive the same command calls and return the same terminal payloads.
Timing starts after the fixture has returned a command batch and ends when Codex
submits that batch's function outputs. Counterbalanced AB/BA pairs remove fixed
process-order bias; A/A and B/B pairs measure same-arm noise.
"""

from __future__ import annotations

import argparse
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import math
import os
from pathlib import Path
import random
import re
import statistics
import subprocess
import sys
import threading
import time
from typing import Any

import codex_scripted_bridge_ab as fixture


SERIAL_BATCHES = tuple((index,) for index in range(len(fixture.COMMANDS)))
PARALLEL_BATCHES = fixture.COMMAND_BATCHES


@dataclass
class ProtocolTrace:
    requests: list[dict[str, Any]] = field(default_factory=list)
    request_arrival_ns: list[int] = field(default_factory=list)
    response_sent_ns: list[int] = field(default_factory=list)
    protocol_errors: list[str] = field(default_factory=list)


@dataclass
class ServerState:
    batches: tuple[tuple[int, ...], ...]
    traces: dict[str, ProtocolTrace] = field(default_factory=dict)
    lock: threading.Lock = field(default_factory=threading.Lock)


def run_key_from_path(path: str) -> str:
    parts = [part for part in path.split("?")[0].split("/") if part]
    return parts[0] if parts else "unknown"


def handler_type(state: ServerState) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
            arrived_ns = time.perf_counter_ns()
            run_key = run_key_from_path(self.path)
            length = int(self.headers.get("content-length", "0"))
            raw = self.rfile.read(length)
            try:
                request = json.loads(raw)
            except json.JSONDecodeError:
                request = {"invalid_json": raw.decode(errors="replace")}

            with state.lock:
                trace = state.traces.setdefault(run_key, ProtocolTrace())
                index = len(trace.requests)
                trace.requests.append(request)
                trace.request_arrival_ns.append(arrived_ns)
                trace.response_sent_ns.append(0)
                if 0 < index <= len(state.batches):
                    expected = {
                        f"script-call-{call_index + 1:02d}"
                        for call_index in state.batches[index - 1]
                    }
                    observed = set(fixture.walk_function_outputs(request))
                    if not expected.issubset(observed):
                        trace.protocol_errors.append(
                            f"request {index + 1} missing {sorted(expected - observed)}"
                        )
                elif index > len(state.batches):
                    trace.protocol_errors.append(
                        f"unexpected request {index + 1} for {run_key}"
                    )

            call_indices = state.batches[index] if index < len(state.batches) else None
            body = fixture.response_event(
                f"{run_key}-response-{index + 1:02d}", call_indices
            )
            self.send_response(200)
            self.send_header("content-type", "text/event-stream")
            self.send_header("content-length", str(len(body)))
            self.send_header("connection", "close")
            self.end_headers()
            self.wfile.write(body)
            self.wfile.flush()
            sent_ns = time.perf_counter_ns()
            with state.lock:
                state.traces[run_key].response_sent_ns[index] = sent_ns

        def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
            body = b'{"data":[]}'
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(body)))
            self.send_header("connection", "close")
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, _format: str, *_args: Any) -> None:
            return

    return Handler


def run_capture(
    argv: list[str],
    cwd: Path,
    env: dict[str, str],
    timeout: float = 120,
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        argv,
        cwd=cwd,
        env=env,
        text=True,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
        timeout=timeout,
    )


def condition_workspace(repo: Path, env: dict[str, str]) -> None:
    """Give both arms the same warm native filesystem and executable state."""
    for command in fixture.COMMANDS:
        proc = run_capture(["/bin/zsh", "-lc", command], repo, env, timeout=30)
        if proc.returncode != 0:
            raise RuntimeError(
                f"conditioning failed for {command!r}: {proc.stderr.strip()}"
            )


def prepare_workspace(bundle: Path, repo: Path) -> dict[str, Any]:
    env = fixture.clean_environment()
    env["PATH"] = os.pathsep.join([str(bundle), env.get("PATH", "")])
    started_ns = time.perf_counter_ns()
    proc = run_capture(
        [str(bundle / "squire"), "runtime", "warm", "--short"],
        repo,
        env,
        timeout=120,
    )
    result = {
        "exit_code": proc.returncode,
        "wall_ms": (time.perf_counter_ns() - started_ns) / 1_000_000,
        "stderr": proc.stderr,
    }
    if proc.returncode != 0:
        raise RuntimeError(f"Squire preparation failed: {proc.stderr.strip()}")
    return result


def metric_delta(before: dict[str, Any], after: dict[str, Any], key: str) -> int:
    return int(after.get(key, 0)) - int(before.get(key, 0))


TERMINAL_OUTPUT_RE = re.compile(
    r"\AChunk ID: [^\n]+\n"
    r"Wall time: [^\n]+\n"
    r"Process exited with code (-?[0-9]+)\n"
    r"Original token count: [0-9]+\n"
    r"Output:\n(.*)\Z",
    re.DOTALL,
)


def terminal_payload(value: Any) -> dict[str, Any] | None:
    if not isinstance(value, str):
        return None
    match = TERMINAL_OUTPUT_RE.match(value)
    if match is None:
        return None
    return {"exit_code": int(match.group(1)), "output": match.group(2)}


def phase_timing(
    trace: ProtocolTrace,
    process_started_ns: int,
    process_ended_ns: int,
    expected_batches: int,
) -> dict[str, Any]:
    expected_requests = expected_batches + 1
    complete = (
        len(trace.request_arrival_ns) == expected_requests
        and len(trace.response_sent_ns) == expected_requests
        and all(value > 0 for value in trace.response_sent_ns)
    )
    if not complete:
        return {
            "complete": False,
            "request_count": len(trace.request_arrival_ns),
            "expected_request_count": expected_requests,
            "batch_roundtrip_ms": [],
            "command_roundtrip_ms": None,
            "startup_ms": None,
            "fixture_response_ms": None,
            "shutdown_ms": None,
        }

    batch_roundtrips = [
        (trace.request_arrival_ns[index + 1] - trace.response_sent_ns[index])
        / 1_000_000
        for index in range(expected_batches)
    ]
    return {
        "complete": True,
        "request_count": expected_requests,
        "expected_request_count": expected_requests,
        "batch_roundtrip_ms": batch_roundtrips,
        "command_roundtrip_ms": sum(batch_roundtrips),
        "startup_ms": (trace.request_arrival_ns[0] - process_started_ns) / 1_000_000,
        "fixture_response_ms": sum(
            sent - arrived
            for arrived, sent in zip(
                trace.request_arrival_ns, trace.response_sent_ns, strict=True
            )
        )
        / 1_000_000,
        "shutdown_ms": (process_ended_ns - trace.response_sent_ns[-1]) / 1_000_000,
    }


def output_map(trace: ProtocolTrace) -> dict[str, Any]:
    return {
        call_id: output
        for request in trace.requests
        for call_id, output in fixture.walk_function_outputs(request).items()
    }


def run_one(
    *,
    run_key: str,
    arm: str,
    bundle: Path,
    repo: Path,
    out: Path,
    base_url: str,
    sandbox: str,
    state: ServerState,
    timeout: float,
) -> dict[str, Any]:
    squire = bundle / "squire"
    codex = bundle / "squire-codex"
    env = fixture.clean_environment()
    env["PATH"] = os.pathsep.join([str(bundle), env.get("PATH", "")])
    home = out / "codex-homes" / run_key
    env["CODEX_HOME"] = str(home)
    fixture.write_codex_config(home, base_url)
    env["SQUIRE_AUTO_PREPARE"] = "0"
    env["SQUIRE_CODEX_AUTO_WARM"] = "0"
    env["SQUIRE_CODEX_BRIDGE_TRACE"] = "1"

    condition_workspace(repo, env)
    if arm == "control":
        prefix = [str(codex)]
        env["SQUIRE_CODEX_BRIDGE"] = "0"
    elif arm == "treatment":
        prefix = [str(squire), "codex"]
        env["SQUIRE_CODEX_BRIDGE"] = "1"
        env["SQUIRE_CODEX_SQUIRE"] = str(squire)
        env["SQUIRE_CODEX_RUNTIME_LIB"] = str(bundle / "libsquire_runtime.dylib")
    else:
        raise ValueError(f"unknown arm: {arm}")

    before = fixture.status_metrics(squire, repo, env) if arm == "treatment" else {}
    event_path = out / f"{run_key}.jsonl"
    stderr_path = out / f"{run_key}.stderr"
    command = prefix + [
        "exec",
        "--ephemeral",
        "--json",
        "--color",
        "never",
        "--sandbox",
        sandbox,
        "--cd",
        str(repo),
        "Execute the scripted local fixture.",
    ]
    process_started_ns = time.perf_counter_ns()
    with event_path.open("w") as stdout, stderr_path.open("w") as stderr:
        proc = subprocess.run(
            command,
            cwd=repo,
            env=env,
            text=True,
            stdin=subprocess.DEVNULL,
            stdout=stdout,
            stderr=stderr,
            check=False,
            timeout=timeout,
        )
    process_ended_ns = time.perf_counter_ns()
    after = fixture.status_metrics(squire, repo, env) if arm == "treatment" else {}
    with state.lock:
        trace = state.traces.get(run_key, ProtocolTrace())
    outputs = output_map(trace)
    terminal_outputs = {
        call_id: terminal_payload(value) for call_id, value in outputs.items()
    }
    stderr_text = stderr_path.read_text(errors="replace")
    timing = phase_timing(
        trace, process_started_ns, process_ended_ns, len(state.batches)
    )
    return {
        "run_key": run_key,
        "arm": arm,
        "exit_code": proc.returncode,
        "wall_ms": (process_ended_ns - process_started_ns) / 1_000_000,
        "timing": timing,
        "terminal_calls": fixture.parse_command_count(event_path),
        "model_requests": len(trace.requests),
        "request_output_orders": fixture.output_orders(trace.requests),
        "protocol_errors": trace.protocol_errors,
        "raw_outputs": outputs,
        "terminal_outputs": terminal_outputs,
        "runtime_hits": stderr_text.count("squire runtime: hit"),
        "replay_delta": metric_delta(before, after, "replays"),
        "diagnostic_mismatch_delta": metric_delta(
            before, after, "diagnostic_mismatches"
        ),
    }


def compare_runs(first: dict[str, Any], second: dict[str, Any]) -> dict[str, Any]:
    expected_ids = [
        f"script-call-{index + 1:02d}" for index in range(len(fixture.COMMANDS))
    ]
    per_call = []
    for call_id, command in zip(expected_ids, fixture.COMMANDS, strict=True):
        first_raw = first["raw_outputs"].get(call_id)
        second_raw = second["raw_outputs"].get(call_id)
        first_terminal = first["terminal_outputs"].get(call_id)
        second_terminal = second["terminal_outputs"].get(call_id)
        per_call.append(
            {
                "call_id": call_id,
                "command": command,
                "observed_first": first_raw is not None,
                "observed_second": second_raw is not None,
                "terminal_payload_exact": first_terminal == second_terminal
                and first_terminal is not None,
                "canonical_envelope_equal": fixture.canonical_output(
                    first_raw, Path("<unused>")
                )
                == fixture.canonical_output(second_raw, Path("<unused>")),
            }
        )
    return {
        "terminal_payloads_exact": all(
            row["terminal_payload_exact"] for row in per_call
        ),
        "canonical_envelopes_equal": all(
            row["canonical_envelope_equal"] for row in per_call
        ),
        "same_function_output_order": first["request_output_orders"]
        == second["request_output_orders"],
        "per_call": per_call,
    }


def run_pair(
    *,
    kind: str,
    pair: int,
    arms: tuple[str, str],
    bundle: Path,
    repo: Path,
    out: Path,
    server_url: str,
    sandbox: str,
    state: ServerState,
    timeout: float,
) -> dict[str, Any]:
    runs = []
    for position, arm in enumerate(arms, 1):
        run_key = f"{kind.lower()}-{pair:03d}-{position}-{arm}"
        print(
            f"{kind} pair {pair}: position {position} {arm}",
            file=sys.stderr,
            flush=True,
        )
        runs.append(
            run_one(
                run_key=run_key,
                arm=arm,
                bundle=bundle,
                repo=repo,
                out=out,
                base_url=f"{server_url}/{run_key}/v1",
                sandbox=sandbox,
                state=state,
                timeout=timeout,
            )
        )
    comparison = compare_runs(runs[0], runs[1])
    return {
        "kind": kind,
        "pair": pair,
        "order": "-".join(arms),
        "runs": runs,
        "comparison": comparison,
    }


def bootstrap_ci(values: list[float], seed: int = 1789) -> tuple[float, float]:
    if not values:
        return 0.0, 0.0
    rng = random.Random(seed)
    means = []
    for _ in range(20_000):
        sample = [values[rng.randrange(len(values))] for _ in values]
        means.append(statistics.fmean(sample))
    means.sort()
    return means[math.floor(0.025 * len(means))], means[
        math.ceil(0.975 * len(means)) - 1
    ]


def percentile(values: list[float], quantile: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    rank = max(0, math.ceil(quantile * len(ordered)) - 1)
    return ordered[rank]


def summarize_values(values: list[float], seed: int) -> dict[str, Any]:
    low, high = bootstrap_ci(values, seed=seed)
    return {
        "count": len(values),
        "mean_ms": statistics.fmean(values) if values else 0.0,
        "median_ms": statistics.median(values) if values else 0.0,
        "p95_ms": percentile(values, 0.95),
        "min_ms": min(values) if values else 0.0,
        "max_ms": max(values) if values else 0.0,
        "mean_bootstrap_95pct_ms": [low, high],
    }


def command_ms(run: dict[str, Any]) -> float:
    value = run["timing"]["command_roundtrip_ms"]
    if value is None:
        raise ValueError(f"incomplete timing for {run['run_key']}")
    return float(value)


def analyze(
    pairs: list[dict[str, Any]], preparation: dict[str, Any]
) -> dict[str, Any]:
    ab_pairs = [pair for pair in pairs if pair["kind"] == "AB"]
    aa_pairs = [pair for pair in pairs if pair["kind"] == "AA"]
    bb_pairs = [pair for pair in pairs if pair["kind"] == "BB"]

    savings = []
    wall_savings = []
    control_times = []
    treatment_times = []
    order_savings: dict[str, list[float]] = {
        "control-treatment": [],
        "treatment-control": [],
    }
    for pair in ab_pairs:
        by_arm = {run["arm"]: run for run in pair["runs"]}
        control = by_arm["control"]
        treatment = by_arm["treatment"]
        saving = command_ms(control) - command_ms(treatment)
        savings.append(saving)
        wall_savings.append(float(control["wall_ms"]) - float(treatment["wall_ms"]))
        control_times.append(command_ms(control))
        treatment_times.append(command_ms(treatment))
        order_savings[pair["order"]].append(saving)

    aa_order_effect = [
        command_ms(pair["runs"][0]) - command_ms(pair["runs"][1])
        for pair in aa_pairs
    ]
    bb_order_effect = [
        command_ms(pair["runs"][0]) - command_ms(pair["runs"][1])
        for pair in bb_pairs
    ]
    saving_summary = summarize_values(savings, seed=1789)
    control_summary = summarize_values(control_times, seed=1793)
    treatment_summary = summarize_values(treatment_times, seed=1801)
    aa_summary = summarize_values(aa_order_effect, seed=1811)
    bb_summary = summarize_values(bb_order_effect, seed=1823)
    order_bias_bound = max(
        abs(float(aa_summary["mean_ms"])), abs(float(bb_summary["mean_ms"]))
    )
    observed_order_noise_bound = max(
        [abs(value) for value in aa_order_effect + bb_order_effect], default=0.0
    )
    mean_saving = float(saving_summary["mean_ms"])
    preparation_ms = float(preparation["wall_ms"])
    lower_bound = float(saving_summary["mean_bootstrap_95pct_ms"][0])
    aa_ci = aa_summary["mean_bootstrap_95pct_ms"]
    bb_ci = bb_summary["mean_bootstrap_95pct_ms"]
    terminal_exact = all(
        pair["comparison"]["terminal_payloads_exact"] for pair in pairs
    )
    canonical_equal = all(
        pair["comparison"]["canonical_envelopes_equal"] for pair in pairs
    )
    function_output_order_equal = all(
        pair["comparison"]["same_function_output_order"] for pair in pairs
    )
    protocol_clean = all(
        not run["protocol_errors"] and run["timing"]["complete"]
        for pair in pairs
        for run in pair["runs"]
    )
    process_clean = all(
        run["exit_code"] == 0
        and run["terminal_calls"] == len(fixture.COMMANDS)
        and run["model_requests"] == len(state_batches_for_pair(pair)) + 1
        and run["diagnostic_mismatch_delta"] == 0
        for pair in pairs
        for run in pair["runs"]
    )
    treatment_runs = [
        run for pair in pairs for run in pair["runs"] if run["arm"] == "treatment"
    ]
    control_runs = [
        run for pair in pairs for run in pair["runs"] if run["arm"] == "control"
    ]
    replay_clean = all(
        run["replay_delta"] == 5 and run["runtime_hits"] == 5
        for run in treatment_runs
    ) and all(run["runtime_hits"] == 0 for run in control_runs)
    order_summaries = {
        order: summarize_values(values, seed=1841 + index)
        for index, (order, values) in enumerate(order_savings.items())
    }
    batches = state_batches_for_pair(pairs[0]) if pairs else ()
    serial_batches = len(batches) == len(fixture.COMMANDS) and all(
        len(batch) == 1 for batch in batches
    )
    per_batch = []
    for batch_index, batch in enumerate(batches):
        batch_control = []
        batch_treatment = []
        batch_savings = []
        for pair in ab_pairs:
            by_arm = {run["arm"]: run for run in pair["runs"]}
            control_value = float(
                by_arm["control"]["timing"]["batch_roundtrip_ms"][batch_index]
            )
            treatment_value = float(
                by_arm["treatment"]["timing"]["batch_roundtrip_ms"][batch_index]
            )
            batch_control.append(control_value)
            batch_treatment.append(treatment_value)
            batch_savings.append(control_value - treatment_value)
        per_batch.append(
            {
                "batch": batch_index + 1,
                "commands": [fixture.COMMANDS[index] for index in batch],
                "control": summarize_values(batch_control, seed=1901 + batch_index),
                "treatment": summarize_values(
                    batch_treatment, seed=1931 + batch_index
                ),
                "paired_savings": summarize_values(
                    batch_savings, seed=1973 + batch_index
                ),
            }
        )
    return {
        "primary_command_roundtrip": {
            "control": control_summary,
            "treatment": treatment_summary,
            "paired_savings": saving_summary,
            "mean_relative_savings": (
                float(saving_summary["mean_ms"]) / float(control_summary["mean_ms"])
                if float(control_summary["mean_ms"]) > 0
                else 0.0
            ),
            "paired_wall_savings": summarize_values(wall_savings, seed=1831),
            "one_time_explicit_preparation_ms": preparation_ms,
            "serialized_setup_plus_first_mean_session_net_savings_ms": (
                float(statistics.fmean(wall_savings)) - preparation_ms
                if wall_savings
                else -preparation_ms
            ),
            "steady_six_command_sessions_to_amortize_preparation": (
                preparation_ms / mean_saving if mean_saving > 0 else None
            ),
            "command_batches_to_amortize_preparation": (
                preparation_ms / (mean_saving / len(batches))
                if mean_saving > 0 and batches
                else None
            ),
            "serial_commands_to_amortize_preparation": (
                preparation_ms / (mean_saving / len(fixture.COMMANDS))
                if mean_saving > 0 and serial_batches
                else None
            ),
            "by_order": order_summaries,
            "per_batch": per_batch,
        },
        "same_arm_order_noise": {
            "control_control_first_minus_second": aa_summary,
            "treatment_treatment_first_minus_second": bb_summary,
            "absolute_mean_order_bias_bound_ms": order_bias_bound,
            "maximum_absolute_observed_order_noise_ms": observed_order_noise_bound,
            "primary_lower_ci_exceeds_maximum_observed_noise": (
                lower_bound > observed_order_noise_bound
            ),
            "control_control_mean_ci_contains_zero": float(aa_ci[0]) <= 0
            <= float(aa_ci[1]),
            "treatment_treatment_mean_ci_contains_zero": float(bb_ci[0]) <= 0
            <= float(bb_ci[1]),
        },
        "invariants": {
            "all_terminal_payloads_exact": terminal_exact,
            "all_canonical_envelopes_equal": canonical_equal,
            "all_function_output_orders_equal": function_output_order_equal,
            "all_protocol_timings_complete": protocol_clean,
            "all_processes_and_counts_valid": process_clean,
            "expected_replay_accounting": replay_clean,
            "zero_diagnostic_mismatches": all(
                run["diagnostic_mismatch_delta"] == 0
                for pair in pairs
                for run in pair["runs"]
            ),
            "paired_savings_ci_above_zero": lower_bound > 0,
            "paired_savings_exceed_same_arm_mean_order_bias": lower_bound
            > order_bias_bound,
            "both_counterbalanced_order_effect_cis_above_zero": all(
                float(summary["mean_bootstrap_95pct_ms"][0]) > 0
                for summary in order_summaries.values()
            ),
            "both_order_effect_cis_exceed_same_arm_mean_order_bias": all(
                float(summary["mean_bootstrap_95pct_ms"][0]) > order_bias_bound
                for summary in order_summaries.values()
            ),
        },
    }


def state_batches_for_pair(pair: dict[str, Any]) -> tuple[tuple[int, ...], ...]:
    # Stored once per report; this helper keeps count validation readable.
    return tuple(tuple(batch) for batch in pair["batches"])


def strip_raw_outputs(pairs: list[dict[str, Any]]) -> None:
    for pair in pairs:
        for run in pair["runs"]:
            run.pop("raw_outputs", None)


def main(args: argparse.Namespace) -> int:
    args.out.mkdir(parents=True, exist_ok=False)
    bundle = args.bundle.resolve()
    for name in ("squire", "squire-codex", "libsquire_runtime.dylib"):
        if not (bundle / name).exists():
            raise FileNotFoundError(bundle / name)
    repo = fixture.create_fixture(args.out)
    batches = SERIAL_BATCHES if args.batching == "serial" else PARALLEL_BATCHES
    preparation = prepare_workspace(bundle, repo)
    state = ServerState(batches=batches)
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler_type(state))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    server_url = f"http://127.0.0.1:{server.server_address[1]}"
    pairs: list[dict[str, Any]] = []
    try:
        for warmup, arm in enumerate(("control", "treatment"), 1):
            run_key = f"warmup-{warmup}-{arm}"
            print(f"warmup: {arm}", file=sys.stderr, flush=True)
            run_one(
                run_key=run_key,
                arm=arm,
                bundle=bundle,
                repo=repo,
                out=args.out,
                base_url=f"{server_url}/{run_key}/v1",
                sandbox=args.sandbox,
                state=state,
                timeout=args.timeout,
            )
        for kind, pair, arms in pair_schedule(
            args.pairs, args.aa_pairs, args.bb_pairs
        ):
            result = run_pair(
                kind=kind,
                pair=pair,
                arms=arms,
                bundle=bundle,
                repo=repo,
                out=args.out,
                server_url=server_url,
                sandbox=args.sandbox,
                state=state,
                timeout=args.timeout,
            )
            result["batches"] = batches
            pairs.append(result)
    finally:
        server.shutdown()
        thread.join(timeout=5)
        server.server_close()

    analysis = analyze(pairs, preparation)
    strip_raw_outputs(pairs)
    report = {
        "design": (
            "same squire-codex binary, deterministic local model, exact commands and "
            "terminal payloads; AB/BA counterbalanced with A/A and B/B controls"
        ),
        "primary_timing_boundary": (
            "fixture response containing command calls sent -> next Responses API request "
            "containing their function outputs received"
        ),
        "bundle": str(bundle),
        "sandbox": args.sandbox,
        "batching": args.batching,
        "commands": fixture.COMMANDS,
        "batches": batches,
        "pair_counts": {
            "ab": args.pairs,
            "aa": args.aa_pairs,
            "bb": args.bb_pairs,
        },
        "preparation": preparation,
        "preparation_accounting_note": (
            "one explicit preparation was measured outside every command interval; "
            "the product normally requests preparation asynchronously"
        ),
        "analysis": analysis,
        "pairs": pairs,
    }
    report_path = args.out / "report.json"
    report_path.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report["analysis"], indent=2))
    return 0 if all(analysis["invariants"].values()) else 1


def ab_arm_order(pair: int) -> tuple[str, str]:
    return (
        ("control", "treatment")
        if pair % 2 == 1
        else ("treatment", "control")
    )


def pair_schedule(
    ab_pairs: int, aa_pairs: int, bb_pairs: int
) -> list[tuple[str, int, tuple[str, str]]]:
    schedule: list[tuple[str, int, tuple[str, str]]] = []
    aa_emitted = 0
    bb_emitted = 0
    for pair in range(1, ab_pairs + 1):
        schedule.append(("AB", pair, ab_arm_order(pair)))
        aa_target = pair * aa_pairs // ab_pairs
        while aa_emitted < aa_target:
            aa_emitted += 1
            schedule.append(("AA", aa_emitted, ("control", "control")))
        bb_target = pair * bb_pairs // ab_pairs
        while bb_emitted < bb_target:
            bb_emitted += 1
            schedule.append(("BB", bb_emitted, ("treatment", "treatment")))
    return schedule


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--bundle", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--pairs", type=int, default=20)
    parser.add_argument("--aa-pairs", type=int, default=6)
    parser.add_argument("--bb-pairs", type=int, default=6)
    parser.add_argument("--batching", choices=("serial", "parallel"), default="serial")
    parser.add_argument(
        "--sandbox",
        choices=("read-only", "workspace-write", "danger-full-access"),
        default="danger-full-access",
    )
    parser.add_argument("--timeout", type=float, default=180)
    args = parser.parse_args()
    if args.pairs < 2 or args.pairs % 2 != 0:
        parser.error("--pairs must be a positive even number of at least 2")
    if args.aa_pairs < 1 or args.bb_pairs < 1:
        parser.error("--aa-pairs and --bb-pairs must be positive")
    return args


if __name__ == "__main__":
    raise SystemExit(main(parse_args()))
