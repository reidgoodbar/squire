package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"squire.run/kernel/internal/kernel"
)

func main() {
	ctx := context.Background()
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
		out, err = kernel.Setup(ctx, cwd, storeRoot)
	case len(args) >= 3 && args[0] == "kernel" && args[1] == "prewarm-adjacent":
		err = runAdjacentPrewarm(ctx, cwd, storeRoot, args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	case len(args) >= 3 && args[0] == "kernel" && args[1] == "run":
		runKernelCommand(ctx, cwd, storeRoot, args[2:])
		return
	case len(args) == 2 && args[0] == "kernel" && args[1] == "status":
		out, err = kernel.KernelStatus(ctx, cwd, storeRoot)
	case len(args) == 3 && args[0] == "kernel" && args[1] == "status" && args[2] == "--short":
		out, err = kernel.KernelStatusSummary(ctx, cwd, storeRoot)
	case len(args) >= 2 && args[0] == "kernel" && args[1] == "warm":
		var format outputFormat
		format, err = outputFormatFromTrailingArgs(args[2:])
		if err != nil {
			break
		}
		var report kernel.WarmReport
		report, err = kernel.Warm(ctx, cwd, storeRoot)
		if err == nil {
			out = warmReportOut(report, format)
		}
	case len(args) >= 2 && args[0] == "kernel" && args[1] == "maintain":
		out, err = runMaintain(ctx, cwd, storeRoot, args[2:])
	case len(args) == 2 && args[0] == "boost" && args[1] == "status":
		out, err = kernel.BoostStatus(ctx, cwd, storeRoot)
	case len(args) >= 3 && args[0] == "boost" && args[1] == "bench" && args[2] == "repo-metadata":
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

func fatalUsage(message string) {
	fmt.Fprintf(os.Stderr, "error: %s\n\n", message)
	usage()
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
	return `Squire Kernel v1

usage:
  squire setup
  squire kernel status [--short]
  squire kernel run -- <command> [args...]
  squire kernel warm [--short|--json]
  squire kernel maintain --once [--short|--json]
  squire kernel maintain --duration <duration> [--poll-interval <duration>] [--short|--json]
  squire kernel maintain --background [--duration <duration>] [--poll-interval <duration>] [--short|--json]
  squire kernel maintain --background-status [--short|--json]
  squire kernel maintain --stop [--short|--json]
  squire boost status
  squire boost bench repo-metadata [--short|--json]
  squire boost bench deep-local [--short|--json]

principles:
  Agent chooses. Squire serves.
  Native fallback always exists.
  Runtime decisions are replay or native.
  Validation, edits, mutations, and package installs are never replayed.

first use:
  squire setup
  squire kernel maintain --background
  squire kernel warm
  squire kernel run -- git rev-parse HEAD
  squire kernel status

help:
  squire help kernel run
  squire help kernel maintain
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
	case "setup":
		return `usage:
  squire setup

Initializes the local Squire Kernel store, prints privacy mode, and does not
install global command shims.
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

Runs an agent-chosen command through Squire Kernel. The "--" delimiter is
required so Squire options cannot be confused with the command being served.
On a valid proof, Squire returns exact stdout, stderr, and exit code. Otherwise
it runs the original command natively.
`
	case "kernel warm":
		return `usage:
  squire kernel warm [--short|--json]

Prepares local read-only proofs and hot outputs for later agent-chosen
commands. It does not suggest commands, change prompts, mutate files, or skip
native fallback. JSON is the default output for automation; use --short for a
compact human-readable summary.
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
  squire boost status
  squire boost bench repo-metadata [--short|--json]
  squire boost bench deep-local [--short|--json]

Shows local acceleration counters and runs scoped benchmarks. Benchmarks make
no broad Codex speedup claim. JSON is the default output for automation; use
--short for a compact human-readable summary.
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
	if res, ok := kernel.FastHotClientReplay(ctx, sessionID, cwd, storeRoot, argv); ok {
		finishKernelCommand(cwd, storeRoot, sessionID, argv, *res)
	}
	k := kernel.New(storeRoot)
	res := k.Run(ctx, sessionID, cwd, argv)
	finishKernelCommand(cwd, storeRoot, sessionID, argv, res)
}

func finishKernelCommand(cwd, storeRoot, sessionID string, argv []string, res kernel.RunResult) {
	_, _ = os.Stdout.Write(res.Stdout)
	_, _ = os.Stderr.Write(res.Stderr)
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
	cmd.Env = append(os.Environ(), "SQUIRE_KERNEL_STORE_ROOT="+storeRoot, "SQUIRE_KERNEL_SESSION_ID="+sessionID)
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
	remaining, format, err := splitOutputFormatFlag(args)
	if err != nil {
		return outputJSON, err
	}
	if len(remaining) > 0 {
		return outputJSON, fmt.Errorf("unknown output option %q", remaining[0])
	}
	return format, nil
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

func warmReportOut(report kernel.WarmReport, format outputFormat) string {
	if format != outputShort {
		return jsonOut(report)
	}
	var b strings.Builder
	fmt.Fprintln(&b, "Squire Kernel warm")
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

func repoMetadataBenchOut(report kernel.BenchReport, format outputFormat) string {
	if format != outputShort {
		return jsonOut(report)
	}
	var b strings.Builder
	fmt.Fprintln(&b, "Squire Kernel repo-metadata benchmark")
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
	fmt.Fprintln(&b, "Squire Kernel deep-local benchmark")
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
	case status.Started:
		state = "started"
	case status.Running:
		state = "running"
	case status.StopRequested && !status.Running:
		state = "stopped"
	}
	var b strings.Builder
	fmt.Fprintln(&b, "Squire Kernel maintainer")
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
	fmt.Fprintln(&b, "Squire Kernel maintainer")
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
