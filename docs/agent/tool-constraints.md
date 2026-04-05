# Squire Tool Constraints

Use this file when an agent needs to choose the right Squire command and stay within the public service constraints.

## Global behavior

- Execution is stateless: fresh container per request, no warm session reuse, workspace cleanup after completion.
- Network is disabled by default inside user workloads.
- Sandboxes run non-root with a read-only rootfs under `runsc`, AppArmor, and seccomp.
- Prefer `--json` when another tool or agent will read the result.
- Artifact-producing commands can download files locally with `--download-artifacts <dir>`.
- Script-style runtimes stage the input file at `SQUIRE_INPUT_PATH` and expect generated files under `SQUIRE_OUTPUT_DIR`.

## Use these first

- `squire --help`
- `squire --help --json`
- `squire <command> --help`

## Validation tools

### `squire verify`

- Purpose: shell, Python, or Node checks across fresh Linux targets.
- Targets: exact target IDs only, such as `alpine-3.20`, `ubuntu-24.04`, and `debian-12`.
- Network: disabled.
- Best for: runtime sanity checks, small repros, OS-sensitive behavior.

### `squire test`

- Purpose: short clean test runs.
- Languages: `python`, `node`, `bash`.
- Network: disabled.
- Best for: small or medium test loops, not full CI pipelines.

### `squire lint`

- Purpose: fixed lint/static-analysis runs in clean toolchains.
- Supported tools: `ruff`, `eslint`, `clippy`.
- Network: disabled.
- Best for: fresh lint output without local environment drift.

### `squire compile`

- Purpose: compile-target checks for Go or Rust.
- Network: disabled.
- Outputs: structured per-target compiler results, not runnable binaries.
- Scope: small projects and target sanity checks, not full repo CI.

### `squire sql`

- Purpose: ephemeral SQLite or Postgres validation.
- Network: disabled.
- SQLite: local CLI inside the sandbox.
- Postgres: fresh request-scoped database helper inside the sandbox.
- Best for: schema, migration, and query validation in a clean DB.

### `squire audit`

- Purpose: secret scanning and local-config static analysis.
- Network: disabled.
- Public service: dependency audit and remote Semgrep configs are disabled.
- Best for: secret scanning and small static checks against staged local files.

### `squire solve`

- Purpose: bounded Z3 or MiniZinc solver runs.
- Network: disabled.
- Best for: satisfiability or small optimization/model checks.

### `squire quantum simulate`

- Purpose: offline Qiskit Aer simulation in a dedicated runtime.
- Runtime: Python 3.11 with `qiskit` and `qiskit-aer`.
- Staging contract: the first `--file` is the entry script. Helper modules or local assets can be staged with additional `--file` values.
- Output contract: write files under `SQUIRE_QUANTUM_OUTPUT_DIR` or directly to `SQUIRE_QUANTUM_OUTPUT_PATH`.
- Network: disabled.
- Public service: available anonymously, still offline-only.
- Best for: bounded simulation jobs, not notebooks or hardware backends.

## Offload tools

### `squire data`

- Purpose: Python data jobs in a disposable remote runtime.
- Runtime: Python 3.11 with `pandas`, `polars`, and `pyarrow`.
- Input contract: read the staged input file from `SQUIRE_INPUT_PATH`, or use `--stdin` for small text payloads.
- Output contract: write generated files to `SQUIRE_OUTPUT_DIR`.
- Network: disabled.
- Best for: CSV/Parquet transforms and medium-weight data work that is awkward locally.

### `squire media`

- Purpose: Python media jobs in a disposable remote runtime.
- Runtime: Python 3.11 with `ffmpeg` installed.
- Input contract: read the staged media input from `SQUIRE_INPUT_PATH`.
- Output contract: write generated files to `SQUIRE_OUTPUT_DIR`.
- Network: disabled.
- Best for: ffmpeg-based transforms, padding, clipping, transcodes, and other offline media tasks.
- Note: do not assume Pillow or other extra Python imaging packages are available unless documented by the runtime.

### `squire browser`

- Purpose: constrained headless Chromium verification.
- Network: disabled.
- Public service: offline-only. Remote `http://` and `https://` URLs are rejected.
- Staging: use `--path` for full local asset trees with images, fonts, and other binaries.
- Output: screenshots and other artifacts can be downloaded with `--download-artifacts`.

### `squire build`

- Purpose: offline packaging/build sanity checks.
- Languages: `python`, `node`.
- Network: disabled.
- Output: per-target build artifacts. Use `--download-artifacts` to fetch them locally.
- Scope: packaging validation only, not publishing or release orchestration.

### `squire bench`

- Purpose: short comparative timing runs.
- Network: disabled.
- Scope: lightweight timing checks, not full performance engineering or load testing.

## Special cases

### `squire deps`

- Exists in the CLI, but is currently disabled on the public service under the zero-egress policy.
- Do not plan workflows around it on the public service.

### `squire scale`

- Compatibility alias for `squire data` and `squire media`.
- Prefer new usage of `squire data` or `squire media`.

## Artifact downloads

Commands that may return downloadable files:

- `squire data`
- `squire media`
- `squire browser`
- `squire build`
- `squire quantum simulate`

Use:

```bash
squire media --script square.py --input image.png --download-artifacts ./out --json
```

Behavior:

- the CLI makes the artifact requests for you
- top-level artifact commands download into the exact directory you provide
- `build` uses target-specific subdirectories under that directory
- large binary outputs should be downloaded as artifacts, not relayed through stdout

## Practical guidance for agents

- Prefer local execution for tiny trivial checks.
- Prefer Squire when correctness depends on a fresh environment or a special runtime.
- Prefer the smallest staged input that proves the point.
- For browser work, stage the whole asset tree instead of relying on remote URLs.
- For data/media/quantum, treat stdout as logs or summaries and treat generated files as artifacts.
