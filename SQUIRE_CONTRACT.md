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
only when a foreground proof recomputes the inputs that can affect that exact
command and matches the prepared epoch.

Depending on the command, proof inputs include:

- normalized argv, cwd, canonical workspace root, and Git common directory;
- HEAD and symbolic-ref identity;
- Git index, config, ignore, attributes, replacement refs, and graft inputs;
- relevant tracked, untracked, and working-tree state;
- canonical file path, mode, size, and cryptographic content hash;
- requested line range or fixed literal and exact supported flag shape;
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

The runtime validates current state synchronously before exposing cached bytes.
The mmap snapshot is atomically published and owner-only; mapping it does not
make its records valid by itself. Old records may remain on disk indefinitely,
but an epoch mismatch makes them unusable.

File proof uses content hashes rather than trusting only size or mtime. The
regression suite specifically replaces files atomically with same-size content
and restores their original mtimes. Such replacements must miss until a fresh
observation is prepared.

## Workspace Model

Workspace selection happens for each actual command cwd. Starting Codex above
a repository and later working inside it requires no manual warm command.
Subdirectories and equivalent `git -C` requests converge on the same workspace,
keyed by the resolved Git storage root. Multiple repositories in one agent
session are prepared independently.

Preparation is background work. A cold first request runs natively while one
preparation request per workspace is in flight. Unsupported commands never
trigger preparation.

## Production Lanes

The production runtime groups operations by proof cost instead of making one
latency claim for every hit.

Metadata lane:

- supported `git rev-parse` metadata forms;
- `git branch --show-current`.

Bounded lane:

- bounded workspace `cat`, `sed -n`, `head`, and `tail`;
- fixed-string single-file `grep -F` and `rg -F` forms;
- tight `ls`, safe `printenv`, `hostname`, and supported `uname` probes.

Repository lane:

- supported `git status`, `git ls-files`, and `git diff` forms;
- read-only compositions over supported sources and filters such as `head`,
  `tail`, `grep -F`, `wc -l`, and no-argument `sort`.

Any unsupported token, expansion, glob, redirect, background job, mutation, or
command causes the entire shell composition to run natively. Squire does not
partially replace a shell plan.

Tool version and PATH lookup probes currently run natively in the production
runtime. Hashing a large executable can cost more than executing the probe, so
these lanes remain disabled until their proof is both exact and profitable.
`rg --files` is also native because unchanged filesystem state does not
guarantee stable byte ordering across invocations.

`file`, `whoami`, and `id` remain native because their output depends on
mutable system magic or identity databases. Revalidating those databases in
the foreground is not currently cheaper than native execution.

## Performance Contract

Correctness is mandatory; acceleration is conditional. Squire reports:

- end-to-end runtime decision latency;
- hit and miss latency separately;
- exactness mismatches and invalidation outcomes;
- estimated native time avoided only for measured hits.

Metadata and bounded lanes target sub-millisecond typical hits. Repository
proof cost scales with relevant repository state and is reported separately.
There is no universal sub-millisecond guarantee and no claim that model or
network time is accelerated.

An eligible lane that is consistently slower than native execution is removed
from the production policy until its proof is improved.

## Security And Privacy

All state is local and owner-readable. Squire does not persist prompts,
completions, credentials, or arbitrary command output. Prepared output is
limited to policy-approved deterministic reads and is bounded and purgeable.
Sensitive paths and environment names are denied. Workspace symlinks that
resolve outside the allowed root are never replayed.

## Release Gates

A release must pass:

- the full Go test suite;
- Codex integration compilation and focused bridge tests;
- byte-for-byte runtime fuzzing across direct and composed commands;
- deterministic invalidation probes for file, index, untracked, diff, config,
  HEAD, symbolic-ref, environment, and symlink boundaries;
- an artifact-level install smoke that verifies the driver, code-mode helper,
  runtime library, and `squire doctor` together.

Any mismatch, unsafe hit, failed invalidation probe, ABI mismatch, or missing
native fallback fails the release.

## Advanced Backends

Scoped preload and Linux microVM paths remain available for backend research,
compatibility tests, and isolated execution experiments. They are not required
by the product contract, are not installed by default, and must not change the
behavior of `squire codex`.
