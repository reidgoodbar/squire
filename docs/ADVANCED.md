# Squire Advanced Notes

This document keeps implementation detail out of the top-level README.

## Architecture Levels

Level 0, Observe:

- Observe repo, file, and process state.
- Record evidence quality.
- No acceleration required.

Level 1, Prepare:

- Maintain repo and world state.
- Precompute safe local metadata, hashes, and bounded read-only outputs.
- Track invalidation epochs.
- Do not alter command execution.

Level 2, Transparent Fast Path:

- Serve exact stdout, stderr, and exit code only for allowlisted or
  proof-gated read-only operations.
- Require a local invalidation proof.
- Fall back to native execution on every miss or unsafe operation.

## Foreground Product Path

`squire-codex` is the normal user path and is equivalent to `squire codex`.
It is a backend router:

- On macOS, use the Linux microVM backend when it is already configured.
- If the VM is unavailable or fails before Codex takes over, fall back to the
  host scoped session.
- On Linux, use the local scoped session path directly.
- Do not stop the user to provision VM assets.

The model-visible command surface stays unchanged. Codex still emits ordinary
commands such as `git status`, `cat package.json`, and `python --version`.

## Scoped Sessions

`squire session -- <command>` is an advanced surface for wrapping an ordinary
shell or agent command.

The session:

- starts or reuses the resident maintainer;
- warms local proofs unless disabled;
- uses scoped preload when the launcher supports it;
- uses session-local mmap helpers for hardened launchers when needed;
- never installs global command shims;
- falls through to native execution on every unsupported or unsafe path.

On macOS, SIP-protected launchers such as `/bin/sh`, `/bin/zsh`, `/bin/bash`,
and `/usr/bin/env` ignore `DYLD_INSERT_LIBRARIES`, so they are preload-unsafe.

## VM Mode

`squire vm session -- <command>` is an advanced Linux-isolated execution mode.

On macOS it uses the optional `squire-vm-darwin` helper built with
Virtualization.framework. The helper shares the host workspace and Squire store
with a Linux guest and sends the already-chosen command to a guest agent.

VM mode is useful for release testing and Linux-compatible projects, but the
default user command remains:

```sh
squire-codex
```

## Hot Replay Path

The foreground hot path checks a daemon-published mmap snapshot before loading
the full ledger or opening daemon sockets.

A replay requires:

- exact normalized operation key;
- exact cwd/repo proof inputs;
- unchanged invalidation epoch;
- allowlisted or proof-gated operator policy;
- exact stdout/stderr/exit-code bytes;
- available native fallback.

The mmap snapshot is a local owner-only file with fixed-size descriptors. It is
not a literal operating-system bypass; native filesystem state remains
authoritative.
The cache can contain stale records; they are replayable only when these proof
inputs match the current local world.

Git repo-summary replay has extra proof inputs because Git output can change
without a source-file edit:

- cwd is part of the hot command key, so cwd-relative outputs such as
  `git status --short` cannot cross directory boundaries;
- Git config fingerprints include local, global, system, environment-provided,
  and recursively included config files;
- ignore proof includes workspace ignore files, `.git/info/exclude`, default
  global Git ignore files, and configured `core.excludesFile` targets;
- diff attribute proof includes workspace attributes, `.git/info/attributes`,
  default global Git attributes files, and configured `core.attributesFile`
  targets.

The regression suite intentionally mutates each of those external inputs after
warming to confirm Squire falls back native or requires a fresh proof instead
of serving stale bytes.

## Background Maintainer

The resident maintainer keeps local proofs warm and publishes hot snapshots.
It may prewarm bounded read-only outputs during idle time, but it never changes
the agent's command choice and never mutates workspace state.

Prepared outputs are split by eligibility:

- replay eligible: exact Git metadata and proof-gated read-only observations;
- read-image eligible: bounded safe workspace file bytes;
- hash-only: tree indexes, project metadata fingerprints, PATH indexes,
  ecosystem proof seeds, source symbol/import summaries, and process diagnostics.

## Operator Sets

Enabled fast paths:

- `git rev-parse HEAD`
- `git rev-parse --git-dir`
- `git rev-parse --abbrev-ref HEAD`
- `git rev-parse --show-toplevel`
- `git rev-parse --is-inside-work-tree`

Proof-gated replay candidates:

- `git ls-files`
- `git status --short`
- `git status --porcelain`
- supported `git diff` forms
- bounded safe `cat`, `sed -n`, `head`, `tail`, `grep -F`, and single-file `rg -F` workspace reads/searches
- common tool version probes
- simple external PATH `which` and `command -v`

Native-only discovery:

- `git remote -v`
- `git remote get-url origin`
- `rg --files`
- recursive, regex, multi-path, or otherwise unbounded `rg` searches

Never replay:

- validation/build/test commands;
- edits and writing formatters;
- mutating Git commands;
- package installs/fetches;
- sensitive reads, `.env`, shell aliases/functions, broad unknown shell, and
  unknown binary reads.

## Production-Safe Levels 3-5

Level 3, Virtual Memory-Mapped Workspace:

- Implemented only as read-only workspace acceleration.
- No virtual write layer, edit replay, async write flush, or rollback system in
  v1.

Level 4, Schema-Native IPC:

- Implemented as internal binary hot-cache IPC and mmap descriptors.
- Agent-visible command output remains exact stdout/stderr/exit-code bytes.

Level 5, Anticipatory Speculation:

- Implemented as bounded local idle-window prewarming.
- No token-stream monitoring, prompt changes, suggestions, or workspace
  mutation.

## Privacy

Standard mode preserves operational metadata such as durations, event names,
operation hashes, file hashes, diff hashes, and repo fingerprints.

It should not persist raw prompts, raw completions, raw tool arguments, auth
values, account identifiers, arbitrary stdout/stderr, or source file contents.
Fast-path output storage is local, bounded, purgeable, and limited to eligible
operations.
