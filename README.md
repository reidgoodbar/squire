# Squire Kernel v1

Squire v1 is an active, transparent local kernel for agent-chosen operations.
Codex and Claude still decide what to do. Squire does not add tools, change
prompts, route models, suggest commands, skip validation, replay edits, or
change final decisions.

The core principle is:

> Agent chooses. Squire serves.

This repository is a fresh kernel implementation. The `old/` tree is retained
as research and development history only; it is not the product architecture.

## Scope

Squire improves how the existing local environment serves operations the agent
already chose to run, and only when behavior preservation is proven.

Squire Kernel must work without OpenTelemetry. OTel can be optional session
metadata, but it is not required for correctness or operation.

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
- Shadow candidate operations.
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

`squire kernel status`

- Shows repo oracle status.
- Shows world state and invalidation epochs.
- Shows enabled fast paths, shadow candidates, and never-replay policy.

`squire boost status`

- Shows enabled accelerators, replacements, fallbacks, mismatches,
  invalidations, and ROI history when available.

`squire shadow status`

- Shows shadow candidates, exactness rate, mismatch examples, and disabled
  reasons.

`squire boost bench repo-metadata`

- Runs a local scoped benchmark for enabled repo metadata fast paths.
- Reports exactness, mutation-boundary invalidation, workload-only wall delta,
  and net ROI.
- Makes no broad Codex speedup claim.

## Core Invariants

1. Agent chooses. Squire serves.
2. Native fallback always exists.
3. Validation is never replayed.
4. Edits are never replayed.
5. Mutating commands are never replayed.
6. Shadow before replacement.
7. Exact stdout/stderr/exit-code equality is required.
8. Every replay needs an invalidation proof.
9. Every derived fact has evidence quality.
10. Local world state proves validity.
11. Standard mode avoids raw sensitive content.
