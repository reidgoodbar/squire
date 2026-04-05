# Squire CLI

Squire is a CLI for running short validation and offload jobs in clean remote runtimes. Use it for cross-environment checks, fresh validation loops, target compilation checks, ephemeral SQL sandboxes, and short heavy jobs that are awkward to run locally.

## Quick start

```bash
curl -fsSL https://squire.run/install.sh | bash
squire --help
```

Defaults to:

- `https://api.squire.run`

Works on:

- macOS
- Linux
- WSL

Update later with:

```bash
squire update
```

## Command discovery

Use these first:

- `squire --help` shows the command catalog
- `squire --help --json` shows the command catalog in machine-readable form
- `squire <command> --help` shows command-specific usage

Prefer `--json` when another tool or agent will read the result.

For the full generated command catalog, public-policy notes, and examples, see:

- `docs/commands.generated.md`

## Task-to-command mapping

Use Squire when a task is environment-sensitive, target-sensitive, or short but heavy.

- shell, Python, or Node runtime validation -> `squire verify`
- short clean test runs -> `squire test`
- lint or static analysis -> `squire lint`
- Go or Rust target compilation -> `squire compile`
- offline Qiskit Aer simulation -> `squire quantum simulate`
- SQLite or Postgres validation -> `squire sql`
- dependency, security, secret, or static checks -> `squire audit`
- solver tasks -> `squire solve`
- pandas, polars, or pyarrow jobs -> `squire data`
- ffmpeg or media transforms -> `squire media`
- offline headless browser verification -> `squire browser`
- packaging or build sanity checks -> `squire build`
- short comparative timing runs -> `squire bench`

Public-service note:

- `deps` exists in the CLI, but is currently disabled on the public service under the zero-egress policy

## Agent workflows

Squire is intentionally CLI-first and works well in terminal-first coding-agent workflows such as Claude Code, Codex, and shell-driven automation.

Copy and trim the example instruction files in `docs/agent/` for your own environment:

- `docs/agent/CLAUDE.md.example`
- `docs/agent/SKILLS.md.example`
- `docs/agent/CODEX.md.example`
- `docs/agent/squire-usage-guidelines.md`
- `docs/agent/tool-constraints.md`

Prefer local execution for tiny trivial checks. Prefer Squire when correctness depends on a fresh environment or when the task is too annoying to run locally.

For per-tool runtime, network, input, output, and public-service constraints, start with:

- `docs/agent/tool-constraints.md`
- `docs/commands.generated.md`

## Current public-service limits

- `browser` is offline-only and does not fetch remote `http://` or `https://` URLs
- `deps` is currently disabled on the public service because sandbox egress is not allowed
- `audit` supports secret scanning and local-config static analysis; dependency audit and remote Semgrep configs are disabled
- `quantum simulate` is offline-only on the public service

## Artifact downloads

Artifact-producing commands can download outputs locally:

```bash
squire media --script square.py --input image.png --download-artifacts ./out --json
```

Use `--download-artifacts <dir>` for:

- `squire data`
- `squire media`
- `squire browser`
- `squire build`
- `squire quantum simulate`

## Command examples

```bash
squire verify --lang bash --file script.sh --targets alpine-3.20,ubuntu-24.04,debian-12 --json
squire test --lang python --file test_app.py --cmd "pytest -q" --targets py310,py311 --json
squire lint --lang python --tool ruff --file app.py --json
squire quantum simulate --file shor.py --shots 2048 --download-artifacts ./quantum-out --json
squire audit --secrets --path src --json
squire build --lang python --file pyproject.toml --path src --targets manylinux,musllinux --download-artifacts ./dist --json
squire bench --lang python --file bench.py --targets py310,py311 --json
squire browser --path website/public --screenshot page.png --download-artifacts ./browser-out --json
squire sql --dialect sqlite --query "SELECT 1" --json
squire compile --lang go --file main.go --targets linux/amd64,linux/arm64 --json
squire solve --solver z3 --file constraints.smt2 --json
squire data --script transform.py --input big.csv --download-artifacts ./data-out --json
squire media --script clip.py --input image.png --download-artifacts ./media-out --json
```

A tiny runnable quantum example is in `examples/quantum/shor.py`.

## Optional MCP mode

Run the local stdio wrapper:

```bash
squire mcp serve
```

This exposes the same Squire command surface over MCP stdio. Public anonymous access works without login. Squire remains CLI-first; MCP is an optional wrapper.

If you want authenticated identity in an MCP host, optionally run:

```bash
squire mcp login
```

That prints the `SQUIRE_TOKEN` and `SQUIRE_API_BASE_URL` values an MCP host can use.

For Cline on a machine that already has a working local Squire session, the shortest path is:

```bash
cline mcp add squire -- squire mcp serve
```

Squire is also published to the MCP Registry as `io.github.reidgoodbar/squire`.
The registry package is a GHCR-backed OCI stdio server that runs the same `squire mcp serve` surface and supports:

- anonymous access by default
- `SQUIRE_TOKEN` optional
- `SQUIRE_API_BASE_URL` optional

Registry metadata lives in `registry/server.json`.

For Cline and other agent-hosted setup, see:

- `llms-install.md`
- `docs/marketplace/glama.md`

## Local build

```bash
go build -o ./bin/squire ./cmd/squire
```

## Release packaging

```bash
./scripts/build-release.sh
```

Artifacts go to `dist/` and use the same stable names that `https://squire.run/install.sh` downloads.

## License

MIT. See `LICENSE`.
