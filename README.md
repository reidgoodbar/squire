# Squire

Squire is a transparent local execution and verification layer for coding
agents. It keeps common repository reads hot and continuously maintains whether
the current declared workspace state is verified.

The agent keeps using ordinary terminal commands. Before Codex starts a local
read-only command, Squire checks current state and either replays a proven mmap
observation or executes a small bounded operation over hash-verified current
file bytes. A valid hit returns the exact stdout, stderr, and exit status.
Every miss follows Codex's original native execution path.

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

## Continuous Verification

Squire Green runs declared tests, lint, typechecks, and builds natively in the
background after repository edits settle. Each result is bound to the exact
declared input bytes, check configuration, environment, and executable. A
later relevant edit makes that result stale; unrelated edits do not.

```toml
# .squire/checks.toml
[[check]]
name = "tests"
command = ["go", "test", "./..."]
inputs = ["**/*.go", "go.mod", "go.sum"]
timeout = "10m"
```

Repository-provided commands never run silently on first use. Review the file
and trust its exact hash once:

```sh
squire green trust
squire verify
```

Any config change revokes trust. `squire codex` then schedules trusted checks
automatically; no second daemon or warm command is required. See
[docs/GREEN.md](docs/GREEN.md) for configuration and proof semantics.

## What It Accelerates

Production lanes are bounded but cover the common read-only command surface:

- Git metadata: supported `git rev-parse` forms and branch discovery.
- Repository reads: supported `git status`, `git ls-files`, and `git diff`
  forms, including path-scoped diffs and `git diff --check`, plus bounded
  `git log -N --oneline -- <literal paths>` history.
- File and search reads: bounded `cat`, ordered single- or multi-range `sed -n`,
  `head`, `tail`, `nl -ba`, `file`, fixed-string `grep`/`rg`, demand-prepared
  bounded repository `rg` searches, and tight `ls` forms.
- Environment discovery: supported version probes, `which`/`command -v`, safe
  `printenv`, `whoami`, `id`, `hostname`, and `uname` forms.
- Compositions: complete read-only plans over supported sources and filters,
  including pipes, sequences, redirection to `/dev/null`, `head`, `tail`,
  bounded `sed -n`, `grep -F`, `wc -l`, and `sort`.

No operation is removed. Builds, tests, edits, installs, mutating Git commands,
expansions, unknown shell syntax, sensitive probes, and unsupported variants
immediately follow Codex's unchanged native path. A safe cold miss does the
same while requesting exact preparation in the background. `rg --files`
remains outside the bounded preparation policy and follows the native path.

Supported commands compile into typed bounded plans rather than exact command
templates. Source proof and execution are separate: one proven file snapshot
can serve different line selections, filters, and compositions without a cache
entry for each command string. The same plan representation is implemented by
the Go engine and native runtime, keeping future read operators additive while
differential tests enforce parity at the ABI boundary.

## Why Hits Are Current

Squire caches observations, not authority. A foreground replay either
recomputes the inputs that can affect that command or reuses a cryptographic
fingerprint while a complete `kqueue`/`inotify` guard reports no dependency
change. The prepared epoch must still match. Proof inputs include the normalized
command and cwd, Git refs/index/config and external behavior files, relevant
workspace state, canonical paths, content hashes, command-specific environment
proof, and executable identity. Guard failure always invalidates the resident
proof.

For bounded file reads, an epoch mismatch may instead use the current-file
lane: Squire retains the exact bytes read while computing the foreground
SHA-256 proof and applies only its fixed byte grammar to those bytes. This
requires no rewarm and does not persist the file or result. The cache may still
contain stale records, but they are never replayed after a proof mismatch.
Missing state, corruption, unsupported syntax, an ABI mismatch, or an
unprofitable proof all become native fallback.

The invalidation suite changes file bytes without changing size or mtime,
mutates the Git index and untracked set, changes same-size diffs, edits Git
config, commits, renames branches, changes loose and packed object namespaces,
changes environment inputs, and probes outside-workspace symlinks. Returning
old bytes or an unsafe hit fails the release.

See [SQUIRE_CONTRACT.md](SQUIRE_CONTRACT.md) for the complete invariants.

## Current Runtime Check

On July 16, 2026, a 500-command randomized production-ABI run recorded 421
exact hits, 79 safe fallbacks, 468 native comparisons, zero mismatches, and
zero unsafe hits. Hit p50/p95/p99 was `0.315/0.630/0.933ms`; the same commands
ran natively at `8.084/27.692/51.820ms`. A separate 500-query repository-search
differential had 500 exact or order-equivalent hits and zero semantic
mismatches. Bounded path history was 28/28 exact at `0.333ms` p50 and `0.492ms`
p99 versus `20.060ms` native p50. All 2,048 steady calls were exact with
`0.433ms` wall p99. Under eight-way load, CPU p99 was `0.334ms`; scheduler-
contended wall p99 was `3.242ms`.

Fresh live `gpt-5.6-luna` treatments independently replayed 2/3 Express calls
(`66.7%`), 5/5 Flask calls (`100%`), and 4/5 fmt calls (`80%`). Every treatment
passed the `50%` all-terminal-call gate with valid accounting and zero
diagnostic mismatches. These small live samples validate coverage, not causal
whole-task wall time; model trajectories diverged before seeing tool results.

A deterministic 40-pair Codex attribution test held model responses, commands,
workspace, and terminal payloads fixed. Six serial calls fell from `374.876ms`
to `103.600ms`, saving `271.277ms` (`72.4%`, paired 95% interval
`265.630-276.540ms`). Two parallel batches saved `52.6%`. AB and BA orders both
remained positive, while interleaved A/A and B/B intervals included zero.

Full methodology and historical tables: [docs/BENCHMARKS.md](docs/BENCHMARKS.md).

Architecture and backend notes: [docs/ADVANCED.md](docs/ADVANCED.md).
