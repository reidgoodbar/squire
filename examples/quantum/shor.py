#!/usr/bin/env python3
"""Minimal Qiskit Aer example for Squire quantum simulate.

Replace the circuit body with a heavier Shor-style experiment when needed.
"""

from __future__ import annotations

import json
import os
from pathlib import Path

from qiskit import QuantumCircuit, transpile
from qiskit_aer import AerSimulator


def main() -> None:
    shots = int(os.environ.get("SQUIRE_QUANTUM_SHOTS", "1024"))

    circuit = QuantumCircuit(2, 2)
    circuit.h(0)
    circuit.cx(0, 1)
    circuit.measure([0, 1], [0, 1])

    backend = AerSimulator()
    compiled = transpile(circuit, backend)
    result = backend.run(compiled, shots=shots).result()
    payload = {
        "backend": backend.name,
        "shots": shots,
        "counts": result.get_counts(),
    }

    print(json.dumps(payload))

    output_path = os.environ.get("SQUIRE_QUANTUM_OUTPUT_PATH", "").strip()
    if output_path:
        path = Path(output_path)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(payload))


if __name__ == "__main__":
    main()
