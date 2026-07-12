# Squire Architecture

Squire separates command interception, proof evaluation, preparation, and
native execution so the fast path stays small and adapters stay replaceable.

## Runtime Flow

```text
Codex command request
        |
        v
thin Squire-Codex hook
        |
        v
runtime ABI 1 (cwd + argv + env)
        |
        +-- HIT --------> exact Codex result
        |
        +-- MISS -------+--> original Codex execution path
        |
        +-- UNSUPPORTED-+
```

The hook runs before Codex wraps a command in its sandbox launcher. That gives
Squire the original command while leaving native misses under Codex's existing
approval, sandbox, timeout, cancellation, streaming, and process management.

Four upstream execution surfaces currently need small hooks because Codex has
separate classic exec, shell runtime, user-shell, and unified-exec paths. All
policy, mmap, proof, preparation, and library-loading behavior lives in the
vendored Squire modules, whose source of truth is this repository.

## Components

`squire` is the Go control plane:

- resolves repositories and Git common directories;
- records native observations;
- builds cryptographic proof inputs and invalidation epochs;
- maintains bounded prepared state;
- atomically publishes `hot_snapshot.bin`;
- exposes `status`, `doctor`, `explain`, and background preparation.

`libsquire_runtime` is the C data plane:

- implements versioned runtime ABI 1;
- rejects unsupported commands before snapshot discovery;
- maps the prepared snapshot read-only;
- recomputes the command's current proof;
- copies exact result bytes into adapter-owned memory;
- returns hit, miss, or unsupported.

`squire-codex` is the first agent adapter:

- passes the command request into the runtime;
- converts hits into native Codex result types;
- asks `squire prepare` to start asynchronously after an eligible cold miss;
- leaves every miss on the original Codex code path.

The agent-neutral Go runtime package defines the longer-term provider boundary.
A provider may handle a request only when it can materialize complete stdout,
stderr, and exit status. The initial provider is the validated local workspace;
future providers can use the same exact-hit-or-native contract.

## Repository Lifecycle

Repository identity is resolved from each command's cwd, not only the directory
where the agent started. The registry keys workspaces by canonical Git storage
root, so these requests share one prepared workspace:

- a command at the worktree root;
- a command from any subdirectory;
- an equivalent supported `git -C` request.

Different repositories in one Codex session remain isolated. Cold requests run
natively immediately. Preparation requests are deduplicated per repository and
do not run for unsupported commands.

## Snapshot And Proof

The snapshot is an owner-only mmap file with a fixed header, fixed-size entry
descriptors, exact proof epochs, and bounded payloads. Publication uses a new
file plus atomic rename. Readers validate magic, version, sizes, offsets, and
entry bounds before use.

Snapshot lookup is only an index operation. A matching record is still a miss
unless the runtime can reproduce its current proof. Proof functions are
command-specific:

- metadata reads check current Git ref and configuration boundaries;
- file reads hash current canonical file bytes and mode;
- status and diff check the relevant index, tracked, untracked, config, ignore,
  attributes, replacement-ref, and graft inputs;
- environment probes compare their child environment;
- compositions require a valid proof at every source node.

Environment-sensitive tools and filters require exact equality between the
child environment and the preparation process, including unset versus empty
values. Only narrow outputs with a command-specific proof, such as supported
Git metadata, use a selective environment comparison.

The durable store can therefore be stale without creating stale hits. Epoch
mismatch is invalidation.

## Composition Engine

The runtime parses a deliberately small shell grammar containing plain words,
`|`, `&&`, `;`, grouping, and supported `/dev/null` redirection. It rejects
expansion, globbing, command substitution, arbitrary redirection, background
jobs, loops, functions, and unknown syntax.

A plan contains proof-backed source commands and bounded in-memory filters.
Typical filters are `cat`, `head`, `tail`, fixed `grep`, `wc -l`, and no-arg
`sort`. Eligibility is checked structurally before snapshot work. If any node
is unsupported or any proof misses, the entire original shell string runs
natively. Squire does not mix replayed and native nodes inside one shell plan.

## Performance Tiers

Metadata proofs have constant-size inputs and are the strict low-latency lane.
Bounded file proofs scale with the selected file. Repository proofs scale with
the relevant worktree and intentionally have a separate tail distribution.

Version and PATH lookup probes are not enabled in the production runtime. The
strong proof hashes executable identity, which is unprofitable for large
binaries. The legacy backend can still exercise those operators for research,
but public status reports only production lanes.

`file`, `whoami`, and `id` are also excluded from production because their
outputs depend on mutable system magic or identity databases. Squire does not
represent numeric IDs and file bytes alone as a complete proof of those
outputs.

Miss cost matters because most commands should remain native. Unsupported
commands are rejected before map discovery. Eligible misses may inspect the
snapshot and proof state, then return immediately while preparation runs in the
background.

## Installation

Release archives are paired by version. The Squire-Codex archive contains:

- `squire-codex`;
- `codex-code-mode-host`;
- the platform-native Squire runtime library.

The installer verifies archive checksums and places all components together.
If an older archive lacks the runtime, local C compilation is a compatibility
fallback. `squire doctor` validates the installed layout.

Preload helpers and the macOS VM helper are installed only when
`SQUIRE_INSTALL_ADVANCED=1` is explicitly set.

## Optional Backends

The scoped preload path remains useful for Linux process-interposition tests.
macOS system shells strip dynamic-loader injection, so preload is not the
product integration.

The microVM path remains useful for Linux isolation and VM transport research.
It requires explicit guest assets and must not receive host credentials
implicitly. It is not started or provisioned by `squire codex`.

Both backends obey the same exact-result-or-native contract but are outside the
default installation and UX.

## Diagnostics

Normal commands:

```sh
squire doctor
squire status --json
squire explain -- git status --short
```

Backend diagnostics remain under `squire help advanced`, including legacy
kernel, boost, scoped-session, benchmark, and VM commands. They are retained for
release verification rather than normal use.
