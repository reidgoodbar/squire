# Squire Kernel v1

Squire Kernel is a local performance layer for AI coding agents. The agent
still chooses every command. Squire watches and warms the local workspace, then
serves exact cached results for a small set of deterministic read-only
operations only when it can prove the result is still valid. If proof is
missing, stale, too expensive, or unsafe, the command runs natively.

The project is built around one rule:

> Agent chooses. Squire serves.

In practice, Squire is useful because agents repeatedly ask the local machine
the same questions while working: Git metadata, repo status, small source file
reads, manifest reads, tool versions, and command-path probes. Squire keeps the
local world warm so those repeated reads can return faster without changing
stdout, stderr, exit codes, prompts, model routing, or final decisions.

## What It Does

- Maintains a live local model of the repository and workspace.
- Prewarms safe read-only metadata and bounded file-inspection results.
- Publishes a local mmap hot snapshot for exact foreground replays.
- Replays only exact stdout, stderr, and exit code when the proof still matches.
- Falls back to native execution whenever any proof or policy check fails.
- Reports scoped benchmark and edge-stress evidence without broad speedup
  claims.

## What It Does Not Do

- It does not add agent tools, inject MCP servers, change prompts, or route
  models.
- It does not suggest commands to the agent.
- It does not replay validation, build, test, edit, package install, or
  mutating commands.
- It does not approximate command output semantically.
- It does not make the mmap snapshot a literal kernel bypass; native filesystem
  state remains authoritative.
- It does not require OpenTelemetry. OTel is optional session metadata only.

## Install

Install the `squire` binary once for the current user:

```sh
curl -fsSL https://squire.run/install.sh | bash
```

The installer downloads the matching release archive from GitHub, verifies it
with `SHA256SUMS`, and installs `squire` to `~/.local/bin` by default. Override
the install location with `SQUIRE_INSTALL_DIR`:

```sh
curl -fsSL https://squire.run/install.sh | SQUIRE_INSTALL_DIR=/usr/local/bin bash
```

This is a system/user-level install only. It does not install global command
shims, does not start a global daemon, and does not create repo state until
Squire is run inside a workspace. Squire installs and works without Homebrew
zsh. Advanced session transports may use preload or temporary scoped helpers
internally, but the recommended user path does not require choosing a transport.

## Try It

```sh
cd your-repo
squire codex
```

Optional observability after a session:

```sh
squire boost status --short
```

## Normal Product UX

The normal product path is not "teach the model to call Squire." The model or
agent runtime continues to choose ordinary commands such as `git status
--short`, `cat package.json`, or `python --version`.

The recommended path is one command:

```sh
squire codex
```

This starts Codex inside the scoped Squire Kernel product path. Codex still
chooses ordinary commands. Squire starts or reuses the resident maintainer,
warms local proofs, serves exact proven hot-snapshot hits, and falls back to
native execution on every miss or unsafe operation. There is no required
`squire setup` step in the happy path.

On macOS, `squire codex` opportunistically uses the Linux microVM backend when
the VM helper and guest assets are already configured. That gives Codex the
preload-friendly Linux execution path and avoids Apple protected-shell /
hardened-runtime limits. If the VM backend is unavailable or fails before Codex
takes over, Squire falls back to the host scoped session. On Linux hosts, the
same command uses the local scoped session kernel directly.

Squire belongs below the model/tool layer:

- a resident background maintainer keeps local world state warm;
- a scoped preload library reads the maintainer-published mmap hot snapshot
  before selected exec calls when the platform supports it;
- hardened launchers such as Codex on macOS use scoped mmap shims because their
  signed runtime does not accept `DYLD_INSERT_LIBRARIES`;
- Squire either returns exact proven bytes or runs the command natively.

`squire kernel run -- <command>` is a diagnostic/manual surface. A production
agent integration should use `squire codex`, not a model-visible command.
The installed preload library is never installed globally into the user's
shell. Session launchers scope the transport to one child process tree and set
exact native fallback paths when preload can attach. The stdio adapter remains
as a compatibility path for host runtimes that can already forward
already-chosen commands over JSON lines.

Under the hood, `squire codex` is a backend router. It uses a ready VM backend
when available, then uses the host scoped session fallback. It should not stop
the user to provision VM assets. Host sessions may use temporary scoped mmap
helpers for hardened launchers because their signed runtime does not load
`DYLD_INSERT_LIBRARIES`; preload-safe launchers can use the scoped preload
transport. The user does not need to pick. Misses, unsafe commands, invalid
proofs, mutations, validation, and package setup still execute natively.

For Linux-isolated work, `squire vm session -- codex` remains an advanced path.
It avoids host protected-shell and hardened runtime behavior by running the
agent loop inside a Linux guest. It is useful for VM release testing and
Linux-compatible projects, but it is not the default happy path.
The macOS VM helper forwards a narrow Squire diagnostic allowlist into the
guest, including `SQUIRE_VM_GUEST_SESSION_TRANSPORT`,
`SQUIRE_PRELOAD_TRACE`, `SQUIRE_SHIM_DEBUG`, and `SQUIRE_SHIM_REQUIRE_HIT`, so
preload reachability can be tested without forwarding arbitrary host
environment variables. Those flags are diagnostic only. Performance runs should
leave `SQUIRE_PRELOAD_TRACE` and `SQUIRE_SHIM_REQUIRE_HIT` unset so misses can
fall back natively and stderr tracing does not dominate sub-millisecond replay
measurements.
For Codex, the preload path must catch simple shell execution too: Codex may
`execve` a dynamic `/bin/sh -c <command>` instead of spawning `git` directly.
Squire handles those simple shell commands before native shell fallback. Replay
accounting uses a session-owned inherited event FD first, then falls back to a
direct event-file append. The session also passes the current hot snapshot as an
inherited read-only FD so replay hits can `mmap` the already-open snapshot
instead of opening `hot_snapshot.bin` by path on every child command. VM guest
sessions additionally request a read-only session-local snapshot copy, which
keeps replay hits from faulting snapshot pages through the shared store mount.
That lets sandboxed Codex child processes record replays without direct write
access to the mounted Squire store and keeps production preload replay below
the 1ms p95 target.

For workloads that can run in Linux, Squire also exposes a separate guest-mode
surface:

```sh
squire vm status --short
squire vm session -- codex
```

This is not a host-native macOS compatibility mode. It is an isolated Linux
execution mode intended to avoid Apple protected-shell and hardened-runtime
limits entirely. It must not depend on host command shims or session-local host
PATH tricks. On Linux hosts, `squire vm session` immediately uses the same
scoped session kernel locally. On macOS, `squire vm session` uses the optional
`squire-vm-darwin` helper built from `vm/squire_vm_darwin.swift` when it is
installed and the Linux guest assets are configured.

The Darwin helper uses Virtualization.framework, boots a configured Linux
kernel with an initrd or disk image, shares the host workspace and Squire store
through virtiofs, and sends the already-chosen command to a guest agent over a
virtio serial protocol by default. A vsock transport remains an opt-in
experiment through `SQUIRE_VM_TRANSPORT=vsock`, but the working macOS MVP uses
serial because the Alpine virt initramfs does not expose AF_VSOCK by default.
The guest agent returns exact stdout/stderr/exit-code bytes, or native fallback
happens inside Linux. `install.sh` compiles the helper with Swift and ad-hoc
signs it with the `com.apple.security.virtualization` entitlement when
`codesign` is present. Squire does not bundle a Linux guest image yet, so macOS
status may show the helper installed while guest configuration or host
Virtualization.framework support is missing.

Required macOS guest configuration:

```sh
export SQUIRE_VM_KERNEL=/path/to/kernel.Image
export SQUIRE_VM_INITRD=/path/to/initrd   # or SQUIRE_VM_DISK=/path/to/disk.img
export SQUIRE_VM_TRANSPORT=serial         # optional, default serial
```

From a source checkout, build a local Alpine-based test guest bundle:

```sh
scripts/build_vm_guest_bundle.sh /tmp/squire-vm-guest
export SQUIRE_VM_KERNEL=/tmp/squire-vm-guest/kernel.Image
export SQUIRE_VM_INITRD=/tmp/squire-vm-guest/initrd
squire vm session -- /bin/sh -lc 'echo hello from guest'
```

To build a Codex-enabled local guest bundle, include the Linux Codex release
binary and explicitly mount Codex auth/config:

```sh
SQUIRE_VM_INCLUDE_CODEX=1 scripts/build_vm_guest_bundle.sh /tmp/squire-vm-guest-codex
export SQUIRE_VM_KERNEL=/tmp/squire-vm-guest-codex/kernel.Image
export SQUIRE_VM_INITRD=/tmp/squire-vm-guest-codex/initrd
export SQUIRE_VM_CODEX_HOME=$HOME/.codex
squire vm session -- codex doctor --summary
squire vm session -- codex exec --skip-git-repo-check 'Print exactly VM_CODEX_OK.'
```

`SQUIRE_VM_CODEX_HOME` is intentionally explicit because it exposes local Codex
auth/config files to the local Linux guest. The Codex-enabled Alpine bundle
installs guest-side `bubblewrap`, `git`, and `ripgrep` at boot so Codex can use
normal Linux sandbox and discovery tools. The guest switches from the raw
initramfs rootfs into a tmpfs-backed root at boot so `bubblewrap` can use
`pivot_root`, and `/tmp` is created as a normal `01777` directory. A local macOS
smoke verified Codex v0.140.0 inside the guest: network and WebSocket
connectivity passed, auth loaded, `bubblewrap 0.11.2`, `git version 2.54.0`,
and `ripgrep 15.1.0` were available, a sandboxed `git status --short --branch`
ran without approval fallback, and a minimal `codex exec` returned
`VM_CODEX_OK`. Interactive `squire vm session -- codex` uses a second virtio
serial console as the guest TTY, propagates host terminal rows/columns into
that guest TTY, and keeps the first serial channel as the control protocol for
startup and exit-code reporting.

This test must be run from a normal terminal context that can use
Virtualization.framework. The Codex sandbox used while developing Squire can
report `Virtualization.framework reports this host is unsupported` even when
the same signed helper works unsandboxed.

As an escape hatch, an external Linux guest runner can still be configured
through `SQUIRE_VM_RUNNER` or `--runner`:

```sh
SQUIRE_VM_RUNNER=/path/to/linux-guest-runner squire vm session -- codex
```

The guest runner contract is:

```sh
<runner> session --cwd <host-cwd> --store-root <store-root> -- <command> [args...]
```

Inside the guest, Codex or another agent still emits ordinary commands, and
Squire still returns exact proven bytes or falls back to native execution inside
Linux. This path is useful for JS, Python, Go, Rust, and server-style projects
that are already Linux-compatible. It is not equivalent for Xcode, iOS,
Homebrew-only, or macOS-specific projects.

The scoped preload proof is `scripts/session_preload_bench.py`. It uses a
non-protected local launcher and repeated ordinary `posix_spawnp("git",
"rev-parse", "HEAD")` calls with stdout captured through `posix_spawn` file
actions, so the benchmark exercises the preload transport used by agent-style
pipe capture, not the legacy transport. By default, measured replay rounds use
production fallback mode; pass `--require-hit-measurement` only for diagnostic
hard-hit measurements. A current `/private/tmp` production run measured native
p50 `15.40ms` versus preload-session replay p50 `0.75ms` and p95 `0.87ms`,
with exact output and native fallback still available. The hard-hit diagnostic
run measured replay p50 `0.81ms`. Use `--shell-launcher` to measure the
Codex-shaped `sh -c "git rev-parse HEAD"` path; the current shell-launch
production run measured native p50 `20.10ms` versus preload replay p50
`0.62ms` and p95 `0.69ms`. VM/Codex traces should be run separately because
`SQUIRE_PRELOAD_TRACE=1` intentionally adds stderr writes.

A live macOS VM/Codex session using `squire vm session -- codex` and production
preload settings produced fresh replay samples of `486us`, `619us`, `929us`,
`587us`, and `555us` for repeated plain `git rev-parse HEAD` calls. Those
samples were all exact `c-mmap-hot-snapshot` replays and all below the 1ms
target. Older entries in `hot_client_events.log` may include pre-optimization
samples, so release checks should evaluate a fresh post-start window rather
than the lifetime average.

The mixed scoped-session UX proof is `scripts/session_mixed_bench.py`. It
creates a fresh small Python/TypeScript Git repo, starts one normal scoped
session shell, and has that shell issue plain commands across Git metadata,
repo summaries, bounded file reads, tool probes, and native-control commands.
The benchmark compares exact stdout hash, stderr hash, output sizes, and exit
code against native execution. A 2026-06-24 `/private/tmp` run with
`/opt/homebrew/bin/zsh` measured:

- commands: `270`
- exactness: `true`
- hot mmap replays: `204`
- native total: `3797.337ms`
- Squire session total: `816.421ms`
- workload delta: `2980.916ms`
- speedup: `4.651x`
- replay p50/p95/p99/max: `132us` / `880us` / `1243us` / `1428us`

The benchmark still makes a scoped local-command claim only. It does not
measure model thinking time, network time, or broad Codex task speed.

Tiny `cat`/`sed` reads pass through native tools by default because proving a
replay by hashing the file is usually more expensive than native process
startup for very small files. `squire session --enable-warm-file-replay -- ...`
opts into bounded preload coverage experiments; product mode keeps the
unprofitable tiny-reader path native while preserving exact fallback.

The preload transport uses `LD_PRELOAD`/`DYLD_INSERT_LIBRARIES` to hook
exec-family calls inside the session and calls the same mmap proof engine before
native exec loads the target binary. It is the preferred transport when
available, but preload support still depends on the platform and launcher. On
macOS, SIP-protected launchers such as `/bin/zsh`, `/bin/sh`, and
`/usr/bin/env` ignore `DYLD_INSERT_LIBRARIES`, so normal auto mode treats them
as preload-unsafe and runs native. Non-protected user binaries can use the
preload path, including simple `posix_spawn` / `posix_spawnp` launches and the
common pipe-capture file actions built from `posix_spawn_file_actions_addclose`
and `posix_spawn_file_actions_adddup2`. In a controlled `/private/tmp` driver,
repeated `posix_spawnp("git", "rev-parse", "HEAD")` with stdout pipe capture
measured native p50 `7.10ms` versus preload-session replay p50 `0.386ms`, with
exact output and native fallback still available. Native exec fallback inside
the preload library is guarded against interposer recursion, including macOS
startup tools such as `/usr/libexec/path_helper`. `--preload` requires preload
for any launcher and is intended for diagnostics and launcher-specific testing.

The Codex-on-macOS scoped mmap helper remains an internal fallback behind
`squire codex`, not a user-facing setup step. For advanced Linux-isolated
testing, `squire vm session -- codex` runs the whole agent loop in the guest so
the host macOS launcher does not need to be intercepted.

The long-lived adapter UX proof is `scripts/adapter_path_bench.py`. It creates a
fresh temp Git repo, starts the long-lived adapter as the hidden backend, and
measures ordinary visible commands. The measured command stream contains no
`squire kernel run -- ...` command. A 1000-round `/private/tmp` run served 3000
commands with zero violations:

- replay hit p95: `0.487ms`
- invalid/miss overhead p95: `-0.274ms`
- never-direct overhead p95: `0.127ms`
- first post-mutation invalid request: `native`

The replay SLA is intentionally strict: replay hits should stay below `1ms`
p95 wall time. Invalid/missing-cache and never-replay paths are native
executions, so their performance budget is Squire overhead above native, not
total command wall time.

The normal-UX many-agent proof is `scripts/multi_agent_bench.py`. It starts one
resident maintainer plus one long-lived adapter client per simulated agent, then
A/B compares ordinary command argv against native subprocess execution in a
fresh temp repo. The workload intentionally mixes replayable local discovery
with a never-replay native command, and every measured result is byte-compared
against native stdout, stderr, and exit code. A `/private/tmp` run with 1, 2, 4,
8, and 16 concurrent agents over 10 rounds reported zero mismatches and a
Squire-free measured command stream:

- 1 agent, 80 commands: `4.780x`, `650.765ms` wall delta
- 2 agents, 160 commands: `4.559x`, `528.408ms` wall delta
- 4 agents, 320 commands: `4.730x`, `753.041ms` wall delta
- 8 agents, 640 commands: `5.533x`, `1301.093ms` wall delta
- 16 agents, 1280 commands: `5.793x`, `2247.894ms` wall delta

For release checks:

```sh
go test ./...
squire boost bench repo-metadata
scripts/release_smoke.sh ./squire
scripts/session_preload_bench.py ./squire --rounds 5 --commands 100
scripts/adapter_path_bench.py ./squire
scripts/edge_stress.py ./squire --scenario echo --normal-ux --strict-performance
scripts/multi_agent_bench.py ./squire --agents 1,2 --rounds 3
scripts/edge_stress.py ./squire --normal-ux
scripts/edge_stress.py ./squire
```

## GitHub Releases

The repository includes GitHub Actions for release-quality testing and package
publishing:

- `ci.yml` runs fast tests, the repo metadata benchmark, and smoke checks on
  pushes and pull requests.
- `nightly.yml` runs the deeper baseline, adapter benchmarks, multi-agent A/B,
  and edge stress.
- `release.yml` runs the release gate, builds cross-platform archives, generates
  `SHA256SUMS`, and creates or updates a GitHub Release.

Create a release by pushing a `v*` tag or by running the `release` workflow
manually:

```sh
git tag v0.1.0-beta.1
git push origin v0.1.0-beta.1
```

Current releases should use the `v0.x.y-beta.N` shape and are published as
GitHub prereleases. The workflow also marks any `v0.*`, `*-alpha*`, `*-beta*`,
or `*-rc*` version as a prerelease.

Local artifact builds use the same script as CI:

```sh
VERSION=v0.1.0-beta.1 scripts/build_release_artifacts.sh .tmp/release
```

This repository is the fresh kernel implementation. Deprecated report-first,
wrapped-first, and trace-dashboard experiments have been removed from this
workspace.

The top-level contract for this baseline is
[`SQUIRE_KERNEL_CONTRACT.md`](SQUIRE_KERNEL_CONTRACT.md). Product language must
stay scoped: "Scoped kernel proof for repeated local Git metadata plus
hot-prepared deterministic read-only discovery operations."

Release readiness checks live in
[`RELEASE_CHECKLIST.md`](RELEASE_CHECKLIST.md).

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

- Advanced preflight/repair command. It is not required before `squire codex`.
- Initializes local config and store, initializes the repo oracle, and prints
  privacy mode.
- Does not install global command shims by default.

`squire codex [codex args...]`

- Recommended product path.
- Starts Codex inside a scoped Squire Kernel session.
- Uses a ready Linux microVM backend on macOS when configured; otherwise falls
  back to the host scoped session without requiring setup.
- Starts or reuses the resident background maintainer and warms local proofs.
- Keeps the model-visible command surface unchanged: Codex still emits ordinary
  commands such as `git status`, `cat package.json`, and `python --version`.
- Uses the best available scoped transport for the platform and launcher.
- Falls back to native execution on every miss, unsupported command, invalid
  proof, mutation, validation command, or package setup operation.

`squire session [--quiet] [--metadata-only|--no-warm] [--no-maintainer] [--enable-warm-file-replay] [--preload] [--preload-lib <path>] -- <command> [args...]`

- Advanced surface for starting a scoped Squire session around an ordinary shell
  or agent command.
- Starts or reuses the resident background maintainer unless `--no-maintainer`
  is set.
- Warms local proofs unless `--no-warm` is set. `--metadata-only` narrows warm
  work to enabled metadata fast paths.
- Prefers the scoped preload transport when `squire-preload.dylib` or
  `squire-preload.so` is available and the launcher is safe for preload
  inheritance.
- Uses scoped mmap shims automatically for hardened Codex on macOS, where the
  Codex binary does not load `DYLD_INSERT_LIBRARIES` but its `/bin/zsh -lc ...`
  child commands can still resolve eligible tools through a session-local PATH.
- Runs native without command interception for other known unsafe launchers or
  when no scoped transport is available.
- On macOS, system launchers protected by SIP, including `/bin/sh`,
  `/bin/zsh`, `/bin/bash`, and `/usr/bin/env`, are treated as preload-unsafe
  because they ignore `DYLD_INSERT_LIBRARIES`.
- Passes tiny native file readers such as `cat` and `sed` through by default;
  `--enable-warm-file-replay` opts into bounded preload coverage experiments.
- `--preload` requires the scoped preload transport for any launcher and errors
  if the local library is unavailable.
- `--path-shims` is a diagnostic escape hatch. It creates session-local symlinks
  to `squire-mmap-shim`; it never installs global command shims.
- This is the preferred no-model-change UX. The model sees and emits normal
  commands; Squire remains outside the prompt/tool surface.

`squire vm status [--short|--json]`

- Reports whether the Linux guest execution mode is available.
- On Linux, reports `linux-local` when the host can use the same scoped session
  kernel directly.
- On macOS, reports the `virtualization-framework` backend, the optional
  `squire-vm-darwin` helper, and whether Linux guest assets are configured.
- Reports `uses_host_command_shims: false`; VM mode is guest-owned execution,
  not the host C shim fallback.
- Does not claim macOS host-semantic preservation; the guest OS is Linux.

`squire vm session [--quiet] [--backend auto|linux-local|external-runner] [--runner <path>] -- <command> [args...]`

- Starts an isolated Linux execution session for an ordinary agent or shell
  command.
- On Linux hosts, runs through the existing scoped `squire session` machinery,
  so Linux-native interception can be used without macOS protected-shell limits.
- On macOS, uses the built-in Virtualization.framework helper when installed
  and configured.
- With `--runner` or `SQUIRE_VM_RUNNER`, delegates lifecycle to an external
  Linux guest runner using the documented runner contract.
- On macOS without a helper/guest config or runner, fails clearly instead of
  silently falling back to host-native execution with different guarantees.
- Keeps the model-visible command unchanged. The guest must preserve exact
  stdout/stderr/exit-code replay or native fallback inside Linux.

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
- This is a diagnostic/manual CLI surface; normal product integrations should
  prefer scoped preload sessions or, for host runtimes with executor
  integration, the long-lived terminal adapter so the model never has to call
  Squire.
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

`squire kernel adapter --stdio [--no-maintainer]`

- Starts a long-lived foreground compatibility adapter for terminal runtimes.
- Reads newline-delimited JSON requests from stdin and writes newline-delimited
  JSON responses to stdout.
- The request contains the already-chosen `argv`, `cwd`, optional environment
  overlay, optional exact environment replacement, and optional session ID.
- The response contains exact stdout/stderr bytes as base64 plus the exact exit
  code, mode, family, proof label, and diagnostics.
- Requests may set `debug: true` to include phase timings; they are omitted on
  the default hot path to keep the foreground adapter small and fast.
- Reuses kernel state across requests so repeated command serving does not pay
  one process startup and kernel construction per command.
- Reuses the kernel's cached mmap hot snapshot view on replay hits and writes
  the default non-debug response through a pooled, manual JSON writer.
- Starts or reuses the resident background maintainer before serving requests.
- `--ensure-maintainer` is accepted as a compatibility no-op. `--no-maintainer`
  disables automatic maintainer lifecycle only for diagnostics.
- This is a host/runtime integration point. It does not add an agent tool,
  change prompts, suggest commands, or require the model to emit a Squire
  command.

`squire-preload.dylib` / `squire-preload.so`

- Optional scoped preload library compiled by `install.sh` when `cc` is
  available.
- Preferred by `squire session` when available; it is never installed as a
  global shell preload.
- Hooks exec-family calls, refuses custom-env `execve` replays it cannot prove,
  and uses the mmap hot snapshot proof engine.
- Falls through to native exec on every miss, unsupported argv, unsupported
  launcher, or absent/corrupt snapshot.

`squire-mmap-shim`

- Optional local helper compiled by `install.sh` when `cc` is available.
- Used by `squire codex` on macOS as a scoped fallback for Codex's
  hardened runtime.
- Exposed only through a temporary session PATH. It is not installed into the
  user's shell profile and is not visible as a command the model has to choose.
- Recomputes the same local invalidation proof before replay and execs the real
  tool natively on every miss unless diagnostics explicitly require a hit.

`squire kernel warm [--metadata-only] [--short|--json]`

- Prepares local world state for likely future operations.
- Precomputes exact outputs for the enabled metadata fast paths and bounded
  proof-gated read-only candidates.
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

Squire-owned setup, status, warm, maintain, benchmark, and adjacent-prewarm Git
reads run with `GIT_OPTIONAL_LOCKS=0` to avoid contending with planner commits
in multi-agent workflows. `squire kernel run -- <command>` and `squire kernel
adapter --stdio` preserve the agent-chosen command environment for native
fallback.

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
- Includes aggregate foreground mmap hot-client replay counters without storing
  argv, cwd, stdout, stderr, or source bytes.
- Breaks out hot-client replays by foreground family:
  `hot_client_go_replays`, `hot_client_prepared_replays`, and
  `hot_client_synthetic_replays`.
- Reports aggregate native wall time avoided, measured replay wall time, average
  replay wall time, and measured net wall saved for hot-client replays.
- Reports `hot_client_last_replay_unix_nano` and
  `hot_client_last_event_unix_nano`, so a user can prove whether the visible
  counters changed during the current run rather than coming from an older
  ledger.
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
- `scripts/release_smoke.sh .tmp/squire`
- `scripts/adapter_path_bench.py .tmp/squire`, which verifies the invisible
  terminal-adapter UX path, exactness, first-invalid native fallback, and
  sub-1ms replay-hit and native-overhead budgets
- `scripts/edge_stress.py .tmp/squire --scenario echo --normal-ux --strict-performance`,
  which enforces the sub-1ms replay p95 budget in the normal terminal adapter
  path

The nightly/manual workflow runs the default `deep-local` profile plus
process-level edge stress when hot replay, prewarming, invalidation, or
maintainer lifecycle code changes:

- safety gates
- invalidation gates
- native-only discovery boundary reporting
- strict normal-UX replay budget with `scripts/edge_stress.py .tmp/squire --scenario echo --normal-ux --strict-performance`
- normal-UX adapter edge stress with `scripts/edge_stress.py .tmp/squire --normal-ux`
- normal-UX multi-agent A/B with `scripts/multi_agent_bench.py .tmp/squire`
- `scripts/edge_stress.py .tmp/squire`

Replay performance gates apply to replay wall time and require p95 below
`1ms`. Invalid/missing-cache and never-replay commands run native, so their
gate is Squire overhead above the native command, not total command wall time.

Nightly reports repo-summary proof-gated diagnostics and native-only discovery
diagnostics separately from enabled metadata fast-path exactness. Performance
gates are reported with violations, but a current `needs_optimization` result
does not fail nightly unless it also violates safety, invalidation, or
never-replay boundaries.

## Advanced

This section is for implementers and operators who want to understand how the
kernel stays transparent while still improving local command latency.

### World State And Proofs

Squire keeps a local world state model: repo root, HEAD, branch, Git directory,
config/index fingerprints, workspace epochs, file content epochs, PATH/tool
identity, and privacy-safe prepared metadata. Every replay candidate is keyed by
the exact command, cwd, relevant environment/tool identity, and the proof inputs
that can affect stdout, stderr, or exit code.

A replay is allowed only when all proof elements match:

- the operation key matches;
- input fingerprints match;
- the invalidation epoch is unchanged;
- the operator is allowlisted or proof-gated;
- exact output bytes are available locally;
- stdout, stderr, and exit code are exact;
- native fallback is available.

### Hot Replay Path

The foreground CLI first checks a daemon-published mmap hot snapshot before it
constructs the full kernel object, hydrates the ledger, or opens the socket hot
cache. Snapshot descriptors are fixed-size records for exact command outputs and
bounded workspace files. On a hit, Squire writes exact cached stdout/stderr and
exits with the cached exit code. On any miss, stale proof, corrupt descriptor,
permission error, or size violation, Squire runs the original command natively.

The mmap snapshot is a local user-space optimization. It still uses ordinary OS
memory mapping and still requires local invalidation proof; it is not a literal
kernel bypass.

### Background Maintainer

`squire kernel maintain --background` runs the prewarm loop in a separate local
process. That keeps repo scanning, proof construction, and eligible native
prewarm commands off the foreground hot path. The maintainer publishes hot
snapshots and status metadata, refreshes on changed proof signals, and avoids
agent-visible suggestions.

### Proof-Gated Replay

Proof-gated replay is broader than the tiny Git metadata fast path but still
conservative. It can cover hot-prepared read-only operations such as repo state,
bounded `cat`, bounded `sed -n`, manifest reads, tool version probes, and
command-path lookups. It does not cover validation, edits, mutating commands,
package setup, sensitive file reads, shell aliases/functions, broad search, or
network-dependent operations.

### Edge-Stress Testing

`scripts/edge_stress.py` runs process-level stress in fresh temp repos. With
`--normal-ux`, command-serving checks route ordinary argv through long-lived
adapter sessions so the measured command stream stays model-clean while setup,
warm, status, and maintainer lifecycle remain Squire-owned control-plane calls.
Process-interruption scenarios still use foreground CLI clients where the
client process is the failure target. The suite checks concurrent hot reads,
mid-warm invalidation, interrupted clients, dynamic `.gitignore`,
environment-key separation, multi-CWD proof separation, symlink boundaries,
SIGPIPE/early exit, ANSI/control-byte fidelity, mtime skew, store write
failures, EMFILE pressure, signal floods, oversized native fallback, and scoped
child-process cleanup. These tests are intentionally local and fault-open:
failures must not corrupt the cache or remove native fallback.

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
All numbers are local measurements and should be treated as scoped evidence,
not a broad Codex or agent-speedup claim.

Current measurements from this repo on 2026-06-24:

- Scoped preload mixed-command UX, `/private/tmp`, one normal zsh session:
  - Workload: Git metadata, repo summaries, bounded file reads, tool probes,
    and native-control commands in a fresh Python/TypeScript repo.
  - Agent-visible Squire command inside the measured session: `false`
  - Commands: `270`
  - Exact stdout/stderr/exit-code mismatches: `0`
  - Hot mmap replays: `204`
  - Native total: `3797.337ms`
  - Squire total: `816.421ms`
  - Workload delta: `2980.916ms`
  - Speedup: `4.651x`
  - Hot replay p50/p95/p99/max: `132us` / `880us` / `1243us` / `1428us`
- Invisible terminal-adapter UX, 1000-round `/private/tmp` run:
  - Backend: hidden `squire kernel adapter --stdio`
  - Visible measured commands: `git status --short`, `git add -h`
  - Agent-visible Squire command: `false`
  - Measured command stream contains `squire`: `false`
  - Total served commands: `3000`
  - Replay hit p95: `0.487ms`
  - Invalid/miss overhead p95: `-0.274ms`
  - Never-direct overhead p95: `0.127ms`
  - First post-mutation invalid request: `native`
  - Violations: `0`
- Strict replay echo, `/private/tmp`, 40 concurrent normal-UX requests:
  - Adapter hot replay p95: `293us`
  - Process CLI hot replay p95: `341us`
  - Budget: `1000us`
  - Violations: `0`
- Multi-agent normal-UX A/B, `/private/tmp`, 10 rounds:
  - Workload: mixed local discovery plus one never-replay native command.
  - Agent-visible Squire command: `false`
  - Exact stdout/stderr/exit-code mismatches: `0`
  - 1 agent, 80 commands: `4.780x`, `650.765ms` wall delta
  - 2 agents, 160 commands: `4.559x`, `528.408ms` wall delta
  - 4 agents, 320 commands: `4.730x`, `753.041ms` wall delta
  - 8 agents, 640 commands: `5.533x`, `1301.093ms` wall delta
  - 16 agents, 1280 commands: `5.793x`, `2247.894ms` wall delta
- Deep-local `/private/tmp` profile:
  - Runs: `1219`
  - Packages: `48`
  - Tracked files: `341`
  - Commits: `39`
  - Incremental turns: `36`
  - Safety gates: `pass`
  - Enabled fast-path exactness: `true`
  - Enabled fast-path mismatches: `0`
  - Stale HEAD replays: `0`
  - Stale branch replays: `0`
  - Validation replays: `0`
  - Metadata workload delta: `+2.550s`
  - Metadata fast-path p95: `1641us`
  - Performance status: `needs_optimization` for the older standalone
    `kernel run` native-fallback overhead profile. This is reported separately
    from the long-lived adapter product path.

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
