# Squire

Squire is a local performance layer for AI coding agents.

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
`SHA256SUMS`, and installs `squire` plus `squire-codex` to `~/.local/bin` by
default. If a C compiler is available, it also builds the optional local hot
library used by `squire-codex`.

Squire does not install or authenticate Codex. Install Codex normally first, or
put `codex` on `PATH` before starting `squire-codex`.

Install somewhere else:

```sh
curl -fsSL https://squire.run/install.sh | SQUIRE_INSTALL_DIR=/usr/local/bin bash
```

## Start

From a repo:

```sh
squire-codex
```

That is the normal product path. Codex opens normally, the model sees the same
tool surface, and it keeps emitting ordinary commands such as `git status`,
`sed -n '1,80p' file`, or `python --version`. Squire runs underneath that
execution boundary and only replaces a command result when the local proof says
the bytes are exact.

There is no required setup step, no global command shim, no prompt change, no
new agent tool, and no MCP injection.

## What Squire Does

- Starts or reuses a local maintainer for the workspace.
- Warms repo/file/tool proofs in the background.
- Serves exact stdout, stderr, and exit code for proven read-only hits.
- Falls back to native execution on every miss, unsafe command, proof failure,
  corruption, or unavailable daemon.

Validation, builds, tests, edits, mutating Git commands, package installs, and
unknown shell commands are native-only.

## Why The Cache Is Valid

Squire does not trust a cached answer just because it exists. Every replay must
pass a local proof against the current world:

- normalized command, cwd, repo root, and tool identity match;
- relevant Git epochs, config/index fingerprints, file hashes, selected
  environment inputs, and executable identity match;
- Git-sensitive outputs also prove cwd-relative boundaries plus external Git
  inputs such as included config files, global ignore files, global attributes,
  `core.excludesFile`, and `core.attributesFile`;
- cached stdout, stderr, and exit code come from an exact native observation or
  an exact hot-prepared observation;
- the operation is still allowed by policy.

If any proof element is missing or stale, Squire runs the original command
natively. Stale records may remain on disk, but they are not replayable without
a fresh proof.

## Replay Coverage

Current proven lanes include repeated local Git metadata, repo summaries,
bounded source/config reads, line windows, fixed-string single-file search,
tight directory listings, safe environment probes, common version probes, and
selected read-only shell compositions.

Examples:

- `git rev-parse HEAD`
- `git status --short`
- `git ls-files`
- `git diff --stat`
- `cat src/app.js`
- `sed -n '1,80p' src/app.js`
- `head -n 20 src/app.js`
- `tail -n 20 src/app.js`
- `grep -F token src/app.js`
- `rg -F token src/app.js`
- `git status --short | head -n 5`

Unsupported forms run natively.

## Status

After a session:

```sh
squire boost status --short
```

Advanced diagnostics:

```sh
squire boost status --json
squire kernel status --short
squire setup
```

The `squire kernel ...` namespace remains for diagnostics and compatibility,
but the product is just Squire.

## Benchmarks

Latest scoped preload panel:

- `31/31` commands exact
- direct command group: `52.8x` native p50-sum speedup
- composed shell group: `7.3x` native p50-sum speedup
- full panel: `13.1x` native p50-sum speedup
- representative direct p95:
  - `git rev-parse HEAD`: `0.097ms`
  - `git status --short`: `0.379ms`
  - `git ls-files`: `0.156ms`
  - `cat src/app.js`: `0.129ms`
  - `grep -F token src/app.js`: `0.122ms`

These are local command-serving measurements only. They do not include model
thinking time, API/network latency, or a broad task-speedup claim.

Full tables and fuzz results: [docs/BENCHMARKS.md](docs/BENCHMARKS.md)

Advanced architecture notes: [docs/ADVANCED.md](docs/ADVANCED.md)
