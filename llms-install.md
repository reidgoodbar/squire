# Squire MCP Install For Cline

This file is for agent-driven setup.

Set up Squire in Cline as a `stdio` MCP server.

## What Squire is

Squire exposes short remote validation and offload jobs over MCP.

Use it for:

- runtime validation with `verify`
- clean test and lint runs
- compile checks
- ephemeral SQL
- data and media jobs
- offline browser checks
- offline quantum simulation

Do not treat it as a general remote shell or a persistent environment.

## Prerequisites

You need:

- a working `squire` CLI install
- a valid `SQUIRE_TOKEN`

If `squire` is not installed yet:

```bash
curl -fsSL https://squire.run/install.sh | bash
```

## Get MCP auth settings

Preferred:

```bash
squire mcp login
```

This prints the values needed for MCP hosts:

- `SQUIRE_TOKEN`
- `SQUIRE_API_BASE_URL`

If the user already has a Squire token, use that directly.

## Configure Squire in Cline

Add a `stdio` MCP server that runs:

```bash
squire mcp serve
```

If Cline is already running on a machine where `squire login` has been completed, the fastest setup is:

```bash
cline mcp add squire -- squire mcp serve
```

That uses the existing local Squire session from `~/.squire/config.json`.

Use these settings:

```json
{
  "mcpServers": {
    "squire": {
      "type": "stdio",
      "command": "squire",
      "args": ["mcp", "serve"],
      "env": {
        "SQUIRE_TOKEN": "<paste token here>",
        "SQUIRE_API_BASE_URL": "https://api.squire.run"
      }
    }
  }
}
```

If the environment already has a working local Squire session in `~/.squire/config.json`, `squire mcp serve` can reuse it. For portable Cline setups, prefer the explicit `env` block above.

## Registry note

Squire is also published in the MCP Registry as:

```text
io.github.reidgoodbar/squire
```

The registry package still expects the same environment variables:

- `SQUIRE_TOKEN` required
- `SQUIRE_API_BASE_URL` optional, defaults to `https://api.squire.run`

## Quick validation

After setup, confirm the MCP server starts and tools are visible.

Good first checks:

- list tools
- call `help`
- call `whoami`
- run a tiny `verify`

Example `verify` input:

```bash
squire verify --lang bash --file script.sh --targets ubuntu-24.04 --json
```

## Important policy notes

- `browser` is offline-only on the public service
- `deps` exists in the CLI but is disabled publicly under the zero-egress policy
- `audit` public support is limited to secrets and local-config static analysis
- `quantum simulate` is currently trusted-only on the public service

## Artifact-producing tools

These support local artifact download through the CLI:

- `data`
- `media`
- `browser`
- `build`
- `quantum simulate`

Use:

```bash
--download-artifacts <dir>
```
