package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func Setup(ctx context.Context, cwd, storeRoot string) (string, error) {
	k := New(storeRoot)
	if err := k.Store.Init(); err != nil {
		return "", err
	}
	ws := k.Oracle.Snapshot(ctx, cwd)
	var b strings.Builder
	fmt.Fprintln(&b, "Squire Kernel setup complete")
	fmt.Fprintln(&b, "privacy_mode: standard")
	fmt.Fprintf(&b, "store: %s\n", storeRoot)
	fmt.Fprintf(&b, "repo_oracle: %s\n", availability(ws.OracleAvailable))
	fmt.Fprintf(&b, "repo_root: %s\n", ws.RepoRoot)
	fmt.Fprintln(&b, "global_shims: not installed")
	return b.String(), nil
}

func KernelStatus(ctx context.Context, cwd, storeRoot string) (string, error) {
	k := New(storeRoot)
	_ = k.Store.Init()
	ws := k.Oracle.Snapshot(ctx, cwd)
	js, _ := json.MarshalIndent(ws, "", "  ")
	var b strings.Builder
	fmt.Fprintln(&b, "Squire Kernel status")
	fmt.Fprintln(&b, "repo_oracle:", availability(ws.OracleAvailable))
	fmt.Fprintln(&b, "world_state:")
	fmt.Fprintln(&b, indent(string(js)))
	fmt.Fprintln(&b, "enabled_fast_paths:")
	for _, item := range EnabledFastPaths() {
		fmt.Fprintln(&b, "  -", item)
	}
	fmt.Fprintln(&b, "shadow_candidates:")
	for _, item := range ShadowCandidates() {
		fmt.Fprintln(&b, "  -", item)
	}
	fmt.Fprintln(&b, "never_replay_policy:")
	for _, item := range NeverReplayPolicy() {
		fmt.Fprintln(&b, "  -", item)
	}
	return b.String(), nil
}

func BoostStatus(ctx context.Context, cwd, storeRoot string) (string, error) {
	_ = ctx
	_ = cwd
	store := NewLedgerStore(storeRoot)
	_ = store.Init()
	ledger, err := store.Load()
	if err != nil {
		return "", err
	}
	var replacements, fallbacks, mismatches int
	var roi []int64
	for _, e := range ledger.Entries {
		replacements += e.ReplacementCount
		fallbacks += e.FallbackCount
		mismatches += e.ShadowMismatchCount
		roi = append(roi, e.NetROIHistoryMS...)
	}
	var b strings.Builder
	fmt.Fprintln(&b, "Squire boost status")
	fmt.Fprintln(&b, "enabled_accelerators:")
	for _, item := range EnabledFastPaths() {
		fmt.Fprintln(&b, "  -", item)
	}
	fmt.Fprintf(&b, "replacements: %d\n", replacements)
	fmt.Fprintf(&b, "fallbacks: %d\n", fallbacks)
	fmt.Fprintf(&b, "mismatches: %d\n", mismatches)
	fmt.Fprintf(&b, "invalidations: derived from epoch mismatch\n")
	fmt.Fprintf(&b, "roi_history_ms: %v\n", roi)
	return b.String(), nil
}

func ShadowStatus(ctx context.Context, cwd, storeRoot string) (string, error) {
	k := New(storeRoot)
	_ = k.Store.Init()
	ws := k.Oracle.Snapshot(ctx, cwd)
	ledger, err := k.Store.Load()
	if err != nil {
		return "", err
	}
	var matches, mismatches int
	var examples []string
	for _, e := range ledger.Entries {
		matches += e.ShadowMatchCount
		mismatches += e.ShadowMismatchCount
		examples = append(examples, e.MismatchExamples...)
	}
	total := matches + mismatches
	rate := "n/a"
	if total > 0 {
		rate = fmt.Sprintf("%.2f", float64(matches)/float64(total))
	}
	var b strings.Builder
	fmt.Fprintln(&b, "Squire shadow status")
	fmt.Fprintln(&b, "shadow_candidates:")
	for _, item := range ShadowCandidates() {
		fmt.Fprintln(&b, "  -", item)
	}
	fmt.Fprintf(&b, "exactness_rate: %s\n", rate)
	fmt.Fprintf(&b, "matches: %d\n", matches)
	fmt.Fprintf(&b, "mismatches: %d\n", mismatches)
	fmt.Fprintln(&b, "mismatch_examples:")
	for _, ex := range examples {
		fmt.Fprintln(&b, "  -", ex)
	}
	if !ws.OracleAvailable {
		fmt.Fprintln(&b, "disabled_reasons:")
		fmt.Fprintln(&b, "  - repo oracle unavailable")
	}
	return b.String(), nil
}

func availability(ok bool) string {
	if ok {
		return "available"
	}
	return "unavailable"
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n")
}

type BenchReport struct {
	Runs                         int      `json:"runs"`
	Exactness                    bool     `json:"exactness"`
	MutationBoundaryInvalidation bool     `json:"mutation_boundary_invalidation"`
	WorkloadOnlyWallDeltaMS      int64    `json:"workload_only_wall_delta_ms"`
	NetROIMS                     int64    `json:"net_roi_ms"`
	Mismatches                   int      `json:"mismatches"`
	QuarantinedRuns              int      `json:"quarantined_runs"`
	NoBroadCodexSpeedupClaim     bool     `json:"no_broad_codex_speedup_claim"`
	Commands                     []string `json:"commands"`
}

func BenchRepoMetadata(ctx context.Context) (BenchReport, error) {
	dir, cleanup, err := makeBenchRepo(ctx)
	if err != nil {
		return BenchReport{}, err
	}
	defer cleanup()
	k := New(DefaultStoreRoot(dir))
	cmds := [][]string{
		{"git", "rev-parse", "HEAD"},
		{"git", "rev-parse", "--git-dir"},
		{"git", "rev-parse", "--abbrev-ref", "HEAD"},
	}
	report := BenchReport{Runs: 5, Exactness: true, NoBroadCodexSpeedupClaim: true}
	for _, argv := range cmds {
		report.Commands = append(report.Commands, displayCommand(argv))
		first := k.Run(ctx, "bench", dir, argv)
		if first.ExitCode != 0 {
			report.Exactness = false
			report.Mismatches++
			continue
		}
		for i := 0; i < report.Runs; i++ {
			nativeStart := time.Now()
			native := runNative(ctx, dir, argv)
			nativeWall := time.Since(nativeStart)
			replayStart := time.Now()
			replay := k.Run(ctx, "bench", dir, argv)
			replayWall := time.Since(replayStart)
			report.WorkloadOnlyWallDeltaMS += nativeWall.Milliseconds() - replayWall.Milliseconds()
			report.NetROIMS += int64(first.NativeWall.Milliseconds()) - replayWall.Milliseconds()
			if replay.Mode != ModeReplay || replay.ExitCode != native.ExitCode || string(replay.Stdout) != string(native.Stdout) || string(replay.Stderr) != string(native.Stderr) {
				report.Exactness = false
				report.Mismatches++
			}
		}
	}
	oldHead := strings.TrimSpace(string(k.Run(ctx, "bench", dir, []string{"git", "rev-parse", "HEAD"}).Stdout))
	if err := benchCommit(ctx, dir, "second"); err != nil {
		return report, err
	}
	after := k.Run(ctx, "bench", dir, []string{"git", "rev-parse", "HEAD"})
	newHead := strings.TrimSpace(string(after.Stdout))
	report.MutationBoundaryInvalidation = oldHead != "" && newHead != "" && oldHead != newHead && after.Mode != ModeReplay
	return report, nil
}
