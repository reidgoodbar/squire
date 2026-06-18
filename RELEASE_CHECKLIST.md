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

## Dogfood Smoke

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
