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
	case len(args) == 2 && args[0] == "kernel" && args[1] == "warm":
		var report kernel.WarmReport
		report, err = kernel.Warm(ctx, cwd, storeRoot)
		if err == nil {
			b, _ := json.MarshalIndent(report, "", "  ")
			out = string(b) + "\n"
		}
	case len(args) >= 2 && args[0] == "kernel" && args[1] == "maintain":
		out, err = runMaintain(ctx, cwd, storeRoot, args[2:])
	case len(args) == 2 && args[0] == "boost" && args[1] == "status":
		out, err = kernel.BoostStatus(ctx, cwd, storeRoot)
	case len(args) == 3 && args[0] == "boost" && args[1] == "bench" && args[2] == "repo-metadata":
		var report kernel.BenchReport
		report, err = kernel.BenchRepoMetadata(ctx)
		if err == nil {
			err = kernel.NewLedgerStore(storeRoot).SaveLatestBenchmarkStatus(kernel.LatestBenchmarkFromRepoMetadata(report))
		}
		if err == nil {
			b, _ := json.MarshalIndent(report, "", "  ")
			out = string(b) + "\n"
		}
	case len(args) == 3 && args[0] == "boost" && args[1] == "bench" && args[2] == "deep-local":
		var report kernel.DeepBenchReport
		report, err = kernel.BenchDeepLocal(ctx)
		if err == nil {
			err = kernel.NewLedgerStore(storeRoot).SaveLatestBenchmarkStatus(kernel.LatestBenchmarkFromDeepLocal(report))
		}
		if err == nil {
			b, _ := json.MarshalIndent(report, "", "  ")
			out = string(b) + "\n"
		}
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

func usageText() string {
	return `Squire Kernel v1

usage:
  squire setup
  squire kernel status [--short]
  squire kernel run -- <command> [args...]
  squire kernel warm
  squire kernel maintain --once
  squire kernel maintain --duration <duration>
  squire kernel maintain --background [--duration <duration>] [--poll-interval <duration>]
  squire kernel maintain --background-status
  squire kernel maintain --stop
  squire boost status
  squire boost bench repo-metadata
  squire boost bench deep-local

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
  squire kernel warm

Prepares local read-only proofs and hot outputs for later agent-chosen
commands. It does not suggest commands, change prompts, mutate files, or skip
native fallback.
`
	case "kernel maintain":
		return `usage:
  squire kernel maintain --once
  squire kernel maintain --duration <duration>
  squire kernel maintain --background [--duration <duration>] [--poll-interval <duration>]
  squire kernel maintain --background-status
  squire kernel maintain --stop

Runs or manages the resident maintainer. The background mode keeps warm state
fresh outside the foreground command-serving path. It is local, bounded, and
fault-open.
`
	case "boost":
		return `usage:
  squire boost status
  squire boost bench repo-metadata
  squire boost bench deep-local

Shows local acceleration counters and runs scoped benchmarks. Benchmarks make
no broad Codex speedup claim.
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
		return jsonOut(status), nil
	case len(args) == 1 && args[0] == "--background-status":
		status, err := kernel.LoadBackgroundMaintainerStatus(ctx, cwd, storeRoot)
		if err != nil {
			return "", err
		}
		return jsonOut(status), nil
	case len(args) == 1 && args[0] == "--stop":
		status, err := kernel.StopBackgroundMaintainer(ctx, cwd, storeRoot)
		if err != nil {
			return "", err
		}
		return jsonOut(status), nil
	default:
		opts, err := parseForegroundOptions(args)
		if err != nil {
			return "", err
		}
		report, err := kernel.Maintain(ctx, cwd, storeRoot, opts)
		if err != nil {
			return "", err
		}
		return jsonOut(report), nil
	}
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
