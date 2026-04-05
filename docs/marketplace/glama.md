# Squire MCP On Glama

Squire's Glama deployment should run the same stdio MCP server as every other MCP host:

```bash
squire mcp serve
```

This repo is Go-based, so Glama should build the local CLI binary from source and then launch it through `mcp-proxy`.

## Recommended Glama configuration

Node.js version:

```text
25
```

Python version:

```text
3.14
```

Those values are only Glama platform defaults. Squire's MCP server does not require Node.js or Python at runtime.

Build steps:

```json
["bash ./scripts/build-glama.sh"]
```

CMD arguments:

```json
["./bin/squire","mcp","serve"]
```

Environment variables JSON schema:

```json
{
  "type": "object",
  "properties": {
    "SQUIRE_TOKEN": {
      "type": "string",
      "description": "Required Squire session or headless token used by the MCP wrapper to authenticate to https://api.squire.run."
    },
    "SQUIRE_API_BASE_URL": {
      "type": "string",
      "description": "Optional API base URL override. Defaults to https://api.squire.run.",
      "default": "https://api.squire.run"
    }
  },
  "required": ["SQUIRE_TOKEN"],
  "additionalProperties": false
}
```

Placeholder parameters:

```json
{
  "SQUIRE_TOKEN": "sqh_placeholder",
  "SQUIRE_API_BASE_URL": "https://api.squire.run"
}
```

The placeholder token is only for Glama startup checks. Real tool calls require a valid Squire token.

Pinned commit SHA:

Use the current `main` commit if you want a fixed build, or leave it blank to track `main`.

## Why this works

- `bash ./scripts/build-glama.sh` avoids execute-bit problems in builders that clone files without preserving script mode.
- `scripts/build-glama.sh` installs Go `1.26` if Glama's base image does not already have it.
- The script builds `./bin/squire` from this repo.
- `squire mcp serve` accepts `SQUIRE_TOKEN` from the environment, so Glama does not need to pre-write `~/.squire/config.json`.
- `SQUIRE_API_BASE_URL` is optional and defaults to `https://api.squire.run`.

## Local equivalent

After the build step finishes, the effective MCP startup command is:

```bash
./bin/squire mcp serve
```

For local validation with a real token:

```bash
SQUIRE_TOKEN="your_real_token" ./bin/squire mcp serve
```
