# Squire Benchmarks

These are local command-serving measurements. They do not include model
thinking, API latency, network time, or a broad task-speedup claim.

## Production ABI Fuzz Panel

Run date: July 13, 2026

Command:

```sh
python3 scripts/hot_api_fuzz.py /path/to/squire \
  --cases 500 \
  --seed 20260712 \
  --json-out /tmp/squire-runtime-500.json \
  --md-out /tmp/squire-runtime-500.md
```

The harness creates a fresh mixed JavaScript/Python Git repository, prepares a
snapshot, compiles `libsquire_runtime` from the current source, and calls
runtime ABI 1 directly with the same cwd/argv/environment shape used by the
Codex bridge.

When no Squire binary is supplied, the harness builds `./cmd/squire` from the
same checkout into its temporary directory. This prevents an installed older
CLI from preparing snapshots for a newer runtime under test.

For each safe command, it also runs the native command and compares stdout,
stderr, and exit status byte-for-byte. Runtime execution happens first, so the
native reference can benefit from any OS page-cache warming caused by Squire.
This is conservative for the reported Squire/native comparison.

### Summary

- generated commands: `500`
- exact Squire hits: `296`
- safe native fallbacks: `204`
- unsupported decisions before snapshot work: `183`
- eligible cold/stale misses: `21`
- byte-for-byte native comparisons: `458`
- output/exit mismatches: `0`
- unsafe must-miss hits: `0`
- Codex user-shell request-shape regression: passed
- invalidation matrix: passed
- measured net command-serving time avoided on hits: `3450.353ms`

Overall matching distributions:

| Path | Samples | p50 | p95 | p99 | max |
| --- | ---: | ---: | ---: | ---: | ---: |
| Squire hits | 296 | 0.528ms | 2.516ms | 3.169ms | 3.319ms |
| Same commands, native | 296 | 8.194ms | 34.760ms | 52.433ms | 64.108ms |
| Squire misses | 204 | 0.092ms | 0.297ms | 0.998ms | 2.822ms |

The overall hit tail mixes proof tiers. Metadata and bounded operations are the
strict low-latency lanes; repository and composed commands intentionally report
their scaling proof cost separately.

### Dedicated Metadata Tail

The mixed panel contains only 41 metadata samples, so its nearest-rank p99 is
too sensitive to a single scheduler pause. A separate 2,000-command run used
only the six production Git metadata forms:

```sh
python3 scripts/hot_api_fuzz.py /path/to/squire \
  --cases 2000 \
  --seed 20260715 \
  --buckets git_metadata
```

| Path | Samples | p50 | p95 | p99 | max |
| --- | ---: | ---: | ---: | ---: | ---: |
| Squire metadata hit | 2000 | 0.434ms | 0.648ms | 0.893ms | 9.922ms |
| Same commands, native | 2000 | 19.438ms | 24.008ms | 52.886ms | 68.252ms |

All 2,000 results matched native bytes and exit status. The p99 target is below
1ms on this machine; the maximum is not. Squire does not claim a p100 bound
against host scheduling pauses.

### Hit Latency By Class

Native columns contain only commands that hit Squire in that class.

| Class | Hits | Squire p50 | Squire p95 | Squire p99 | Native p50 | Native p95 | Native p99 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Git metadata | 41 | 0.378ms | 0.451ms | 0.467ms | 14.841ms | 19.261ms | 39.073ms |
| Line windows | 38 | 0.449ms | 0.553ms | 0.571ms | 3.190ms | 5.115ms | 7.954ms |
| Fixed search | 48 | 0.486ms | 0.570ms | 0.599ms | 3.627ms | 5.635ms | 7.159ms |
| Bounded file reads | 20 | 0.459ms | 0.542ms | 0.576ms | 3.476ms | 7.509ms | 7.963ms |
| Directory/environment | 31 | 0.594ms | 0.943ms | 1.036ms | 3.486ms | 6.022ms | 24.847ms |
| Repository state | 41 | 0.870ms | 2.572ms | 3.319ms | 16.430ms | 22.412ms | 32.610ms |
| Composed pipelines | 31 | 0.704ms | 1.162ms | 2.847ms | 8.671ms | 20.906ms | 23.306ms |
| Composed sequences | 46 | 1.518ms | 3.150ms | 3.170ms | 28.544ms | 52.433ms | 64.108ms |

Repository-state outliers come from synchronous hashing of the relevant
worktree proof. They are still materially faster than matched native commands
in this fixture, but they are not represented as sub-millisecond metadata hits.

### Native-Fallback Decision Cost

| Class | Misses | p50 | p95 | p99 |
| --- | ---: | ---: | ---: | ---: |
| Ordinary unsupported controls | 43 | 0.094ms | 0.182ms | 0.580ms |
| Tool version/PATH probes | 54 | 0.079ms | 0.117ms | 0.126ms |
| Unsafe policy cases | 42 | 0.089ms | 0.309ms | 0.339ms |

Tool version and PATH probes are deliberately unsupported in production. A
strong executable-content proof was slower than native execution for large
binaries, so the profitable behavior is an early native decision.

## Invalidation Matrix

The same run starts from a known hit, mutates one authority boundary, and then
requires either a miss or a result exactly equal to the new native state. After
fresh preparation, the command must hit with exact bytes again.

| Probe | Stale result accepted | Rewarm exact |
| --- | --- | --- |
| File content change | no | yes |
| Same-size atomic file replacement with restored mtime | no | yes |
| Git index-only change | no | yes |
| New untracked file | no | yes |
| Same-size diff change with restored mtime | no | yes |
| Local Git config change | no | yes |
| New commit/HEAD change | no | yes |
| Symbolic HEAD/branch rename | no | yes |
| Child `PATH` mismatch | no | n/a |
| Symlink outside workspace | no | n/a |

The run passes only if every row is safe. A stale byte, wrong exit status, or
unsafe hit makes the script exit nonzero.

## What This Does And Does Not Measure

This is a true measurement of Squire's local decision and result-serving work:

- the production C ABI and mmap proof path are exercised;
- real native commands provide the exactness and latency reference;
- direct and composed command shapes are varied deterministically;
- safe misses and rejected mutations are included;
- native samples are matched to actual hits for comparison.

It is not a complete Codex task benchmark. Model output varies, model latency
dominates many turns, and Codex may use internal file-reading tools that do not
cross the terminal boundary. Separate fork tests compile the bridge inside
current upstream Codex, and the product install smoke verifies release layout,
but neither is folded into the microsecond ABI table.

## Release Commands

```sh
go test ./... -count=1
scripts/install_product_smoke.sh
python3 scripts/hot_api_fuzz.py --cases 500 --seed 20260712
```

In the Codex fork:

```sh
just fmt
just test -p codex-core squire_codex_bridge
cargo build -p codex-cli --bin codex
cargo build -p codex-code-mode-host --bin codex-code-mode-host
```
