# Squire Kernel

Squire Kernel is a local performance layer for AI coding agents.

The agent still chooses every command. Squire watches and warms the local
workspace, then serves exact cached results for deterministic read-only
operations only when it can prove the result is still valid. If proof is
missing, stale, too expensive, or unsafe, the command runs natively.

> Agent chooses. Squire serves.

## Install

```sh
curl -fsSL https://squire.run/install.sh | bash
```

The installer downloads the matching GitHub release archive, verifies
`SHA256SUMS`, and installs `squire` to `~/.local/bin` by default.

To install somewhere else:

```sh
curl -fsSL https://squire.run/install.sh | SQUIRE_INSTALL_DIR=/usr/local/bin bash
```

## Use

From a repo:

```sh
squire codex
```

That is the normal product path. There is no required `squire setup` step.
Codex opens normally, sees normal commands, and keeps making the same decisions
it would have made without Squire.

Squire runs underneath Codex:

- starts or reuses a local maintainer;
- warms repo and workspace proofs;
- serves exact hot-snapshot hits when safe;
- falls back to native execution on every miss or unsafe operation.

Optional observability after a session:

```sh
squire boost status --short
```

## UX Contract

Squire is not a new agent tool and does not change the prompt/tool surface.

- No prompt changes.
- No MCP tool injection.
- No model routing.
- No agent-visible command suggestions.
- No validation skipping.
- Native fallback always.

On macOS, `squire codex` uses a ready Linux microVM backend when it is already
configured. If the VM is unavailable or fails before Codex takes over, Squire
falls back to the host scoped session. On Linux, it uses the local scoped
session kernel directly.

`squire setup` still exists, but it is an advanced preflight/repair command, not
part of the happy path.

## What Can Replay

Enabled local Git metadata fast paths:

- `git rev-parse HEAD`
- `git rev-parse --git-dir`
- `git rev-parse --abbrev-ref HEAD`
- `git rev-parse --show-toplevel`
- `git rev-parse --is-inside-work-tree`

Proof-gated read-only candidates include:

- `git status --short`
- `git status --porcelain`
- `git ls-files`
- supported `git diff` forms
- bounded `cat` and `sed -n` reads for safe workspace files
- common `<tool> --version` probes
- simple `which` and external `command -v` lookups

These replay only from exact native observations while the local proof still
matches.

## What Never Replays

- validation, build, and test commands;
- edits and formatters that write files;
- mutating Git commands;
- package installs and package fetches;
- `.env`, sensitive file reads, shell aliases/functions, broad unknown shell
  commands, and unknown binary reads.

## Current Evidence

Current scoped proof:

> Squire Kernel accelerates repeated local Git metadata plus hot-prepared
> deterministic read-only discovery operations with exact stdout/stderr/exit
> code preservation, invalidation proof, and native fallback.

Latest mixed local UX benchmark:

- commands: `270`
- exact mismatches: `0`
- hot mmap replays: `204`
- native total: `3797.337ms`
- Squire total: `816.421ms`
- workload delta: `2980.916ms`
- speedup: `4.651x`
- replay p50/p95/p99/max: `132us` / `880us` / `1243us` / `1428us`

This is a scoped local-command claim, not a broad Codex speedup claim.

More detail: [docs/BENCHMARKS.md](docs/BENCHMARKS.md)

## CLI

```sh
squire codex [codex args...]
squire boost status [--short|--json]
squire kernel status [--short]
squire setup
```

Advanced surfaces:

```sh
squire session -- <command> [args...]
squire vm status [--short|--json]
squire vm session -- <command> [args...]
squire kernel run -- <command> [args...]
squire kernel warm [--metadata-only] [--short|--json]
squire kernel maintain --background [--short|--json]
squire boost bench repo-metadata [--short|--json]
squire boost bench deep-local [--short|--json]
```

## Development

```sh
go test ./...
git diff --check
```

Release checklist: [RELEASE_CHECKLIST.md](RELEASE_CHECKLIST.md)

Kernel contract: [SQUIRE_KERNEL_CONTRACT.md](SQUIRE_KERNEL_CONTRACT.md)

Advanced architecture: [docs/ADVANCED.md](docs/ADVANCED.md)
