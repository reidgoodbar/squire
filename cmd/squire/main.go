package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"squire.run/internal/kernel"
)

var (
	buildVersion = "dev"
	buildCommit  = "unknown"
	buildDate    = "unknown"
)

type versionReport struct {
	Product  string `json:"product"`
	Contract string `json:"contract"`
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Date     string `json:"date"`
}

func main() {
	ctx, stop := rootContext()
	defer stop()
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	storeRoot := storeRootFor(cwd)
	args := os.Args[1:]
	if text, ok := helpTextForArgs(args); ok {
		fmt.Print(text)
		return
	}
	if len(args) == 0 {
		fatalUsage("missing command")
		os.Exit(2)
	}
	var out string
	switch {
	case len(args) == 1 && args[0] == "setup":
		enableSquireOwnedGitReads()
		out, err = kernel.Setup(ctx, cwd, storeRoot)
	case len(args) >= 1 && args[0] == "codex":
		enableSquireOwnedGitReads()
		code, runErr := runCodex(ctx, cwd, storeRoot, args[1:])
		if runErr != nil {
			fmt.Fprintln(os.Stderr, runErr)
			os.Exit(1)
		}
		os.Exit(code)
		return
	case len(args) >= 1 && args[0] == "session":
		enableSquireOwnedGitReads()
		err = runSession(ctx, cwd, storeRoot, args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	case len(args) >= 1 && args[0] == "vm":
		enableSquireOwnedGitReads()
		out, err = runVM(ctx, cwd, storeRoot, args[1:])
	case len(args) >= 1 && args[0] == "version":
		var format outputFormat
		format, err = outputFormatFromTrailingArgsDefault(args[1:], outputShort)
		if err != nil {
			break
		}
		out = versionOut(format)
	case len(args) >= 3 && args[0] == "kernel" && args[1] == "prewarm-adjacent":
		enableSquireOwnedGitReads()
		err = runAdjacentPrewarm(ctx, cwd, storeRoot, args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	case len(args) >= 3 && args[0] == "kernel" && args[1] == "adapter":
		err = runKernelAdapter(ctx, cwd, args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	case len(args) >= 3 && args[0] == "kernel" && args[1] == "shim-helper":
		err = runKernelShimHelper(ctx, cwd, args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	case len(args) >= 3 && args[0] == "kernel" && args[1] == "run":
		runKernelCommand(ctx, cwd, storeRoot, args[2:])
		return
	case len(args) == 2 && args[0] == "kernel" && args[1] == "status":
		enableSquireOwnedGitReads()
		out, err = kernel.KernelStatus(ctx, cwd, storeRoot)
	case len(args) == 3 && args[0] == "kernel" && args[1] == "status" && args[2] == "--short":
		enableSquireOwnedGitReads()
		out, err = kernel.KernelStatusSummary(ctx, cwd, storeRoot)
	case len(args) >= 2 && args[0] == "kernel" && args[1] == "warm":
		enableSquireOwnedGitReads()
		var format outputFormat
		var metadataOnly bool
		format, metadataOnly, err = parseWarmArgs(args[2:])
		if err != nil {
			break
		}
		var report kernel.WarmReport
		if metadataOnly {
			report, err = kernel.WarmMetadata(ctx, cwd, storeRoot)
		} else {
			report, err = kernel.Warm(ctx, cwd, storeRoot)
		}
		if err == nil {
			out = warmReportOut(report, format)
		}
	case len(args) >= 2 && args[0] == "kernel" && args[1] == "maintain":
		enableSquireOwnedGitReads()
		out, err = runMaintain(ctx, cwd, storeRoot, args[2:])
	case len(args) >= 2 && args[0] == "boost" && args[1] == "status":
		var format outputFormat
		format, err = outputFormatFromTrailingArgsDefault(args[2:], outputShort)
		if err != nil {
			break
		}
		var report kernel.BoostStatusReport
		report, err = kernel.BoostStatusReportForStore(ctx, cwd, storeRoot)
		if err == nil {
			out = boostStatusOut(report, format)
		}
	case len(args) >= 3 && args[0] == "boost" && args[1] == "bench" && args[2] == "repo-metadata":
		enableSquireOwnedGitReads()
		var format outputFormat
		format, err = outputFormatFromTrailingArgs(args[3:])
		if err != nil {
			break
		}
		var report kernel.BenchReport
		report, err = kernel.BenchRepoMetadata(ctx)
		if err == nil {
			err = kernel.NewLedgerStore(storeRoot).SaveLatestBenchmarkStatus(kernel.LatestBenchmarkFromRepoMetadata(report))
		}
		if err == nil {
			out = repoMetadataBenchOut(report, format)
		}
	case len(args) >= 3 && args[0] == "boost" && args[1] == "bench" && args[2] == "deep-local":
		enableSquireOwnedGitReads()
		var format outputFormat
		format, err = outputFormatFromTrailingArgs(args[3:])
		if err != nil {
			break
		}
		var report kernel.DeepBenchReport
		report, err = kernel.BenchDeepLocal(ctx)
		if err == nil {
			err = kernel.NewLedgerStore(storeRoot).SaveLatestBenchmarkStatus(kernel.LatestBenchmarkFromDeepLocal(report))
		}
		if err == nil {
			out = deepLocalBenchOut(report, format)
		}
	case args[0] == "kernel":
		fatalUsage(kernelUsageError(args[1:]))
		os.Exit(2)
	case args[0] == "boost":
		fatalUsage(boostUsageError(args[1:]))
		os.Exit(2)
	case args[0] == "vm":
		fatalUsage(vmUsageError(args[1:]))
		os.Exit(2)
	default:
		fatalUsage(fmt.Sprintf("unknown command %q", args[0]))
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(out)
}

func usage() {
	fmt.Fprint(os.Stderr, usageText())
}

func enableSquireOwnedGitReads() {
	_ = os.Setenv("GIT_OPTIONAL_LOCKS", "0")
}

func fatalUsage(message string) {
	fmt.Fprintf(os.Stderr, "error: %s\n\n", message)
	usage()
}

func codexCommand(args []string) []string {
	command := []string{"codex"}
	return append(command, args...)
}

func codexVMOptionsForStatus(status vmStatusReport, args []string) (vmSessionOptions, bool) {
	if !status.Available {
		return vmSessionOptions{}, false
	}
	return vmSessionOptions{
		Command: codexCommand(args),
		Backend: status.Backend,
		Runner:  status.Runner,
		Quiet:   true,
	}, true
}

func runCodex(ctx context.Context, cwd, storeRoot string, args []string) (int, error) {
	status := detectVMStatus(cwd, storeRoot, vmBackendAuto, "")
	if opts, ok := codexVMOptionsForStatus(status, args); ok {
		code, err := runVMSession(ctx, cwd, storeRoot, opts)
		if err == nil || ctx.Err() != nil {
			return code, err
		}
	}
	return runScopedSession(ctx, cwd, storeRoot, sessionOptions{Command: codexCommand(args), Quiet: true})
}

func squireCodexBridgeEnv() []string {
	env := []string{"SQUIRE_CODEX_BRIDGE=1", "SQUIRE_SHIM_ENABLE_WARM_FILE_REPLAY=1"}
	if exe, err := os.Executable(); err == nil && exe != "" {
		env = append(env, "SQUIRE_CODEX_SQUIRE="+exe)
		if hotLib := squireHotLibraryNextTo(exe); hotLib != "" {
			env = append(env, "SQUIRE_CODEX_HOT_LIB="+hotLib)
		}
	}
	return env
}

func squireHotLibraryNextTo(exe string) string {
	name := "libsquire_hot.so"
	if runtime.GOOS == "darwin" {
		name = "libsquire_hot.dylib"
	}
	dir := filepath.Dir(exe)
	for _, candidate := range []string{
		filepath.Join(dir, name),
		filepath.Join(dir, "lib", name),
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

func kernelUsageError(args []string) string {
	if len(args) == 0 {
		return `missing kernel subcommand (try "squire help kernel")`
	}
	switch args[0] {
	case "status":
		if len(args) > 1 {
			return fmt.Sprintf("unknown kernel status option %q (valid: --short)", args[1])
		}
	case "run":
		return "squire kernel run requires -- before the command"
	case "warm":
		if len(args) > 1 {
			return fmt.Sprintf("squire kernel warm does not accept option %q", args[1])
		}
	case "maintain":
		return `invalid kernel maintain usage (try "squire help kernel maintain")`
	case "prewarm-adjacent":
		return "squire kernel prewarm-adjacent requires -- before the command"
	case "adapter":
		return `invalid kernel adapter usage (try "squire help kernel adapter")`
	case "shim-helper":
		return `invalid kernel shim-helper usage (try "squire help kernel shim-helper")`
	default:
		return fmt.Sprintf(`unknown kernel subcommand %q (try "squire help kernel")`, args[0])
	}
	return `invalid kernel command (try "squire help kernel")`
}

func boostUsageError(args []string) string {
	if len(args) == 0 {
		return `missing boost subcommand (try "squire help boost")`
	}
	switch args[0] {
	case "status":
		if len(args) > 1 {
			return fmt.Sprintf("squire boost status does not accept option %q", args[1])
		}
	case "bench":
		if len(args) == 1 {
			return "missing boost bench target (valid: repo-metadata, deep-local)"
		}
		return fmt.Sprintf("unknown boost bench target %q (valid: repo-metadata, deep-local)", args[1])
	default:
		return fmt.Sprintf(`unknown boost subcommand %q (try "squire help boost")`, args[0])
	}
	return `invalid boost command (try "squire help boost")`
}

func usageText() string {
	return `Squire v1

usage:
  squire-codex [codex args...]
  squire codex [codex args...]
  squire boost status --short

advanced:
  squire setup
  squire session [--quiet] [--metadata-only|--no-warm] [--no-maintainer] [--enable-warm-file-replay] [--preload] [--preload-lib <path>] -- <command> [args...]
  squire vm status [--short|--json]
  squire vm session [--quiet] [--backend auto|linux-local|external-runner] [--runner <path>] -- <command> [args...]
  squire version [--short|--json]
  squire kernel status [--short]
  squire kernel run -- <command> [args...]
  squire kernel warm [--metadata-only] [--short|--json]
  squire kernel maintain --once [--short|--json]
  squire kernel maintain --duration <duration> [--poll-interval <duration>] [--short|--json]
  squire kernel maintain --background [--duration <duration>] [--poll-interval <duration>] [--short|--json]
  squire kernel maintain --background-status [--short|--json]
  squire kernel maintain --stop [--short|--json]
  squire kernel adapter --stdio [--no-maintainer]
  squire kernel shim-helper --socket <path>
  squire boost status [--short|--json]
  squire boost bench repo-metadata [--short|--json]
  squire boost bench deep-local [--short|--json]

principles:
  Agent chooses. Squire serves.
  Native fallback always exists.
  Runtime decisions are replay or native.
  Validation, edits, mutations, and package installs are never replayed.

first use:
  cd your-repo
  squire-codex

help:
  squire help codex
  squire help session
  squire help vm
  squire help version
  squire help kernel run
  squire help kernel maintain
  squire help kernel adapter
  squire help kernel shim-helper
  squire help boost
`
}

func helpTextForArgs(args []string) (string, bool) {
	switch {
	case len(args) == 1 && isHelpToken(args[0]):
		return usageText(), true
	case len(args) >= 1 && args[0] == "help":
		return helpTopic(args[1:]), true
	case len(args) >= 2 && !hasDelimiter(args) && isHelpToken(args[len(args)-1]):
		return helpTopic(args[:len(args)-1]), true
	default:
		return "", false
	}
}

func helpTopic(topic []string) string {
	if len(topic) == 0 {
		return usageText()
	}
	switch strings.Join(topic, " ") {
	case "codex":
		return `usage:
  squire-codex [codex args...]
  squire codex [codex args...]

squire-codex is the recommended UX and normal product path. It is the real
Codex fork built with the Squire execution bridge and does not require a
separate setup command.

squire codex is a compatibility and diagnostics wrapper around a separately
installed codex executable. It is useful for local experiments, but it is not
the primary release driver.

The model still sees and emits ordinary commands. Squire scopes local store
prep, warm state, the resident maintainer, hot snapshot access, exact replay,
and native fallback below Codex. It does not add tools, alter prompts, suggest
commands, stop for VM provisioning, or require Codex to call Squire.

Examples:
  squire-codex
  squire codex
  squire codex exec "Explain this repo"
  squire codex --skip-git-repo-check exec "Print OK"
`
	case "setup":
		return `usage:
  squire setup

Advanced preflight/repair command. It is not required before squire codex.
Initializes the local Squire store, prints privacy mode, and does not
install global command shims.
`
	case "session":
		return `usage:
  squire session [--quiet] [--metadata-only|--no-warm] [--no-maintainer] [--enable-warm-file-replay] [--preload] [--preload-lib <path>] -- <command> [args...]

Starts a scoped Squire session around an ordinary shell or agent command. Squire
starts or reuses the resident maintainer and warms local proofs. By default it
prefers a scoped preload library when the launcher is safe and the library is
available, tries the same mmap proof before selected exec calls, and falls
through to native exec on any miss. For known unsafe launchers, or when preload
is unavailable, the session runs native with no command interception. The
command and anything it launches still emits ordinary commands such as git,
cat, sed, node, or python; the model never has to call Squire.

--preload requires the preload transport for any launcher and errors if the
local library is not available.

Misses, unsupported argv, invalid proofs, validation commands, edits,
mutations, and package setup all execute natively through exact fallback paths.
Tiny native file readers such as cat and sed are not replayed by default because
native is faster on small local files; --enable-warm-file-replay opts into
bounded preload coverage experiments.
`
	case "vm", "vm status", "vm session":
		return `usage:
  squire vm status [--short|--json]
  squire vm session [--quiet] [--backend auto|linux-local|external-runner] [--runner <path>] -- <command> [args...]

Runs Squire as an isolated Linux execution mode. On Linux hosts, vm session uses
the same scoped session path directly. On macOS, vm session runs the
Virtualization.framework Linux guest when SQUIRE_VM_HELPER, SQUIRE_VM_KERNEL,
and SQUIRE_VM_INITRD are configured, or falls back to an external runner when
SQUIRE_VM_RUNNER or --runner is provided to hand the session to a guest lifecycle
runner. Codex launches inside the guest use session-scoped guest-local mmap
shims for reliable child-command interception. This is the guest lifecycle runner
contract.

The model still emits ordinary commands. The guest runner contract receives:

  <runner> session --cwd <host-cwd> --store-root <store-root> -- <command> [args...]

The guest must run Squire inside the Linux environment and preserve the
same contract: exact stdout/stderr/exit-code or native fallback inside the
guest. Host-native and vm sessions are intentionally separate because Linux
guest execution does not preserve macOS-specific command semantics.
`
	case "version":
		return `usage:
  squire version [--short|--json]

Prints Squire build identity. Release builds can set version, commit,
and date with Go linker flags. Human-readable output is the default; use
--json for automation.
`
	case "kernel", "kernel status":
		return `usage:
  squire kernel status [--short]

Shows kernel readiness. The default output includes repo oracle state,
invalidation epochs, enabled fast paths, proof-gated replay candidates,
native-only discovery boundaries, prepared world counts, background maintainer
status, and latest benchmark status. Use --short for a compact readiness view.
`
	case "kernel run":
		return `usage:
  squire kernel run -- <command> [args...]

Runs an agent-chosen command through Squire. The "--" delimiter is
required so Squire options cannot be confused with the command being served.
On a valid proof, Squire returns exact stdout, stderr, and exit code. Otherwise
it runs the original command natively.
`
	case "kernel adapter":
		return `usage:
  squire kernel adapter --stdio [--no-maintainer]

Starts a long-lived terminal adapter process for host runtimes. The model still
emits ordinary commands; the terminal layer sends those already-chosen commands
to this adapter over JSON lines. Responses contain exact stdout/stderr bytes as
base64 plus the exact exit code. By default, the adapter starts or reuses the
resident background maintainer before serving requests. --no-maintainer is a
diagnostic escape hatch for measuring native-direct adapter overhead.

This is now a compatibility path for host runtimes that can already integrate
over stdio. The primary scoped foreground path is the preload session, which
reads the maintainer-published hot snapshot from inside the launched process
tree when preload can attach and falls back to native exec on any miss.
`
	case "kernel shim-helper":
		return `usage:
  squire kernel shim-helper --socket <path>

Experimental scoped-shim helper. It listens on a local Unix socket, accepts
already-chosen argv/cwd requests from temporary per-session shims, serves them
through the same adapter decision path, and returns exact stdout/stderr bytes
plus exit code. It is not a global shim installer and should be launched only
inside an explicit Squire-owned session.
`
	case "kernel warm":
		return `usage:
  squire kernel warm [--metadata-only] [--short|--json]

Prepares local read-only proofs and hot outputs for later agent-chosen
commands. It does not suggest commands, change prompts, mutate files, or skip
native fallback. JSON is the default output for automation; use --short for a
compact human-readable summary. Use --metadata-only to prepare only enabled
local metadata fast paths and their hot snapshot.
`
	case "kernel maintain":
		return `usage:
  squire kernel maintain --once [--short|--json]
  squire kernel maintain --duration <duration> [--poll-interval <duration>] [--short|--json]
  squire kernel maintain --background [--duration <duration>] [--poll-interval <duration>] [--short|--json]
  squire kernel maintain --background-status [--short|--json]
  squire kernel maintain --stop [--short|--json]

Runs or manages the resident maintainer. The background mode keeps warm state
fresh outside the foreground command-serving path. It is local, bounded, and
fault-open. JSON is the default output for automation; use --short for a compact
human-readable status.
`
	case "boost":
		return `usage:
  squire boost status [--short|--json]
  squire boost bench repo-metadata [--short|--json]
  squire boost bench deep-local [--short|--json]

Shows local acceleration counters and runs scoped benchmarks. Boost status
includes hot-client replay counts, Go-client/prepared-child/synthetic replay
breakdowns, and last event/replay Unix nanosecond timestamps so recent session
activity can be distinguished from older ledger data.
Benchmarks make no broad Codex speedup claim. Benchmark JSON is the default
output for automation; use --short for a compact human-readable summary. Boost
status is human-readable by default; use --json for automation.
`
	default:
		return usageText()
	}
}

func isHelpToken(s string) bool {
	return s == "--help" || s == "-h"
}

func hasDelimiter(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return true
		}
	}
	return false
}

func runKernelCommand(ctx context.Context, cwd, storeRoot string, args []string) {
	argv, err := commandAfterDelimiter("squire kernel run", args)
	if err != nil {
		fatalUsage(err.Error())
		os.Exit(2)
	}
	sessionID := os.Getenv("SQUIRE_KERNEL_SESSION_ID")
	if sessionID == "" {
		sessionID = "cli"
	}
	serveStart := time.Now()
	if res, ok := kernel.FastHotClientReplay(ctx, sessionID, cwd, storeRoot, argv); ok {
		finishKernelCommand(cwd, storeRoot, sessionID, argv, *res, serveStart)
	}
	k := kernel.New(storeRoot)
	res := k.Run(ctx, sessionID, cwd, argv)
	finishKernelCommand(cwd, storeRoot, sessionID, argv, res, time.Time{})
}

func finishKernelCommand(cwd, storeRoot, sessionID string, argv []string, res kernel.RunResult, replayStart time.Time) {
	_, _ = os.Stdout.Write(res.Stdout)
	_, _ = os.Stderr.Write(res.Stderr)
	var replayWall time.Duration
	if !replayStart.IsZero() {
		replayWall = time.Since(replayStart)
	}
	kernel.RecordHotClientResult(storeRoot, res, replayWall)
	if os.Getenv("SQUIRE_KERNEL_DEBUG_RESULT") == "1" {
		debug := struct {
			Mode         kernel.Mode           `json:"mode"`
			Family       kernel.OperatorFamily `json:"family"`
			Proof        string                `json:"proof,omitempty"`
			NativeWallMS int64                 `json:"native_wall_ms,omitempty"`
			Phases       kernel.PhaseTimings   `json:"phases"`
		}{
			Mode:         res.Mode,
			Family:       res.Family,
			NativeWallMS: res.Observation.NativeWallMS,
			Phases:       res.Phases,
		}
		if res.Proof != nil {
			debug.Proof = res.Proof.OperationKey
		}
		if b, err := json.Marshal(debug); err == nil {
			fmt.Fprintln(os.Stderr, "SQUIRE_KERNEL_RESULT "+string(b))
		}
	}
	startAdjacentPrewarmProcess(cwd, storeRoot, sessionID, argv)
	os.Exit(res.ExitCode)
}

func runAdjacentPrewarm(ctx context.Context, cwd, storeRoot string, args []string) error {
	argv, err := commandAfterDelimiter("squire kernel prewarm-adjacent", args)
	if err != nil {
		return err
	}
	sessionID := os.Getenv("SQUIRE_KERNEL_SESSION_ID")
	if sessionID == "" {
		sessionID = "cli"
	}
	prewarmCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err = kernel.New(storeRoot).PrewarmAdjacent(prewarmCtx, cwd, sessionID, argv)
	return err
}

func commandAfterDelimiter(command string, args []string) ([]string, error) {
	if len(args) == 0 || args[0] != "--" {
		return nil, fmt.Errorf("%s requires -- before the command", command)
	}
	if len(args) == 1 {
		return nil, fmt.Errorf("%s requires a command after --", command)
	}
	return args[1:], nil
}

func startAdjacentPrewarmProcess(cwd, storeRoot, sessionID string, argv []string) {
	if !kernel.HasAdaptivePrewarmCandidates(argv) {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	args := append([]string{"kernel", "prewarm-adjacent", "--"}, argv...)
	cmd := exec.Command(exe, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "SQUIRE_KERNEL_STORE_ROOT="+storeRoot, "SQUIRE_KERNEL_SESSION_ID="+sessionID, "GIT_OPTIONAL_LOCKS=0")
	if err := cmd.Start(); err != nil {
		return
	}
	_ = cmd.Process.Release()
}

func storeRootFor(cwd string) string {
	if root := os.Getenv("SQUIRE_KERNEL_STORE_ROOT"); root != "" {
		return root
	}
	if root, ok := kernel.FastStoreRoot(cwd); ok {
		return root
	}
	return kernel.DefaultStoreRoot(cwd)
}

func runMaintain(ctx context.Context, cwd, storeRoot string, args []string) (string, error) {
	args, format, err := splitOutputFormatFlag(args)
	if err != nil {
		return "", err
	}
	switch {
	case len(args) > 0 && args[0] == "--background":
		opts, err := parseBackgroundOptions(args[1:])
		if err != nil {
			return "", err
		}
		status, err := kernel.StartBackgroundMaintainer(ctx, cwd, storeRoot, opts)
		if err != nil {
			return "", err
		}
		return backgroundStatusOut(status, format), nil
	case len(args) == 1 && args[0] == "--background-status":
		status, err := kernel.LoadBackgroundMaintainerStatus(ctx, cwd, storeRoot)
		if err != nil {
			return "", err
		}
		return backgroundStatusOut(status, format), nil
	case len(args) == 1 && args[0] == "--stop":
		status, err := kernel.StopBackgroundMaintainer(ctx, cwd, storeRoot)
		if err != nil {
			return "", err
		}
		return backgroundStatusOut(status, format), nil
	default:
		opts, err := parseForegroundOptions(args)
		if err != nil {
			return "", err
		}
		report, err := kernel.Maintain(ctx, cwd, storeRoot, opts)
		if err != nil {
			return "", err
		}
		return maintainerReportOut(report, format), nil
	}
}

type outputFormat int

const (
	outputJSON outputFormat = iota
	outputShort
)

func splitOutputFormatFlag(args []string) ([]string, outputFormat, error) {
	format := outputJSON
	seenFormat := false
	out := make([]string, 0, len(args))
	for _, arg := range args {
		switch arg {
		case "--short":
			if seenFormat {
				return nil, outputJSON, fmt.Errorf("only one output format may be specified")
			}
			seenFormat = true
			format = outputShort
		case "--json":
			if seenFormat {
				return nil, outputJSON, fmt.Errorf("only one output format may be specified")
			}
			seenFormat = true
		default:
			out = append(out, arg)
		}
	}
	return out, format, nil
}

func outputFormatFromTrailingArgs(args []string) (outputFormat, error) {
	return outputFormatFromTrailingArgsDefault(args, outputJSON)
}

func outputFormatFromTrailingArgsDefault(args []string, def outputFormat) (outputFormat, error) {
	if len(args) == 0 {
		return def, nil
	}
	remaining, format, err := splitOutputFormatFlag(args)
	if err != nil {
		return outputJSON, err
	}
	if len(remaining) > 0 {
		return outputJSON, fmt.Errorf("unknown output option %q", remaining[0])
	}
	return format, nil
}

func parseWarmArgs(args []string) (outputFormat, bool, error) {
	remaining, format, err := splitOutputFormatFlag(args)
	if err != nil {
		return outputJSON, false, err
	}
	metadataOnly := false
	for _, arg := range remaining {
		switch arg {
		case "--metadata-only":
			if metadataOnly {
				return outputJSON, false, fmt.Errorf("squire kernel warm option %q specified more than once", arg)
			}
			metadataOnly = true
		default:
			return outputJSON, false, fmt.Errorf("unknown warm option %q", arg)
		}
	}
	return format, metadataOnly, nil
}

func parseForegroundOptions(args []string) (kernel.MaintainerOptions, error) {
	opts := kernel.DefaultMaintainerOptions()
	if len(args) == 0 {
		return opts, fmt.Errorf("usage: squire kernel maintain --once | --duration <duration> [--poll-interval <duration>]")
	}
	seenMode := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--once":
			if seenMode {
				return opts, fmt.Errorf("only one maintain mode may be specified")
			}
			opts.MaxCycles = 1
			seenMode = true
		case "--duration":
			if seenMode {
				return opts, fmt.Errorf("only one maintain mode may be specified")
			}
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--duration requires a value")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return opts, err
			}
			opts.MaxRuntime = d
			seenMode = true
			i++
		case "--poll-interval":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--poll-interval requires a value")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return opts, err
			}
			opts.PollInterval = d
			i++
		default:
			return opts, fmt.Errorf("unknown maintain option %q", args[i])
		}
	}
	if !seenMode {
		return opts, fmt.Errorf("usage: squire kernel maintain --once | --duration <duration> [--poll-interval <duration>]")
	}
	return opts, nil
}

func parseBackgroundOptions(args []string) (kernel.BackgroundMaintainerOptions, error) {
	opts := kernel.DefaultBackgroundMaintainerOptions()
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--duration":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--duration requires a value")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return opts, err
			}
			opts.Duration = d
			i++
		case "--poll-interval":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--poll-interval requires a value")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return opts, err
			}
			opts.PollInterval = d
			i++
		default:
			return opts, fmt.Errorf("unknown background maintain option %q", args[i])
		}
	}
	return opts, nil
}

func jsonOut(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b) + "\n"
}

func currentVersionReport() versionReport {
	return versionReport{
		Product:  "Squire",
		Contract: "v1",
		Version:  buildVersion,
		Commit:   buildCommit,
		Date:     buildDate,
	}
}

func versionOut(format outputFormat) string {
	report := currentVersionReport()
	if format == outputJSON {
		return jsonOut(report)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", report.Product, report.Contract)
	fmt.Fprintf(&b, "version: %s\n", report.Version)
	fmt.Fprintf(&b, "commit: %s\n", report.Commit)
	fmt.Fprintf(&b, "date: %s\n", report.Date)
	return b.String()
}

func warmReportOut(report kernel.WarmReport, format outputFormat) string {
	if format != outputShort {
		return jsonOut(report)
	}
	var b strings.Builder
	fmt.Fprintln(&b, "Squire warm")
	fmt.Fprintf(&b, "repo_oracle: %s\n", boolAvailability(report.OracleAvailable))
	if report.RepoRoot != "" {
		fmt.Fprintf(&b, "repo_root: %s\n", report.RepoRoot)
	}
	fmt.Fprintf(&b, "fast_path_prepared: %d\n", report.FastPathPrepared)
	fmt.Fprintf(&b, "proof_gated_prewarmed: %d\n", report.ProofGatedPrewarmed)
	fmt.Fprintf(&b, "warm_files_prepared: %d\n", report.WarmFilesPrepared)
	fmt.Fprintf(&b, "file_tree_indexes_prepared: %d\n", report.FileTreeIndexesPrepared)
	fmt.Fprintf(&b, "project_metadata_prepared: %d\n", report.ProjectMetadataPrepared)
	fmt.Fprintf(&b, "command_path_prepared: %d\n", report.CommandPathPrepared)
	fmt.Fprintf(&b, "ecosystem_prepared: %d\n", report.EcosystemPrepared)
	fmt.Fprintf(&b, "dependency_metadata_prepared: %d\n", report.DependencyPrepared)
	fmt.Fprintf(&b, "source_symbol_indexes_prepared: %d\n", report.SourceSymbolPrepared)
	fmt.Fprintf(&b, "prepared_entries: %d\n", len(report.Prepared))
	fmt.Fprintf(&b, "privacy_mode: %s\n", report.PrivacyMode)
	fmt.Fprintf(&b, "replay_set_unchanged: %t\n", report.ReplaySetUnchanged)
	fmt.Fprintf(&b, "agent_visible_suggestions: %t\n", report.AgentVisibleSuggestions)
	for _, note := range report.Notes {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	return b.String()
}

func boostStatusOut(report kernel.BoostStatusReport, format outputFormat) string {
	if format == outputJSON {
		return jsonOut(report)
	}
	return kernel.FormatBoostStatusReport(report)
}

func repoMetadataBenchOut(report kernel.BenchReport, format outputFormat) string {
	if format != outputShort {
		return jsonOut(report)
	}
	var b strings.Builder
	fmt.Fprintln(&b, "Squire repo-metadata benchmark")
	fmt.Fprintf(&b, "exactness: %t\n", report.Exactness)
	fmt.Fprintf(&b, "mismatches: %d\n", report.Mismatches)
	fmt.Fprintf(&b, "mutation_boundary_invalidation: %t\n", report.MutationBoundaryInvalidation)
	fmt.Fprintf(&b, "workload_only_wall_delta_ms: %d\n", report.WorkloadOnlyWallDeltaMS)
	fmt.Fprintf(&b, "net_roi_ms: %d\n", report.NetROIMS)
	fmt.Fprintf(&b, "quarantined_runs: %d\n", report.QuarantinedRuns)
	fmt.Fprintf(&b, "no_broad_codex_speedup_claim: %t\n", report.NoBroadCodexSpeedupClaim)
	fmt.Fprintln(&b, "commands:")
	for _, command := range report.Commands {
		fmt.Fprintf(&b, "  - %s\n", command)
	}
	return b.String()
}

func deepLocalBenchOut(report kernel.DeepBenchReport, format outputFormat) string {
	if format != outputShort {
		return jsonOut(report)
	}
	var b strings.Builder
	fmt.Fprintln(&b, "Squire deep-local benchmark")
	fmt.Fprintf(&b, "safety_gates: %s\n", gateStatusForCLI(report.SafetyGates))
	fmt.Fprintf(&b, "performance_gates: %s\n", gateStatusForCLI(report.PerformanceGates))
	fmt.Fprintf(&b, "enabled_fast_path_exactness: %t\n", report.EnabledFastPathExactness)
	fmt.Fprintf(&b, "enabled_fast_path_mismatches: %d\n", report.EnabledFastPathMismatches)
	fmt.Fprintf(&b, "native_only_candidate_exactness: %t\n", report.NativeOnlyCandidateExactness)
	fmt.Fprintf(&b, "native_only_candidate_mismatches: %d\n", report.NativeOnlyCandidateMismatches)
	fmt.Fprintf(&b, "stale_replay_observed: %t\n", report.StaleReplayObserved)
	fmt.Fprintf(&b, "validation_replays: %d\n", report.NeverReplayDiagnostics.ValidationReplays)
	fmt.Fprintf(&b, "metadata_fast_path_p95_us: %d\n", report.Performance.MetadataFastPathP95US)
	fmt.Fprintf(&b, "proof_gated_replay_p95_us: %d\n", report.Performance.ProofGatedReplayP95US)
	fmt.Fprintf(&b, "native_fallback_overhead_p95_us: %d\n", report.Performance.NativeFallbackOverheadP95US)
	fmt.Fprintf(&b, "native_only_bookkeeping_p95_us: %d\n", report.Performance.NativeOnlyBookkeepingP95US)
	fmt.Fprintf(&b, "no_broad_codex_speedup_claim: %t\n", report.NoBroadCodexSpeedupClaim)
	for _, violation := range report.SafetyGates.Violations {
		fmt.Fprintf(&b, "safety_violation: %s\n", violation)
	}
	for _, violation := range report.PerformanceGates.Violations {
		fmt.Fprintf(&b, "performance_violation: %s\n", violation)
	}
	return b.String()
}

func gateStatusForCLI(gate kernel.GateReport) string {
	if gate.Status != "" {
		return gate.Status
	}
	if gate.Passed {
		return "pass"
	}
	if gate.Required {
		return "fail"
	}
	return "n/a"
}

func backgroundStatusOut(status kernel.BackgroundMaintainerStatus, format outputFormat) string {
	if format == outputShort {
		return formatBackgroundStatusShort(status)
	}
	return jsonOut(status)
}

func maintainerReportOut(report kernel.MaintainerReport, format outputFormat) string {
	if format == outputShort {
		return formatMaintainerReportShort(report)
	}
	return jsonOut(report)
}

func formatBackgroundStatusShort(status kernel.BackgroundMaintainerStatus) string {
	state := "stopped"
	switch {
	case status.AlreadyRunning:
		state = "already_running"
	case status.StopRequested && status.Running:
		state = "stop_failed"
	case status.StopRequested && !status.Running:
		state = "stopped"
	case status.Started:
		state = "started"
	case status.Running:
		state = "running"
	}
	var b strings.Builder
	fmt.Fprintln(&b, "Squire maintainer")
	fmt.Fprintf(&b, "status: %s\n", state)
	fmt.Fprintf(&b, "running: %t\n", status.Running)
	if status.PID > 0 {
		fmt.Fprintf(&b, "pid: %d\n", status.PID)
	}
	if status.RepoRoot != "" {
		fmt.Fprintf(&b, "repo_root: %s\n", status.RepoRoot)
	}
	fmt.Fprintf(&b, "store: %s\n", status.StoreRoot)
	if status.Duration != "" {
		fmt.Fprintf(&b, "duration: %s\n", status.Duration)
	}
	if status.PollInterval != "" {
		fmt.Fprintf(&b, "poll_interval: %s\n", status.PollInterval)
	}
	fmt.Fprintf(&b, "native_fallback: %t\n", status.NativeFallbackAvailable)
	fmt.Fprintf(&b, "agent_visible_suggestions: %t\n", status.AgentVisibleSuggestions)
	if status.HotCacheSocket != "" {
		fmt.Fprintf(&b, "hot_cache_socket: %s\n", status.HotCacheSocket)
	}
	if status.LogPath != "" {
		fmt.Fprintf(&b, "log_path: %s\n", status.LogPath)
	}
	fmt.Fprintf(&b, "status_path: %s\n", status.StatusPath)
	for _, diagnostic := range status.Diagnostics {
		fmt.Fprintf(&b, "diagnostic: %s\n", diagnostic)
	}
	return b.String()
}

func formatMaintainerReportShort(report kernel.MaintainerReport) string {
	var b strings.Builder
	fmt.Fprintln(&b, "Squire maintainer")
	fmt.Fprintf(&b, "mode: %s\n", report.Mode)
	fmt.Fprintf(&b, "repo_oracle: %s\n", boolAvailability(report.OracleAvailable))
	if report.RepoRoot != "" {
		fmt.Fprintf(&b, "repo_root: %s\n", report.RepoRoot)
	}
	fmt.Fprintf(&b, "poll_cycles: %d\n", report.PollCycles)
	fmt.Fprintf(&b, "warm_cycles: %d\n", report.WarmCycles)
	fmt.Fprintf(&b, "invalidations_observed: %d\n", report.InvalidationsObserved)
	fmt.Fprintf(&b, "fast_path_prepared: %d\n", report.FastPathPrepared)
	fmt.Fprintf(&b, "proof_gated_prewarmed: %d\n", report.ProofGatedPrewarmed)
	fmt.Fprintf(&b, "prepared_entries_observed: %d\n", report.PreparedEntriesObserved)
	fmt.Fprintf(&b, "native_fallback: %t\n", report.NativeFallbackAvailable)
	fmt.Fprintf(&b, "agent_visible_suggestions: %t\n", report.AgentVisibleSuggestions)
	if report.HotCacheSocket != "" {
		fmt.Fprintf(&b, "hot_cache_socket: %s\n", report.HotCacheSocket)
	}
	for _, diagnostic := range report.Diagnostics {
		fmt.Fprintf(&b, "diagnostic: %s\n", diagnostic)
	}
	return b.String()
}

func boolAvailability(ok bool) string {
	if ok {
		return "available"
	}
	return "unavailable"
}
