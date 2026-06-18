# Squire Kernel v1

Squire Kernel v1 is an active, transparent local kernel for agent-chosen
operations. It preserves agent intent: models (for example, Codex or Claude)
still decide what to do; Squire only serves exact, provable local outputs and
falls back to native execution when proof is missing.

The core principle is:

> Agent chooses. Squire serves.

This repository is the fresh kernel implementation. Deprecated report-first,
wrapped-first, and trace-dashboard experiments have been removed from this
workspace.

The top-level contract for this baseline is
[`SQUIRE_KERNEL_CONTRACT.md`](SQUIRE_KERNEL_CONTRACT.md). Product language must
stay scoped: "Scoped kernel proof for repeated local Git metadata plus
hot-prepared deterministic read-only discovery operations."

Release readiness checks live in
[`RELEASE_CHECKLIST.md`](RELEASE_CHECKLIST.md).

## Quickstart (first-use)

1. Initialize local state: `squire setup`
2. Start the resident maintainer: `squire kernel maintain --background --short`
3. Prime the current repo once: `squire kernel warm --short`
4. Run an agent-chosen command through the kernel:
   - `squire kernel run -- git rev-parse HEAD`
5. Check readiness and enabled fast paths: `squire kernel status --short`

## Scope

Squire improves how the existing local environment serves operations the agent
already chose to run, and only when behavior preservation is proven. It must
work without OpenTelemetry; OTel is optional session metadata only and is not
required for correctness or operation.

## Hard Boundaries

- No new agent tools.
- No MCP tool injection.
- No prompt changes.
- No model routing.
- No agent-visible command suggestions.
- No validation skipping.
- No edit replay.
- No mutating command replay.
- No package install or fetch replay.
- Native fallback always.
- Validation, build, and test commands are never replayed.
- Edits are effects, never replay targets.
- Mutating commands are never replay targets.

## Architecture Levels

Level 0, Observe:

- Observe repo, file, and process state.
- Record evidence quality.
- No acceleration required.

Level 1, Prepare:

- Maintain repo and world state.
- Precompute safe local metadata and indexes.
- Track invalidation epochs.
- Do not alter command execution.

Level 2, Transparent Fast Path:

- For a tiny allowlist only, serve exact stdout, stderr, and exit code for an
  agent-chosen command if validity and exactness are proven.
- Native fallback always exists.
- No semantic approximation.
- No validation, edit, or mutating-command replay.

## CLI

`squire setup`

- Initializes local config and store.
- Initializes the repo oracle.
- Prints privacy mode.
- Does not install global command shims by default.

`squire version [--short|--json]`

- Prints Squire Kernel build identity.
- Release builds can set version, commit, and date with Go linker flags.
- Human-readable output is the default. Use `--json` for automation.

`squire kernel status [--short]`

- Shows repo oracle status.
- Shows world state and invalidation epochs.
- Shows enabled fast paths, proof-gated replay candidates, native-only
  discovery operations, and never-replay policy.
- Shows prepared world counts from `squire kernel warm`.
- Shows the mmap virtual workspace snapshot descriptor counts and payload size.
- Shows background maintainer process status when started.
- Shows observe-only process guard diagnostics.
- Use `--short` for a one-screen readiness view.

`squire kernel run -- <command> [args...]`

- Runs the agent-chosen command through Squire Kernel.
- First tries the foreground CLI mmap hot-client path before constructing the
  full kernel object, loading ledgers, or touching the daemon/socket path.
- Writes exact stdout and stderr bytes and exits with the exact native or replay
  exit code.
- Records replacement/fallback counters in the local ledger.
- After bounded file-inspection commands, may launch a short-lived local helper
  process to prewarm adjacent read windows, ecosystem follow-up metadata, local
  version/path probes, and eligible file bytes during the agent thinking window.
  This does not change the current command result, expose suggestions, or make
  future file-inspection commands replayable unless exact local proof and output
  bytes are available.

`squire kernel warm [--short|--json]`

- Prepares local world state for likely future operations.
- Precomputes exact outputs only for the existing enabled metadata fast paths.
- Concurrently prewarms bounded proof-gated read-only candidates, including
  repo-summary commands, workspace source/config inspection, well-known manifest
  reads, eligible workspace file bytes for bounded `cat`/`sed -n` replay, tool
  version probes, and command-path lookups.
- Publishes a bounded read-only Level 3 workspace image into the mmap hot
  snapshot so future eligible `cat` and `sed -n` windows can materialize from
  proven bytes without per-command file I/O.
- Records hash-only file-tree and project metadata fingerprints for future
  proof work.
- Records hash-only PATH executable index data for future command-path
  discovery proof work.
- Records hash-only ecosystem metadata groups around manifests and lockfiles.
- Does not add replay operators, suggest commands, alter prompts, or expose
  agent-visible guidance.
- JSON is the default output for automation. Use `--short` for a compact
  human-readable summary.

`squire kernel warm` is deterministic environment optimization. It uses idle
local time to prepare proofs and indexes that may be useful if the agent later
chooses matching operations. It does not predict commands in a way visible to
the agent and does not change command execution.

Process guard telemetry is observe-only. Squire may report local process/FD
diagnostics in status surfaces, but it never kills processes, closes file
descriptors, or mutates the process tree.

`squire kernel maintain --once [--short|--json]`

- Runs one bounded resident-maintainer cycle.
- Refreshes repo/world state.
- Invalidates by changed world/proof signals.
- Prewarms enabled fast paths and proof-gated read-only discovery outputs.
- Does not expose suggestions to the agent.
- JSON is the default output for automation. Use `--short` for a compact
  human-readable summary.

`squire kernel maintain --duration <duration> [--poll-interval <duration>] [--short|--json]`

- Runs the same maintainer loop until the duration expires.
- Polls with a bounded interval.
- Skips prewarm work when the local proof signal is unchanged.
- Rewarms when HEAD, config, index, workspace, manifest, PATH, environment, or
  executable identity signals change.

`squire kernel maintain --background [--duration <duration>] [--poll-interval <duration>] [--short|--json]`

- Starts the same maintainer loop in a separate local process.
- Writes PID/status/log metadata into the local store.
- Serves a local Unix-socket hot cache for exact replay from the resident
  prepared world when the OS supports it.
- Returns immediately so prewarm work can overlap the agent thinking/generation
  window instead of adding command-path latency.
- A second start detects the already-running maintainer and does not spawn a
  duplicate process.
- JSON is the default output for automation. Use `--short` for a compact
  human-readable start/status summary.

`squire kernel maintain --background-status [--short|--json]`

- Shows whether the separate maintainer process is running.
- Shows PID, store path, hot-cache socket path, status path, and log path when
  available.
- JSON is the default output for automation. Use `--short` for a compact
  human-readable status.

`squire kernel maintain --stop [--short|--json]`

- Requests termination of the separate maintainer process.
- Does not kill unrelated processes.
- JSON is the default output for automation. Use `--short` for a compact
  human-readable stop result.

`squire boost status [--short|--json]`

- Shows enabled accelerators, replacements, fallbacks, mismatches,
  invalidations, and ROI history when available.
- Human-readable output is the default. Use `--json` for automation.

`squire boost bench repo-metadata [--short|--json]`

- Runs a local scoped benchmark for enabled repo metadata fast paths.
- Reports exactness, mutation-boundary invalidation, workload-only wall delta,
  and net ROI.
- Makes no broad Codex speedup claim.
- JSON is the default output for automation. Use `--short` for a compact
  human-readable summary.

`squire boost bench deep-local [--short|--json]`

- Runs a deeper local benchmark with synthetic repo updates and branch changes.
- Reports metadata, proof-gated replay, native-only discovery, and validation
  metrics separately.
- Reports performance budget checks and makes no broad Codex speedup claim.
- JSON is the default output for automation. Use `--short` for a compact
  human-readable summary.

## CI And Nightly

Normal CI stays fast:

- `go test ./...`
- `squire boost bench repo-metadata`

The nightly/manual workflow runs the default `deep-local` profile and asserts:

- safety gates
- invalidation gates
- native-only discovery boundary reporting

Nightly reports repo-summary proof-gated diagnostics and native-only discovery
diagnostics separately from enabled metadata fast-path exactness. Performance
gates are reported with violations, but a current `needs_optimization` result
does not fail nightly unless it also violates safety, invalidation, or
never-replay boundaries.

## Current Operator Sets

Enabled fast paths:

- `git rev-parse HEAD`
- `git rev-parse --git-dir`
- `git rev-parse --abbrev-ref HEAD`
- `git rev-parse --show-toplevel`
- `git rev-parse --is-inside-work-tree`

The fast path also accepts conservative normalized Git forms that preserve the
same command semantics, including:

- `git -c core.hooksPath=/dev/null -c core.fsmonitor=false rev-parse HEAD`
- `git -C <repo> rev-parse HEAD`

Safe Git global normalization is used only for policy and proof keys. Native
fallback still executes the original agent-chosen argv.

Native-only discovery:

- `git remote -v`
- `git remote get-url origin`
- `rg --files`
- `rg <literal> <workspace paths...>`

Proof-gated replay candidates:

- `git ls-files`
- `git status --short`
- `git status --porcelain`
- `git diff`
- `git diff --stat`
- `git diff -- <relative source/config path>`
- `cat <bounded workspace source/config file>`
- `sed -n <bounded-range>p <bounded workspace source/config file>`
- `<tool> --version` and `<tool> version` for common local tools
- `pip/pip3 --version`
- `which <common-tool>`
- `command -v <common-tool>` for external PATH executables only

Proof-gated candidates replay only from hot-prepared records maintained by
`squire kernel warm` or the background maintainer. The foreground serving path
checks the prepared command key, cheap hot fingerprints, hot invalidation epoch,
and in-memory output bytes before replaying. If any element is missing or stale,
native execution wins immediately.

The foreground CLI path first checks a daemon-published mmap hot snapshot before
constructing the full kernel object. If that exact read-only snapshot proof
hits, the CLI writes the cached stdout/stderr bytes and exits with the cached
exit code immediately. If the CLI hot client misses, the regular kernel path
checks resident in-memory prepared output and then the bounded resident
hot-cache daemon when it is running. The mmap snapshot is a local, owner-only,
atomically published file with fixed-size descriptors laid out for
cache-friendly lookup. It avoids foreground ledger hydration and Unix-socket
round trips on hits, but it still uses normal OS memory mapping and still
requires local invalidation proof; it is not a literal kernel bypass. If the
snapshot, resident cache, or daemon is unavailable, misses, times out, returns
invalid hashes, or fails the hot proof, native execution wins.

The preferred production shape is a long-lived foreground client plus a
resident background maintainer. The long-lived foreground keeps the hot-cache
connection open, avoids per-command process startup, and maintains short
session-local caches for daemon-unavailable and exact-command miss cases. These
caches are fault-open: they only skip replay attempts briefly and never skip
native execution.

Repo-summary candidates are default hot-prepared replay candidates when a
complete local proof is available:

- `git ls-files` is keyed by the Git index, relevant Git config, cwd, exact
  argv, and Git executable identity.
- `git status --short`, `git status --porcelain`, and supported `git diff`
  forms are keyed by the Git index, relevant Git config/attributes, exact argv,
  cwd, executable identity, and a bounded exact workspace tree/content proof.
- `rg --files` remains native-only in this baseline because native ordering and
  ignore semantics are not yet proven as cheap exact replay inputs.

If the workspace proof would be too expensive, output is too large, a command
uses unsupported flags, or any proof element is missing, native execution wins.

Workspace file inspection replay is limited to safe relative paths inside the
workspace, regular files below the bounded size limits, non-hidden/VCS paths,
and source/config extensions or well-known project metadata such as `go.mod`,
`package.json`, lockfiles, `Cargo.toml`, `pyproject.toml`, `tsconfig.json`, and
`Makefile`. Exact command observations are keyed to the exact argv, relative
path, file content hash, size, and mode. Warm-file entries are keyed to the
relative path, file content hash, size, and mode, and can materialize arbitrary
eligible bounded `sed -n` windows or bounded `cat` output from the same proven
file bytes. Unrelated workspace edits do not invalidate a warm file. `.env`,
hidden paths, likely secret/token/key files, absolute paths, parent-directory
escapes, and unknown binary formats remain native.

Tool version and command-path discovery are keyed to hashed PATH, selected
version-affecting environment variables, and executable identity signals.
`command -v` replay is limited to simple external PATH executable lookups; shell
aliases, functions, and shell-specific startup state remain native.

Proof-gated replay has its own benchmark metric
`proof_gated_replay_p95_us`; it is not folded into the metadata fast-path p95
gate.

Level 1 prepared, not replayed:

- hash-only workspace file-tree index
- hash-only well-known project metadata fingerprints
- hash-only PATH executable index
- hash-only ecosystem metadata groups around manifests and lockfiles
- hash-only dependency proof seeds for Node, Python, Go, and Rust manifests,
  lockfiles, tool identity, and candidate dependency-list commands
- hash-only top-level source symbol/import indexes for common source languages
- observe-only process/FD diagnostics

Never replay:

- validation/build/test commands
- edits and formatters that write files, including `gofmt -w`
- mutating Git commands
- package installs and package fetches
- `.env`, `printenv`, sensitive file reads, unknown binary reads, shell aliases,
  and shell functions
- process cleanup or file-descriptor mutation

## Deterministic Environment Optimization

`squire kernel warm` implements Level 1 preparation plus the production-safe
Level 3 read-only workspace image. It may run during idle local time to prepare
proofs, fingerprints, and bounded eligible file bytes for operations the agent
might later choose, but it never exposes suggestions to the agent and never
changes command selection.

`squire kernel maintain` is the resident version of the same mechanism. The
production path is a separate background process started with
`squire kernel maintain --background`, so prewarm work can overlap idle
thinking/generation windows instead of being charged to the command-serving hot
path. The background maintainer also owns the resident hot-cache socket used by
fresh foreground processes. For lowest latency, the foreground serving process
should also be long-lived so it can reuse that socket and its session-local
negative caches. The foreground `--once` and `--duration` modes are for manual
inspection, tests, and scoped diagnostics. The maintainer keeps the local proof
cache fresh through a bounded polling loop and only runs prewarm work when a
hashed proof signal changes.

Current warm outputs are split by replay eligibility:

- Existing Git metadata fast-path outputs are replay eligible because exact
  stdout, stderr, exit code, input fingerprints, and invalidation epochs are
  stored and proven.
- Proof-gated warm outputs are replay eligible only while their specific proof
  inputs still match. Warm runs them in a bounded worker pool as local native
  subprocesses and stores exact output bytes locally.
- Warm-file entries store eligible workspace file bytes locally and can replay
  bounded `cat` or arbitrary eligible `sed -n A,Bp` windows without precomputing
  every range, while the file path, content hash, size, and mode proof still
  match.
- The mmap hot snapshot stores fixed-size binary descriptors for exact command
  outputs and workspace-image files. `squire kernel status` parses this
  structured descriptor table directly for internal diagnostics.
- File-tree, project metadata, PATH, ecosystem, and process guard preparations
  are not replay eligible. They store hashes, counts, and diagnostics only.

This keeps the product claim scoped: Squire can currently accelerate repeated
local Git metadata operations plus hot-prepared deterministic read-only
discovery operations such as bounded workspace file inspection, well-known
manifest reads, arbitrary bounded `sed -n` windows over warmed eligible files,
tool version probes, and command-path lookups. Broader deterministic
environment optimization is preparation and evidence gathering until exactness,
cheap validity checks, and privacy are proven.

The general rule for future replay candidates is:

- The operation must be read-only, deterministic, local, and bounded.
- The exact argv and cwd must be part of the proof key.
- Every input that can affect stdout, stderr, or exit code must have a cheap hot
  proof: content hash, environment hash, executable identity, repo epoch, or an
  equivalent local signal.
- Exact stdout, stderr, and exit code must come from a native warm observation
  or an already proven exact cache entry.
- If shell state, network state, package state, validation behavior, mutations,
  or privacy cannot be cheaply proven, native execution wins.

## Benchmarking And Replay Behavior

This section documents current Squire Kernel benchmark and replay behavior.

Current dogfood measurements from this repo on 2026-06-18:

- Foreground CLI hot-client probe, 11 warmed local-discovery commands:
  - Native wall time: `59.081ms`
  - Squire wall time: `29.999ms`
  - User-visible wall saved: `29.083ms`
  - Replays: `11/11`
  - Proof path: `cli-mmap-hot-snapshot`
- Same probe with debug result frames enabled:
  - Native wall time: `57.630ms`
  - Squire wall time: `30.224ms`
  - Kernel-phase saved: `51.847ms`
  - Replays: `11/11`
- Earlier pre-hot-client CLI probe:
  - Squire wall time: `133.173ms`
  - The regression source was foreground CLI/process setup and store-root
    discovery overhead, not replay exactness. The CLI mmap hot client fixed
    this by checking the hot snapshot before constructing the full kernel.
- Deep-local benchmark after the hot-client work:
  - Safety gates: pass
  - Exactness: true
  - Enabled fast-path mismatches: `0`
  - Stale HEAD replays: `0`
  - Stale branch replays: `0`
  - Validation replays: `0`
  - Metadata workload delta: `+3.105s` across `1167` metadata runs
  - Metadata fast-path p95: `1931us`
  - Current performance status: `needs_optimization` because native fallback
    overhead p95 remains over budget in the deep-local profile.

- Benchmark metrics are split by operator family:
  - `metadata exactness/boost` - measures exactness and ROI for metadata fast paths.
  - `proof-gated replay` - measures hot-prepared read-only discovery replay,
    including repo-summary commands when bounded exact proofs are available.
  - `repo-summary fallback` - verifies status/list/search/diff operations fall
    back native when a complete cheap proof is unavailable.
  - `native-only discovery` - verifies non-replay discovery commands run native.
  - `validation non-replay` - measures validation workloads which are not replayed.

- Runtime decision policy:
  - Squire either serves an exact replay from a maintained proof or runs the
    agent-chosen command natively.
  - Runtime decisions are replay or native; there is no "shadow" execution
    mode.

- Deep-local CLI command:
  - `squire boost bench deep-local` - runs a deeper local benchmark that
    aggregates operator-family metrics and native/replay statistics for on-disk
    operations.

- Search/list/diff semantics:
  - `git ls-files`, `git status --short`, `git status --porcelain`,
    and supported `git diff` forms replay only from hot-prepared
    exact native observations and only while their bounded proof still matches.
  - `rg --files` and literal `rg` discovery remain native-only in this baseline.
  - Unsupported flags, missing proof inputs, oversized output, or expensive
    workspace proof requirements fall back to native.

- Validation / build / test policy:
  - Validation, build, and test commands are never replayed. Exact stdout
    comparison is not used as proof of replayability; these workloads remain
    governed by the never-replay policy.

Deep-local benchmark report surface (scoped claim)

- The `deep-local` benchmark produces a tiered report separating:
  - `metadata` operator-family exactness, replay/native counts, and workload delta;
  - `proof-gated` replay/native counts and exactness; and
  - `validation` operator-family runs and `never`-replay observations.

- The report exposes safety vs performance gates:
  - Safety gates are derived from exactness and `never`/validation observations and must pass for any replacement claim.
  - Performance gates are budget checks (P95 budgets) and may fail independently while safety gates pass; benchmarks must report violations but do not claim broad speedups.
  - Metadata fast-path p95 and proof-gated hot-prepared replay p95 are reported separately.

- The report separates `enabled` (metadata fast path), `proof-gated`,
  `repo-summary fallback`, `native-only discovery`, and `never` sets in its
  operator-family buckets. Examples include enabled metadata fast paths,
  hot-prepared repo summaries and manifest/tool/path reads, native-only remote
  and search discovery, and never-replay validation runs.

- Consistency note: some derived counts use different denominators: metadata exactness/fast-path rates use metadata-run denominators, while `invalidation-native` and `native-mode` counters may use incremental-sampled denominators or per-turn aggregations; native-mode and invalidation-native counts are therefore not strictly commensurate and should be interpreted with their reported denominators.

## Core Invariants (unchanged)

1. Agent chooses. Squire serves.
2. Native fallback always exists.
3. Validation is never replayed.
4. Edits are never replayed.
5. Mutating commands are never replayed.
6. Runtime decisions are replay or native; there is no "shadow" execution mode.
7. Exact stdout/stderr/exit-code equality is required for any replacement.
8. Every replay needs an invalidation proof.
9. Every derived fact has evidence quality.
10. Local world state proves validity.
11. Standard mode avoids raw sensitive content.

## Production-Safe Levels 3-5

These are the production-safe parts of the Level 3-5 direction. They preserve
the kernel contract, keep native filesystem state authoritative, and prioritize
safety, auditability, and native fallback.

- Level 3, Virtual Memory-Mapped Workspace: The production-safe subset is
  implemented as read-only workspace acceleration. Bounded eligible file bytes
  are warmed into a daemon-published mmap hot snapshot, and arbitrary bounded
  `cat`/`sed -n` windows can be materialized from those proven bytes while the
  file proof still matches. This improves repeated local reads without creating
  a virtual write layer. There is no edit replay, no mutating CoW overlay, no
  asynchronous write flush, and no production rollback layer in v1.

- Level 4, Schema-Native IPC: The production-safe subset is structured internal
  diagnostics, binary hot-cache IPC, and mmap snapshot descriptors. `squire
  kernel status` parses the binary snapshot header and descriptor table to
  report exact command entries, workspace image files, payload bytes, and
  availability. This cannot replace the agent-visible command contract: CLI
  commands must still return exact stdout, stderr, and native exit codes.

- Level 5, Anticipatory Speculative Execution: The production-safe subset is
  local idle-window prewarming after observed native operations. Bounded
  file-inspection commands prewarm adjacent read windows and eligible file
  bytes; manifest/config reads prewarm local follow-up files plus deterministic
  tool version/path probes. It must not monitor model token streams, change
  prompts, surface agent-visible suggestions, or mutate workspace state.
  Speculation remains bounded, local-only, and fault-open.
