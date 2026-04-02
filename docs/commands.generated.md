<!-- Generated from internal/catalog/commands.json. Do not edit by hand. -->
# Squire Command Reference

Use Squire when correctness depends on a fresh environment, a specific target runtime, or a short heavy job that is better off your local machine.

## Discovery

### `squire help`

Show the top-level Squire command catalog or help for a specific command. This is the canonical discovery surface for humans and agents before choosing a command.

- Usage: `squire help [--json] [command [subcommand]]`
- Public status: `enabled`
- Supports `--json` output
- MCP tool: `help`
- The richer machine-readable form is available through squire --help --json.
- Use when:
  - You need to discover the available command surface.
  - An agent needs structured capability metadata before selecting a tool.
- Avoid when:
  - You already know the exact command and flags you need.

Flags:

- `--json`: Print the full machine-readable command catalog.

Examples:

```bash
squire --help
squire --help --json
squire help media
squire help quantum simulate
```

## Session

### `squire login`

Authenticate the local CLI against the Squire API. Browser-based GitHub OAuth is the default path; headless token login is available for automation and CI.

- Usage: `squire login [--token <SQUIRE_TOKEN>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- The default public login flow uses GitHub OAuth.
- Use when:
  - You are setting up a new machine or shell session.
  - You need a fresh session token with the latest scopes.
- Avoid when:
  - You are already logged in and the current session still works.

Flags:

- `--token` `<string>`: Squire-issued headless token for CI or non-browser login.
- `--json`: Print machine-readable login output.

Examples:

```bash
squire login
squire login --token sqh_...
```

### `squire update`

Download the published Squire CLI release for the current OS and architecture and replace the local binary in place.

- Usage: `squire update [--version <tag|latest>] [--install-dir <path>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- Use when:
  - You want the newest published CLI with the latest command surface and fixes.
- Avoid when:
  - You are developing the CLI locally and want to run the current source tree instead.

Flags:

- `--version` `<string>`: Release tag to install. Defaults to latest.
- `--install-dir` `<string>`: Directory to install the binary into.
- `--json`: Print machine-readable update output.

Examples:

```bash
squire update
squire update --version v0.6.3 --json
```

### `squire whoami`

Return the current authenticated identity, trust tier, feature flags, token metadata, and server-side quotas.

- Usage: `squire whoami [--json]`
- Public status: `enabled`
- Supports `--json` output
- MCP tool: `whoami`
- Use when:
  - You need to confirm the current trust tier or public feature access.
  - An agent needs to inspect the current quotas before planning heavier work.
- Avoid when:
  - You only need to run a command and do not care about session metadata.

Flags:

- `--json`: Print the full machine-readable identity and quota payload.

Examples:

```bash
squire whoami
squire whoami --json
```

### `squire logout`

Remove the locally stored Squire session configuration from the current machine.

- Usage: `squire logout [--json]`
- Public status: `enabled`
- Supports `--json` output
- Use when:
  - You want to clear the current local session.
  - You are switching between identities on the same machine.
- Avoid when:
  - You still need the current session token for ongoing work.

Flags:

- `--json`: Print machine-readable logout output.

Examples:

```bash
squire logout
squire logout --json
```

## Integration

### `squire mcp`

Manage Squire's MCP integration surface. Use mcp login to bootstrap token-based MCP configuration, or mcp serve to expose the Squire command surface over MCP stdio.

- Usage: `squire mcp <login|serve>`
- Public status: `enabled`
- Use squire mcp login to print SQUIRE_TOKEN and SQUIRE_API_BASE_URL for MCP hosts.
- Use when:
  - Your host expects MCP tool discovery and stdio transport.
  - You need copy-paste MCP auth settings for a registry-installed or locally launched Squire MCP server.
- Avoid when:
  - A plain terminal workflow is simpler.

Examples:

```bash
squire mcp login
squire mcp serve
```

### `squire mcp login`

Authenticate with Squire and print the MCP-oriented environment variables a local or registry-installed Squire MCP server expects.

- Usage: `squire mcp login [--token <SQUIRE_TOKEN>] [--api-base-url <url>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- This prints a bearer token. Treat SQUIRE_TOKEN as a secret.
- Use when:
  - You are configuring Squire in an MCP host that expects environment variables.
  - You want a copy-paste SQUIRE_TOKEN and SQUIRE_API_BASE_URL snippet after logging in.
- Avoid when:
  - You only need the normal CLI and do not plan to use an MCP host.

Flags:

- `--token` `<string>`: Squire-issued headless token for CI or non-browser login.
- `--api-base-url` `<string>`: Override API base URL for this login.
- `--json`: Print machine-readable MCP bootstrap output, including env vars.

Examples:

```bash
squire mcp login
squire mcp login --token sqh_... --json
```

### `squire mcp serve`

Start an MCP stdio server that exposes Squire tools and reuses the current local Squire session or an injected SQUIRE_TOKEN.

- Usage: `squire mcp serve`
- Public status: `enabled`
- Run squire mcp login first if you do not already have a local session.
- SQUIRE_TOKEN and SQUIRE_API_BASE_URL are also accepted through the environment.
- Use when:
  - Your MCP client needs Squire tools over stdio.
- Avoid when:
  - You only need the normal CLI.

Examples:

```bash
squire mcp login
squire mcp serve
```

## Validation

### `squire verify`

Run small inline snippets or staged scripts in fresh Linux containers across the supported target images.

- Usage: `squire verify --lang <bash|python|node> [--code <inline> | --file <path>] [--targets <csv>] [--timeout <seconds>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- Offline only
- MCP tool: `verify`
- Use exact target IDs such as alpine-3.20, ubuntu-24.04, and debian-12.
- Outbound network is disabled.
- Use when:
  - You need a clean runtime check across Linux targets.
  - You are debugging OS-sensitive or environment-sensitive behavior.
- Avoid when:
  - The command is trivial and local execution is faster.
  - You need a persistent environment or long-running workflow.

Flags:

- `--lang` `<string>`: Verify language: bash, python, or node.
- `--code` `<string>`: Inline code snippet to run.
- `--file` `<string>`: Local script file to upload.
- `--targets` `<csv>`: Comma-separated target list.
- `--timeout` `<integer>`: Per-target timeout in seconds.
- `--json`: Print raw JSON response.

Examples:

```bash
squire verify --lang bash --file script.sh --targets alpine-3.20,ubuntu-24.04,debian-12 --json
squire verify --lang python --code "print('ok')" --targets ubuntu-24.04 --json
```

### `squire deps`

Validate whether dependency manifests install in a clean environment. The CLI surface exists, but the public zero-egress service currently rejects deps jobs.

- Usage: `squire deps --lang <python|node> --file <path> [--targets <csv>] [--timeout <seconds>] [--json]`
- Public status: `disabled: disabled on the public service under the zero-egress policy`
- Supports `--json` output
- MCP tool: `deps`
- The current public service rejects deps requests because outbound sandbox network access is not allowed.
- Use when:
  - You need dependency-install validation in a clean environment when the service policy allows it.
- Avoid when:
  - You are targeting the current public service, where deps is disabled.

Flags:

- `--lang` `<string>`: Dependency language.
- `--file` `<string>`: Dependency manifest file to upload.
- `--targets` `<csv>`: Comma-separated dependency targets.
- `--timeout` `<integer>`: Dependency check timeout in seconds.
- `--json`: Print raw JSON response.

Examples:

```bash
squire deps --lang python --file requirements.txt --targets py310,py311 --json
```

### `squire sql`

Run SQLite or Postgres schema, query, and migration validation in a fresh disposable database sandbox.

- Usage: `squire sql --dialect <sqlite|postgres-16> [--file <path> | --schema <path> --query <sql> | --query-file <path>] [--explain] [--timeout <seconds>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- Offline only
- MCP tool: `sql`
- Outbound network is disabled.
- Postgres runs in a fresh request-scoped helper inside the sandbox.
- Use when:
  - You need clean SQLite or Postgres validation without local database setup.
- Avoid when:
  - You need a persistent database or long-lived connection.

Flags:

- `--dialect` `<string>`: SQL dialect.
- `--file` `<string>`: SQL file containing statements to apply.
- `--schema` `<string>`: Schema file to apply before a query.
- `--query` `<string>`: Inline SQL query.
- `--query-file` `<string>`: Query file to execute after schema setup.
- `--explain`: Request an execution plan when supported.
- `--timeout` `<integer>`: SQL timeout in seconds.
- `--json`: Print raw JSON response.

Examples:

```bash
squire sql --dialect sqlite --query "SELECT 1" --json
squire sql --dialect postgres-16 --file migration.sql --json
```

### `squire test`

Run small or medium test jobs in clean runtimes with a target matrix for Python, Node, or Bash.

- Usage: `squire test --lang <python|node|bash> --file <path> [--file <path> ...] [--cmd <command>] [--targets <csv>] [--timeout <seconds>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- Offline only
- MCP tool: `test`
- Outbound network is disabled.
- Use when:
  - You need a fresh test environment without local drift.
  - You want a short target matrix instead of full CI.
- Avoid when:
  - You need a full CI pipeline or long-running test suite.

Flags:

- `--lang` `<string>`: Test language.
- `--file` `<string>`: File to stage for the test run.
- `--cmd` `<string>`: Restricted test command such as pytest -q or npm test.
- `--targets` `<csv>`: Comma-separated runtime targets.
- `--timeout` `<integer>`: Test timeout in seconds.
- `--json`: Print raw JSON response.

Examples:

```bash
squire test --lang python --file test_app.py --cmd "pytest -q" --targets py310,py311 --json
squire test --lang node --file test/app.test.mjs --cmd "node --test" --targets node20,node22 --json
```

### `squire lint`

Run fixed lint and static-analysis tools in a fresh toolchain so local environment drift does not affect the result.

- Usage: `squire lint --lang <python|js|ts|rust> --tool <ruff|eslint|clippy> --file <path> [--file <path> ...] [--targets <csv>] [--timeout <seconds>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- Offline only
- MCP tool: `lint`
- Outbound network is disabled.
- The public service supports ruff, eslint, and clippy.
- Use when:
  - You need a clean lint result from a fixed supported toolchain.
- Avoid when:
  - You need arbitrary plugins or unsupported linters.

Flags:

- `--lang` `<string>`: Lint language.
- `--tool` `<string>`: Supported lint tool.
- `--file` `<string>`: File to stage for linting.
- `--targets` `<csv>`: Comma-separated lint targets.
- `--timeout` `<integer>`: Lint timeout in seconds.
- `--json`: Print raw JSON response.

Examples:

```bash
squire lint --lang python --tool ruff --file app.py --json
squire lint --lang rust --tool clippy --file Cargo.toml --file src/main.rs --json
```

### `squire audit`

Run the supported security-focused audit surfaces against staged local files. On the public service this currently means secret scanning and local-config static analysis.

- Usage: `squire audit [--secrets | --static] [--lang <language>] [--tool <tool>] [--config <path>] [--file <path> | --path <dir>] ... [--targets <csv>] [--timeout <seconds>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- Offline only
- MCP tool: `audit`
- The public service supports secret scanning and local-config static analysis.
- Dependency audit and remote Semgrep configs are disabled on the public service.
- Outbound network is disabled.
- Use when:
  - You need a quick secret scan or a constrained local-config static analysis pass.
- Avoid when:
  - You need dependency audit on the current public service.
  - You need remote Semgrep configs or unrestricted external lookups.

Flags:

- `--lang` `<string>`: Dependency audit language. Public dependency audit is currently disabled.
- `--secrets`: Run the built-in secret scanner.
- `--static`: Run static analysis.
- `--tool` `<string>`: Audit tool, such as semgrep.
- `--config` `<string>`: Static analysis config as a staged local file path.
- `--file` `<string>`: File to stage for auditing.
- `--path` `<string>`: Directory tree to stage recursively.
- `--targets` `<csv>`: Comma-separated audit targets.
- `--timeout` `<integer>`: Audit timeout in seconds.
- `--json`: Print raw JSON response.

Examples:

```bash
squire audit --secrets --path src --json
squire audit --static --tool semgrep --config semgrep.yml --path src --json
```

### `squire build`

Run offline packaging and build sanity checks in clean environments and optionally pull the resulting artifacts back locally.

- Usage: `squire build --lang <python|node> [--file <path> | --path <dir>] ... [--targets <csv>] [--timeout <seconds>] [--download-artifacts <dir>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- Offline only
- MCP tool: `build`
- Outbound network is disabled.
- Use --download-artifacts to fetch build outputs locally.
- Use when:
  - You want a clean package/build sanity check without publishing anything.
- Avoid when:
  - You need a full release pipeline or registry publishing.

Flags:

- `--lang` `<string>`: Build language.
- `--file` `<string>`: File to stage for the build.
- `--path` `<string>`: Directory tree to stage recursively.
- `--targets` `<csv>`: Comma-separated build targets.
- `--timeout` `<integer>`: Build timeout in seconds.
- `--download-artifacts` `<string>`: Local directory to download build artifacts into.
- `--json`: Print raw JSON response.

Examples:

```bash
squire build --lang python --file pyproject.toml --path src --targets manylinux,musllinux --download-artifacts ./dist --json
```

### `squire bench`

Run small, short-lived benchmark jobs in clean runtimes to compare simple timing behavior without turning Squire into a full performance platform.

- Usage: `squire bench --lang <python|bash|go> [--file <path> | --path <dir>] ... [--targets <csv>] [--iterations <count>] [--timeout <seconds>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- Offline only
- MCP tool: `bench`
- Outbound network is disabled.
- Use when:
  - You want a quick timing comparison in clean runtimes.
- Avoid when:
  - You need full performance engineering, profiling, or large load tests.

Flags:

- `--lang` `<string>`: Benchmark language.
- `--file` `<string>`: File to stage for benchmarking.
- `--path` `<string>`: Directory tree to stage recursively.
- `--targets` `<csv>`: Comma-separated benchmark targets.
- `--iterations` `<integer>`: Number of iterations per target.
- `--timeout` `<integer>`: Benchmark timeout in seconds.
- `--json`: Print raw JSON response.

Examples:

```bash
squire bench --lang python --file bench.py --targets py310,py311 --json
```

### `squire browser`

Run headless Chromium in a constrained offline sandbox and optionally download screenshots or other generated browser artifacts locally.

- Usage: `squire browser [--browser chromium] [--script <path>] [--url <file://url>] [--file <path> | --path <dir>] ... [--screenshot <name>] [--timeout <seconds>] [--download-artifacts <dir>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- Offline only
- MCP tool: `browser`
- The public service is offline-only for browser runs.
- Stage full local asset trees with --path when you need images, fonts, or other binary files.
- Use --download-artifacts to fetch screenshots or other generated files locally.
- Use when:
  - You need offline browser verification against staged local assets.
  - You need screenshots or simple DOM/assertion checks in a disposable browser runtime.
- Avoid when:
  - You need to fetch remote http:// or https:// URLs on the public service.

Flags:

- `--browser` `<string>`: Browser runtime.
- `--script` `<string>`: Browser automation script to stage.
- `--url` `<string>`: Offline URL to open, such as file://.
- `--file` `<string>`: File to stage for browser execution.
- `--path` `<string>`: Directory tree to stage recursively.
- `--screenshot` `<string>`: Screenshot filename to produce.
- `--timeout` `<integer>`: Browser timeout in seconds.
- `--download-artifacts` `<string>`: Local directory to download screenshots or other browser artifacts into.
- `--json`: Print raw JSON response.

Examples:

```bash
squire browser --path website/public --screenshot page.png --download-artifacts ./browser-out --json
```

### `squire compile`

Run target-specific Go or Rust compilation checks in clean toolchains without turning Squire into a full CI or release system.

- Usage: `squire compile --lang <go|rust> --file <path> [--file <path> ...] [--targets <csv>] [--timeout <seconds>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- Offline only
- MCP tool: `compile`
- Outbound network is disabled.
- Use when:
  - You need target compilation validation in a clean environment.
- Avoid when:
  - You need full repository builds or arbitrary Docker-based build systems.

Flags:

- `--lang` `<string>`: Compile language.
- `--file` `<string>`: File to stage for compilation.
- `--targets` `<csv>`: Comma-separated compile targets.
- `--timeout` `<integer>`: Compile timeout in seconds.
- `--json`: Print raw JSON response.

Examples:

```bash
squire compile --lang go --file main.go --targets linux/amd64,linux/arm64 --json
squire compile --lang rust --file Cargo.toml --file src/main.rs --targets linux/amd64-musl,linux/arm64 --json
```

### `squire solve`

Run bounded solver jobs for Z3 or MiniZinc in a fresh disposable sandbox.

- Usage: `squire solve --solver <z3|minizinc> --file <path> [--data <path>] [--timeout <seconds>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- Offline only
- MCP tool: `solve`
- Outbound network is disabled.
- Use when:
  - You need a satisfiability or small optimization/model check without local solver setup.
- Avoid when:
  - You need arbitrary scripting around solvers or long-running research workloads.

Flags:

- `--solver` `<string>`: Solver backend.
- `--file` `<string>`: Solver input file.
- `--data` `<string>`: Optional MiniZinc .dzn data file.
- `--timeout` `<integer>`: Solver timeout in seconds.
- `--json`: Print raw JSON response.

Examples:

```bash
squire solve --solver z3 --file constraints.smt2 --json
squire solve --solver minizinc --file model.mzn --data data.dzn --json
```

### `squire quantum`

Run bounded offline quantum simulations in a dedicated Qiskit Aer runtime.

- Usage: `squire quantum simulate --file <path> [--file <path> ...] [--shots <count>] [--backend aer_simulator] [--timeout <seconds>] [--download-artifacts <dir>] [--json]`
- Public status: `trusted-only`
- Offline only
- Trusted access required on the public service
- Quantum runs are offline-only on the public service.
- Trusted access or higher is currently required.
- Use when:
  - You need an offline Qiskit Aer simulation in a fresh remote runtime.
- Avoid when:
  - You need notebooks, persistent sessions, hardware providers, or multiple frameworks.

Examples:

```bash
squire quantum simulate --file shor.py --download-artifacts ./quantum-out --json
```

### `squire quantum simulate`

Stage a small Python/Qiskit file set, run the entry file inside an offline Qiskit Aer image, and optionally download generated artifacts locally.

- Usage: `squire quantum simulate --file <path> [--file <path> ...] [--shots <count>] [--backend aer_simulator] [--timeout <seconds>] [--download-artifacts <dir>] [--json]`
- Public status: `trusted-only`
- Supports `--json` output
- Offline only
- Trusted access required on the public service
- MCP tool: `quantum_simulate`
- The first --file is the Python entry script.
- Additional --file values stage helper modules or local assets.
- Write files under /workspace/output and use --download-artifacts to retrieve them locally.
- The public service is offline-only and trusted-only for this module.
- Use when:
  - You need bounded Qiskit Aer simulation and the local job is too heavy or noisy.
- Avoid when:
  - You need hardware providers, notebooks, or persistent sessions.

Flags:

- `--file` `<string>`: First file is the entry script; repeat to stage helper modules or local assets.
- `--shots` `<integer>`: Shot count passed to the simulation.
- `--backend` `<string>`: Quantum backend.
- `--timeout` `<integer>`: Simulation timeout in seconds.
- `--download-artifacts` `<string>`: Local directory to download generated quantum artifacts into.
- `--json`: Print raw JSON response.

Examples:

```bash
squire quantum simulate --file shor.py --shots 2048 --download-artifacts ./quantum-out --json
```

## Jobs

### `squire data`

Run Python data-processing jobs in a disposable remote runtime with pandas, polars, and pyarrow available.

- Usage: `squire data --script <path> [--input <path> | --stdin] [--timeout <seconds>] [--download-artifacts <dir>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- Offline only
- MCP tool: `data`
- Runs in Python 3.11 with pandas, polars, and pyarrow available.
- Scripts read SQUIRE_INPUT_PATH and write generated files to SQUIRE_OUTPUT_DIR.
- Outbound network is disabled.
- Use when:
  - You need a heavier data transform in a clean disposable runtime.
  - The script should read one staged input and write result files.
- Avoid when:
  - The task is tiny and faster to run locally.

Flags:

- `--script` `<string>`: Python script to execute.
- `--input` `<string>`: Input file for multipart upload.
- `--stdin`: Read a small input payload from stdin instead of --input.
- `--timeout` `<integer>`: Job timeout in seconds.
- `--download-artifacts` `<string>`: Local directory to download generated data artifacts into.
- `--json`: Print raw JSON response.

Examples:

```bash
squire data --script transform.py --input big.csv --download-artifacts ./data-out --json
```

### `squire media`

Run Python media jobs in a disposable remote runtime with ffmpeg installed and optionally download the generated files locally.

- Usage: `squire media --script <path> [--input <path>] [--timeout <seconds>] [--download-artifacts <dir>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- Offline only
- MCP tool: `media`
- Runs in Python 3.11 with ffmpeg installed.
- Scripts read SQUIRE_INPUT_PATH and write output files to SQUIRE_OUTPUT_DIR.
- Outbound network is disabled.
- Use when:
  - You need an ffmpeg-based image, audio, or video transformation off your local machine.
- Avoid when:
  - The task is tiny and simpler to run locally.
  - You need network access or arbitrary package installs in the media runtime.

Flags:

- `--script` `<string>`: Python script to execute.
- `--input` `<string>`: Input file for multipart upload.
- `--timeout` `<integer>`: Job timeout in seconds.
- `--download-artifacts` `<string>`: Local directory to download generated media artifacts into.
- `--json`: Print raw JSON response.

Examples:

```bash
squire media --script clip.py --input image.png --download-artifacts ./media-out --json
```

### `squire scale`

Compatibility alias for older scripts that still use squire scale. New usage should prefer squire data or squire media directly.

- Usage: `squire scale --mode <data|media> --script <path> [--input <path> | --stdin] [--timeout <seconds>] [--download-artifacts <dir>] [--json]`
- Public status: `enabled`
- Supports `--json` output
- Offline only
- Prefer new usage of squire data or squire media.
- Use when:
  - You are maintaining older scripts that still call squire scale.
- Avoid when:
  - You are writing new command invocations.

Flags:

- `--mode` `<string>`: Compatibility mode.
- `--script` `<string>`: Python script to execute.
- `--input` `<string>`: Input file for multipart upload.
- `--stdin`: Read a small input payload from stdin instead of --input.
- `--timeout` `<integer>`: Job timeout in seconds.
- `--download-artifacts` `<string>`: Local directory to download generated artifacts into.
- `--json`: Print raw JSON response.

Examples:

```bash
squire scale --mode media --script clip.py --input image.png --download-artifacts ./media-out --json
```

