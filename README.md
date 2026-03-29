# Squire CLI

Standalone repo for the Squire command-line client.

## What It Does

- authenticates against the Squire API
- updates the installed CLI from the published release channel
- stores local session state in `~/.squire/config.json`
- runs `verify`
- runs `test` for short fresh-runtime test suites
- runs `lint` for fresh-toolchain lint and static-analysis checks
- runs `audit` for dependency, secret, and Semgrep checks
- runs `build` for clean packaging/build sanity checks
- runs `bench` for short comparative timing runs
- runs `browser` for constrained offline headless Chromium jobs
- includes `deps`, which is currently disabled on the public service under the zero-egress policy
- runs `sql` against ephemeral SQLite and Postgres sandboxes
- runs `compile` for cross-target Go and Rust build checks
- runs `solve` for Z3 and MiniZinc jobs
- runs `data` and `media` jobs in isolated fresh runtimes

## Agent Workflows

Squire is intentionally CLI-first. It works well in terminal-first coding-agent workflows and automated shell-based validation loops that need clean disposable runtimes without adding a new protocol layer.

Claude Code, Codex, and similar tools are examples, not special integrations.

Copy the example instruction files from `docs/agent/` into your own repo or environment and trim them to fit your workflow:

- `docs/agent/CLAUDE.md.example`
- `docs/agent/SKILLS.md.example`
- `docs/agent/CODEX.md.example`
- `docs/agent/squire-usage-guidelines.md`

These are templates for common agent setups, not product-specific lock-in.

Use Squire when a task is environment-sensitive, dependency-sensitive, compile-target-sensitive, short but heavy, or easier to reason about with structured JSON output. Prefer local execution for tiny trivial checks.

Current zero-egress policy notes:

- `browser` is offline-only and does not fetch remote `http://` or `https://` URLs
- `deps` is currently disabled on the public service because sandbox egress is not allowed
- `audit` supports secret scanning and local-config static analysis; dependency audit and remote Semgrep configs are disabled

For command discovery:

- `squire --help` prints the full command catalog with one-line guidance
- `squire --help --json` prints the command catalog in a machine-readable form for agents
- `squire <command> --help` prints command-specific usage

Optional MCP mode:

- `squire mcp serve` exposes the same Squire command surface over MCP stdio for MCP-compatible hosts and editors
- the MCP server reuses the current local Squire login/session
- Squire remains CLI-first; MCP is an optional wrapper, not a separate backend

## Default API

The published CLI defaults to:

```text
https://api.squire.run
```

Override per command with `--api-base-url` or globally with `SQUIRE_API_BASE_URL`.

## Commands

```bash
squire update
squire mcp serve
squire verify --lang python --file script.py --json
squire test --lang python --file test_app.py --cmd "pytest -q" --targets py310,py311 --json
squire lint --lang python --tool ruff --file app.py --json
squire audit --secrets --path src --json
squire build --lang python --file pyproject.toml --path src --targets manylinux,musllinux --json
squire bench --lang python --file bench.py --targets py310,py311 --json
squire browser --file index.html --screenshot page.png --json
squire sql --dialect sqlite --query "SELECT 1" --json
squire compile --lang go --file main.go --targets linux/amd64,linux/arm64 --json
squire solve --solver z3 --file constraints.smt2 --json
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
The published archives also embed the release version so `squire update` can report what it installed.
