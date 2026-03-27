# Squire CLI

Standalone repo for the Squire command-line client.

## What It Does

- authenticates against the Squire API
- stores local session state in `~/.squire/config.json`
- runs `verify`
- runs `scale` in `data` and `media` modes

## Default API

The published CLI defaults to:

```text
https://api.squire.run
```

Override per command with `--api-base-url` or globally with `SQUIRE_API_BASE_URL`.

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
