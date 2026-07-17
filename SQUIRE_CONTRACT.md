# Squire Contract

Squire is a transparent local execution accelerator for coding agents. The
agent chooses an ordinary command. Squire may serve its exact result when the
current machine state proves a prepared observation is still valid. Otherwise
the agent runtime executes the original command normally.

## Product Boundary

The normal path is:

```text
agent -> Squire adapter -> versioned local runtime -> exact hit or native path
```

Squire does not add a model tool, modify prompts, choose commands, bypass the
agent sandbox, or own OpenAI authentication. `squire codex` launches the
maintained Codex adapter and uses the normal Codex home and configuration.

Adapters submit the command cwd, argv, and child environment to runtime ABI 1.
The runtime returns one decision:

- `HIT`: exact stdout bytes, stderr bytes, and exit status are complete. The
  adapter returns them through the agent's normal result type.
- `MISS`: the operation is supported, but no current proof is available. The
  adapter follows its original native execution path and may request
  preparation asynchronously.
- `UNSUPPORTED`: the production policy does not accelerate this command. The
  adapter immediately follows its original native execution path and does not
  prepare for it.

The adapter, not Squire, owns native fallback. This preserves the runtime's
existing approvals, sandbox, timeout, cancellation, streaming, and process
lifecycle behavior.

## Invariants

1. The agent's command text and intent are unchanged.
2. A hit preserves exact stdout, stderr, and exit status.
3. A miss never suppresses or delays native execution for preparation.
4. Mutations, builds, tests, installs, and unknown operations are native.
5. Every hit is validated against current local state in the foreground.
6. Corrupt, incomplete, incompatible, or stale state is a miss.
7. A composed command is accelerated only when the complete parsed plan is
   supported and every source has a valid proof.
8. Native fallback remains available even when every Squire component fails.

## Validity Proof

The cache stores observations, not authority. A stored result is replayable
only when a foreground proof establishes that the inputs affecting that exact
command still match the prepared epoch. The runtime either recomputes that
fingerprint or reuses a process-resident fingerprint while a complete kernel
change guard proves that none of its watched dependencies changed. A bounded
file operation may also hit without replay: the runtime can execute its fixed
byte grammar over the exact current bytes retained by the foreground
content-hash read.

Depending on the command, proof inputs include:

- normalized argv, cwd, canonical workspace root, and Git common directory;
- HEAD and symbolic-ref identity;
- Git index, config, ignore, attributes, replacement refs, and graft inputs;
- Git log-visible refs plus the loose and packed object namespace for bounded
  history queries;
- relevant tracked, untracked, and working-tree state;
- canonical file path, mode, size, and cryptographic content hash;
- the validated bounded operation plan, including ordered line selections or a
  fixed literal and the exact supported flag shape;
- command-specific environment inputs and executable identity;
- prepared stdout, stderr, exit status, and output hashes.

Environment-sensitive lanes such as `printenv`, `ls`, `file`, fixed searches,
and compositions containing search or sort require the child environment to
match the preparation process exactly, including unset versus empty values.
Narrow Git metadata forms compare only the command-specific variables that can
affect those fixed outputs. Any mismatch is a native fallback.

Git config proof includes local, global, system, environment-selected, and
recursively included config files. Status and diff proof also includes ignore
and attributes sources outside the worktree when Git can consult them.

The process-resident repository guard uses `kqueue` on macOS and `inotify` on
Linux. Its first proof registers the workspace and every external dependency
consulted by that proof. Reuse is allowed only if registration completed, the
event queue drains cleanly, dependency identity still matches, and the command
epoch exists in the current snapshot. Queue failure, overflow, a missing watch,
environment change, or any material event discards the resident world and
forces recomputation or native fallback. Notifications are an optimization for
reusing a cryptographic proof, not authority by themselves.

The runtime validates current state synchronously before exposing cached bytes.
The mmap snapshot is atomically published and owner-only; mapping it does not
make its records valid by itself. Old records may remain on disk indefinitely,
but an epoch mismatch makes them unusable.

File proof uses content hashes rather than trusting only size or mtime. The
regression suite specifically replaces files atomically with same-size content
and restores their original mtimes. Such replacements must never expose old
bytes: they either execute over the newly hashed bytes in-process or fall back
to native execution. A matching prepared snapshot remains the faster path.

## Workspace Model

Workspace selection happens for each actual command cwd. Starting Codex above
a repository and later working inside it requires no manual warm command.
Subdirectories and equivalent `git -C` requests converge on the same workspace,
keyed by the resolved Git storage root. Multiple repositories in one agent
session are prepared independently.

Preparation is background work. A cold snapshot-backed request runs natively
while one preparation request per workspace is in flight. Supported bounded
file operations can execute directly over current bytes without waiting for
preparation. Unsupported commands never trigger preparation.

## Production Lanes

The production runtime groups operations by proof cost instead of making one
latency claim for every hit.

Metadata lane:

- supported `git rev-parse` metadata forms;
- `git branch --show-current`.

File and search lane:

- bounded workspace `cat`, ordered single- or multi-range `sed -n`, `head`,
  `tail`, bounded `.log` reads, and fused
  `nl -ba FILE | sed -n 'X,Yp;A,Bp'`;
- `file`, fixed-string single-file `grep -F`/`rg -F`, and bounded repository
  fixed-string `rg` forms;
- tight `ls` forms.

Environment and tool lane:

- supported tool version and `which`/`command -v` probes with executable
  identity proof;
- safe `printenv`, `whoami`, `id`, `hostname`, and supported `uname` probes.

Repository lane:

- supported `git status`, `git ls-files`, and `git diff` forms, including
  `git diff --check`;
- bounded `git log -N --oneline -- <literal paths>` history when the repository
  uses standard object storage and the inspected history has no merge ambiguity;
- read-only compositions over supported sources and filters such as `head`,
  `tail`, bounded `sed -n`, `grep -F`, `wc -l`, and no-argument `sort`.

Any unsupported token, expansion, glob, redirect other than an explicitly
supported `/dev/null` form, background job, mutation, or unknown command causes
the entire shell composition to run natively. Squire does not partially replace
a shell plan. `rg --files` is also native because unchanged filesystem state
does not guarantee stable byte ordering across invocations.

No operation is removed from the agent. `UNSUPPORTED` and cold or stale `MISS`
decisions preserve the original native execution path. Safe misses may enqueue
bounded exact preparation for the next occurrence, but preparation never delays
the current native command.

## Extension Boundary

The runtime compiles supported argv or shell text into a typed bounded plan.
Source nodes acquire proof-gated bytes; pure filter nodes transform only those
bytes; composition nodes connect complete supported subplans. File identity and
content proof are independent of the selected read operation, so adding another
bounded selector does not create a new cache key or invalidation scheme.

A new production operator must have matching Go and native parsers, a current
state proof, a bounded evaluator, exact native fallback, and byte-for-byte
differential and invalidation tests. If any stage cannot represent the command
completely, the entire plan is `UNSUPPORTED`; Squire never partially replaces
an unknown shell program.

## Performance Contract

Correctness is mandatory; acceleration is conditional. Squire reports:

- end-to-end runtime decision latency;
- hit and miss latency separately;
- exactness mismatches and invalidation outcomes;
- estimated native time avoided only for measured hits.

All supported hot lanes target end-to-end runtime p99 below 1ms in the release
fixture, including repository state and complete supported compositions. This
is a measured target, not a p100 or real-time guarantee: output copying, cold
proof construction, repository size, and host scheduling can exceed 1ms. There
is no claim that model or network time is accelerated.

An acceleration shape that is not exact or profitable is returned as
`UNSUPPORTED` until its proof is improved. The underlying operation still runs
through the unchanged native path.

## Security And Privacy

All state is local and owner-readable. Squire does not persist prompts,
completions, credentials, or arbitrary command output. Prepared output is
limited to policy-approved deterministic reads and is bounded and purgeable.
Current-file execution returns requested bytes without storing them. Sensitive
paths and environment names are denied. Workspace symlinks that resolve
outside the allowed root are never replayed or directly served.

## Release Gates

A release must pass:

- the full Go test suite;
- Codex integration compilation and focused bridge tests;
- byte-for-byte runtime fuzzing across direct and composed commands;
- deterministic invalidation probes for file, index, untracked, diff, config,
  HEAD, symbolic-ref, Git refs and object storage, environment, and symlink
  boundaries, including immediate post-edit direct and composed file reads;
- every newly recorded live read-only A/B treatment independently replaying at
  least 50% of all observed terminal calls, with valid replay accounting and
  zero diagnostic mismatches;
- deterministic command-attribution AB/BA trials preserving exact terminal
  payloads, call count, and function-output order, with both order-specific
  paired savings intervals above zero and larger than measured same-arm mean
  order bias; interleaved A/A and B/B controls remain visible in the report;
- an artifact-level install smoke that verifies the driver, code-mode helper,
  runtime library, and `squire doctor` together.

Any mismatch, unsafe hit, failed invalidation probe, ABI mismatch, or missing
native fallback fails the release.

## Advanced Backends

Scoped preload and Linux microVM paths remain available for backend research,
compatibility tests, and isolated execution experiments. They are not required
by the product contract, are not installed by default, and must not change the
behavior of `squire codex`.
