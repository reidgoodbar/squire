package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const scopedKernelClaim = "Scoped kernel proof for repeated local Git metadata plus hot-prepared read-only discovery operations."

type LatestBenchmarkStatus struct {
	Name                         string     `json:"name"`
	Claim                        string     `json:"claim"`
	RanAt                        time.Time  `json:"ran_at"`
	Exactness                    bool       `json:"exactness"`
	Mismatches                   int        `json:"mismatches"`
	MutationBoundaryInvalidation bool       `json:"mutation_boundary_invalidation,omitempty"`
	MetadataFastPathP95US        int64      `json:"metadata_fast_path_p95_us,omitempty"`
	ProofGatedReplayP95US        int64      `json:"proof_gated_replay_p95_us,omitempty"`
	NativeFallbackOverheadP95US  int64      `json:"native_fallback_overhead_p95_us,omitempty"`
	NativeOnlyBookkeepingP95US   int64      `json:"native_only_bookkeeping_p95_us,omitempty"`
	ShadowBookkeepingP95US       int64      `json:"shadow_bookkeeping_p95_us,omitempty"`
	SafetyGates                  GateReport `json:"safety_gates,omitempty"`
	PerformanceGates             GateReport `json:"performance_gates,omitempty"`
	StaleReplayObserved          bool       `json:"stale_replay_observed"`
	ValidationReplays            int        `json:"validation_replays"`
}

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
	fmt.Fprintln(&b, "next: squire kernel maintain --background")
	fmt.Fprintln(&b, "try: squire kernel run -- git rev-parse HEAD")
	return b.String(), nil
}

func KernelStatus(ctx context.Context, cwd, storeRoot string) (string, error) {
	k := New(storeRoot)
	_ = k.Store.Init()
	ws := k.Oracle.Snapshot(ctx, cwd)
	latest, latestOK := k.Store.LoadLatestBenchmarkStatus()
	ledger, ledgerErr := k.Store.Load()
	ledgerOK := ledgerErr == nil
	snapshot := HotSnapshotStatsForStore(storeRoot)
	background, backgroundErr := LoadBackgroundMaintainerStatus(ctx, cwd, storeRoot)
	js, _ := json.MarshalIndent(ws, "", "  ")
	var b strings.Builder
	fmt.Fprintln(&b, "Squire Kernel status")
	fmt.Fprintln(&b, "claim:", scopedKernelClaim)
	fmt.Fprintln(&b, "readiness:")
	fmt.Fprintf(&b, "  status: %s\n", readinessStatus(ws, snapshot, background, backgroundErr))
	fmt.Fprintln(&b, "  native_fallback: available")
	fmt.Fprintln(&b, "  runtime_decisions: replay_or_native")
	fmt.Fprintln(&b, "  agent_visible_suggestions: false")
	fmt.Fprintln(&b, "  background_hint: squire kernel maintain --background")
	fmt.Fprintln(&b, "repo_oracle:", availability(ws.OracleAvailable))
	fmt.Fprintln(&b, "current_repo_oracle_state:")
	fmt.Fprintln(&b, indent(string(js)))
	fmt.Fprintln(&b, "invalidation_epoch:")
	fmt.Fprintf(&b, "  head: %s\n", emptyAsNA(ws.HeadEpoch))
	fmt.Fprintf(&b, "  config: %s\n", emptyAsNA(ws.ConfigEpoch))
	fmt.Fprintf(&b, "  index: %s\n", emptyAsNA(ws.IndexEpoch))
	fmt.Fprintf(&b, "  file_tree: %s\n", emptyAsNA(ws.FileTreeEpoch))
	fmt.Fprintf(&b, "  file_content: %s\n", emptyAsNA(ws.FileContentEpoch))
	fmt.Fprintf(&b, "  workspace: %s\n", emptyAsNA(ws.WorkspaceEpoch))
	fmt.Fprintln(&b, "enabled_fast_paths:")
	for _, item := range EnabledFastPaths() {
		fmt.Fprintln(&b, "  -", item)
	}
	fmt.Fprintln(&b, "proof_gated_replay_candidates:")
	for _, item := range ProofGatedReplayCandidates() {
		fmt.Fprintln(&b, "  -", item)
	}
	fmt.Fprintln(&b, "native_only_discovery:")
	fmt.Fprintln(&b, "  - git remote -v")
	fmt.Fprintln(&b, "  - git remote get-url origin")
	fmt.Fprintln(&b, "  - rg --files")
	fmt.Fprintln(&b, "  - rg <literal> <workspace paths...>")
	fmt.Fprintln(&b, "never_replay_boundaries:")
	for _, item := range NeverReplayPolicy() {
		fmt.Fprintln(&b, "  -", item)
	}
	fmt.Fprintln(&b, "p95_fast_path_overhead:")
	if latestOK && latest.MetadataFastPathP95US > 0 {
		fmt.Fprintf(&b, "  latest_metadata_fast_path_p95_us: %d\n", latest.MetadataFastPathP95US)
	} else {
		fmt.Fprintln(&b, "  latest_metadata_fast_path_p95_us: n/a")
	}
	if latestOK && latest.ProofGatedReplayP95US > 0 {
		fmt.Fprintf(&b, "  latest_proof_gated_replay_p95_us: %d\n", latest.ProofGatedReplayP95US)
	} else {
		fmt.Fprintln(&b, "  latest_proof_gated_replay_p95_us: n/a")
	}
	fmt.Fprintln(&b, "prepared_world:")
	if ledgerOK {
		fast, proofGated, warmFiles, indexes, metadata, pathIndex, ecosystem, dependency, sourceSymbols := preparedCounts(ledger.Prepared)
		fmt.Fprintf(&b, "  prepared_entries: %d\n", len(ledger.Prepared))
		fmt.Fprintf(&b, "  fast_path_outputs: %d\n", fast)
		fmt.Fprintf(&b, "  proof_gated_outputs: %d\n", proofGated)
		fmt.Fprintf(&b, "  warm_files: %d\n", warmFiles)
		fmt.Fprintf(&b, "  file_tree_indexes: %d\n", indexes)
		fmt.Fprintf(&b, "  project_metadata_fingerprints: %d\n", metadata)
		fmt.Fprintf(&b, "  command_path_indexes: %d\n", pathIndex)
		fmt.Fprintf(&b, "  ecosystem_metadata_fingerprints: %d\n", ecosystem)
		fmt.Fprintf(&b, "  dependency_metadata_fingerprints: %d\n", dependency)
		fmt.Fprintf(&b, "  source_symbol_indexes: %d\n", sourceSymbols)
	} else {
		fmt.Fprintln(&b, "  prepared_entries: n/a")
	}
	fmt.Fprintln(&b, "virtual_workspace_snapshot:")
	fmt.Fprintf(&b, "  available: %t\n", snapshot.Available)
	if snapshot.Path != "" {
		fmt.Fprintf(&b, "  path: %s\n", snapshot.Path)
	}
	if snapshot.Available {
		fmt.Fprintf(&b, "  shared_memory_mode: %s\n", snapshot.SharedMemoryMode)
		fmt.Fprintf(&b, "  version: %d\n", snapshot.Version)
		fmt.Fprintf(&b, "  size_bytes: %d\n", snapshot.SizeBytes)
		fmt.Fprintf(&b, "  descriptor_entries: %d\n", snapshot.Entries)
		fmt.Fprintf(&b, "  exact_command_entries: %d\n", snapshot.ExactEntries)
		fmt.Fprintf(&b, "  workspace_image_files: %d\n", snapshot.WarmFileEntries)
		fmt.Fprintf(&b, "  payload_bytes: %d\n", snapshot.PayloadBytes)
	} else if snapshot.Diagnostic != "" {
		fmt.Fprintf(&b, "  diagnostic: %s\n", snapshot.Diagnostic)
	}
	fmt.Fprintln(&b, "background_maintainer:")
	if backgroundErr != nil {
		fmt.Fprintln(&b, "  status: unavailable")
		fmt.Fprintln(&b, "  diagnostic:", backgroundErr)
	} else {
		fmt.Fprintf(&b, "  mode: %s\n", background.Mode)
		fmt.Fprintf(&b, "  running: %t\n", background.Running)
		if background.PID > 0 {
			fmt.Fprintf(&b, "  pid: %d\n", background.PID)
		} else {
			fmt.Fprintln(&b, "  pid: n/a")
		}
		if background.HotCacheSocket != "" {
			fmt.Fprintf(&b, "  hot_cache_socket: %s\n", background.HotCacheSocket)
		}
		if !background.StartedAt.IsZero() {
			fmt.Fprintf(&b, "  started_at: %s\n", background.StartedAt.Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "  native_fallback_available: %t\n", background.NativeFallbackAvailable)
		fmt.Fprintf(&b, "  agent_visible_suggestions: %t\n", background.AgentVisibleSuggestions)
		if background.LogPath != "" {
			fmt.Fprintf(&b, "  log_path: %s\n", background.LogPath)
		}
		fmt.Fprintf(&b, "  status_path: %s\n", background.StatusPath)
		for _, diag := range background.Diagnostics {
			fmt.Fprintln(&b, "  diagnostic:", diag)
		}
	}
	processGuard := ProcessGuardStatus()
	fmt.Fprintln(&b, "process_guard:")
	fmt.Fprintf(&b, "  mode: %s\n", processGuard.Mode)
	fmt.Fprintf(&b, "  current_pid: %d\n", processGuard.CurrentPID)
	fmt.Fprintf(&b, "  parent_pid: %d\n", processGuard.ParentPID)
	if processGuard.CurrentFDCount >= 0 {
		fmt.Fprintf(&b, "  current_fd_count: %d\n", processGuard.CurrentFDCount)
	} else {
		fmt.Fprintln(&b, "  current_fd_count: n/a")
	}
	fmt.Fprintf(&b, "  cleanup_actions: %d\n", processGuard.CleanupActions)
	for _, diag := range processGuard.Diagnostics {
		fmt.Fprintln(&b, "  diagnostic:", diag)
	}
	fmt.Fprintln(&b, "latest_benchmark_status:")
	if latestOK {
		fmt.Fprintf(&b, "  name: %s\n", latest.Name)
		fmt.Fprintf(&b, "  claim: %s\n", latest.Claim)
		fmt.Fprintf(&b, "  ran_at: %s\n", latest.RanAt.Format(time.RFC3339))
		fmt.Fprintf(&b, "  exactness: %t\n", latest.Exactness)
		fmt.Fprintf(&b, "  mismatches: %d\n", latest.Mismatches)
		fmt.Fprintf(&b, "  mutation_boundary_invalidation: %t\n", latest.MutationBoundaryInvalidation)
		fmt.Fprintf(&b, "  safety_gates: %s\n", gateStatus(latest.SafetyGates))
		fmt.Fprintf(&b, "  performance_gates: %s\n", gateStatus(latest.PerformanceGates))
		fmt.Fprintf(&b, "  stale_replay_observed: %t\n", latest.StaleReplayObserved)
		fmt.Fprintf(&b, "  validation_replays: %d\n", latest.ValidationReplays)
	} else {
		fmt.Fprintln(&b, "  none")
	}
	return b.String(), nil
}

func readinessStatus(ws WorldState, snapshot HotSnapshotStats, background BackgroundMaintainerStatus, backgroundErr error) string {
	if !ws.OracleAvailable {
		return "native_fallback"
	}
	if backgroundErr == nil && background.Running && snapshot.Available {
		return "hot"
	}
	if snapshot.Available {
		return "warm"
	}
	return "needs_warm_or_maintainer"
}

func preparedCounts(entries []PreparedEntry) (fastPathOutputs, proofGatedOutputs, warmFiles, fileTreeIndexes, projectMetadata, commandPath, ecosystem, dependency, sourceSymbols int) {
	for _, entry := range entries {
		switch entry.Kind {
		case PreparedKindFastPathOutput:
			fastPathOutputs++
		case PreparedKindProofGatedOutput:
			proofGatedOutputs++
		case PreparedKindWarmFile:
			warmFiles++
		case PreparedKindFileTreeIndex:
			fileTreeIndexes++
		case PreparedKindProjectMetadata:
			projectMetadata++
		case PreparedKindCommandPath:
			commandPath++
		case PreparedKindEcosystem:
			ecosystem++
		case PreparedKindDependencyMetadata:
			dependency++
		case PreparedKindSourceSymbolIndex:
			sourceSymbols++
		}
	}
	return fastPathOutputs, proofGatedOutputs, warmFiles, fileTreeIndexes, projectMetadata, commandPath, ecosystem, dependency, sourceSymbols
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
	var replays, fallbacks, mismatches, diagnosticSkips int
	categories := map[string]int{}
	var roi []int64
	for _, e := range ledger.Entries {
		replays += e.ReplacementCount
		fallbacks += e.FallbackCount
		mismatches += e.ShadowMismatchCount
		diagnosticSkips += e.ShadowSkipCount
		categories = mergeIntMaps(categories, e.ShadowMismatchCategories)
		roi = append(roi, e.NetROIHistoryMS...)
	}
	var b strings.Builder
	fmt.Fprintln(&b, "Squire Kernel acceleration status")
	fmt.Fprintln(&b, "enabled_fast_paths:")
	for _, item := range EnabledFastPaths() {
		fmt.Fprintln(&b, "  -", item)
	}
	fmt.Fprintln(&b, "proof_gated_replay_candidates:")
	for _, item := range ProofGatedReplayCandidates() {
		fmt.Fprintln(&b, "  -", item)
	}
	fmt.Fprintf(&b, "replays: %d\n", replays)
	fmt.Fprintf(&b, "native_fallbacks: %d\n", fallbacks)
	fmt.Fprintf(&b, "diagnostic_mismatches: %d\n", mismatches)
	fmt.Fprintf(&b, "diagnostic_mismatch_categories: %v\n", categories)
	fmt.Fprintf(&b, "diagnostic_sample_skips: %d\n", diagnosticSkips)
	fmt.Fprintf(&b, "invalidations: derived from epoch mismatch\n")
	fmt.Fprintf(&b, "roi_history_ms: %v\n", roi)
	return b.String(), nil
}

func (s *LedgerStore) SaveLatestBenchmarkStatus(status LatestBenchmarkStatus) error {
	if err := s.Init(); err != nil {
		return err
	}
	status.Claim = scopedKernelClaim
	if status.RanAt.IsZero() {
		status.RanAt = time.Now()
	}
	b, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.Root, "latest_benchmark_status.json"), append(b, '\n'), 0o600)
}

func (s *LedgerStore) LoadLatestBenchmarkStatus() (LatestBenchmarkStatus, bool) {
	b, err := os.ReadFile(filepath.Join(s.Root, "latest_benchmark_status.json"))
	if err != nil {
		return LatestBenchmarkStatus{}, false
	}
	var status LatestBenchmarkStatus
	if err := json.Unmarshal(b, &status); err != nil {
		return LatestBenchmarkStatus{}, false
	}
	return status, true
}

func LatestBenchmarkFromRepoMetadata(report BenchReport) LatestBenchmarkStatus {
	return LatestBenchmarkStatus{
		Name:                         "repo-metadata",
		Claim:                        scopedKernelClaim,
		RanAt:                        time.Now(),
		Exactness:                    report.Exactness,
		Mismatches:                   report.Mismatches,
		MutationBoundaryInvalidation: report.MutationBoundaryInvalidation,
		SafetyGates: GateReport{
			Required: true,
			Passed:   report.Exactness && report.Mismatches == 0 && report.MutationBoundaryInvalidation,
			Status:   passFail(report.Exactness && report.Mismatches == 0 && report.MutationBoundaryInvalidation),
		},
		PerformanceGates: GateReport{Required: false, Passed: true, Status: "not_measured"},
	}
}

func LatestBenchmarkFromDeepLocal(report DeepBenchReport) LatestBenchmarkStatus {
	return LatestBenchmarkStatus{
		Name:                         "deep-local",
		Claim:                        scopedKernelClaim,
		RanAt:                        time.Now(),
		Exactness:                    report.EnabledFastPathExactness,
		Mismatches:                   report.EnabledFastPathMismatches,
		MutationBoundaryInvalidation: !report.StaleReplayObserved,
		MetadataFastPathP95US:        report.Performance.MetadataFastPathP95US,
		ProofGatedReplayP95US:        report.Performance.ProofGatedReplayP95US,
		NativeFallbackOverheadP95US:  report.Performance.NativeFallbackOverheadP95US,
		NativeOnlyBookkeepingP95US:   report.Performance.NativeOnlyBookkeepingP95US,
		ShadowBookkeepingP95US:       report.Performance.ShadowBookkeepingP95US,
		SafetyGates:                  report.SafetyGates,
		PerformanceGates:             report.PerformanceGates,
		StaleReplayObserved:          report.StaleReplayObserved,
		ValidationReplays:            report.NeverReplayDiagnostics.ValidationReplays,
	}
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

func emptyAsNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}

func gateStatus(gate GateReport) string {
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

func passFail(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
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
	if _, err := k.Warm(ctx, dir); err != nil {
		return report, err
	}
	for _, argv := range cmds {
		report.Commands = append(report.Commands, displayCommand(argv))
		for i := 0; i < report.Runs; i++ {
			nativeStart := time.Now()
			native := runNative(ctx, dir, argv)
			nativeWall := time.Since(nativeStart)
			replayStart := time.Now()
			replay := k.Run(ctx, "bench", dir, argv)
			replayWall := time.Since(replayStart)
			report.WorkloadOnlyWallDeltaMS += nativeWall.Milliseconds() - replayWall.Milliseconds()
			report.NetROIMS += nativeWall.Milliseconds() - replayWall.Milliseconds()
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
