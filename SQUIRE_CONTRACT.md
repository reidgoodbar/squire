# Squire Contract

Squire v1 is a scoped proof for repeated local Git metadata plus
hot-prepared deterministic read-only discovery operations.

## Product Claim

Scoped proof for repeated local Git metadata plus hot-prepared deterministic
read-only discovery operations.

Squire transparently accelerates a tiny allowlist of repeated local Git
metadata operations and hot-prepared deterministic read-only discovery
operations with exact stdout, stderr, and exit-code preservation, local
validity proof, native fallback, and measured hot-path performance.

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

## Validity Proof

Squire caches observations, not authority. A cache entry becomes replayable only
after the foreground path proves that the current local world still matches the
world that produced the bytes.

Every replay proof must include:

- normalized command key and exact original argv policy;
- cwd, repo root, and tool identity;
- relevant invalidation epochs for HEAD, branch, Git config, Git index, file
  tree, file content, and workspace state;
- input fingerprints such as content hashes, config fingerprints, PATH or
  selected environment hashes, executable byte identity, OS change signals, and
  bounded workspace proof inputs;
- command-output namespace inputs such as cwd-relative Git output semantics;
- external Git behavior inputs, including global config files, included config
  files, default global ignore files, default global attributes files,
  configured `core.excludesFile`, and configured `core.attributesFile`;
- output fingerprints and locally stored exact stdout/stderr/exit-code bytes;
- policy confirmation that the operator is enabled or proof-gated and not in a
  never-replay family;
- native fallback availability.

If any element is absent, stale, mismatched, too expensive to prove, or
corrupted, Squire must execute the original command natively. Durable cache
records may be stale; they are valid only after the current proof passes.

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
- `head -n <bounded-lines> <bounded workspace source/config file>`
- `tail -n <bounded-lines> <bounded workspace source/config file>`
- `file <bounded workspace source/config file>` from exact native warm output
- `grep -F <literal> <bounded workspace source/config file>`
- `grep -q -F <literal> <bounded workspace source/config file>`
- `printenv <non-sensitive variable>`
- `ls`, `ls -p`, and `ls -la` for safe workspace directories
- `<tool> --version` and `<tool> version` for common local tools
- `pip/pip3 --version`
- `which <common-tool>`
- `command -v <common-tool>` for external PATH executables only
- `whoami`, `hostname`, `id`, and supported `uname` static environment probes

Scoped product sessions may replay tiny file readers such as `cat`, `sed`,
`head`, and `tail` when warm-file replay is enabled and the bounded file proof
is exact. If a file read is too large, unsupported, stale, or unprofitable for
the current foreground path, native fallback wins. Passing through an
unprofitable operator is a native fallback choice, not a new agent-visible
behavior.

Hot-prepared proof-gated candidates may replay only when the command key, cheap
hot fingerprints, hot invalidation epoch, output hashes, and in-memory output
bytes all match. Their p95 replay wall time is reported separately from
metadata fast-path p95.
For cwd-sensitive Git commands, the hot command key must include cwd. A replay
prepared at the repo root must not satisfy the same argv from a subdirectory
unless the proof was prepared for that subdirectory and the output bytes match
native execution from that cwd.
For Git status and diff outputs, relevant external Git inputs must be part of
the invalidation proof. Changing a global ignore file, global attributes file,
included config file, configured excludes file, or configured attributes file
must force native fallback or a freshly prepared observation.
For directory listings, the proof includes the requested cwd, target directory,
immediate entry stat signals, symlink targets, selected locale/color/blocksize
environment, `ls` executable identity, and local passwd/group/timezone files
used by long-format rendering.
For `file`, the output must come from an exact native warm observation and the
proof includes the target file content hash, size, mode, canonical path,
`file` executable identity, and selected `file(1)` environment inputs such as
locale and `MAGIC`.
For bounded literal `grep`, the proof includes the warmed file content hash,
size, mode, canonical path, exact literal pattern, quiet/non-quiet mode, and
exact argv shape. Regex grep, recursive grep, binary files, multiple input
files, and unsupported flags remain native.
For `printenv`, only a single explicitly safe variable name is eligible. Names
that look like credentials, tokens, secrets, keys, auth values, cookies, or
passwords are denied, and the proof includes the variable name, value/existence,
PATH, and `printenv` executable identity.
For static environment probes, the proof includes executable identity, PATH,
selected session environment, hostname, and process uid/gid/group identity.

The foreground CLI serving path first checks a daemon-published mmap hot
snapshot before loading ledgers or touching daemon/socket paths. If that exact
read-only snapshot proof hits, the CLI writes the cached stdout/stderr bytes
and exits with the cached exit code.
If the CLI hot client misses, the regular serving path checks resident in-memory
prepared output and then the bounded resident hot-cache IPC server when it is
running. The mmap snapshot is a local, owner-only, atomically published file
with fixed-size descriptors laid out for cache-friendly lookup. It avoids
foreground ledger hydration and Unix-socket round trips on hits, but it still
uses normal OS memory mapping and still requires local invalidation proof; it
is not a literal operating-system bypass. If the snapshot, resident cache, or
daemon is unavailable, misses, times out, returns invalid hashes, or fails the
hot proof, native execution wins. A separate background maintainer may produce
durable evidence and reports, publish the mmap snapshot, and serve exact
prepared output to fresh foreground processes over a local Unix-socket daemon
cache.

The primary production foreground is `squire codex`, which launches Codex
through a zero-extra-step backend router. On macOS, Squire must use the Linux
microVM backend when the helper and guest assets are already configured, then
fall back to the host scoped session if the VM is unavailable or fails before
Codex takes over. `squire codex` must not stop the user to provision VM assets.
On Linux hosts, it may use the local scoped session path directly. The
lower-level `squire session -- <command>` surface remains available for
advanced launchers and diagnostics. Scoped sessions prefer a local preload
library when available and the launcher is safe for preload inheritance, set
exact native fallback paths, launch the ordinary shell or agent command, and
hook selected exec-family calls inside that child process tree.
The preload library reads the resident maintainer's mmap hot snapshot directly,
serves only proven entries, refuses replay for custom-env exec calls it cannot
prove, and falls through to native exec on every miss, unsupported launcher,
unsupported argv, or absent/corrupt snapshot.

For known unsafe launchers, or when preload is unavailable, the session runs
native with no command interception. `--preload` requires the preload transport
for any launcher and must fail closed to session startup if the local library is
unavailable. Squire must not install global shell state.

On macOS, SIP-protected system launchers such as `/bin/sh`, `/bin/zsh`,
`/bin/bash`, and `/usr/bin/env` are preload-unsafe because they ignore
`DYLD_INSERT_LIBRARIES`. They must run native unless a future native adapter can
attach below that protected launcher boundary.

In Linux guest mode, Squire launches Codex preload-first when the guest preload
library is available, because the microVM removes macOS `DYLD` and protected
shell limits. The Codex binary itself may be static and ignore preload, but the
preload environment can still reach dynamic child shells if Codex and its
sandbox preserve it. The preload transport must handle both direct tool exec
and simple `execve("/bin/sh", "-c", <allowlisted command>)` shell launches
before falling back to native shell execution. Preload is the only accelerated
session transport; when preload assets are absent, Squire runs native.
The transport remains model-invisible and session-scoped: the agent emits the
same commands, Squire reads only the local hot snapshot, and every miss or
invalid proof falls back to the native executable.
Replay accounting must not require sandboxed child commands to open the ledger
or store directly. Scoped sessions should provide a session-owned inherited
event FD for tiny replay accounting events, validate those event lines in the
parent Squire process, and append them to the local store from that parent.
Scoped sessions should also pass the current hot snapshot as an inherited
read-only FD when available, so replay children can `mmap` a proven snapshot
without reopening `hot_snapshot.bin` by path on every command. Direct
event-file append and path-opened snapshots are fallback paths only.
The host VM helper may forward only a narrow Squire diagnostic/session
allowlist into the guest, such as `SQUIRE_VM_GUEST_SESSION_TRANSPORT`,
`SQUIRE_PRELOAD_TRACE`, and shim debug flags. It must not forward arbitrary host
environment variables into the guest command. Trace and hard-hit flags are
diagnostic; production performance measurements must run without
`SQUIRE_PRELOAD_TRACE` and without `SQUIRE_SHIM_REQUIRE_HIT`.

For non-protected launchers, preload may replay simple `exec*` and
`posix_spawn*` commands. The supported `posix_spawn` file-action subset is
strictly limited to recorded `close` and `dup2` actions, which covers common
stdout/stderr pipe capture. Unknown file actions or spawn attributes must fall
back native. Native fallback from the preload library must not recurse through
the interposed exec/spawn symbols; it must either use direct native exec, a
guarded native spawn path, or a fork/exec fallback with the same tracked file
actions.

Composed shell commands are a separate proof tier. In production this tier runs
only through the helper-owned `posix_spawn` path: the preload library swaps the
shell child for `squire-preload-helper --shell-ir ...`, native file actions wire
stdout/stderr exactly as they would for the shell, and the helper either emits
the proven shell-plan result or execs the original shell path with the original
`-c`/`-lc` command. A deterministic shell-plan engine may parse a tiny grammar
of words, `|`, `&&`, `;`, grouping, and `/dev/null` redirects, then evaluate
only proof-backed source commands and bounded in-memory filters. It must reject
expansion, globbing, aliases, functions, arbitrary redirects, background jobs,
unknown filters, and any node that lacks an exact replay proof. Direct
in-process `execve("/bin/sh", "-c", ...)` shell-plan replacement remains
compile-time disabled until it has separate lifecycle proof.
The shell-plan parser must use small fixed command-specific limits, not generic
process argv/path buffers. A command that exceeds the supported token, node,
argv, or word-size limit must miss the shell-plan path and execute the original
shell natively.

For tiny successful direct outputs, preload may satisfy a compatible
`posix_spawn*` call with a synthetic completed child instead of forking a real
replay child. This is allowed only when stdout is small enough for a single
pipe-safe write, stderr is empty, exit code is zero, the output comes from a
valid hot-snapshot proof, and the caller's subsequent `wait`/`waitpid` status is
byte-compatible with native success. Synthetic replay accounting is reported
separately from prepared-child replay accounting.
The synthetic-safe set is direct proof-gated reads only: local Git metadata and
repo summaries, warm-file transforms, directory/file/env probes, command path
lookups, and version probes. Unsupported output shapes continue through the
prepared helper path or native fallback unless they receive separate lifecycle
and exactness proof.

Long-lived adapter integrations remain a compatibility path for host runtimes
that already expose a command executor. A long-lived foreground may reuse the
resident hot-cache connection and keep short session-local daemon-unavailable
and exact-command miss caches. These caches must be bounded, brief, and
fault-open: they may suppress replay attempts, but they must never suppress
native execution. For adapter integrations, replay checks should reuse the
foreground's cached mmap snapshot view rather than map/unmap the snapshot on
every request. Adapter responses may use pooled buffers to reduce allocation
churn, but the wire protocol must still preserve exact stdout/stderr bytes and
exit code.

The production foreground is host/runtime owned, not model owned. `squire
codex`, a scoped session, or a terminal adapter may serve already-chosen
commands through Squire, but the agent-facing command text must remain the
original command. These foregrounds must not add tools, change prompts, suggest
commands, route models, or require the model to call Squire. The normal session
and adapter start or reuse the resident background maintainer by default so the
maintainer lifecycle is a host concern, not an agent behavior. Manual `squire
kernel run -- <command>` remains a diagnostic surface, not the primary product
UX.

`squire vm session -- <command>` is a separate Linux guest execution mode. It
exists to run the ordinary agent loop inside a Linux environment where
`LD_PRELOAD`, process interception, and Linux deployment parity are first-class
instead of fighting host macOS protected shells and hardened runtimes. The
agent-facing command text must still remain unchanged, and the guest must obey
the same replay contract: exact stdout/stderr/exit-code or native fallback
inside the guest. VM mode must not depend on host command shims,
session-local host PATH tricks, or standalone C PATH shims.

On Linux hosts, `squire vm session` may use the same scoped session path
directly. On macOS, VM mode uses the built-in `squire-vm-darwin`
Virtualization.framework helper when that helper is installed and Linux guest
assets are configured. The helper must be signed with the
`com.apple.security.virtualization` entitlement and the host/session must report
Virtualization.framework support. The helper may boot a configured Linux kernel
with an initrd or disk image, expose the workspace and store through virtiofs,
and send the already-chosen command to a guest agent over an exact framed guest
protocol. The current working macOS MVP uses virtio serial by default; vsock is
allowed only when the guest kernel/initramfs proves AF_VSOCK support. Squire
does not bundle a guest image yet, so helper presence alone is not an
availability proof.
The guest bootstrap may switch from the raw initramfs rootfs into a
tmpfs-backed root so Linux sandbox tools can use `pivot_root`. Guest `/tmp`
must be a normal writable sticky directory.
Interactive guest sessions must keep terminal bytes separate from control
messages. The macOS helper uses one virtio serial channel for the framed
request/response protocol and a second virtio serial channel as the guest TTY
for interactive commands such as `codex`. The helper must propagate the host
terminal row/column size into the guest TTY before launching the interactive
command.

Codex inside VM mode requires an explicit Linux Codex guest bundle and an
explicit `SQUIRE_VM_CODEX_HOME` mount when the user wants to expose local Codex
auth/config to the guest. Squire must not mount Codex credentials implicitly.
The guest may install ordinary Linux sandbox/discovery tools such as
`bubblewrap`, `git`, and `ripgrep` as part of the guest bootstrap, but those
tools run inside the guest and do not change the agent-visible command text.
Codex sandboxed read-only commands such as `git status --short --branch` must
run inside the guest without requiring a host approval fallback.

An explicitly configured `SQUIRE_VM_RUNNER` or `--runner` may provide the same
guest contract externally. VM mode must not pretend to preserve host-native
macOS command semantics. Xcode, iOS, Homebrew-only, and other macOS-specific
workflows should remain host-native.

Workspace file inspection replay is limited to safe relative paths inside the
workspace, regular files below the bounded size limits, non-hidden/VCS paths,
and source/config extensions or well-known project metadata files. It is
invalidated by local file proof. Exact command observations include the exact
argv in their proof. Warm-file entries are keyed by relative path, file content
hash, size, mode, and OS change signal, and may materialize arbitrary eligible
bounded `sed -n`, `head`, `tail`, and literal `grep -F` windows from those same
proven bytes without precomputing every possible range. `file(1)` remains
native-precomputed exact output, not guessed from extension.
Tool discovery replay is invalidated by PATH, selected environment variables,
and executable byte identity. `.env`, hidden paths, likely secret/token/key
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
eligible bounded `cat`, `sed -n`, `head`, `tail`, and literal `grep -F`
requests while the file proof still matches. `file(1)` observations replay only
from native-precomputed exact output. If a command has no complete cheap hot
proof, native execution wins on the foreground serving path.

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

Squire must work without OpenTelemetry. OTel is optional session
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
the Squire contract, keep native filesystem state authoritative, and prioritize
safety, auditability, and native fallback.

- Level 3, Virtual Memory-Mapped Workspace: The production-safe subset is
  implemented as read-only workspace acceleration. Bounded eligible file bytes
  are warmed into a daemon-published mmap hot snapshot, and arbitrary bounded
  `cat`, `sed -n`, `head`, `tail`, and literal `grep -F` outputs can be
  materialized from those proven bytes while the file proof still matches.
  Native-precomputed `file(1)` outputs may replay from the same bounded path
  proof. This improves repeated local reads without creating a virtual write
  layer. There is no edit replay, no mutating CoW overlay, no asynchronous
  write flush, and no production rollback layer in v1.

- Level 4, Schema-Native IPC: The production-safe subset is structured internal
  diagnostics, binary hot-cache IPC, and mmap snapshot descriptors. Status
  diagnostics parse the binary snapshot header and descriptor table to report
  exact command entries, workspace image files, payload bytes, and availability.
  This cannot replace the agent-visible command contract: CLI commands must
  still return exact stdout, stderr, and native exit codes.

- Level 5, Anticipatory Speculative Execution: The production-safe subset is
  local idle-window prewarming after observed native operations. Bounded
  file-inspection commands prewarm adjacent read windows and eligible file
  bytes; manifest/config reads prewarm local follow-up files plus deterministic
  tool version/path probes. It must not monitor model token streams, change
  prompts, surface agent-visible suggestions, or mutate workspace state.
  Speculation remains bounded, local-only, and fault-open.
