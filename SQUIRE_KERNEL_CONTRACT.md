# Squire Kernel Contract

Squire Kernel v1 is a scoped kernel proof for repeated local Git metadata plus
hot-prepared deterministic read-only discovery operations.

## Product Claim

Scoped kernel proof for repeated local Git metadata plus hot-prepared
deterministic read-only discovery operations.

Squire Kernel transparently accelerates a tiny allowlist of repeated local Git
metadata operations and hot-prepared deterministic read-only discovery
operations with exact stdout, stderr, and exit-code preservation, correct
invalidation, native fallback, and measured hot-path performance.

The replay performance target is sub-1ms p95 wall time for replay hits.
Invalid/missing-cache and never-replay paths execute natively, so their
performance target is Squire overhead above native execution, not total command
wall time.

This is not a broad Codex speedup claim.

## Invariants

1. Agent chooses. Squire serves.
2. Native fallback always.
3. Validation never replayed.
4. Edits/mutations never replayed.
5. Runtime decisions are replay or native; there is no "shadow" execution mode.
6. Exact stdout/stderr/exit-code required.
7. Every replay needs invalidation proof.
8. Local world state proves validity.

## Operator Boundaries

Enabled fast paths:

- `git rev-parse HEAD`
- `git rev-parse --git-dir`
- `git rev-parse --abbrev-ref HEAD`
- `git rev-parse --show-toplevel`
- `git rev-parse --is-inside-work-tree`

Hot-prepared proof-gated replay candidates:

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

Hot-prepared proof-gated candidates may replay only when the command key, cheap
hot fingerprints, hot invalidation epoch, output hashes, and in-memory output
bytes all match. Their p95 replay wall time is reported separately from
metadata fast-path p95.

The foreground CLI serving path first checks a daemon-published mmap hot
snapshot before constructing the full kernel object, loading ledgers, or
touching daemon/socket paths. If that exact read-only snapshot proof hits, the
CLI writes the cached stdout/stderr bytes and exits with the cached exit code.
If the CLI hot client misses, the regular kernel path checks resident in-memory
prepared output and then the bounded resident hot-cache IPC server when it is
running. The mmap snapshot is a local, owner-only, atomically published file
with fixed-size descriptors laid out for cache-friendly lookup. It avoids
foreground ledger hydration and Unix-socket round trips on hits, but it still
uses normal OS memory mapping and still requires local invalidation proof; it
is not a literal kernel bypass. If the snapshot, resident cache, or daemon is
unavailable, misses, times out, returns invalid hashes, or fails the hot proof,
native execution wins. A separate background maintainer may produce durable
evidence and reports, publish the mmap snapshot, and serve exact prepared
output to fresh foreground processes over a local Unix-socket daemon cache.

The production foreground should be long-lived. A long-lived foreground may
reuse the resident hot-cache connection and keep short session-local
daemon-unavailable and exact-command miss caches. These caches must be bounded,
brief, and fault-open: they may suppress replay attempts, but they must never
suppress native execution.
For adapter integrations, replay checks should reuse the foreground kernel's
cached mmap snapshot view rather than map/unmap the snapshot on every request.
Adapter responses may use pooled buffers to reduce allocation churn, but the
wire protocol must still preserve exact stdout/stderr bytes and exit code.

The production foreground is host/runtime owned, not model owned. A terminal
adapter may send already-chosen commands to Squire over a local protocol and
receive exact stdout/stderr/exit-code results, but the agent-facing command text
must remain the original command. The adapter must not add tools, change
prompts, suggest commands, route models, or require the model to call Squire.
Manual `squire kernel run -- <command>` remains a diagnostic surface, not the
primary product UX.

Workspace file inspection replay is limited to safe relative paths inside the
workspace, regular files below the bounded size limits, non-hidden/VCS paths,
and source/config extensions or well-known project metadata files. It is
invalidated by local file proof. Exact command observations include the exact
argv in their proof. Warm-file entries are keyed by relative path, file content
hash, size, and mode, and may materialize arbitrary eligible bounded `sed -n`
windows or bounded `cat` output from those same proven bytes without
precomputing every possible range.
Tool discovery replay is invalidated by PATH, selected environment variables,
and executable identity signals. `.env`, hidden paths, likely secret/token/key
files, unknown binary reads, shell aliases, shell functions, and shell-specific
startup state remain native.

Native-only discovery:

- `git remote -v`
- `git remote get-url origin`
- `rg --files`
- `rg <literal> <workspace paths...>`

These commands are native-only in this baseline and are not replay targets.

The general rule for future replay candidates is: read-only, deterministic,
local, bounded, exact-argv keyed, and cheaply provable from local world state.
Every replayable output must come from a native warm observation or an already
proven exact cache entry. If any input that can affect stdout, stderr, or exit
code cannot be cheaply proven, native execution wins.

Repo-summary candidates replay only from hot-prepared native observations and
only while their proof remains valid. `git ls-files` is keyed by the Git index,
Git config, cwd, exact argv, and Git executable identity. `git status` and `git
diff` additionally require a bounded exact workspace proof.
If the workspace proof would be too expensive, output is too large, a command
uses unsupported flags, or any proof element is missing, native execution wins.

`squire kernel warm` may speculatively run bounded proof-gated read-only
commands in a worker pool and may warm eligible workspace file bytes before the
agent asks for them. These warmed file bytes form the production-safe Level 3
read-only virtual workspace image. Exact warm observations replay through the
exact output and hot prepared proof path. Warm-file observations replay only for
eligible bounded `cat`/`sed -n` requests while the file proof still matches. If
a command has no complete cheap hot proof, native execution wins on the
foreground serving path.

After an agent-chosen bounded file-inspection command, `squire kernel run` may
launch a short-lived local helper process to prewarm adjacent read windows,
ecosystem follow-up files, deterministic local version/path probes, and eligible
file bytes during the agent's thinking/generation window. This helper must not
alter the current command result, expose suggestions, change prompts, or make a
future command replayable unless the usual exact output bytes, hashes, hot
fingerprints, and invalidation epoch proof are present.

`squire kernel maintain` is the resident bounded form of warm. In production it
should run as a separate background process via
`squire kernel maintain --background`, so prewarm work overlaps the agent's
thinking/generation window instead of running on the command-serving hot path.
It polls local world/proof signals, skips work while signals are unchanged,
refreshes prewarmed outputs after invalidation, and owns the resident hot-cache
IPC server. It must not expose command suggestions, change prompts, add tools,
skip validation, or replay mutations.

Never replay:

- validation/build/test
- edits
- mutating git
- package installs
- shell-ambiguous commands

Remote metadata remains native-only because `git remote -v` and
`git remote get-url origin` can expose repo URLs. They must not become replay
targets until output-store and privacy policy are explicit.

## OTel Boundary

Squire Kernel must work without OpenTelemetry. OTel is optional session
metadata only and is not required for correctness, invalidation, replay proof,
or native fallback.

## Baseline Evidence

The v1 baseline is valid only when basic correctness and measured safety
checks pass. Minimal evidence includes:

- `go test ./...` passes.
- `squire boost bench repo-metadata` demonstrates exactness (no enabled
  fast-path mismatches) and verifies mutation-boundary invalidation.
- `squire boost bench deep-local` demonstrates enabled fast-path exactness,
  no stale replays, no validation replays, and passing safety gates.

Benchmarks report exactness and mismatch counts explicitly; performance
measurements are reported separately. A performance gate violation marks the
profile `needs_optimization` but does not invalidate safety or exactness
claims.

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
