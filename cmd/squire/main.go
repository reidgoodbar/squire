package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	if len(args) == 0 {
		usage()
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
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(out)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  squire setup")
	fmt.Fprintln(os.Stderr, "  squire kernel status")
	fmt.Fprintln(os.Stderr, "  squire kernel run -- <command> [args...]")
	fmt.Fprintln(os.Stderr, "  squire kernel warm")
	fmt.Fprintln(os.Stderr, "  squire kernel maintain --once")
	fmt.Fprintln(os.Stderr, "  squire kernel maintain --duration <duration>")
	fmt.Fprintln(os.Stderr, "  squire kernel maintain --background [--duration <duration>] [--poll-interval <duration>]")
	fmt.Fprintln(os.Stderr, "  squire kernel maintain --background-status")
	fmt.Fprintln(os.Stderr, "  squire kernel maintain --stop")
	fmt.Fprintln(os.Stderr, "  squire boost status")
	fmt.Fprintln(os.Stderr, "  squire boost bench repo-metadata")
	fmt.Fprintln(os.Stderr, "  squire boost bench deep-local")
}

func runKernelCommand(ctx context.Context, cwd, storeRoot string, args []string) {
	if len(args) < 2 || args[0] != "--" {
		usage()
		os.Exit(2)
	}
	sessionID := os.Getenv("SQUIRE_KERNEL_SESSION_ID")
	if sessionID == "" {
		sessionID = "cli"
	}
	if res, ok := kernel.FastHotClientReplay(ctx, sessionID, cwd, storeRoot, args[1:]); ok {
		finishKernelCommand(cwd, storeRoot, sessionID, args[1:], *res)
	}
	k := kernel.New(storeRoot)
	res := k.Run(ctx, sessionID, cwd, args[1:])
	finishKernelCommand(cwd, storeRoot, sessionID, args[1:], res)
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
	if len(args) < 2 || args[0] != "--" {
		return fmt.Errorf("usage: squire kernel prewarm-adjacent -- <command> [args...]")
	}
	sessionID := os.Getenv("SQUIRE_KERNEL_SESSION_ID")
	if sessionID == "" {
		sessionID = "cli"
	}
	prewarmCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := kernel.New(storeRoot).PrewarmAdjacent(prewarmCtx, cwd, sessionID, args[1:])
	return err
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
