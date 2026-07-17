# Squire Benchmarks

These are local command-serving measurements. They do not include model
thinking, API latency, network time, or a claim that an entire agent task is
accelerated by the same ratio.

## Production ABI Fuzz Panel

Run date: July 14, 2026

```sh
python3 scripts/hot_api_fuzz.py /path/to/squire \
  --cases 5000 \
  --seed 20260714 \
  --parallel-workers 16 \
  --parallel-iterations 128 \
  --steady-iterations 10000 \
  --json-out /tmp/squire-runtime-5000.json \
  --md-out /tmp/squire-runtime-5000.md
```

The harness creates a fresh mixed JavaScript/Python Git repository, prepares
it with the supplied Squire binary, compiles `libsquire_runtime` from the
current source, and invokes runtime ABI 1 with the cwd, argv, and child
environment shape used by Squire-Codex. If no binary is supplied, it builds
`./cmd/squire` from the same checkout.

For every comparable case, the harness calls Squire first and then executes the
real native command. It compares stdout bytes, stderr bytes, and exit status.
Running native second is conservative: native can benefit from any OS page
cache warming caused by the Squire proof. Percentiles use nearest-rank samples
from monotonic wall-clock timing.

### Summary

- generated commands: `5,000`
- exact Squire hits: `4,114`
- safe native fallbacks: `886`
- byte-for-byte native comparisons: `4,599`
- output or exit mismatches: `0`
- unsafe must-miss hits: `0`
- eligible cold/stale misses: `43`
- immediately unsupported decisions: `843`
- invalidation probes passed: `12/12`
- immediate post-edit current-file probes: `11/11` exact
- measured native time represented by hits: `47.404s`
- measured Squire serving time for those hits: `1.263s`
- measured net command-serving time avoided: `46.141s`

| Path | Samples | p50 | p95 | p99 | max |
| --- | ---: | ---: | ---: | ---: | ---: |
| Squire exact hit | 4,114 | 0.274ms | 0.514ms | 0.619ms | 10.942ms |
| Same hit commands, native | 4,114 | 7.506ms | 29.796ms | 47.164ms | 70.014ms |
| Squire native-fallback decision | 886 | 0.083ms | 0.180ms | 0.221ms | 1.872ms |

The p99 target is below 1ms. The maximum is not: Squire does not claim a p100
bound against host scheduling pauses.

### Hit Latency By Class

Native columns contain the exact commands that hit Squire in that class.

| Class | Hits | Squire p50 | Squire p95 | Squire p99 | Native p50 | Native p95 | Native p99 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Git metadata | 420 | 0.209ms | 0.253ms | 0.299ms | 16.236ms | 19.558ms | 29.515ms |
| Repository state | 431 | 0.206ms | 0.254ms | 0.293ms | 17.070ms | 20.837ms | 24.344ms |
| File reads | 428 | 0.277ms | 0.373ms | 0.437ms | 4.770ms | 10.166ms | 11.270ms |
| Line windows | 415 | 0.266ms | 0.313ms | 0.363ms | 4.258ms | 4.911ms | 8.218ms |
| Numbered windows | 423 | 0.312ms | 0.385ms | 0.430ms | 7.276ms | 10.010ms | 14.490ms |
| Fixed search | 431 | 0.273ms | 0.349ms | 0.433ms | 4.652ms | 6.503ms | 8.209ms |
| Directory and environment | 409 | 0.308ms | 0.610ms | 0.689ms | 4.324ms | 7.061ms | 13.400ms |
| Tool probes | 357 | 0.245ms | 0.336ms | 0.366ms | 6.053ms | 18.021ms | 21.393ms |
| Composed pipelines | 401 | 0.316ms | 0.416ms | 0.496ms | 11.117ms | 23.045ms | 25.354ms |
| Composed sequences | 399 | 0.459ms | 0.580ms | 0.807ms | 29.425ms | 51.001ms | 53.498ms |

Every accelerated class is below 1ms at p99 in this fixture. This includes
Git state and complete supported shell compositions, not only metadata lookup.

### Steady And Concurrent Load

The steady panel rotates through 28 direct and composed production commands
after priming. All 10,000 calls hit and exactly matched their references.

| 10,000-call steady panel | p50 | p95 | p99 | max |
| --- | ---: | ---: | ---: | ---: |
| Wall time | 0.110ms | 0.284ms | 0.339ms | 4.824ms |
| Calling-thread CPU | 0.103ms | 0.275ms | 0.332ms | 0.422ms |

The contention panel runs 16 Python threads, 128 calls each. All 2,048 calls
hit and matched exactly.

| 16-thread contention panel | p50 | p95 | p99 | max |
| --- | ---: | ---: | ---: | ---: |
| Wall time | 0.769ms | 1.951ms | 10.822ms | 24.902ms |
| Calling-thread CPU | 0.125ms | 0.206ms | 0.237ms | 0.461ms |

Wall time includes Python thread scheduling and host descheduling. CPU timing
shows the runtime remained below 1ms at p99 under that load, but the wall result
is reported because callers experience scheduler delay too.

### Immediate Post-Edit Reads

Before each probe, the harness changes a tracked file, restores its old mtime,
and does not rewarm. Direct `cat`, `sed`, `head`, `tail`, fixed `grep`/`rg`, a
bounded `.log` tail, `cat | head`, `sed | tail`, and fused `nl | sed` all return
the new bytes exactly.

| Path | Samples | p50 | p95 | p99 |
| --- | ---: | ---: | ---: | ---: |
| Squire current-file execution | 11 | 0.466ms | 0.580ms | 0.580ms |
| Same commands, native | 11 | 3.689ms | 8.861ms | 8.861ms |

These hits execute the fixed grammar over bytes retained by the foreground
SHA-256 read. They do not expose or persist stale snapshot bytes.

### Demand Preparation

The harness starts cold commands for repository fixed `rg`, bounded regular
`rg`, `file` on a log, a path-scoped `git diff`, and `ls -p` in a subdirectory.
Each command first misses, follows native execution, and places a bounded
request in the local preparation queue. One maintainer cycle prepares them,
after which every command hits exactly. A source mutation then forces a miss
and exact refresh. An `rg` no-match result is also prepared with its native
exit status `1`.

This is how Squire expands useful coverage without delaying a miss or making
the foreground parser execute arbitrary commands.

The July 16 Luna-shaped steady check rotated the new regular-search forms with
the rest of the production panel for 12,000 exact hits:

| Prepared command | Calls | p50 | p95 | p99 | max |
| --- | ---: | ---: | ---: | ---: | ---: |
| `rg -n 'a|b' . --glob '!ignored/**'` | 413 | 0.075ms | 0.103ms | 0.121ms | 0.320ms |
| `rg ... | head -n 1` | 413 | 0.109ms | 0.149ms | 0.189ms | 0.767ms |

The complete 1,000-command randomized run had 823 hits, 177 safe fallbacks,
zero mismatches, zero unsafe hits, and a hit p99 of `0.717ms`; matched native
commands had p50 `7.744ms`. All 12 invalidation probes passed. Under 12-thread
contention, calling-thread CPU p99 remained `0.225ms`, while wall p99 was
`1.632ms` because host scheduling is outside the runtime's p100 control.

### Generalized Line-Selection Check

Run date: July 16, 2026

```sh
python3 scripts/hot_api_fuzz.py --cases 1000 --seed 2026071602 \
  --parallel-workers 12 --parallel-iterations 16 --steady-iterations 1000
python3 scripts/hot_api_fuzz.py --cases 1500 --seed 2026071603 \
  --buckets line_window,numbered_window,composed_pipe
python3 scripts/hot_api_fuzz.py --cases 100 --seed 2026071604 \
  --parallel-workers 12 --parallel-iterations 32 --steady-iterations 2000
```

The bounded file plan was generalized from one line range to up to eight
ordered `sed -n` print clauses. The evaluator preserves native line-major
behavior for disjoint, reversed, repeated, and overlapping clauses. The same
selection plan drives direct file reads, composed filters, and fused `nl -ba`
pipelines in both Go and the native runtime.

A 1,000-command seeded broad ABI differential run generated moving single and
multi-range selections alongside every other production class. It recorded
828 exact hits, 172 safe fallbacks, 921 native comparisons, zero mismatches,
and zero unsafe hits. Overall hit p50 was `0.253ms`, p95 was `0.488ms`, and p99
was `0.565ms`; the same hit commands had native p50 `7.405ms`, p95 `24.381ms`,
and p99 `47.323ms`.

A separate 1,500-command panel sampled only line windows, numbered windows, and
composed pipelines so isolated host scheduling pauses could not dominate a
small class percentile. All 1,500 calls hit and matched native output exactly.

| Path | Hits | Squire p50 | Squire p95 | Squire p99 | Native p50 | Native p95 | Native p99 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Line windows | 547 | 0.265ms | 0.314ms | 0.368ms | 3.960ms | 4.671ms | 8.296ms |
| Numbered windows | 475 | 0.324ms | 0.398ms | 0.515ms | 7.071ms | 9.428ms | 12.120ms |
| Composed pipelines | 478 | 0.334ms | 0.415ms | 0.507ms | 9.004ms | 22.665ms | 26.520ms |

The broad panel's 14 immediate post-edit reads all hit without rewarming:
Squire p50 was `0.446ms` and p99 `0.548ms`, versus native p50 `3.996ms` and
p99 `9.820ms`.

The final 2,000-call steady panel included direct, composed, and fused numbered
multi-range reads. Every call was exact at p50 `0.118ms`, p95 `0.282ms`, and
p99 `0.332ms`. The three multi-range commands individually stayed between
`0.152ms` and `0.228ms` at p99. Under 12-way load, all 384 calls were exact;
calling-thread CPU p99 was `0.244ms`, while scheduler-contended wall p99 was
`2.564ms`.

Reclassifying the unchanged 136-command Luna treatment trace with the
generalized policy increased eligible commands from 69 to 77. Eight of ten
previously blocked multi-range reads became eligible; malformed syntax and
commands containing another unsupported operator remained native.

## Proof Corpus And 50% Live Gate

Run date: July 16, 2026

The next coverage pass added proof-bound repository-search and bounded Git
history corpora. The history lane accepts only `git log -N --oneline --`
followed by literal paths, with `N` from 1 through 20. Its epoch covers HEAD,
all log-visible refs, Git config, the Git executable, and loose and packed
object identity. Alternate object stores, pathspec environment overrides,
incomplete history, and merge ambiguity fall back to native execution.

```sh
python3 scripts/hot_api_fuzz.py /private/tmp/squire-proof-v7 \
  --cases 500 --repo-search-cases 500 \
  --steady-iterations 2048 \
  --parallel-workers 8 --parallel-iterations 64
```

| Panel | Samples | Squire p50 | Squire p95 | Squire p99 | Native p50 | Result |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| Randomized hits | 421 | 0.315ms | 0.630ms | 0.933ms | 8.084ms | 0 mismatches |
| Repository-search differential | 500 | 0.620ms | 1.249ms | 3.354ms | 6.512ms | 0 semantic mismatches |
| Bounded path history | 28 | 0.333ms | 0.469ms | 0.492ms | 20.060ms | 28/28 exact |
| Steady mixed replay | 2,048 | 0.140ms | 0.357ms | 0.433ms | n/a | 2,048/2,048 exact |
| Eight-way replay wall | 512 | 0.394ms | 0.798ms | 3.242ms | n/a | 512/512 exact |
| Eight-way replay CPU | 512 | 0.174ms | 0.275ms | 0.334ms | n/a | 512/512 exact |

The randomized panel had 79 safe fallbacks, 468 native comparisons, zero
unsafe hits, and `5.210s` of measured command-serving time avoided. Repository
search scans larger output sets, so its p99 is reported rather than hidden
behind the sub-millisecond lanes. A final 200-command build smoke after adding
the child pathspec-environment guard again had zero mismatches and zero unsafe
hits; all 200 randomized repository searches and all Git history invalidation
probes passed.

The live A/B harness now divides each treatment's replay counter delta by every
terminal call observed in that treatment. It rejects an empty denominator,
counter overcount, diagnostic mismatch, or hit rate below `0.50`. Results are
not pooled across treatments to pass the gate.

| Fresh Luna treatment | Control calls | Treatment calls | Replays | Hit rate | Accounting | Diagnostics |
| --- | ---: | ---: | ---: | ---: | --- | ---: |
| Express | 5 | 3 | 2 | 66.7% | valid | 0 |
| Flask | 7 | 5 | 5 | 100% | valid | 0 |
| fmt | 4 | 5 | 4 | 80% | valid | 0 |

All three initial model requests matched canonically. Their initial command
sequences diverged before any tool result, so the observed task walls
(`31.73s` versus `22.38s`, `45.15s` versus `37.98s`, and `33.99s` versus
`34.76s`) are not treated as causal Squire measurements. The independently
auditable result is that every fresh treatment cleared 50% with valid event
accounting and zero diagnostic mismatches.

## Counterbalanced Codex Attribution

Run date: July 17, 2026

This experiment removes model sampling from the timing question. A local
deterministic Responses API sends the same six terminal calls through the same
Squire-Codex binary in both arms. The control disables the bridge; treatment
enables the production runtime. The measured interval begins after the fixture
has sent a command batch and ends when it receives the corresponding function
outputs. Model generation, Codex startup, fixture response construction, and
shutdown are outside that primary interval.

Forty paired trials alternate control-then-treatment (`AB`) and
treatment-then-control (`BA`). Ten control/control and ten treatment/treatment
pairs are interleaved through the run to measure order and scheduler noise.
Both arms condition the same unchanged workspace before measurement. One proof
preparation is measured separately before all trials; production normally
requests that work asynchronously. The throwaway read-only fixture uses
`danger-full-access` in both arms so sandbox construction cannot be credited to
Squire.

| Six-command mode | Control | Squire | Paired saving | Saving | Bootstrap 95% interval |
| --- | ---: | ---: | ---: | ---: | ---: |
| Serial calls | 374.876ms | 103.600ms | 271.277ms | 72.4% | 265.630-276.540ms |
| Two parallel batches | 163.239ms | 77.365ms | 85.874ms | 52.6% | 78.064-95.925ms |

Order reversal preserved the effect:

| Mode | AB saving | AB 95% interval | BA saving | BA 95% interval |
| --- | ---: | ---: | ---: | ---: |
| Serial | 273.224ms | 264.137-281.017ms | 269.329ms | 262.300-276.167ms |
| Parallel | 77.918ms | 70.847-85.067ms | 93.830ms | 80.829-111.423ms |

The serial A/A mean order effect was `-1.249ms` with interval
`[-24.031, +17.679]`; B/B was `-1.452ms` with interval
`[-5.315, +2.939]`. Parallel A/A and B/B intervals were
`[-47.743, +5.558]` and `[-7.575, +23.297]`. Same-arm intervals include zero,
while both AB and BA effect intervals are strictly positive.

Serial command-boundary attribution was:

| Command | Control | Squire | Paired saving | Saving 95% interval |
| --- | ---: | ---: | ---: | ---: |
| `git rev-parse HEAD` | 104.161ms | 33.587ms | 70.574ms | 66.231-74.652ms |
| `git rev-parse --show-toplevel` | 57.823ms | 7.350ms | 50.473ms | 48.456-52.993ms |
| `git ls-files` | 56.776ms | 7.090ms | 49.686ms | 48.713-50.681ms |
| `git ls-files \| head -n 2` | 58.245ms | 6.567ms | 51.678ms | 49.915-53.821ms |
| `printf 'native-fallback\\n'` | 40.510ms | 41.178ms | -0.668ms | -1.529-0.196ms |
| `git diff --stat` | 57.361ms | 7.827ms | 49.534ms | 48.414-50.670ms |

The first row includes Codex's lazy command-executor initialization in both
arms. The intentional unsupported `printf` call followed the unchanged native
path; its confidence interval includes zero, so this run detected no miss
overhead. Every treatment recorded exactly five hits, every call preserved the
exact exit status and combined terminal bytes, both arms preserved function
output order, and all 120 measured pairs had zero diagnostic mismatches.

The one explicit preparation took `1.093s` for the serial fixture and
`0.992s` for the parallel fixture. If forced to run synchronously and charged
only against this tiny six-command transcript, it would amortize after about
four serial transcripts. That is setup accounting, not observed UX latency:
the product runs a cold command natively and requests preparation in the
background.

Run the attribution test with:

```sh
python3 scripts/codex_attributable_ab.py \
  --bundle /path/to/release-like-bundle \
  --out /tmp/squire-attributable-ab \
  --pairs 40 --aa-pairs 10 --bb-pairs 10 \
  --batching serial --sandbox danger-full-access
```

## Agent Call-Amplification Check

Run date: July 15, 2026

This check asks whether changing command execution can subtly cause Codex to
issue more terminal calls. It has two layers.

The deterministic fixture serves a scripted local Responses API to the same
Squire-Codex binary in both arms. The control disables the bridge. The
treatment uses the public `squire codex` path. Two parallel batches contain
five eligible direct or composed commands and one intentional native fallback.
Both arms made exactly six terminal calls and three model requests, preserved
the same function-output order, and produced canonical-equivalent output for
all six calls. The treatment recorded five replay hits and zero diagnostic
mismatches.

The live check runs counterbalanced pairs against unchanged Express, Flask,
and fmt clones. Each pair uses the same binary, task, repository commit,
read-only workspace, isolated Codex home, and canonical initial model request.
The final nine-pair confirmation disables unrelated dynamic apps, plugins,
recommendations, skills, and tool suggestion surfaces; all nine requests
matched. Earlier matrices remain in the aggregate only when their complete
canonical initial requests match. Raw rollout traces attribute every terminal
call to its inference window. Pairs whose initial requests differ are rejected
rather than normalized into the sample.

Across 32 canonically matched live pairs:

| Measure | Control | Squire | Squire minus control |
| --- | ---: | ---: | ---: |
| Mean terminal calls | 16.375 | 15.969 | -0.406 |
| Mean inference calls | 5.781 | 5.656 | -0.125 |
| Mean initial terminal calls | 2.844 | 2.844 | 0.000 |
| Mean child CPU | 2.203s | 2.213s | +0.010s |
| Mean task wall time | 62.843s | 63.711s | +0.868s |

The paired bootstrap 95% interval for terminal-call amplification was
`[-2.031, +1.188]` calls. Treatment made more calls in 16 pairs, the same in
one, and fewer in 15. In 31 of 32 pairs, the model selected a different first
command sequence despite receiving canonically identical requests and before
receiving any tool result. That is direct evidence of model sampling variance;
the live test does not support a claim that Squire increases calls.

Wall and child-CPU intervals also crossed zero (`[-4.897s, +6.677s]` and
`[-0.103s, +0.114s]`). Whole-task wall time is therefore not used as a command
acceleration metric. Across all 78 processes used by the live matrices, there
were 451 treatment replay hits, zero control hits, zero failed or timed-out
runs, zero dirty repositories, zero foreground preparation launches, and zero
diagnostic mismatches.

### Luna Repeated-Workspace Check

Run date: July 16, 2026

A second live matrix used `gpt-5.6-luna` at low reasoning effort and retained
one unchanged read-only clone per repository across ten Express, Flask, and fmt
pairs. This lets demand-prepared observations become useful on later runs. All
60 processes completed, all 30 initial model requests were canonically equal,
and no repository became dirty.

| Paired measure | Squire minus control | Bootstrap 95% interval |
| --- | ---: | ---: |
| Mean terminal calls | +0.367 | `[-0.067, +0.867]` |
| Mean task wall time | +2.334s | `[-0.203s, +4.973s]` |
| Mean child CPU | +0.060s | `[-0.138s, +0.226s]` |

Treatment recorded 26 replays among 136 terminal calls (`19.1%`). Their event
records represent `195ms` of native command time and `105.450ms` of replay
wall time, for `89.550ms` of measured command-serving time avoided. Replay
event p50 was `1.186ms`; p95 was `16.359ms`. The direct ABI panel above is the
better runtime measurement because these event values include bridge work,
foreground proof, first-touch costs, and host scheduling.

The whole-task intervals all cross zero. Luna still spent tens of seconds in
model inference, so this sample does not expose a causal end-to-end wall-time
win from `89.550ms` of terminal savings. In 29 of 30 pairs the model selected a
different first command sequence before receiving any tool result. The one
matched initial `rg` call returned the same line multiset in a different order;
30 native reruns produced 14 byte orders because ripgrep traverses files in
parallel. The analyzer therefore treats only plain, direct, non-context `rg`
output as an unordered line multiset. Pipelines, context output, and explicitly
sorted searches remain byte-strict. Squire itself never sorts or rewrites the
native bytes it prepares.

The generalized bounded-plan implementation was then tested with a fresh
repeat of the same 30-pair design and the same Squire-Codex binary. All 60
processes completed, all initial requests matched canonically, no repository
became dirty, and there were zero preparation launches, unavailable runtimes,
or diagnostic mismatches. Control made 135 terminal calls and treatment made
129. The paired mean was `-0.2` calls with a bootstrap 95% interval of
`[-0.7, +0.233]`, again providing no evidence of call amplification.

Treatment recorded 21 replays among 129 terminal calls (`16.3%`). Those hits
represented `221ms` of measured native work and used `90.671ms` of replay wall
time, avoiding `130.329ms` of command-serving time. Replay p50 was `1.249ms`
and p95 was `15.168ms`; the tail events replaced repository searches that each
took `32-34ms` natively. Five sampled commands contained multi-range line
selections. Four were accepted as complete bounded plans, while one malformed
or unsupported composition remained native.

Treatment task wall time averaged `3.068s` lower, but this is not attributed to
Squire: 28 of 30 pairs selected different initial command sequences before any
tool result, treatment averaged `0.367` fewer inference calls, and one control
inference was a `70s` outlier. Child CPU remained inconclusive at `-0.056s`
with a bootstrap 95% interval of `[-0.224s, +0.120s]`. The replay event delta,
not whole-task wall time, is the causal performance result from this matrix.

Run the deterministic check with:

```sh
python3 scripts/codex_scripted_bridge_ab.py \
  --bundle /path/to/release-like-bundle \
  --out /tmp/squire-scripted-call-ab
```

Run a live matrix with:

```sh
python3 scripts/codex_call_amplification_ab.py \
  --bundle /path/to/release-like-bundle \
  --out /tmp/squire-live-call-ab \
  --pairs 10 \
  --repos express flask fmt \
  --cohort warm \
  --model gpt-5.6-luna \
  --effort low \
  --reuse-workspaces
```

The deterministic fixture proves that the bridge itself does not insert,
drop, or reorder model tool calls. The live matrix is a statistical regression
check around that invariant. It cannot make stochastic model trajectories
identical, and it does not claim that a future model can never react to a
different timing or transport envelope.

## Invalidation Matrix

Each probe starts from a hit, changes one authority boundary, and requires a
miss or output exactly equal to the new native state. A stale byte, wrong exit
status, or unsafe hit makes the benchmark exit nonzero.

| Probe | Stale result accepted | Fresh result exact |
| --- | --- | --- |
| File content change | no | yes |
| Numbered-window source change | no | yes |
| Same-size atomic replacement with restored mtime | no | yes |
| Git index-only change | no | yes |
| New untracked file | no | yes |
| Same-size diff change with restored mtime | no | yes |
| `git diff --check` whitespace change | no | yes |
| Local Git config change | no | yes |
| New commit and HEAD change | no | yes |
| Symbolic HEAD and branch rename | no | yes |
| Child Git pathspec environment override | no | n/a |
| Unreachable loose Git object | no | yes after refresh |
| Packed-object namespace change | no | yes after refresh |
| New loose Git ref | no | yes after refresh |
| Git abbreviation config change | no | yes after refresh |
| Merge-topology history ambiguity | no | native fallback |
| Child `PATH` mismatch | no | n/a |
| Symlink outside workspace | no | n/a |

## What This Measures

This panel measures the production C ABI, mmap lookup, foreground proof,
bounded in-process execution, exact fallback decision, demand preparation, and
native reference commands. It varies direct and composed command shapes and
includes supported, cold, unsupported, and unsafe cases.

It is not a complete Codex task benchmark. Model output varies and model
latency dominates many turns. The product install smoke separately verifies the
driver, code-mode helper, runtime library, and `squire doctor` from release-like
artifacts.

## Release Commands

```sh
GOCACHE=/tmp/squire-gocache go test ./... -count=1
scripts/install_product_smoke.sh
python3 scripts/hot_api_fuzz.py /path/to/squire \
  --cases 5000 --seed 20260714 \
  --parallel-workers 16 --parallel-iterations 128 \
  --steady-iterations 10000
```

In the Codex fork:

```sh
just fmt
just test -p codex-core squire_codex_bridge
cargo build -p codex-cli --bin codex
cargo build -p codex-code-mode-host --bin codex-code-mode-host
```
