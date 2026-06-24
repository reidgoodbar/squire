# Squire Kernel Benchmarks

All numbers here are scoped local measurements. They are not broad Codex or
agent-speedup claims.

## Current Claim

Squire Kernel accelerates repeated local Git metadata plus hot-prepared
deterministic read-only discovery operations with:

- exact stdout/stderr/exit-code preservation;
- invalidation proof;
- native fallback;
- no prompt/tool/model changes.

## Mixed Scoped-Session UX

Run date: `2026-06-24`

Workload:

- fresh Python/TypeScript repo in `/private/tmp`;
- one normal scoped zsh session;
- plain commands across Git metadata, repo summaries, bounded file reads, tool
  probes, and native-control commands;
- no agent-visible Squire command inside the measured session.

Results:

- commands: `270`
- exactness: `true`
- exact stdout/stderr/exit-code mismatches: `0`
- hot mmap replays: `204`
- native total: `3797.337ms`
- Squire session total: `816.421ms`
- workload delta: `2980.916ms`
- speedup: `4.651x`
- replay p50/p95/p99/max: `132us` / `880us` / `1243us` / `1428us`

Script:

```sh
python3 scripts/session_mixed_bench.py /private/tmp/squire-kernel-ux --json
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

## Concurrent Echo Stress

Workload:

- `/private/tmp`
- 40 concurrent normal-UX requests

Results:

- adapter hot replay p95: `293us`
- process CLI hot replay p95: `341us`
- budget: `1000us`
- violations: `0`

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
- repo-summary fallback;
- native-only discovery;
- never-replay validation/build/test workloads.

Validation, build, and test commands are never replayed, even when their output
could be compared exactly.
