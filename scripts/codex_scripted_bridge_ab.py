#!/usr/bin/env python3
"""Deterministically prove that the Squire bridge preserves Codex tool dispatches.

A local Responses API fixture emits the same fixed exec_command sequence to a
direct squire-codex control and a warmed `squire codex` treatment. The fixture
records the function-call outputs Codex sends back after every command.
"""

from __future__ import annotations

import argparse
from dataclasses import dataclass, field
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import subprocess
import threading
import time
from typing import Any


COMMANDS = [
    "git rev-parse HEAD",
    "git rev-parse --show-toplevel",
    "git ls-files",
    "git ls-files | head -n 2",
    "printf 'native-fallback\\n'",
    "git diff --stat",
]

COMMAND_BATCHES = ((0, 1, 2), (3, 4, 5))


def run(
    argv: list[str],
    cwd: Path,
    env: dict[str, str] | None = None,
    timeout: float = 120,
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


def create_fixture(root: Path) -> Path:
    repo = root / "repo"
    (repo / "src").mkdir(parents=True)
    (repo / "README.md").write_text("# Scripted fixture\n\nneedle alpha\n")
    (repo / "src" / "app.txt").write_text("needle beta\nsecond line\n")
    (repo / "src" / "other.txt").write_text("third line\n")
    commands = [
        ["git", "init", "--quiet"],
        ["git", "add", "."],
        [
            "git",
            "-c",
            "user.name=Squire Test",
            "-c",
            "user.email=squire@example.invalid",
            "commit",
            "--quiet",
            "-m",
            "fixture",
        ],
    ]
    for command in commands:
        proc = run(command, repo)
        if proc.returncode != 0:
            raise RuntimeError(proc.stderr.strip() or f"failed: {' '.join(command)}")
    return repo


def response_event(response_id: str, call_indices: tuple[int, ...] | None) -> bytes:
    events: list[dict[str, Any]] = [
        {"type": "response.created", "response": {"id": response_id}}
    ]
    if call_indices is None:
        events.append(
            {
                "type": "response.output_item.done",
                "item": {
                    "type": "message",
                    "role": "assistant",
                    "id": f"{response_id}-message",
                    "content": [{"type": "output_text", "text": "script complete"}],
                },
            }
        )
    else:
        for call_index in call_indices:
            arguments = json.dumps(
                {
                    "cmd": COMMANDS[call_index],
                    "yield_time_ms": 10_000,
                    "max_output_tokens": 4_000,
                },
                separators=(",", ":"),
            )
            events.append(
                {
                    "type": "response.output_item.done",
                    "item": {
                        "type": "function_call",
                        "call_id": f"script-call-{call_index + 1:02d}",
                        "name": "exec_command",
                        "arguments": arguments,
                    },
                }
            )
    events.append(
        {
            "type": "response.completed",
            "response": {
                "id": response_id,
                "usage": {
                    "input_tokens": 0,
                    "input_tokens_details": None,
                    "output_tokens": 0,
                    "output_tokens_details": None,
                    "total_tokens": 0,
                },
            },
        }
    )
    chunks = []
    for event in events:
        chunks.append(f"event: {event['type']}\n")
        chunks.append(f"data: {json.dumps(event, separators=(',', ':'))}\n\n")
    return "".join(chunks).encode()


@dataclass
class ServerState:
    requests: dict[str, list[dict[str, Any]]] = field(
        default_factory=lambda: {"control": [], "treatment": []}
    )
    protocol_errors: list[str] = field(default_factory=list)
    lock: threading.Lock = field(default_factory=threading.Lock)


def handler_type(state: ServerState) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
            arm = "control" if "/control/" in self.path else "treatment"
            length = int(self.headers.get("content-length", "0"))
            raw = self.rfile.read(length)
            try:
                request = json.loads(raw)
            except json.JSONDecodeError:
                request = {"invalid_json": raw.decode(errors="replace")}
            with state.lock:
                state.requests[arm].append(request)
                index = len(state.requests[arm]) - 1
                if 0 < index <= len(COMMAND_BATCHES):
                    prior_ids = {
                        f"script-call-{call_index + 1:02d}"
                        for call_index in COMMAND_BATCHES[index - 1]
                    }
                    observed_ids = set(walk_function_outputs(request))
                    if not prior_ids.issubset(observed_ids):
                        state.protocol_errors.append(
                            f"{arm} request {index + 1} missing "
                            f"{sorted(prior_ids - observed_ids)}"
                        )
                elif index > len(COMMAND_BATCHES):
                    state.protocol_errors.append(f"{arm} emitted unexpected request {index + 1}")
            call_indices = COMMAND_BATCHES[index] if index < len(COMMAND_BATCHES) else None
            body = response_event(f"{arm}-response-{index + 1:02d}", call_indices)
            self.send_response(200)
            self.send_header("content-type", "text/event-stream")
            self.send_header("content-length", str(len(body)))
            self.send_header("connection", "close")
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
            body = json.dumps({"data": []}).encode()
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(body)))
            self.send_header("connection", "close")
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, _format: str, *_args: Any) -> None:
            return

    return Handler


def clean_environment() -> dict[str, str]:
    env = os.environ.copy()
    for key in list(env):
        if key.startswith("SQUIRE_") or key == "CODEX_ROLLOUT_TRACE_ROOT":
            env.pop(key, None)
    env.pop("DYLD_INSERT_LIBRARIES", None)
    env["TMPDIR"] = "/private/tmp"
    env["GIT_OPTIONAL_LOCKS"] = "0"
    env["MOCK_API_KEY"] = "scripted-test-key"
    return env


def write_codex_config(home: Path, base_url: str) -> None:
    home.mkdir(parents=True, exist_ok=True)
    (home / "config.toml").write_text(
        "\n".join(
            [
                'model = "scripted-model"',
                'model_provider = "scripted"',
                "",
                "[model_providers.scripted]",
                'name = "Squire scripted fixture"',
                f'base_url = "{base_url}"',
                'env_key = "MOCK_API_KEY"',
                'wire_api = "responses"',
                "request_max_retries = 0",
                "stream_max_retries = 0",
                "",
            ]
        )
    )


def status_metrics(squire: Path, repo: Path, env: dict[str, str]) -> dict[str, Any]:
    proc = run([str(squire), "status", "--json"], repo, env)
    if proc.returncode != 0:
        return {}
    try:
        value = json.loads(proc.stdout)
    except json.JSONDecodeError:
        return {}
    metrics = value.get("metrics", {}) if isinstance(value, dict) else {}
    return metrics if isinstance(metrics, dict) else {}


def metric_delta(before: dict[str, Any], after: dict[str, Any], key: str) -> int:
    return int(after.get(key, 0)) - int(before.get(key, 0))


def parse_command_count(path: Path) -> int:
    count = 0
    for line in path.read_text(errors="replace").splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        item = event.get("item")
        if (
            event.get("type") == "item.completed"
            and isinstance(item, dict)
            and item.get("type") == "command_execution"
        ):
            count += 1
    return count


def run_arm(
    arm: str,
    bundle: Path,
    repo: Path,
    out: Path,
    base_url: str,
) -> dict[str, Any]:
    squire = bundle / "squire"
    codex = bundle / "squire-codex"
    env = clean_environment()
    env["PATH"] = os.pathsep.join([str(bundle), env.get("PATH", "")])
    env["CODEX_HOME"] = str(out / "codex-home")
    write_codex_config(Path(env["CODEX_HOME"]), base_url)
    prewarm: dict[str, Any] | None = None
    if arm == "control":
        prefix = [str(codex)]
        env["SQUIRE_CODEX_BRIDGE"] = "0"
    else:
        prepared = run([str(squire), "runtime", "warm", "--short"], repo, env)
        prewarm = {"exit_code": prepared.returncode, "stderr": prepared.stderr}
        prefix = [str(squire), "codex"]
        env["SQUIRE_CODEX_BRIDGE"] = "1"
        env["SQUIRE_CODEX_SQUIRE"] = str(squire)
        env["SQUIRE_CODEX_RUNTIME_LIB"] = str(bundle / "libsquire_runtime.dylib")
    env["SQUIRE_AUTO_PREPARE"] = "0"
    env["SQUIRE_CODEX_AUTO_WARM"] = "0"
    env["SQUIRE_CODEX_BRIDGE_TRACE"] = "1"
    before = status_metrics(squire, repo, env) if arm == "treatment" else {}
    event_path = out / f"{arm}.jsonl"
    stderr_path = out / f"{arm}.stderr"
    command = prefix + [
        "exec",
        "--ephemeral",
        "--json",
        "--color",
        "never",
        "--sandbox",
        "read-only",
        "--cd",
        str(repo),
        "Execute the scripted local fixture.",
    ]
    started = time.perf_counter_ns()
    with event_path.open("w") as stdout, stderr_path.open("w") as stderr:
        proc = subprocess.run(
            command,
            cwd=repo,
            env=env,
            text=True,
            stdout=stdout,
            stderr=stderr,
            check=False,
            timeout=180,
        )
    wall_seconds = (time.perf_counter_ns() - started) / 1_000_000_000
    after = status_metrics(squire, repo, env) if arm == "treatment" else {}
    stderr = stderr_path.read_text(errors="replace")
    return {
        "exit_code": proc.returncode,
        "wall_seconds": wall_seconds,
        "terminal_calls": parse_command_count(event_path),
        "runtime_hits": stderr.count("squire runtime: hit"),
        "replay_delta": metric_delta(before, after, "replays"),
        "diagnostic_mismatch_delta": metric_delta(
            before, after, "diagnostic_mismatches"
        ),
        "prewarm": prewarm,
    }


def walk_function_outputs(value: Any) -> dict[str, Any]:
    found: dict[str, Any] = {}
    if isinstance(value, dict):
        if value.get("type") == "function_call_output" and isinstance(
            value.get("call_id"), str
        ):
            found[value["call_id"]] = value.get("output")
        for member in value.values():
            found.update(walk_function_outputs(member))
    elif isinstance(value, list):
        for member in value:
            found.update(walk_function_outputs(member))
    return found


def ordered_function_output_ids(value: Any) -> list[str]:
    found: list[str] = []
    if isinstance(value, dict):
        if value.get("type") == "function_call_output" and isinstance(
            value.get("call_id"), str
        ):
            found.append(value["call_id"])
        for member in value.values():
            found.extend(ordered_function_output_ids(member))
    elif isinstance(value, list):
        for member in value:
            found.extend(ordered_function_output_ids(member))
    return found


def canonical_output(value: Any, repo: Path) -> Any:
    if not isinstance(value, str):
        return value
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


def sha256(value: Any) -> str:
    return hashlib.sha256(
        json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()


def analyze_requests(
    requests: dict[str, list[dict[str, Any]]], repo: Path
) -> list[dict[str, Any]]:
    outputs = {
        arm: {
            key: value
            for request in arm_requests
            for key, value in walk_function_outputs(request).items()
        }
        for arm, arm_requests in requests.items()
    }
    comparisons = []
    for index, command in enumerate(COMMANDS, 1):
        call_id = f"script-call-{index:02d}"
        control = outputs["control"].get(call_id)
        treatment = outputs["treatment"].get(call_id)
        canonical_control = canonical_output(control, repo)
        canonical_treatment = canonical_output(treatment, repo)
        comparisons.append(
            {
                "call_id": call_id,
                "command": command,
                "observed_control": control is not None,
                "observed_treatment": treatment is not None,
                "exact_equal": control == treatment,
                "canonical_equal": canonical_control == canonical_treatment,
                "control_sha256": sha256(control),
                "treatment_sha256": sha256(treatment),
                "control_canonical_sha256": sha256(canonical_control),
                "treatment_canonical_sha256": sha256(canonical_treatment),
            }
        )
    return comparisons


def output_orders(requests: list[dict[str, Any]]) -> list[list[str]]:
    return [ordered_function_output_ids(request) for request in requests]


def main(args: argparse.Namespace) -> int:
    args.out.mkdir(parents=True, exist_ok=False)
    bundle = args.bundle.resolve()
    repo = create_fixture(args.out)
    state = ServerState()
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler_type(state))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        port = server.server_address[1]
        arms = {}
        for arm in ("control", "treatment"):
            base_url = f"http://127.0.0.1:{port}/{arm}/v1"
            arms[arm] = run_arm(arm, bundle, repo, args.out, base_url)
    finally:
        server.shutdown()
        thread.join(timeout=5)
        server.server_close()

    for arm, requests in state.requests.items():
        for index, request in enumerate(requests, 1):
            path = args.out / f"{arm}-request-{index:02d}.json"
            path.write_text(json.dumps(request, indent=2) + "\n")

    comparisons = analyze_requests(state.requests, repo)
    expected_requests = len(COMMAND_BATCHES) + 1
    request_output_orders = {
        arm: output_orders(requests) for arm, requests in state.requests.items()
    }
    invariants = {
        "same_scripted_tool_count": arms["control"]["terminal_calls"]
        == arms["treatment"]["terminal_calls"]
        == len(COMMANDS),
        "same_model_request_count": len(state.requests["control"])
        == len(state.requests["treatment"])
        == expected_requests,
        "all_model_outputs_observed": all(
            row["observed_control"] and row["observed_treatment"]
            for row in comparisons
        ),
        "all_canonical_outputs_equal": all(
            row["canonical_equal"] for row in comparisons
        ),
        "same_function_output_order": request_output_orders["control"]
        == request_output_orders["treatment"],
        "no_fixture_protocol_errors": not state.protocol_errors,
        "treatment_used_replays": arms["treatment"]["replay_delta"] > 0,
        "no_diagnostic_mismatches": arms["treatment"][
            "diagnostic_mismatch_delta"
        ]
        == 0,
        "all_processes_succeeded": all(
            arm["exit_code"] == 0 for arm in arms.values()
        ),
    }
    report = {
        "design": "scripted local model; same binary, workspace, commands, and request sequence",
        "comparison_note": (
            "canonical output removes volatile timing/chunk metadata and the macOS "
            "sandbox confstr warning; exact hashes retain every model-visible byte"
        ),
        "commands": COMMANDS,
        "arms": arms,
        "model_requests": {arm: len(value) for arm, value in state.requests.items()},
        "request_output_orders": request_output_orders,
        "fixture_protocol_errors": state.protocol_errors,
        "comparisons": comparisons,
        "invariants": invariants,
    }
    (args.out / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    return 0 if all(invariants.values()) else 1


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--bundle", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    return parser.parse_args()


if __name__ == "__main__":
    raise SystemExit(main(parse_args()))
