#!/usr/bin/env python3

from __future__ import annotations

import importlib.util
from pathlib import Path
import sys
import unittest


SCRIPT_DIR = Path(__file__).parent
sys.path.insert(0, str(SCRIPT_DIR))
SCRIPT = SCRIPT_DIR / "codex_attributable_ab.py"
SPEC = importlib.util.spec_from_file_location("codex_attributable_ab", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
MODULE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


class AttributableABTests(unittest.TestCase):
    def test_terminal_payload_removes_only_codex_timing_envelope(self) -> None:
        value = (
            "Chunk ID: abc123\n"
            "Wall time: 0.0042 seconds\n"
            "Process exited with code 7\n"
            "Original token count: 2\n"
            "Output:\nhello\nworld\n"
        )

        self.assertEqual(
            MODULE.terminal_payload(value),
            {"exit_code": 7, "output": "hello\nworld\n"},
        )
        self.assertIsNone(MODULE.terminal_payload("not a Codex result"))

    def test_phase_timing_isolates_command_roundtrips(self) -> None:
        trace = MODULE.ProtocolTrace(
            request_arrival_ns=[1_100_000, 2_500_000, 4_400_000],
            response_sent_ns=[1_500_000, 3_000_000, 4_600_000],
        )

        timing = MODULE.phase_timing(trace, 1_000_000, 5_000_000, 2)

        self.assertTrue(timing["complete"])
        self.assertEqual(timing["batch_roundtrip_ms"], [1.0, 1.4])
        self.assertAlmostEqual(timing["command_roundtrip_ms"], 2.4)
        self.assertAlmostEqual(timing["startup_ms"], 0.1)
        self.assertAlmostEqual(timing["fixture_response_ms"], 1.1)
        self.assertAlmostEqual(timing["shutdown_ms"], 0.4)

    def test_phase_timing_rejects_incomplete_protocol(self) -> None:
        timing = MODULE.phase_timing(
            MODULE.ProtocolTrace(
                request_arrival_ns=[1_000_000], response_sent_ns=[1_100_000]
            ),
            900_000,
            1_200_000,
            2,
        )

        self.assertFalse(timing["complete"])
        self.assertIsNone(timing["command_roundtrip_ms"])

    def test_ab_order_is_counterbalanced(self) -> None:
        self.assertEqual(MODULE.ab_arm_order(1), ("control", "treatment"))
        self.assertEqual(MODULE.ab_arm_order(2), ("treatment", "control"))
        self.assertEqual(MODULE.ab_arm_order(3), ("control", "treatment"))
        self.assertEqual(MODULE.ab_arm_order(4), ("treatment", "control"))

    def test_same_arm_controls_are_interleaved_across_ab_run(self) -> None:
        schedule = MODULE.pair_schedule(6, 2, 2)

        self.assertEqual([kind for kind, _, _ in schedule].count("AB"), 6)
        self.assertEqual([kind for kind, _, _ in schedule].count("AA"), 2)
        self.assertEqual([kind for kind, _, _ in schedule].count("BB"), 2)
        self.assertLess(
            [kind for kind, _, _ in schedule].index("AA"), len(schedule) - 2
        )
        self.assertLess(
            [kind for kind, _, _ in schedule].index("BB"), len(schedule) - 1
        )

    def test_bootstrap_is_deterministic_and_positive(self) -> None:
        values = [20.0, 22.0, 24.0, 26.0, 28.0, 30.0]
        first = MODULE.bootstrap_ci(values)

        self.assertEqual(first, MODULE.bootstrap_ci(values))
        self.assertGreater(first[0], 0)
        self.assertLess(first[0], 25.0)
        self.assertGreater(first[1], 25.0)


if __name__ == "__main__":
    unittest.main()
