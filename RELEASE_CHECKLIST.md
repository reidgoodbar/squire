# Squire Kernel Release Checklist

Use this checklist before publishing a Squire Kernel build. Keep release claims
scoped to the proven kernel behavior.

## Release Claim

Allowed claim:

> Scoped kernel proof for repeated local Git metadata plus hot-prepared
> deterministic read-only discovery operations.

Do not claim broad Codex, Claude, or general agent speedups. Report measured
workload deltas only for the benchmark or dogfood run that produced them.

## Build Identity

Release builds should set build identity with Go linker flags:

```sh
go build \
  -ldflags "-X main.buildVersion=v0.0.0 -X main.buildCommit=$(git rev-parse --short HEAD) -X main.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o ./squire ./cmd/squire
```

Smoke the identity surface:

```sh
./squire version
./squire version --json
./squire --help
```

## Required Checks

Normal release checks:

```sh
go test ./...
go run ./cmd/squire boost bench repo-metadata
go run ./cmd/squire boost bench deep-local
scripts/release_smoke.sh ./squire
scripts/edge_stress.py ./squire
```

Required gates:

- Enabled fast-path exactness is true.
- Enabled fast-path mismatches are zero.
- Stale HEAD replays are zero.
- Stale branch replays are zero.
- Validation replays are zero.
- Safety gates pass.
- Mutation-boundary invalidation is observed in the repo-metadata benchmark.

Performance gates must be reported. A performance gate failure blocks a release
only when the release claim depends on that budget.

## GitHub Release Flow

GitHub Actions provides three release surfaces:

- `.github/workflows/ci.yml` runs fast tests, the repo metadata benchmark, and
  release smoke on pushes and pull requests.
- `.github/workflows/nightly.yml` runs the deeper baseline plus edge stress on
  a schedule and manually.
- `.github/workflows/release.yml` runs the release gate, builds artifacts, and
  publishes a GitHub Release from either a `v*` tag or manual dispatch.

To publish from a tag:

```sh
git tag v0.1.0-beta.1
git push origin v0.1.0-beta.1
```

To publish manually, run the `release` workflow in GitHub Actions and provide
the desired version, for example `v0.1.0-beta.1`.

Current releases should use the `v0.x.y-beta.N` shape. The release workflow
publishes `v0.*`, `*-alpha*`, `*-beta*`, and `*-rc*` versions as GitHub
prereleases.

The release workflow must pass before artifacts are published. It runs:

- `go test ./...`
- release-candidate build with version ldflags
- `squire boost bench repo-metadata`
- `squire boost bench deep-local`
- `scripts/release_smoke.sh`
- `scripts/edge_stress.py`
- release safety-gate assertions

The workflow then builds:

- `linux/amd64`
- `linux/arm64`
- `darwin/amd64`
- `darwin/arm64`
- `windows/amd64`

Each release includes `.tar.gz` archives, `SHA256SUMS`, and
`RELEASE_MANIFEST.txt`. Verify downloaded artifacts with:

```sh
shasum -a 256 -c SHA256SUMS
# or, on Linux:
sha256sum -c SHA256SUMS
```

Local artifact builds use the same script as GitHub:

```sh
VERSION=v0.1.0-beta.1 scripts/build_release_artifacts.sh .tmp/release
```

## Dogfood Smoke

Use the scripted smoke when possible:

```sh
scripts/release_smoke.sh ./squire
```

Run the process-level edge stress suite before release candidates that touch
hot replay, prewarming, invalidation, or maintainer lifecycle code:

```sh
scripts/edge_stress.py ./squire
```

Expected behavior:

- Concurrent identical hot requests preserve exact output and report hot replay
  p50/p95/max timing.
- Mid-warm workspace mutations never replay stale file bytes.
- Interrupted foreground clients leave the resident maintainer healthy, with
  file-descriptor counts flat when the platform exposes them.
- Dynamic `.gitignore` changes invalidate stale status output.
- Environment-sensitive path discovery keeps distinct PATH footprints separate.

Run the release binary through a fresh local repo:

```sh
tmpdir=$(mktemp -d)
cd "$tmpdir"
git init
git config user.email squire@example.invalid
git config user.name "Squire Release"
printf "hello\n" > README.md
git add README.md
git commit -m init

/path/to/squire setup
/path/to/squire kernel maintain --background --short
/path/to/squire kernel warm --short
/path/to/squire kernel status --short
/path/to/squire kernel run -- git rev-parse HEAD
/path/to/squire kernel run -- git status --short
/path/to/squire boost status --short
/path/to/squire kernel maintain --stop --short
```

Expected behavior:

- Native fallback is reported as available.
- The maintainer starts once and stops cleanly.
- `kernel status --short` reports a repo oracle and readiness state.
- `kernel run` preserves exact stdout, stderr, and exit code.
- Mutating and validation commands remain native-only.

## Privacy And Boundary Review

Before release, confirm the README and CLI output still state:

- Agent chooses. Squire serves.
- Native fallback always exists.
- Validation, edits, mutations, and package installs are never replayed.
- Exact stdout, stderr, and exit code are required for replacement.
- Standard mode avoids raw prompts, completions, arbitrary stdout/stderr,
  source contents, auth values, and account identifiers.

## Artifact Notes

Attach benchmark JSON artifacts for:

- `repo-metadata`
- `deep-local`

Release notes should summarize exactness, invalidation, replay counts, and
measured wall-time deltas with the command count and workload scope that
produced them.
