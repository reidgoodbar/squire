#!/usr/bin/env python3

from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import statistics
import sys
import tempfile
from types import SimpleNamespace
import unittest


SCRIPT = Path(__file__).with_name("codex_call_amplification_ab.py")
SPEC = importlib.util.spec_from_file_location("codex_call_amplification_ab", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
MODULE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


class CallAmplificationAnalysisTests(unittest.TestCase):
    def test_treatment_replay_coverage_accepts_exact_threshold(self) -> None:
        coverage = MODULE.replay_coverage("treatment", 10, 5, 0.50)

        self.assertEqual(coverage["hit_rate"], 0.50)
        self.assertTrue(coverage["accounting_valid"])
        self.assertTrue(coverage["passed"])

    def test_treatment_replay_coverage_rejects_below_threshold(self) -> None:
        coverage = MODULE.replay_coverage("treatment", 10, 4, 0.50)

        self.assertEqual(coverage["hit_rate"], 0.40)
        self.assertTrue(coverage["accounting_valid"])
        self.assertFalse(coverage["passed"])

    def test_treatment_replay_coverage_rejects_empty_denominator(self) -> None:
        coverage = MODULE.replay_coverage("treatment", 0, 0, 0.50)

        self.assertIsNone(coverage["hit_rate"])
        self.assertTrue(coverage["accounting_valid"])
        self.assertFalse(coverage["passed"])

    def test_treatment_replay_coverage_rejects_hit_overcount(self) -> None:
        coverage = MODULE.replay_coverage("treatment", 3, 4, 0.50)

        self.assertGreater(coverage["hit_rate"], 1.0)
        self.assertFalse(coverage["accounting_valid"])
        self.assertFalse(coverage["passed"])

    def test_control_replay_coverage_is_not_gated(self) -> None:
        coverage = MODULE.replay_coverage("control", 0, 0, 0.50)

        self.assertIsNone(coverage["hit_rate"])
        self.assertTrue(coverage["accounting_valid"])
        self.assertTrue(coverage["passed"])

    def test_workspace_modes_are_explicit(self) -> None:
        spec = MODULE.RepoSpec("flask", Path("/tmp/base"), "prompt")
        isolated = SimpleNamespace(out=Path("/tmp/out"), reuse_workspaces=False)
        reused = SimpleNamespace(out=Path("/tmp/out"), reuse_workspaces=True)

        self.assertEqual(
            MODULE.benchmark_workspace(isolated, spec, 3),
            Path("/tmp/out/repos/flask-pair03"),
        )
        self.assertEqual(
            MODULE.benchmark_workspace(reused, spec, 3),
            Path("/tmp/out/repos/flask-shared"),
        )

    def test_isolated_home_disables_dynamic_tool_surfaces(self) -> None:
        with tempfile.TemporaryDirectory() as raw_root:
            root = Path(raw_root)
            auth = root / "auth.json"
            auth.write_text("{}\n")
            out = root / "result"
            out.mkdir()

            home = MODULE.setup_isolated_codex_home(out, auth)
            config = (home / "config.toml").read_text()

            self.assertIn("apps = false", config)
            self.assertIn("plugins = false", config)
            self.assertIn("remote_plugin = false", config)
            self.assertIn("tool_suggest = false", config)
            self.assertTrue((home / "auth.json").is_symlink())

    def test_bootstrap_handles_power_of_two_sample_size(self) -> None:
        values = [-7.0, -3.0, -1.0, 0.0, 2.0, 4.0, 6.0, 11.0]
        low, high = MODULE.bootstrap_ci(values)

        self.assertLess(low, statistics.fmean(values))
        self.assertGreater(high, statistics.fmean(values))
        self.assertGreater(high - low, 1.0)
        self.assertEqual((low, high), MODULE.bootstrap_ci(values))

    def test_request_canonicalization_removes_only_run_identity(self) -> None:
        repo = Path("/private/tmp/repo")
        request = {
            "prompt_cache_key": "session-a",
            "client_metadata": {
                "session_id": "session-a",
                "thread_id": "thread-a",
                "turn_id": "turn-a",
                "x-codex-window-id": "window-a",
                "x-codex-turn-metadata": json.dumps(
                    {
                        "session_id": "session-a",
                        "turn_started_at_unix_ms": 123,
                        "sandbox": "seatbelt",
                        "workspaces": {str(repo): {"has_changes": False}},
                    }
                ),
                "x-codex-installation-id": "stable-installation",
            },
            "input": [{"text": f"inspect {repo}/src/app.py"}],
        }

        canonical = MODULE.canonicalize_model_value(request, repo)

        self.assertNotIn("prompt_cache_key", canonical)
        self.assertEqual(
            canonical["client_metadata"],
            {
                "x-codex-installation-id": "stable-installation",
                "x-codex-turn-metadata": {
                    "sandbox": "seatbelt",
                    "workspaces": {"<REPO>": {"has_changes": False}},
                },
            },
        )
        self.assertEqual(canonical["input"][0]["text"], "inspect <REPO>/src/app.py")

    def test_plain_rg_result_canonicalization_ignores_native_line_order(self) -> None:
        repo = Path("/private/tmp/repo")
        first = {
            "type": "code_mode_response",
            "value": {"exit_code": 0, "output": "b.py:2:match\na.py:1:match\n"},
        }
        second = {
            "type": "code_mode_response",
            "value": {"exit_code": 0, "output": "a.py:1:match\nb.py:2:match\n"},
        }
        command = 'rg -n "match|other" . --glob "!vendor/**"'

        self.assertTrue(MODULE.is_plain_unordered_rg_command(command))
        self.assertEqual(
            MODULE.canonicalize_tool_result(command, first, repo),
            MODULE.canonicalize_tool_result(command, second, repo),
        )

    def test_plain_rg_with_stderr_discard_remains_unordered(self) -> None:
        command = 'rg -n "match|other" README.md missing docs 2>/dev/null'

        self.assertTrue(MODULE.is_plain_unordered_rg_command(command))

    def test_order_sensitive_rg_forms_remain_byte_strict(self) -> None:
        repo = Path("/private/tmp/repo")
        first = {"value": {"exit_code": 0, "output": "b\na\n"}}
        second = {"value": {"exit_code": 0, "output": "a\nb\n"}}
        commands = (
            'rg -n "match|other" . | head -n 1',
            'rg -n -C 2 "match|other" .',
            'rg -n --sort path "match|other" .',
            'rg --passthru "match|other" .',
            'rg -n "match|other" .\nhead -n 1 README.md',
        )

        for command in commands:
            with self.subTest(command=command):
                self.assertFalse(MODULE.is_plain_unordered_rg_command(command))
                self.assertNotEqual(
                    MODULE.canonicalize_tool_result(command, first, repo),
                    MODULE.canonicalize_tool_result(command, second, repo),
                )

    def test_tool_records_are_attributed_to_inference_windows(self) -> None:
        with tempfile.TemporaryDirectory() as raw_root:
            root = Path(raw_root)
            payloads = root / "payloads"
            payloads.mkdir()
            raw_payloads = {}
            for index, command in enumerate(("git status --short", "git ls-files"), 1):
                invocation = {
                    "payload": {
                        "arguments": json.dumps({"cmd": command}),
                    }
                }
                result = {"output": f"result-{index}"}
                invocation_path = f"payloads/invocation-{index}.json"
                result_path = f"payloads/result-{index}.json"
                (root / invocation_path).write_text(json.dumps(invocation))
                (root / result_path).write_text(json.dumps(result))
                raw_payloads[f"invocation-{index}"] = {"path": invocation_path}
                raw_payloads[f"result-{index}"] = {"path": result_path}

            state = {
                "raw_payloads": raw_payloads,
                "inference_calls": {
                    "inference-1": {"execution": {"started_seq": 6}},
                    "inference-2": {"execution": {"started_seq": 20}},
                },
                "tool_calls": {
                    "tool-1": {
                        "tool_call_id": "tool-1",
                        "execution": {"started_seq": 8},
                        "raw_invocation_payload_id": "invocation-1",
                        "raw_result_payload_id": "result-1",
                    },
                    "tool-2": {
                        "tool_call_id": "tool-2",
                        "execution": {"started_seq": 22},
                        "raw_invocation_payload_id": "invocation-2",
                        "raw_result_payload_id": "result-2",
                    },
                },
            }

            records = MODULE.extract_tool_records(state, root, root)

            self.assertEqual([row["inference_ordinal"] for row in records], [0, 1])
            self.assertEqual(
                [row["command"] for row in records],
                ["git status --short", "git ls-files"],
            )


if __name__ == "__main__":
    unittest.main()
