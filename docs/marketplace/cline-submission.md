# Cline MCP Marketplace Submission

GitHub Repository URL:

https://github.com/reidgoodbar/squire

Logo Image:

https://raw.githubusercontent.com/reidgoodbar/squire/main/docs/marketplace/squire-cline-logo.png

Additional Information:

Squire is a CLI-first MCP server for short remote validation and offload jobs in clean disposable runtimes. It gives Cline users a narrow, practical tool surface for tasks that are environment-sensitive, dependency-sensitive, target-sensitive, or just too heavy and annoying to run locally.

Useful workflows for Cline users:

- `verify` for fresh Linux runtime checks
- `test` and `lint` for clean validation loops
- `compile` for Go and Rust target checks
- `sql` for ephemeral SQLite and Postgres validation
- `data` and `media` for heavier offload jobs
- `browser` for offline headless page verification
- `quantum simulate` for bounded offline Qiskit Aer jobs

Squire keeps the same hardened execution model across these tools:

- fresh container per request
- runsc
- AppArmor
- seccomp
- non-root
- read-only rootfs
- no outbound network by default

I added dedicated Cline-oriented setup guidance in `llms-install.md`, and the MCP bootstrap flow is:

```bash
squire mcp login
```

That prints the `SQUIRE_TOKEN` and `SQUIRE_API_BASE_URL` values needed by MCP hosts. The MCP server itself is:

```bash
squire mcp serve
```

Squire is also published in the MCP Registry as:

```text
io.github.reidgoodbar/squire
```

Validation completed before submission:

- verified fresh CLI install from `https://squire.run/install.sh`
- verified `squire mcp login --json`
- verified the published MCP Registry package `ghcr.io/reidgoodbar/squire-mcp:0.6.4`
- verified `initialize`, `tools/list`, and a real `verify` tool call through the published package
- verified Cline CLI MCP config generation with `cline mcp add`
- verified a docs-only Cline task with just `README.md` and `llms-install.md`; Cline created a valid `cline_mcp_settings.json` entry for `squire` and verified it
