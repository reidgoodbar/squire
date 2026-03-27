# Squire CLI

Standalone repo for the Squire command-line client.

## What It Does

- authenticates against the Squire API
- stores local session state in `~/.squire/config.json`
- runs `verify`
- runs `deps` for fresh dependency-install checks
- runs `sql` against ephemeral SQLite and Postgres sandboxes
- runs `data` and `media` jobs in isolated fresh runtimes

## Default API

The published CLI defaults to:

```text
https://api.squire.run
```

Override per command with `--api-base-url` or globally with `SQUIRE_API_BASE_URL`.

## Commands

```bash
squire verify --lang python --file script.py --json
squire deps --lang python --file requirements.txt --targets py310,py311,py312 --json
squire sql --dialect sqlite --query "SELECT 1" --json
squire data --script transform.py --input big.csv --json
squire media --script clip.py --input video.mp4 --json
```

## Local Build

```bash
go build -o ./bin/squire ./cmd/squire
```

## Release Packaging

Create release archives locally:

```bash
./scripts/build-release.sh
```

Artifacts are written to `dist/` using stable names such as:

- `squire_darwin_amd64.tar.gz`
- `squire_darwin_arm64.tar.gz`
- `squire_linux_amd64.tar.gz`
- `squire_linux_arm64.tar.gz`

Those names are what `https://squire.run/install.sh` downloads from the latest GitHub release.
