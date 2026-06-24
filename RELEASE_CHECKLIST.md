# Squire Kernel Release Checklist

Use this checklist before publishing a Squire Kernel build. Keep release claims
scoped to the proven kernel behavior.

## Release Claim

Allowed claim:

> Scoped kernel proof for repeated local Git metadata plus hot-prepared
> deterministic read-only discovery operations.

Do not claim broad Codex, Claude, or general agent speedups. Report measured
workload deltas only for the benchmark or dogfood run that produced them.

## Build Identity

Release builds should set build identity with Go linker flags:

```sh
go build \
  -ldflags "-X main.buildVersion=v0.0.0 -X main.buildCommit=$(git rev-parse --short HEAD) -X main.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o ./squire ./cmd/squire
```

Smoke the identity surface:

```sh
./squire version
./squire version --json
./squire --help
./squire vm status --short
```

## Required Checks

Normal release checks:

```sh
go test ./...
go run ./cmd/squire boost bench repo-metadata
go run ./cmd/squire boost bench deep-local
go run ./cmd/squire vm status --short
scripts/release_smoke.sh ./squire
scripts/adapter_path_bench.py ./squire
scripts/edge_stress.py ./squire --scenario echo --normal-ux --strict-performance
scripts/multi_agent_bench.py ./squire --agents 1,2 --rounds 3
scripts/edge_stress.py ./squire --normal-ux
scripts/edge_stress.py ./squire
```

Required gates:

- Enabled fast-path exactness is true.
- Enabled fast-path mismatches are zero.
- Stale HEAD replays are zero.
- Stale branch replays are zero.
- Validation replays are zero.
- Safety gates pass.
- Mutation-boundary invalidation is observed in the repo-metadata benchmark.
- Adapter replay hits stay below the scoped `1ms` p95 wall-time budget.
- Adapter invalid/miss fallback and never-direct paths stay below the scoped
  p95 Squire-overhead budget above native execution.
- Multi-agent adapter A/B keeps exact stdout/stderr/exit-code equality with
  zero mismatches and a Squire-free measured command stream.
- VM status reports honest availability. On Linux it may report `linux-local`;
  on macOS it must report `virtualization-framework` unavailable unless an
  explicit guest runner is configured.

Performance gates must be reported. A performance gate failure blocks a release
only when the release claim depends on that budget.

## GitHub Release Flow

GitHub Actions provides three release surfaces:

- `.github/workflows/ci.yml` runs fast tests, the repo metadata benchmark, and
  release smoke on pushes and pull requests.
- `.github/workflows/nightly.yml` runs the deeper baseline, strict replay
  budget, adapter benchmarks, multi-agent A/B, and edge stress on a schedule
  and manually.
- `.github/workflows/release.yml` runs the release gate, builds artifacts, and
  publishes a GitHub Release from either a `v*` tag or manual dispatch.

To publish from a tag:

```sh
git tag v0.1.0-beta.1
git push origin v0.1.0-beta.1
```

To publish manually, run the `release` workflow in GitHub Actions and provide
the desired version, for example `v0.1.0-beta.1`.

Current releases should use the `v0.x.y-beta.N` shape. The release workflow
publishes `v0.*`, `*-alpha*`, `*-beta*`, and `*-rc*` versions as GitHub
prereleases.

The release workflow must pass before artifacts are published. It runs:

- `go test ./...`
- release-candidate build with version ldflags
- `squire vm status --short`
- `squire boost bench repo-metadata`
- `squire boost bench deep-local`
- `scripts/release_smoke.sh`
- `scripts/adapter_path_bench.py`
- `scripts/session_preload_bench.py ./squire --rounds 5 --commands 100`
- `scripts/edge_stress.py --scenario echo --normal-ux --strict-performance`
- `scripts/multi_agent_bench.py --agents 1,2 --rounds 3`
- `scripts/edge_stress.py --normal-ux`
- `scripts/edge_stress.py`
- release safety-gate assertions

The workflow then builds:

- `linux/amd64`
- `linux/arm64`
- `darwin/amd64`
- `darwin/arm64`
- `windows/amd64`

Each release includes `.tar.gz` archives, `SHA256SUMS`, and
`install.sh`, and `RELEASE_MANIFEST.txt`. Verify downloaded artifacts with:

```sh
shasum -a 256 -c SHA256SUMS
# or, on Linux:
sha256sum -c SHA256SUMS
```

The public install UX is:

```sh
curl -fsSL https://squire.run/install.sh | bash
```

The installer is a user-level binary install. It must not install global
command shims, start a global daemon, or create repo-local state outside the
workspace where Squire later runs. On macOS, it should also print the Homebrew
zsh note without claiming shell preload is enabled, because protected Apple
shells ignore `DYLD_INSERT_LIBRARIES`. It should install the local
`squire-mmap-shim` helper when `cc` is available; this helper is used only
through scoped session PATHs, never as a global command shim.

Local artifact builds use the same script as GitHub:

```sh
VERSION=v0.1.0-beta.1 scripts/build_release_artifacts.sh .tmp/release
```

Smoke the installer against local release artifacts before publishing:

```sh
tmpbin=$(mktemp -d)
SQUIRE_RELEASE_TARGETS="darwin arm64" \
VERSION=v0.1.0-beta.1 \
scripts/build_release_artifacts.sh .tmp/release
SQUIRE_KERNEL_ARTIFACT_DIR=.tmp/release \
SQUIRE_VERSION=v0.1.0-beta.1 \
SQUIRE_INSTALL_DIR="$tmpbin" \
./install.sh
"$tmpbin/squire" version --short
```

Omit `SQUIRE_RELEASE_TARGETS` in CI and for real release builds; the default
builds every supported platform.

## Dogfood Smoke

Use the scripted smoke when possible:

```sh
scripts/release_smoke.sh ./squire
```

Run the normal-UX and process-level edge stress suites before release candidates
that touch hot replay, prewarming, invalidation, or maintainer lifecycle code:

```sh
scripts/edge_stress.py ./squire --normal-ux
scripts/edge_stress.py ./squire
```

Run the adapter path benchmark before release candidates that touch foreground
serving, classification, hot replay, native fallback, or terminal adapter code:

```sh
scripts/adapter_path_bench.py ./squire
```

Run the scoped preload session benchmark before release candidates that touch
the product UX launcher, preload publication, hot snapshot replay, native
fallback, or C replay coverage:

```sh
scripts/session_preload_bench.py ./squire --rounds 5 --commands 100
scripts/session_preload_bench.py ./squire --rounds 5 --commands 100 --shell-launcher
```

This benchmark measures the intended product direction: `squire session` with a
scoped preload library, a non-protected local launcher, and ordinary
`posix_spawnp("git", "rev-parse", "HEAD")` child commands with stdout captured
through `posix_spawn` file actions. It must show exact output and a positive
preload replay delta, with production replay p95 below `1ms` for the metadata
workload. Run both direct `git` spawn and shell-launched
`sh -c "git rev-parse HEAD"` because Codex frequently emits the shell-shaped
path. The default measurement is production fallback mode. Use
`--require-hit-measurement` only when debugging hard-hit replay misses.

Compile the optional preload transport before release candidates that touch
`shims/` or session transport code:

```sh
# macOS
cc -O3 -DNDEBUG -dynamiclib -o /tmp/squire-preload-test.dylib shims/squire_preload.c
# Linux
cc -O3 -DNDEBUG -shared -fPIC -o /tmp/squire-preload-test.so shims/squire_preload.c -ldl -lcrypto
```

The preload transport is the preferred scoped-session path when its local
library is available and the launcher is safe for preload inheritance.
`squire session --preload -- ...` requires that transport for diagnostics and
launcher-specific testing.

On macOS, verify that SIP-protected launchers such as `/bin/zsh`, `/bin/sh`,
`/bin/bash`, and `/usr/bin/env` run native in auto mode. Verify preload
performance with a non-protected user binary; protected system shells cannot
prove the preload path because the OS ignores `DYLD_INSERT_LIBRARIES`. Homebrew
shell launchers must also remain native until `squire session -- /opt/homebrew/bin/zsh -lc true`
and an equivalent command smoke complete without hangs.

For VM mode, verify the CLI surface before every release:

```sh
squire vm status --short
squire vm status --json
```

Expected behavior on macOS without a VM helper or guest runner:

- backend is `virtualization-framework`
- available is `false`
- diagnostics say `squire-vm-darwin` is not installed or configured
- `uses_host_command_shims` is `false`
- no claim is made that Linux guest execution preserves macOS host semantics

On macOS release candidates, compile the built-in Virtualization.framework
helper from the release archive source:

```sh
swiftc -parse-as-library -module-cache-path /tmp/squire-swift-module-cache \
  -O -o /tmp/squire-vm-darwin vm/squire_vm_darwin.swift
codesign --force --sign - --entitlements vm/squire_vm_darwin.entitlements \
  /tmp/squire-vm-darwin
```

Then check helper detection without guest assets:

```sh
SQUIRE_VM_HELPER=/tmp/squire-vm-darwin squire vm status --short
```

Expected behavior:

- backend is `virtualization-framework`
- available is `false`
- `vm_helper` points at the helper
- `guest_configured` is `false`
- diagnostics identify missing `SQUIRE_VM_KERNEL` and `SQUIRE_VM_INITRD` or
  `SQUIRE_VM_DISK`
- if the host/session cannot use Virtualization.framework, diagnostics say
  `Virtualization.framework reports this host is unsupported`

When a test guest bundle exists, verify configured status:

```sh
SQUIRE_VM_HELPER=/tmp/squire-vm-darwin \
SQUIRE_VM_KERNEL=/path/to/kernel.Image \
SQUIRE_VM_INITRD=/path/to/initrd \
  squire vm status --json
```

Expected behavior:

- `guest_configured` is `true`
- `uses_host_command_shims` is `false`
- no broad speedup claim is made

From a source checkout, build and smoke a local Alpine-based guest bundle:

```sh
scripts/build_vm_guest_bundle.sh /tmp/squire-vm-guest
SQUIRE_VM_HELPER=/tmp/squire-vm-darwin \
SQUIRE_VM_KERNEL=/tmp/squire-vm-guest/kernel.Image \
SQUIRE_VM_INITRD=/tmp/squire-vm-guest/initrd \
  squire vm session --quiet -- /bin/sh -lc 'echo hello from guest'
```

Expected behavior:

- stdout is exactly `hello from guest\n`
- exit code is `0`
- `uses_host_command_shims` remains `false`
- the test is run from a normal macOS terminal or CI runner that can use
  Virtualization.framework; sandboxed development sessions may report host
  support as unavailable even with a correctly signed helper

For Codex-capable macOS release candidates, build the Codex-enabled guest and
verify the full Codex path from a normal terminal:

```sh
SQUIRE_VM_INCLUDE_CODEX=1 scripts/build_vm_guest_bundle.sh /tmp/squire-vm-guest-codex
SQUIRE_VM_HELPER=/tmp/squire-vm-darwin \
SQUIRE_VM_KERNEL=/tmp/squire-vm-guest-codex/kernel.Image \
SQUIRE_VM_INITRD=/tmp/squire-vm-guest-codex/initrd \
SQUIRE_VM_CODEX_HOME="$HOME/.codex" \
SQUIRE_VM_BOOT_TIMEOUT_SECONDS=120 \
  squire vm session --quiet -- codex doctor --summary
SQUIRE_VM_HELPER=/tmp/squire-vm-darwin \
SQUIRE_VM_KERNEL=/tmp/squire-vm-guest-codex/kernel.Image \
SQUIRE_VM_INITRD=/tmp/squire-vm-guest-codex/initrd \
SQUIRE_VM_CODEX_HOME="$HOME/.codex" \
SQUIRE_VM_BOOT_TIMEOUT_SECONDS=120 \
  squire vm session --quiet -- codex exec --skip-git-repo-check \
  'Print exactly VM_CODEX_OK.'
SQUIRE_VM_HELPER=/tmp/squire-vm-darwin \
SQUIRE_VM_KERNEL=/tmp/squire-vm-guest-codex/kernel.Image \
SQUIRE_VM_INITRD=/tmp/squire-vm-guest-codex/initrd \
SQUIRE_VM_CODEX_HOME="$HOME/.codex" \
SQUIRE_VM_BOOT_TIMEOUT_SECONDS=120 \
  squire vm session --quiet -- codex exec --skip-git-repo-check \
  'Run git status --short --branch and report the exact output. Do not modify files.'
SQUIRE_VM_INTERACTIVE=1 \
SQUIRE_VM_HELPER=/tmp/squire-vm-darwin \
SQUIRE_VM_KERNEL=/tmp/squire-vm-guest-codex/kernel.Image \
SQUIRE_VM_INITRD=/tmp/squire-vm-guest-codex/initrd \
SQUIRE_VM_CODEX_HOME="$HOME/.codex" \
SQUIRE_VM_BOOT_TIMEOUT_SECONDS=120 \
  squire vm session --quiet -- /bin/sh -lc \
  'if test -t 0; then echo TTY_OK; else echo NO_TTY; fi; /usr/bin/busybox stty size'
```

Expected behavior:

- guest networking resolves and reaches active Codex provider endpoints
- Codex auth/config loads only when `SQUIRE_VM_CODEX_HOME` is explicit
- guest-side `bubblewrap`, `git`, and `ripgrep` are available
- `codex exec` exits `0` and prints the requested response
- sandboxed `git status --short --branch` runs without `/tmp` or `pivot_root`
  bubblewrap failures
- the interactive serial path prints `TTY_OK` and a nonzero row/column size
- `uses_host_command_shims` remains `false`

Expected behavior on Linux:

- backend is `linux-local`
- available is `true`
- `uses_host_command_shims` is `false`
- `squire vm session -- <command>` uses the scoped session kernel directly

If `SQUIRE_VM_RUNNER` or `--runner` is configured, smoke the runner contract in
a temp repo:

```sh
SQUIRE_VM_RUNNER=/path/to/linux-guest-runner \
  squire vm session -- codex exec --ephemeral --sandbox workspace-write \
  -C "$repo" \
  "Run git rev-parse HEAD in this repo and report the exact output. Do not modify files."
```

The runner must receive:

```sh
<runner> session --cwd <host-cwd> --store-root <store-root> -- <command> [args...]
```

and must preserve exact stdout/stderr/exit-code or native fallback inside the
Linux guest.

For Codex on macOS, verify the Linux guest preload path with a fresh temp Git
repo. Leave trace and hard-hit flags unset for performance measurements; enable
`SQUIRE_PRELOAD_TRACE=1 SQUIRE_SHIM_REQUIRE_HIT=1` only for replay reachability
diagnostics.

```sh
squire vm session -- codex exec --skip-git-repo-check \
  "Run git rev-parse HEAD in this repo and report the exact output. Do not modify files."
```

Expected behavior:

- Codex still logs the ordinary command shape `git rev-parse HEAD`.
- With diagnostic flags enabled, the preload trace reaches
  `shell-execve-attempt git` and accounting reaches `event-write-fd-ok`.
- `squire boost status --json` increments `hot_client_replays`.

Run the strict replay-budget check before release candidates that touch hot
snapshot replay or the terminal adapter path:

```sh
scripts/edge_stress.py ./squire --scenario echo --normal-ux --strict-performance
```

Run the multi-agent adapter benchmark before release candidates that touch
adapter concurrency, hot snapshot reads, resident maintainer behavior, or
normal terminal UX:

```sh
scripts/multi_agent_bench.py ./squire --agents 1,2 --rounds 3
```

Expected behavior:

- Normal-UX command-serving checks route ordinary argv through long-lived
  adapter sessions or scoped C mmap sessions; model-visible command text
  remains Squire-free.
- `squire session -- <command>` prefers scoped preload when available and runs
  native when preload cannot attach.
- Scoped sessions pass replay accounting and the hot snapshot through inherited
  FDs when available, avoiding per-command store writes and snapshot path opens
  on replay hits.
- The normal adapter command is `squire kernel adapter --stdio`; it starts or
  reuses the resident background maintainer by default.
- Replay hits return from the mmap hot snapshot under the 1ms p95 budget.
- Replayable invalid/missing-cache commands fall back native with small p95
  overhead.
- Never-replay commands use native-direct behavior with small p95
  overhead.
- Concurrent identical hot requests preserve exact output and report hot replay
  p50/p95/max timing.
- Mid-warm workspace mutations never replay stale file bytes.
- Interrupted foreground clients leave the resident maintainer healthy, with
  file-descriptor counts flat when the platform exposes them.
- Dynamic `.gitignore` changes invalidate stale status output.
- Environment-sensitive path discovery keeps distinct PATH footprints separate.
- Multi-agent runs preserve exact output across concurrent long-lived adapter
  clients and report scoped wall-time deltas for that workload only.

Run the release binary through a fresh local repo:

```sh
tmpdir=$(mktemp -d)
cd "$tmpdir"
git init
git config user.email squire@example.invalid
git config user.name "Squire Release"
printf "hello\n" > README.md
git add README.md
git commit -m init

/path/to/squire setup
/path/to/squire kernel maintain --background --short
/path/to/squire kernel warm --short
/path/to/squire kernel status --short
/path/to/squire kernel run -- git rev-parse HEAD
/path/to/squire kernel run -- git status --short
/path/to/squire boost status --short
/path/to/squire kernel maintain --stop --short
```

Expected behavior:

- Native fallback is reported as available.
- The maintainer starts once and stops cleanly.
- `kernel status --short` reports a repo oracle and readiness state.
- `kernel run` preserves exact stdout, stderr, and exit code.
- Mutating and validation commands remain native-only.

## Privacy And Boundary Review

Before release, confirm the README and CLI output still state:

- Agent chooses. Squire serves.
- Native fallback always exists.
- Validation, edits, mutations, and package installs are never replayed.
- Exact stdout, stderr, and exit code are required for replacement.
- Standard mode avoids raw prompts, completions, arbitrary stdout/stderr,
  source contents, auth values, and account identifiers.

## Artifact Notes

Attach benchmark JSON artifacts for:

- `repo-metadata`
- `deep-local`
- `adapter-path-bench`
- `strict-replay-budget`
- `multi-agent-bench`

Release notes should summarize exactness, invalidation, replay counts, and
measured wall-time deltas with the command count and workload scope that
produced them.
