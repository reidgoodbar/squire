# Squire Green

Squire Green continuously maintains one local fact for coding agents and
developers: whether every required validation check is current and passing for
its declared inputs.

It does not alter prompts, choose agent commands, or replay test output. Checks
run as native child processes at reduced priority. Scheduling happens after a
short workspace quiescence period, so validation naturally overlaps model
inference and other quiet time.

## Configuration

Green reads `.squire/checks.toml` from the repository root.

```toml
version = 1
quiescence = "750ms"
poll_interval = "30s"
concurrency = 2

[[check]]
name = "tests"
command = ["python3", "-m", "unittest", "discover"]
inputs = ["**/*.py", "pyproject.toml"]
exclude = [".venv/**", "**/__pycache__/**"]
timeout = "10m"

[[check]]
name = "lint"
command = ["ruff", "check", "."]
inputs = ["**/*.py", "pyproject.toml", "ruff.toml"]
exclude = [".venv/**", "**/__pycache__/**"]
required = true

[[check]]
name = "optional-build"
command = ["python3", "-m", "build"]
inputs = ["src/**/*.py", "pyproject.toml"]
required = false
env = { SOURCE_DATE_EPOCH = "0" }
```

Top-level fields:

- `version`: schema version; omitted means `1`.
- `quiescence`: delay after a relevant filesystem event; default `750ms`.
- `poll_interval`: exact reconciliation fallback; default `30s`.
- `concurrency`: maximum simultaneous checks; default `2`, maximum `8`.

Check fields:

- `name`: unique display name.
- `command`: nonempty argv array. No shell is implied; use an explicit
  `sh -c` entry only when shell behavior is required.
- `inputs`: repository-relative doublestar glob patterns.
- `exclude`: optional repository-relative glob patterns.
- `cwd`: optional repository-relative working directory; default repository
  root. Symlink resolution may not escape the repository.
- `required`: whether this check gates Green; default `true`.
- `timeout`: native process timeout; default `10m`, maximum `24h`.
- `env`: optional literal environment overrides.

At least one required check is necessary. Configuration is strict: unknown
fields, duplicate names, invalid globs, escaping paths, and unsupported schema
versions fail closed.

## Trust

A cloned repository can contain arbitrary commands, so Green never executes a
checks file merely because it exists. Review it and trust its exact content:

```sh
squire green trust
```

Trust is local, owner-readable, repository-scoped, and bound to the config
SHA-256 digest. Editing any command, input, timeout, environment override, or
other config byte immediately changes the digest and returns the repository to
`UNTRUSTED`.

```sh
squire green revoke
```

Revocation is immediate. The scheduler also rechecks trust before every batch.

## Scheduling

The existing Squire repository maintainer owns Green; there is no separate
daemon. `squire codex` starts or reuses that maintainer. Starting above a
repository and later entering it works through the same command-cwd repository
discovery used by acceleration.

Scheduling is event-driven. Filesystem notifications only wake and debounce
the scheduler. Before a native check starts, Green computes the exact declared
input proof. It watches relevant directories while the command runs, computes
the proof again afterward, and publishes only when:

- no relevant mutation event or watcher error occurred;
- the before and after proofs match;
- the exact config is still trusted.

A periodic reconciliation catches missed notifications. Notifications are not
authority. A queue error, unavailable mutation guard, unstable hash read, or
out-of-bounds proof discards the result.

Checks run with bounded concurrency and reduced OS scheduling priority. A
current pass, failure, or timeout is not rerun until its declared scope changes.
This prevents retry loops for deterministic failures.

## Validity

Each result records hashes and provenance for:

- exact config bytes and normalized argv;
- canonical repository and check working directory;
- the sorted set of matched paths, modes, symlink targets, sizes, and SHA-256
  file content hashes;
- the normalized native child environment;
- the resolved primary executable path and content hash;
- stdout and stderr hashes and byte counts, exit status, timing, and the
  observed workspace epoch.

File hash reuse requires stable identity, size, mode, modification time, and
change time. Same-size rewrites with restored mtimes therefore invalidate. A
change-and-revert during execution is still discarded because the mutation
guard observed the intermediate event.

The per-check input proof is authority. The global observed workspace epoch is
provenance and display context. This lets a Python check remain current after
an unrelated Markdown edit while invalidating immediately when a declared
Python or configuration input changes.

Green proves the declared dependency contract; it does not make arbitrary
tests hermetic. A check that reads undeclared files, the network, the clock, a
database, or secondary executables can still depend on state not represented
by `inputs`. Declare all workspace dependencies and use deterministic checks
where a strong current-state claim matters.

## Status

```sh
squire status
squire verify
squire verify --json
```

`squire verify` exits zero only when every required check is trusted, current,
and passing. States are `UNTRUSTED`, `PENDING`, `RUNNING`, `PASS`, `FAIL`,
`TIMEOUT`, `STALE`, and `ERROR`.

Green persists no native stdout or stderr content. It stores only output
hashes, byte counts, exit status, timing, and proof metadata in the owner-only
Squire store. Run a failed check normally to inspect its complete diagnostics.

## Future Boundary

The result key already separates command, cwd, environment, executable, and
input proof. A later single-flight layer can use that key to deduplicate
simultaneous identical validation requests across agents without replaying a
past test result. That is intentionally outside the first release.
