# Squire

Squire is a transparent local execution accelerator for coding agents.

The agent keeps using ordinary terminal commands. Before Codex starts a local
read-only command, Squire checks a proof-backed mmap snapshot. A valid hit
returns the exact stdout, stderr, and exit status. Every miss follows Codex's
original native execution path.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/reidgoodbar/squire/main/install.sh | bash
```

The installer verifies matching Squire and Squire-Codex release archives and
installs the driver, Codex runtime helper, and host-native Squire runtime to
`~/.local/bin`. It does not change Codex authentication or configuration.
Supported hosts are macOS and Linux on `amd64` or `arm64`.

Check the installation:

```sh
squire doctor
```

`doctor` exits nonzero when any required driver, helper, runtime, or ABI
component is missing.

## Use

Start from any directory:

```sh
squire codex
```

That is the complete user path. There is no setup command, global shell shim,
prompt change, MCP tool, preload injection, or VM provisioning step. If Codex
moves into a repository later, Squire discovers and prepares that repository
from the command's actual cwd.

`squire-codex` is also installed as a direct convenience command.

Inspect the current repository and runtime:

```sh
squire status
squire status --json
squire explain -- git status --short
```

## What It Accelerates

Production lanes are intentionally narrow:

- Git metadata: supported `git rev-parse` forms and branch discovery.
- Repository reads: supported `git status`, `git ls-files`, and `git diff`
  forms.
- Bounded reads: `cat`, `sed -n`, `head`, `tail`, tight `ls`, and
  fixed-string single-file `grep`/`rg` forms.
- Compositions: complete read-only plans over supported sources and filters,
  including `head`, `tail`, `grep -F`, `wc -l`, and `sort`.

Builds, tests, edits, package operations, mutating Git commands, expansions,
unknown shell syntax, and unsupported commands run natively. Tool version and
PATH lookup probes also run natively for now because proving a large executable
can cost more than running the probe. `rg --files` is native because its output
order is not deterministic enough for byte-exact replay.

`file`, `whoami`, and `id` are also native: their output depends on mutable
system magic or identity databases whose strong foreground proof is not
profitable.

## Why Hits Are Current

Squire caches observations, not authority. A foreground hit recomputes the
inputs that can affect that command and requires the prepared epoch to match.
Proof inputs include the normalized command and cwd, Git refs/index/config and
external behavior files, relevant workspace state, canonical paths, content
hashes, command-specific environment proof, and executable identity.

The cache may contain stale records. They are not replayable after a proof
mismatch. Missing state, corruption, unsupported syntax, an ABI mismatch, or a
proof that is too expensive all become native fallback.

The invalidation suite changes file bytes without changing size or mtime,
mutates the Git index and untracked set, changes same-size diffs, edits Git
config, commits, renames branches, changes environment inputs, and probes
outside-workspace symlinks. A stale replay fails the release.

See [SQUIRE_CONTRACT.md](SQUIRE_CONTRACT.md) for the complete invariants.

## Current Runtime Check

On July 13, 2026, a 500-command randomized ABI run covered direct commands,
moving line windows, fixed searches, repository state, composed pipelines,
native controls, and unsafe commands:

- 296 exact hits and 204 safe native fallbacks;
- 458 byte-for-byte native comparisons;
- 0 mismatches and 0 unsafe hits;
- all 10 invalidation probes passed;
- hit p50 `0.528ms`;
- dedicated 2,000-sample Git metadata p99 `0.893ms`;
- unsupported/native-control decision p50 about `0.08ms`;
- measured net command-serving time avoided: `3.450s`.

Repository and composed proofs have higher tail latency than metadata and
bounded-file lanes, so Squire reports them separately. These measurements cover
local command serving only, not model thinking or network latency.

Full methodology and historical tables: [docs/BENCHMARKS.md](docs/BENCHMARKS.md).

Architecture and backend notes: [docs/ADVANCED.md](docs/ADVANCED.md).
