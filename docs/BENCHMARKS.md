# Squire Benchmarks

All numbers here are scoped local measurements. They are not broad Codex or
agent-speedup claims.

## Current Claim

Squire accelerates repeated local Git metadata plus hot-prepared
deterministic read-only discovery operations with:

- exact stdout/stderr/exit-code preservation;
- local validity proof;
- native fallback;
- no prompt/tool/model changes.

Squire reports command-serving measurements. These benchmarks do not include
model thinking time, API/network latency, or broad end-to-end Codex task time.

## Mixed Scoped-Session UX

Run date: `2026-06-24`

Workload:

- fresh Python/TypeScript repo in `/private/tmp`;
- one normal scoped zsh session;
- plain commands across Git metadata, repo summaries, bounded file reads, tool
  probes, literal file searches, and native-control commands;
- no agent-visible Squire command inside the measured session.

Results:

- commands: `270`
- exactness: `true`
- exact stdout/stderr/exit-code mismatches: `0`
- hot mmap replays: `204`
- native fallbacks remained available for every miss and never-replay command
- native total: `3797.337ms`
- Squire session total: `816.421ms`
- workload delta: `2980.916ms`
- speedup: `4.651x`
- replay p50/p95/p99/max: `132us` / `880us` / `1243us` / `1428us`

Interpretation:

- `204/270` commands were served from exact hot mmap replay.
- The remaining commands either were native-control operations or did not have a
  complete cheap proof.
- The reported `4.651x` is for this local command-serving workload only.

Script:

```sh
python3 scripts/session_mixed_bench.py /private/tmp/squire-ux --json
```

## VM/Codex Preload Samples

Live macOS VM/Codex session using production preload settings produced fresh
plain `git rev-parse HEAD` replay samples:

- `486us`
- `619us`
- `929us`
- `587us`
- `555us`

These were exact `c-mmap-hot-snapshot` replays and below the 1ms target.
Release checks should evaluate a fresh post-start window rather than lifetime
averages that may contain pre-optimization entries.

## Scoped Preload Full Panel

Run date: `2026-07-01`

Workload:

- fresh Git repo in `/private/tmp`;
- scoped preload transport only, no PATH shims;
- `31` commands across Git metadata/state, bounded file reads, native
  precomputed `file(1)`, literal grep, directory listings, safe environment
  probes, static system probes, and selected shell compositions;
- `1000` operations per command per e2e sample;
- `5` e2e samples per command;
- `1000` native direct samples per command.

Results:

- exactness: `31/31`;
- native fallback remained available;
- direct command group: native p50 sum `167.549ms`, Squire p50 sum
  `3.172ms`, `52.8x`;
- composed shell group: native p50 sum `155.398ms`, Squire p50 sum
  `21.420ms`, `7.3x`;
- full panel: native p50 sum `322.948ms`, Squire p50 sum `24.591ms`,
  `13.1x`.

Direct commands use synthetic completed-child replay where the stdout pipe,
wait status, stderr emptiness, exit code, output size, and hot-snapshot proof
are all compatible. Composed shell commands use the helper-owned shell-plan path
so pipe EOF and wait semantics stay native-shaped; they improve substantially,
but remain milliseconds e2e because the shell composition envelope still exists.

| Command | Native p50 | Native p95 | Native p99 | Squire p50 | Squire p95 | Squire p99 | Speedup |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `git rev-parse HEAD` | `13.777ms` | `13.874ms` | `13.874ms` | `0.092ms` | `0.097ms` | `0.097ms` | `150.0x` |
| `git rev-parse --git-dir` | `13.762ms` | `13.928ms` | `13.928ms` | `0.095ms` | `0.100ms` | `0.100ms` | `145.4x` |
| `git rev-parse --abbrev-ref HEAD` | `13.613ms` | `13.668ms` | `13.668ms` | `0.092ms` | `0.102ms` | `0.102ms` | `148.0x` |
| `git rev-parse --show-toplevel` | `13.584ms` | `13.748ms` | `13.748ms` | `0.091ms` | `0.103ms` | `0.103ms` | `149.4x` |
| `git rev-parse --is-inside-work-tree` | `13.433ms` | `13.493ms` | `13.493ms` | `0.089ms` | `0.095ms` | `0.095ms` | `151.5x` |
| `git status --short` | `14.913ms` | `14.998ms` | `14.998ms` | `0.373ms` | `0.379ms` | `0.379ms` | `40.0x` |
| `git status --porcelain` | `15.007ms` | `15.090ms` | `15.090ms` | `0.357ms` | `0.384ms` | `0.384ms` | `42.0x` |
| `git ls-files` | `13.697ms` | `13.784ms` | `13.784ms` | `0.143ms` | `0.156ms` | `0.156ms` | `96.1x` |
| `git diff` | `13.661ms` | `13.693ms` | `13.693ms` | `0.228ms` | `0.237ms` | `0.237ms` | `59.9x` |
| `git diff --stat` | `13.792ms` | `13.801ms` | `13.801ms` | `0.218ms` | `0.230ms` | `0.230ms` | `63.2x` |
| `cat src/app.js` | `1.770ms` | `1.788ms` | `1.788ms` | `0.117ms` | `0.129ms` | `0.129ms` | `15.1x` |
| `sed -n '1,2p' src/app.js` | `1.746ms` | `1.791ms` | `1.791ms` | `0.114ms` | `0.130ms` | `0.130ms` | `15.4x` |
| `head -n 2 src/app.js` | `1.631ms` | `1.666ms` | `1.666ms` | `0.116ms` | `0.120ms` | `0.120ms` | `14.1x` |
| `tail -n 2 src/app.js` | `1.666ms` | `1.675ms` | `1.675ms` | `0.113ms` | `0.120ms` | `0.120ms` | `14.8x` |
| `file src/app.js` | `4.777ms` | `4.790ms` | `4.790ms` | `0.114ms` | `0.126ms` | `0.126ms` | `41.8x` |
| `grep -F two src/app.js` | `1.893ms` | `1.920ms` | `1.920ms` | `0.120ms` | `0.122ms` | `0.122ms` | `15.7x` |
| `grep -q -F two src/app.js` | `1.882ms` | `1.920ms` | `1.920ms` | `0.123ms` | `0.130ms` | `0.130ms` | `15.3x` |
| `ls src` | `1.855ms` | `1.868ms` | `1.868ms` | `0.179ms` | `0.186ms` | `0.186ms` | `10.4x` |
| `printenv PATH` | `1.608ms` | `1.624ms` | `1.624ms` | `0.077ms` | `0.080ms` | `0.080ms` | `20.8x` |
| `uname -m` | `1.627ms` | `1.636ms` | `1.636ms` | `0.079ms` | `0.085ms` | `0.085ms` | `20.7x` |
| `whoami` | `2.383ms` | `2.385ms` | `2.385ms` | `0.078ms` | `0.089ms` | `0.089ms` | `30.4x` |
| `hostname` | `1.635ms` | `1.714ms` | `1.714ms` | `0.081ms` | `0.085ms` | `0.085ms` | `20.1x` |
| `id` | `3.837ms` | `3.879ms` | `3.879ms` | `0.082ms` | `0.091ms` | `0.091ms` | `46.6x` |
| `git rev-parse HEAD | cat` | `17.959ms` | `18.059ms` | `18.059ms` | `2.428ms` | `2.433ms` | `2.433ms` | `7.4x` |
| `git rev-parse HEAD >/dev/null && git status --short >/dev/null && cat src/app.js >/dev/null` | `33.453ms` | `33.717ms` | `33.717ms` | `3.075ms` | `3.091ms` | `3.091ms` | `10.9x` |
| `(git ls-files | grep -F app >/dev/null) && (sed -n '1,4p' src/app.js | tail -n 2 >/dev/null)` | `21.253ms` | `21.648ms` | `21.648ms` | `2.691ms` | `2.710ms` | `2.710ms` | `7.9x` |
| `git status --short | head -n 5` | `19.344ms` | `19.374ms` | `19.374ms` | `2.892ms` | `2.924ms` | `2.924ms` | `6.7x` |
| `git ls-files | grep -F src` | `17.984ms` | `18.034ms` | `18.034ms` | `2.568ms` | `2.587ms` | `2.587ms` | `7.0x` |
| `cat src/app.js | grep -F two | head -n 1` | `6.593ms` | `6.624ms` | `6.624ms` | `2.524ms` | `2.536ms` | `2.536ms` | `2.6x` |
| `sed -n '1,4p' src/app.js | tail -n 2` | `6.151ms` | `6.193ms` | `6.193ms` | `2.502ms` | `2.536ms` | `2.536ms` | `2.5x` |
| `git rev-parse HEAD >/dev/null; git ls-files >/dev/null; cat src/app.js >/dev/null` | `32.660ms` | `32.925ms` | `32.925ms` | `2.740ms` | `2.769ms` | `2.769ms` | `11.9x` |

Script:

```sh
python3 scripts/preload_ops_bench.py /path/to/squire \
  --rounds 1000 \
  --native-direct-rounds 1000 \
  --native-batch-rounds 1000 \
  --e2e-samples 5 \
  --json
```

## Concurrent Echo Stress

Workload:

- `/private/tmp`
- 40 concurrent normal-UX requests

Results:

- adapter hot replay p95: `293us`
- process CLI hot replay p95: `341us`
- budget: `1000us`
- violations: `0`

## Cache-Break Stress

Run date: `2026-06-30`

Target:

- intentionally create cache entries that would be stale if cwd or external Git
  behavior inputs were not part of the replay proof.

Reproduced and fixed stale replay candidates:

- root-prepared `git status --short` reused from a subdirectory with different
  cwd-relative bytes;
- default global Git ignore file changes affecting `git status`;
- included Git config file changes affecting `git status`;
- default global Git attributes file changes affecting `git diff`;
- configured `core.excludesFile` target changes affecting `git status`;
- configured `core.attributesFile` target changes affecting `git diff`.

Verification:

- targeted regressions pass;
- preload proof-engine smoke hits at repo root and misses safely from a subdir
  when only the root proof was prepared;
- `go test ./...` passes.

## Multi-Agent Normal UX A/B

Workload:

- mixed local discovery plus one never-replay native command;
- no agent-visible Squire command;
- exact stdout/stderr/exit-code mismatches: `0`.

Results:

- 1 agent, 80 commands: `4.780x`, `650.765ms` wall delta
- 2 agents, 160 commands: `4.559x`, `528.408ms` wall delta
- 4 agents, 320 commands: `4.730x`, `753.041ms` wall delta
- 8 agents, 640 commands: `5.533x`, `1301.093ms` wall delta
- 16 agents, 1280 commands: `5.793x`, `2247.894ms` wall delta

## Deep-Local Profile

Workload:

- runs: `1219`
- packages: `48`
- tracked files: `341`
- commits: `39`
- incremental turns: `36`

Safety:

- safety gates: `pass`
- enabled fast-path exactness: `true`
- enabled fast-path mismatches: `0`
- stale HEAD replays: `0`
- stale branch replays: `0`
- validation replays: `0`

Performance:

- metadata workload delta: `+2.550s`
- metadata fast-path p95: `1641us`
- older standalone `kernel run` native-fallback overhead profile:
  `needs_optimization`

## Policy Notes

Benchmark reports separate:

- enabled metadata fast paths;
- proof-gated replay candidates;
- warm-file materialized reads/searches;
- repo-summary fallback;
- native-only discovery;
- never-replay validation/build/test workloads.

Validation, build, and test commands are never replayed, even when their output
could be compared exactly.
