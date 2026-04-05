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

If `squire` is not installed yet:

```bash
curl -fsSL https://squire.run/install.sh | bash
```

## Start With Anonymous MCP

Start with:

```bash
squire mcp serve
```

No login is required for the public service.

If the user wants authenticated identity instead of anonymous public access, run:

```bash
squire mcp login
```

That prints optional MCP host values such as:

- `SQUIRE_TOKEN`
- `SQUIRE_API_BASE_URL`

## Configure Squire in Cline

Add a `stdio` MCP server that runs:

```bash
squire mcp serve
```

The fastest setup is:

```bash
cline mcp add squire -- squire mcp serve
```

Use these settings:

```json
{
  "mcpServers": {
    "squire": {
      "type": "stdio",
      "command": "squire",
      "args": ["mcp", "serve"]
    }
  }
}
```

If you want authenticated identity, add an `env` block with `SQUIRE_TOKEN` and optionally `SQUIRE_API_BASE_URL`.

## Registry note

Squire is also published in the MCP Registry as:

```text
io.github.reidgoodbar/squire
```

The registry package supports anonymous access by default. Optional environment variables:

- `SQUIRE_TOKEN`
- `SQUIRE_API_BASE_URL`, defaults to `https://api.squire.run`

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
- `quantum simulate` is offline-only on the public service

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
