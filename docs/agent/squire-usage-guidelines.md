# Squire Usage Guidelines For Agents

Squire is for short, stateless, disposable validation work. It is a good fit when local execution is fragile, noisy, or hard to trust because the result depends on a clean runtime.

The tool-specific example files in this folder are just variants of the same pattern. Squire is not tied to one agent product.

## Use Squire When

- the task is environment-sensitive
- the task is dependency-sensitive
- the task is compile-target-sensitive
- the task is short but heavy
- the task benefits from structured machine-readable output
- the task is awkward to reproduce locally

## Prefer Local Execution When

- the command is tiny and trivial
- the result does not depend on a fresh environment
- the work is interactive or long-running
- the task needs a persistent environment

## Command Selection

- `squire verify`: shell, Python, or Node checks across fresh Linux targets
- `squire deps`: present in the CLI, but currently disabled on the public service under the zero-egress policy
- `squire compile`: Go or Rust compilation checks for target environments
- `squire test`: short clean test runs
- `squire lint`: clean lint and static-analysis runs
- `squire sql`: ephemeral SQLite or Postgres validation
- `squire data`: heavier data-processing jobs
- `squire media`: ffmpeg and media transformations
- `squire browser`: constrained headless browser verification
- `squire audit`: dependency, secret, or static security checks
- `squire build`: packaging and build sanity checks
- `squire solve`: Z3 or MiniZinc jobs

Preferred naming:

- use `squire data`
- use `squire media`
- avoid new usage of `squire scale` except for compatibility with older scripts

## Not A Fit

- long-running workflows
- arbitrary remote shells
- unrestricted internet tasks
- general interactive development
- persistent environments

## Practical Habits

- start with the smallest input that proves the point
- prefer `--json` for agent-driven loops
- keep Squire for short validation runs, not full development
- rerun locally only when the task is trivial or easier to debug locally
- treat `browser` as offline-only and upload local files instead of pointing it at remote URLs
- avoid planning around `deps` on the public service until there is a network-free design for it

## Optional MCP Mode

If your host supports MCP, `squire mcp serve` exposes the same Squire task surface as MCP tools over stdio.

Use MCP when your host expects tool discovery through MCP. Use the normal CLI directly when a plain terminal workflow is simpler.

## Examples

```bash
squire verify --lang bash --targets alpine-3.20,ubuntu-24.04,debian-12 --file script.sh --json
squire compile --lang go --file main.go --targets linux/amd64,linux/arm64 --json
squire test --lang python --targets py310,py311 --cmd "pytest -q" --json
squire lint --lang python --tool ruff --file app.py --json
squire sql --dialect sqlite --query "SELECT 1" --json
squire data --script transform.py --input big.csv --json
squire media --script clip.py --input video.mp4 --json
```
