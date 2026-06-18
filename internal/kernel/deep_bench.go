package kernel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type DeepBenchOptions struct {
	Packages             int
	Turns                int
	MetadataRepeats      int
	InitialRepeats       int
	FinalRepeats         int
	ShadowRepeats        int
	ValidationEveryTurns int
}

func DefaultDeepBenchOptions() DeepBenchOptions {
	return DeepBenchOptions{
		Packages:             48,
		Turns:                36,
		MetadataRepeats:      8,
		InitialRepeats:       40,
		FinalRepeats:         60,
		ShadowRepeats:        4,
		ValidationEveryTurns: 12,
	}
}

type OperatorFamilyMetrics struct {
	Commands                 map[string]CommandMetrics `json:"commands"`
	Runs                     int                       `json:"runs"`
	ExactMismatches          int                       `json:"exact_mismatches"`
	ExitCodeMismatches       int                       `json:"exit_code_mismatches"`
	ShadowMismatchCategories map[string]int            `json:"shadow_mismatch_categories,omitempty"`
	WorkloadDeltaUS          int64                     `json:"workload_delta_us"`
	ReplayModes              int                       `json:"replay_modes"`
	NativeModes              int                       `json:"native_modes"`
	ShadowModes              int                       `json:"shadow_modes"`
	NeverModes               int                       `json:"never_modes"`
	NonReplayObserved        bool                      `json:"non_replay_observed"`
}

type CommandMetrics struct {
	Command                  string         `json:"command"`
	Runs                     int            `json:"runs"`
	ExactMismatches          int            `json:"exact_mismatches"`
	ExitCodeMismatches       int            `json:"exit_code_mismatches"`
	ShadowMismatchCategories map[string]int `json:"shadow_mismatch_categories,omitempty"`
	NativeWallUS             int64          `json:"native_wall_us"`
	SquireWallUS             int64          `json:"squire_wall_us"`
	WorkloadDeltaUS          int64          `json:"workload_delta_us"`
	ReplayModes              int            `json:"replay_modes"`
	NativeModes              int            `json:"native_modes"`
	ShadowModes              int            `json:"shadow_modes"`
	NeverModes               int            `json:"never_modes"`
}

type IncrementalBenchMetrics struct {
	Turns                     int   `json:"turns"`
	MetadataRepeatsPerTurn    int   `json:"metadata_repeats_per_turn"`
	HeadInvalidationsNative   int   `json:"head_invalidations_native"`
	BranchInvalidationsNative int   `json:"branch_invalidations_native"`
	StaleHeadReplays          int   `json:"stale_head_replays"`
	StaleBranchReplays        int   `json:"stale_branch_replays"`
	ValidationRuns            int   `json:"validation_runs"`
	ValidationNeverModes      int   `json:"validation_never_modes"`
	MetadataReplayModes       int   `json:"metadata_replay_modes"`
	MetadataNativeModes       int   `json:"metadata_native_modes"`
	ExactMismatches           int   `json:"exact_mismatches"`
	MetadataWorkloadDeltaUS   int64 `json:"metadata_workload_delta_us"`
}

type PerformanceBudgetReport struct {
	MetadataFastPathP95US         int64            `json:"metadata_fast_path_p95_us"`
	ProofGatedReplayP95US         int64            `json:"proof_gated_replay_p95_us"`
	NativeFallbackOverheadP95US   int64            `json:"native_fallback_overhead_p95_us"`
	NativeOnlyBookkeepingP95US    int64            `json:"native_only_bookkeeping_p95_us"`
	ShadowBookkeepingP95US        int64            `json:"shadow_bookkeeping_p95_us"`
	MetadataFastPathBudgetUS      int64            `json:"metadata_fast_path_budget_us"`
	NativeFallbackBudgetUS        int64            `json:"native_fallback_budget_us"`
	NativeOnlyBookkeepingBudgetUS int64            `json:"native_only_bookkeeping_budget_us"`
	ShadowBookkeepingBudgetUS     int64            `json:"shadow_bookkeeping_budget_us"`
	Violations                    []string         `json:"violations,omitempty"`
	PhaseTimings                  PhaseTimingStats `json:"phase_timings"`
	SlowestOperations             []SlowOperation  `json:"slowest_operations,omitempty"`
}

type PhaseStats struct {
	P50MS float64 `json:"p50_ms"`
	P75MS float64 `json:"p75_ms"`
	P95MS float64 `json:"p95_ms"`
	MaxMS float64 `json:"max_ms"`
}

type PhaseTimingStats struct {
	ClassifyMS              PhaseStats `json:"classify_ms"`
	RepoRootLookupMS        PhaseStats `json:"repo_root_lookup_ms"`
	WorldStateLookupMS      PhaseStats `json:"world_state_lookup_ms"`
	EpochCheckMS            PhaseStats `json:"epoch_check_ms"`
	LedgerLookupMS          PhaseStats `json:"ledger_lookup_ms"`
	OutputMaterializeMS     PhaseStats `json:"output_materialize_ms"`
	EventAppendMS           PhaseStats `json:"event_append_ms"`
	DBOrFileWriteMS         PhaseStats `json:"db_or_file_write_ms"`
	LockWaitMS              PhaseStats `json:"lock_wait_ms"`
	NativeOnlyBookkeepingMS PhaseStats `json:"native_only_bookkeeping_ms"`
	ShadowBookkeepingMS     PhaseStats `json:"shadow_bookkeeping_ms"`
	FallbackDecisionMS      PhaseStats `json:"fallback_decision_ms"`
	NativeExecWaitMS        PhaseStats `json:"native_exec_wait_ms"`
}

type SlowOperation struct {
	Command string         `json:"command"`
	Family  OperatorFamily `json:"operator_family"`
	Mode    Mode           `json:"mode"`
	TotalMS float64        `json:"total_ms"`
	Phases  PhaseTimings   `json:"phases"`
}

type GateReport struct {
	Required   bool     `json:"required"`
	Passed     bool     `json:"passed"`
	Status     string   `json:"status"`
	Violations []string `json:"violations,omitempty"`
}

type NeverReplayDiagnostics struct {
	ValidationRuns        int  `json:"validation_runs"`
	ValidationNeverModes  int  `json:"validation_never_modes"`
	ValidationReplays     int  `json:"validation_replays"`
	ValidationNeverReplay bool `json:"validation_never_replay"`
}

type DeepBenchReport struct {
	Runs                          int                     `json:"runs"`
	RepoPath                      string                  `json:"repo_path"`
	Packages                      int                     `json:"packages"`
	TrackedFiles                  int                     `json:"tracked_files"`
	Commits                       int                     `json:"commits"`
	Branches                      int                     `json:"branches"`
	ElapsedMS                     int64                   `json:"elapsed_ms"`
	Metadata                      OperatorFamilyMetrics   `json:"metadata"`
	NativeOnlyDiscovery           OperatorFamilyMetrics   `json:"native_only_discovery"`
	Shadow                        OperatorFamilyMetrics   `json:"shadow"`
	Validation                    OperatorFamilyMetrics   `json:"validation"`
	Incremental                   IncrementalBenchMetrics `json:"incremental"`
	Performance                   PerformanceBudgetReport `json:"performance"`
	MetadataExactness             bool                    `json:"metadata_exactness"`
	NativeOnlyCandidateExactness  bool                    `json:"native_only_candidate_exactness"`
	ShadowExactness               bool                    `json:"shadow_exactness"`
	EnabledFastPathExactness      bool                    `json:"enabled_fast_path_exactness"`
	EnabledFastPathMismatches     int                     `json:"enabled_fast_path_mismatches"`
	NativeOnlyCandidateMismatches int                     `json:"native_only_candidate_mismatches"`
	ShadowCandidateExactness      bool                    `json:"shadow_candidate_exactness"`
	ShadowCandidateMismatches     int                     `json:"shadow_candidate_mismatches"`
	NeverReplayDiagnostics        NeverReplayDiagnostics  `json:"never_replay_diagnostics"`
	SafetyGates                   GateReport              `json:"safety_gates"`
	PerformanceGates              GateReport              `json:"performance_gates"`
	ConsistencyNotes              []string                `json:"consistency_notes,omitempty"`
	ValidationNeverReplayObserved bool                    `json:"validation_never_replay_observed"`
	StaleReplayObserved           bool                    `json:"stale_replay_observed"`
	StorePath                     string                  `json:"store_path"`
	StoreInsideGitDir             bool                    `json:"store_inside_git_dir"`
	NoBroadCodexSpeedupClaim      bool                    `json:"no_broad_codex_speedup_claim"`
}

type benchSamples struct {
	metadataReplayUS   []int64
	proofGatedReplayUS []int64
	nativeOverheadUS   []int64
	shadowUS           []int64
	phaseSamples       map[string][]float64
	slowest            []SlowOperation
}

func BenchDeepLocal(ctx context.Context) (DeepBenchReport, error) {
	return BenchDeepLocalWithOptions(ctx, DefaultDeepBenchOptions())
}

func BenchDeepLocalWithOptions(ctx context.Context, opts DeepBenchOptions) (DeepBenchReport, error) {
	opts = normalizeDeepBenchOptions(opts)
	dir, cleanup, err := makeDeepBenchRepo(ctx, opts.Packages)
	if err != nil {
		return DeepBenchReport{}, err
	}
	defer cleanup()

	start := time.Now()
	k := New(DefaultStoreRoot(dir))
	report := DeepBenchReport{
		RepoPath:                 dir,
		Packages:                 opts.Packages,
		TrackedFiles:             countBenchTracked(ctx, dir),
		Metadata:                 newFamilyMetrics(),
		Shadow:                   newFamilyMetrics(),
		Validation:               newFamilyMetrics(),
		NoBroadCodexSpeedupClaim: true,
		StorePath:                DefaultStoreRoot(dir),
	}
	report.StoreInsideGitDir = pathInside(report.StorePath, filepath.Join(dir, ".git"))
	samples := &benchSamples{}
	metadataCommands := [][]string{
		{"git", "rev-parse", "HEAD"},
		{"git", "rev-parse", "--git-dir"},
		{"git", "rev-parse", "--abbrev-ref", "HEAD"},
	}
	shadowCommands := [][]string{
		{"git", "status", "--short"},
		{"git", "status", "--porcelain"},
		{"git", "ls-files"},
		{"rg", "--files"},
		{"git", "remote", "-v"},
		{"git", "remote", "get-url", "origin"},
	}
	validationCommands := [][]string{{"go", "test", "./..."}}

	for _, argv := range metadataCommands {
		mergeFamily(&report.Metadata, runBenchCommand(ctx, k, dir, argv, 1, compareExact, samples))
	}
	if _, err := k.Warm(ctx, dir); err != nil {
		return DeepBenchReport{}, err
	}
	for _, argv := range metadataCommands {
		mergeFamily(&report.Metadata, runBenchCommand(ctx, k, dir, argv, opts.InitialRepeats, compareExact, samples))
	}
	for _, argv := range shadowCommands {
		mergeFamily(&report.Shadow, runBenchCommand(ctx, k, dir, argv, opts.ShadowRepeats, compareExact, samples))
	}
	for _, argv := range validationCommands {
		mergeFamily(&report.Validation, runBenchCommand(ctx, k, dir, argv, 1, compareExitOnly, samples))
	}

	report.Incremental = runDeepIncremental(ctx, k, dir, opts, metadataCommands, validationCommands, samples)
	for _, argv := range metadataCommands {
		mergeFamily(&report.Metadata, runBenchCommand(ctx, k, dir, argv, opts.FinalRepeats, compareExact, samples))
	}
	if err := addDeepDirtySurface(dir); err != nil {
		return DeepBenchReport{}, err
	}
	for _, argv := range shadowCommands {
		mergeFamily(&report.Shadow, runBenchCommand(ctx, k, dir, argv, opts.ShadowRepeats, compareExact, samples))
	}

	report.Metadata.WorkloadDeltaUS += report.Incremental.MetadataWorkloadDeltaUS
	report.Metadata.Runs += opts.Turns * opts.MetadataRepeats * len(metadataCommands)
	report.Metadata.ReplayModes += report.Incremental.MetadataReplayModes
	report.Metadata.NativeModes += report.Incremental.MetadataNativeModes
	report.Metadata.ExactMismatches += report.Incremental.ExactMismatches
	report.Validation.NeverModes += report.Incremental.ValidationNeverModes
	report.Validation.Runs += report.Incremental.ValidationRuns
	report.Validation.NonReplayObserved = report.Validation.NeverModes > 0
	report.Runs = report.Metadata.Runs + report.Shadow.Runs + report.Validation.Runs
	report.Commits = countBenchCommits(ctx, dir)
	report.Branches = countBenchBranches(ctx, dir)
	report.ElapsedMS = time.Since(start).Milliseconds()
	report.MetadataExactness = report.Metadata.ExactMismatches == 0
	report.ShadowExactness = report.Shadow.ExactMismatches == 0
	report.ValidationNeverReplayObserved = report.Validation.NeverModes > 0
	report.StaleReplayObserved = report.Incremental.StaleHeadReplays > 0 || report.Incremental.StaleBranchReplays > 0
	report.Performance = buildPerformanceBudgetReport(samples)
	report.EnabledFastPathExactness = report.MetadataExactness
	report.EnabledFastPathMismatches = report.Metadata.ExactMismatches + report.Metadata.ExitCodeMismatches
	report.ShadowCandidateExactness = report.ShadowExactness
	report.ShadowCandidateMismatches = report.Shadow.ExactMismatches + report.Shadow.ExitCodeMismatches
	report.NativeOnlyDiscovery = report.Shadow
	report.NativeOnlyCandidateExactness = report.ShadowCandidateExactness
	report.NativeOnlyCandidateMismatches = report.ShadowCandidateMismatches
	report.NeverReplayDiagnostics = NeverReplayDiagnostics{
		ValidationRuns:        report.Validation.Runs,
		ValidationNeverModes:  report.Validation.NeverModes,
		ValidationReplays:     report.Validation.ReplayModes,
		ValidationNeverReplay: report.Validation.Runs > 0 && report.Validation.ReplayModes == 0,
	}
	report.SafetyGates = buildSafetyGates(report, opts)
	report.PerformanceGates = buildPerformanceGates(report.Performance)
	report.ConsistencyNotes = []string{
		"metadata native modes count all metadata operations that ran native; HEAD and branch invalidation-native counts use explicit mutation-boundary checks, so their denominators differ.",
	}
	return report, nil
}

func pathInside(path, dir string) bool {
	pathAbs, pathErr := filepath.Abs(path)
	dirAbs, dirErr := filepath.Abs(dir)
	if pathErr != nil || dirErr != nil {
		return false
	}
	if resolvedPath, err := filepath.EvalSymlinks(pathAbs); err == nil {
		pathAbs = resolvedPath
	}
	if resolvedDir, err := filepath.EvalSymlinks(dirAbs); err == nil {
		dirAbs = resolvedDir
	}
	rel, err := filepath.Rel(dirAbs, pathAbs)
	return err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != ".."
}

type compareMode int

const (
	compareExact compareMode = iota
	compareExitOnly
)

func runBenchCommand(ctx context.Context, k *Kernel, repo string, argv []string, runs int, cmp compareMode, samples *benchSamples) CommandMetrics {
	metrics := CommandMetrics{Command: displayCommand(argv), Runs: runs}
	for i := 0; i < runs; i++ {
		native := runNative(ctx, repo, argv)
		start := time.Now()
		squire := k.Run(ctx, "deep-local-bench", repo, argv)
		squireWall := time.Since(start)
		if samples != nil {
			samples.recordOperation(displayCommand(argv), squire.Family, squire.Mode, squire.Phases)
		}
		metrics.NativeWallUS += native.Wall.Microseconds()
		metrics.SquireWallUS += squireWall.Microseconds()
		if native.ExitCode != squire.ExitCode {
			metrics.ExitCodeMismatches++
		}
		if cmp == compareExact && (native.ExitCode != squire.ExitCode || string(native.Stdout) != string(squire.Stdout) || string(native.Stderr) != string(squire.Stderr)) {
			metrics.ExactMismatches++
		}
		switch squire.Mode {
		case ModeReplay:
			metrics.ReplayModes++
			if IsFastPathAllowed(argv) {
				samples.metadataReplayUS = append(samples.metadataReplayUS, squireWall.Microseconds())
			} else if IsProofGatedReplayCandidate(argv) {
				samples.proofGatedReplayUS = append(samples.proofGatedReplayUS, squireWall.Microseconds())
			}
		case ModeNative:
			metrics.NativeModes++
			if !IsFastPathAllowed(argv) {
				samples.nativeOverheadUS = append(samples.nativeOverheadUS, squireWall.Microseconds()-native.Wall.Microseconds())
			}
		case ModeNever:
			metrics.NeverModes++
		}
	}
	metrics.WorkloadDeltaUS = metrics.NativeWallUS - metrics.SquireWallUS
	return metrics
}

func runDeepIncremental(ctx context.Context, k *Kernel, repo string, opts DeepBenchOptions, metadataCommands, validationCommands [][]string, samples *benchSamples) IncrementalBenchMetrics {
	summary := IncrementalBenchMetrics{Turns: opts.Turns, MetadataRepeatsPerTurn: opts.MetadataRepeats}
	for turn := 0; turn < opts.Turns; turn++ {
		if err := applyDeepTurn(ctx, repo, turn, opts.Packages); err != nil {
			continue
		}
		head := runBenchCommand(ctx, k, repo, []string{"git", "rev-parse", "HEAD"}, 1, compareExact, samples)
		summary.ExactMismatches += head.ExactMismatches
		if head.NativeModes == 1 {
			summary.HeadInvalidationsNative++
		}
		if head.ReplayModes == 1 {
			summary.StaleHeadReplays++
		}
		for _, argv := range metadataCommands {
			metrics := runBenchCommand(ctx, k, repo, argv, opts.MetadataRepeats, compareExact, samples)
			summary.ExactMismatches += metrics.ExactMismatches
			summary.MetadataWorkloadDeltaUS += metrics.WorkloadDeltaUS
			summary.MetadataReplayModes += metrics.ReplayModes
			summary.MetadataNativeModes += metrics.NativeModes
		}
		if opts.ValidationEveryTurns > 0 && turn%opts.ValidationEveryTurns == 0 {
			for _, argv := range validationCommands {
				validation := runBenchCommand(ctx, k, repo, argv, 1, compareExitOnly, samples)
				summary.ValidationRuns += validation.Runs
				summary.ValidationNeverModes += validation.NeverModes
			}
		}
		if turn > 0 && turn%12 == 0 {
			branch := fmt.Sprintf("deep-local-%02d", turn)
			if runNative(ctx, repo, []string{"git", "checkout", "-b", branch}).ExitCode == 0 {
				_ = appendBenchFile(filepath.Join(repo, "docs", "branches.md"), "branch "+branch+"\n")
				benchCommitAll(ctx, repo, "branch update "+branch)
				branchCheck := runBenchCommand(ctx, k, repo, []string{"git", "rev-parse", "--abbrev-ref", "HEAD"}, 1, compareExact, samples)
				summary.ExactMismatches += branchCheck.ExactMismatches
				if branchCheck.NativeModes == 1 {
					summary.BranchInvalidationsNative++
				}
				if branchCheck.ReplayModes == 1 {
					summary.StaleBranchReplays++
				}
				_ = runNative(ctx, repo, []string{"git", "checkout", "main"})
			}
		}
	}
	return summary
}

func normalizeDeepBenchOptions(opts DeepBenchOptions) DeepBenchOptions {
	if opts.Packages <= 0 {
		opts.Packages = 8
	}
	if opts.Turns <= 0 {
		opts.Turns = 4
	}
	if opts.MetadataRepeats <= 0 {
		opts.MetadataRepeats = 2
	}
	if opts.InitialRepeats <= 0 {
		opts.InitialRepeats = 2
	}
	if opts.FinalRepeats <= 0 {
		opts.FinalRepeats = 2
	}
	if opts.ShadowRepeats <= 0 {
		opts.ShadowRepeats = 1
	}
	return opts
}

func newFamilyMetrics() OperatorFamilyMetrics {
	return OperatorFamilyMetrics{Commands: map[string]CommandMetrics{}}
}

func mergeFamily(family *OperatorFamilyMetrics, metrics CommandMetrics) {
	existing := family.Commands[metrics.Command]
	existing.Command = metrics.Command
	existing.Runs += metrics.Runs
	existing.ExactMismatches += metrics.ExactMismatches
	existing.ExitCodeMismatches += metrics.ExitCodeMismatches
	existing.ShadowMismatchCategories = mergeIntMaps(existing.ShadowMismatchCategories, metrics.ShadowMismatchCategories)
	existing.NativeWallUS += metrics.NativeWallUS
	existing.SquireWallUS += metrics.SquireWallUS
	existing.WorkloadDeltaUS += metrics.WorkloadDeltaUS
	existing.ReplayModes += metrics.ReplayModes
	existing.NativeModes += metrics.NativeModes
	existing.ShadowModes += metrics.ShadowModes
	existing.NeverModes += metrics.NeverModes
	family.Commands[metrics.Command] = existing
	family.Runs += metrics.Runs
	family.ExactMismatches += metrics.ExactMismatches
	family.ExitCodeMismatches += metrics.ExitCodeMismatches
	family.ShadowMismatchCategories = mergeIntMaps(family.ShadowMismatchCategories, metrics.ShadowMismatchCategories)
	family.WorkloadDeltaUS += metrics.WorkloadDeltaUS
	family.ReplayModes += metrics.ReplayModes
	family.NativeModes += metrics.NativeModes
	family.ShadowModes += metrics.ShadowModes
	family.NeverModes += metrics.NeverModes
	family.NonReplayObserved = family.NonReplayObserved || metrics.NeverModes > 0
}

func buildPerformanceBudgetReport(samples *benchSamples) PerformanceBudgetReport {
	nativeOnlyBookkeepingP95US := p95(samples.shadowUS)
	report := PerformanceBudgetReport{
		MetadataFastPathP95US:         p95(samples.metadataReplayUS),
		ProofGatedReplayP95US:         p95(samples.proofGatedReplayUS),
		NativeFallbackOverheadP95US:   p95(samples.nativeOverheadUS),
		NativeOnlyBookkeepingP95US:    nativeOnlyBookkeepingP95US,
		ShadowBookkeepingP95US:        nativeOnlyBookkeepingP95US,
		MetadataFastPathBudgetUS:      2000,
		NativeFallbackBudgetUS:        5000,
		NativeOnlyBookkeepingBudgetUS: 10000,
		ShadowBookkeepingBudgetUS:     10000,
		PhaseTimings:                  samples.phaseTimingStats(),
		SlowestOperations:             samples.topSlowestOperations(),
	}
	if report.MetadataFastPathP95US > report.MetadataFastPathBudgetUS {
		report.Violations = append(report.Violations, "metadata fast path p95 over budget")
	}
	if report.NativeFallbackOverheadP95US > report.NativeFallbackBudgetUS {
		report.Violations = append(report.Violations, "native fallback overhead p95 over budget")
	}
	if report.NativeOnlyBookkeepingP95US > report.NativeOnlyBookkeepingBudgetUS {
		report.Violations = append(report.Violations, "native-only bookkeeping p95 over budget")
	}
	return report
}

func buildSafetyGates(report DeepBenchReport, opts DeepBenchOptions) GateReport {
	var violations []string
	if report.Metadata.ExactMismatches != 0 || report.Metadata.ExitCodeMismatches != 0 {
		violations = append(violations, "enabled metadata mismatches observed")
	}
	if report.Incremental.StaleHeadReplays != 0 {
		violations = append(violations, "stale HEAD replay observed")
	}
	if report.Incremental.StaleBranchReplays != 0 {
		violations = append(violations, "stale branch replay observed")
	}
	if report.Validation.ReplayModes != 0 {
		violations = append(violations, "validation replay observed")
	}
	if report.Metadata.ReplayModes == 0 {
		violations = append(violations, "enabled metadata replay was not observed")
	}
	if report.Incremental.HeadInvalidationsNative != opts.Turns {
		violations = append(violations, "HEAD invalidation did not go native")
	}
	if report.Incremental.BranchInvalidationsNative != expectedBranchInvalidations(opts.Turns) {
		violations = append(violations, "branch invalidation did not go native")
	}
	status := "pass"
	if len(violations) > 0 {
		status = "fail"
	}
	return GateReport{Required: true, Passed: len(violations) == 0, Status: status, Violations: violations}
}

func buildPerformanceGates(perf PerformanceBudgetReport) GateReport {
	status := "pass"
	if len(perf.Violations) > 0 {
		status = "needs_optimization"
	}
	return GateReport{Required: false, Passed: len(perf.Violations) == 0, Status: status, Violations: append([]string(nil), perf.Violations...)}
}

func expectedBranchInvalidations(turns int) int {
	var count int
	for turn := 0; turn < turns; turn++ {
		if turn > 0 && turn%12 == 0 {
			count++
		}
	}
	return count
}

func (s *benchSamples) recordOperation(command string, family OperatorFamily, mode Mode, phases PhaseTimings) {
	if s.phaseSamples == nil {
		s.phaseSamples = map[string][]float64{}
	}
	s.phaseSamples["classify_ms"] = append(s.phaseSamples["classify_ms"], phases.ClassifyMS)
	s.phaseSamples["repo_root_lookup_ms"] = append(s.phaseSamples["repo_root_lookup_ms"], phases.RepoRootLookupMS)
	s.phaseSamples["world_state_lookup_ms"] = append(s.phaseSamples["world_state_lookup_ms"], phases.WorldStateLookupMS)
	s.phaseSamples["epoch_check_ms"] = append(s.phaseSamples["epoch_check_ms"], phases.EpochCheckMS)
	s.phaseSamples["ledger_lookup_ms"] = append(s.phaseSamples["ledger_lookup_ms"], phases.LedgerLookupMS)
	s.phaseSamples["output_materialize_ms"] = append(s.phaseSamples["output_materialize_ms"], phases.OutputMaterializeMS)
	s.phaseSamples["event_append_ms"] = append(s.phaseSamples["event_append_ms"], phases.EventAppendMS)
	s.phaseSamples["db_or_file_write_ms"] = append(s.phaseSamples["db_or_file_write_ms"], phases.DBOrFileWriteMS)
	s.phaseSamples["lock_wait_ms"] = append(s.phaseSamples["lock_wait_ms"], phases.LockWaitMS)
	s.phaseSamples["shadow_bookkeeping_ms"] = append(s.phaseSamples["shadow_bookkeeping_ms"], phases.ShadowBookkeepingMS)
	s.phaseSamples["fallback_decision_ms"] = append(s.phaseSamples["fallback_decision_ms"], phases.FallbackDecisionMS)
	s.phaseSamples["native_exec_wait_ms"] = append(s.phaseSamples["native_exec_wait_ms"], phases.NativeExecWaitMS)
	s.slowest = append(s.slowest, SlowOperation{
		Command: command,
		Family:  family,
		Mode:    mode,
		TotalMS: phases.TotalMS(),
		Phases:  phases,
	})
}

func (s *benchSamples) phaseTimingStats() PhaseTimingStats {
	if s == nil {
		return PhaseTimingStats{}
	}
	return PhaseTimingStats{
		ClassifyMS:              phaseStats(s.phaseSamples["classify_ms"]),
		RepoRootLookupMS:        phaseStats(s.phaseSamples["repo_root_lookup_ms"]),
		WorldStateLookupMS:      phaseStats(s.phaseSamples["world_state_lookup_ms"]),
		EpochCheckMS:            phaseStats(s.phaseSamples["epoch_check_ms"]),
		LedgerLookupMS:          phaseStats(s.phaseSamples["ledger_lookup_ms"]),
		OutputMaterializeMS:     phaseStats(s.phaseSamples["output_materialize_ms"]),
		EventAppendMS:           phaseStats(s.phaseSamples["event_append_ms"]),
		DBOrFileWriteMS:         phaseStats(s.phaseSamples["db_or_file_write_ms"]),
		LockWaitMS:              phaseStats(s.phaseSamples["lock_wait_ms"]),
		NativeOnlyBookkeepingMS: phaseStats(s.phaseSamples["shadow_bookkeeping_ms"]),
		ShadowBookkeepingMS:     phaseStats(s.phaseSamples["shadow_bookkeeping_ms"]),
		FallbackDecisionMS:      phaseStats(s.phaseSamples["fallback_decision_ms"]),
		NativeExecWaitMS:        phaseStats(s.phaseSamples["native_exec_wait_ms"]),
	}
}

func (s *benchSamples) topSlowestOperations() []SlowOperation {
	if s == nil || len(s.slowest) == 0 {
		return nil
	}
	cp := append([]SlowOperation(nil), s.slowest...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].TotalMS > cp[j].TotalMS })
	if len(cp) > 20 {
		cp = cp[:20]
	}
	return cp
}

func phaseStats(values []float64) PhaseStats {
	if len(values) == 0 {
		return PhaseStats{}
	}
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	return PhaseStats{
		P50MS: percentileFloat(cp, 50),
		P75MS: percentileFloat(cp, 75),
		P95MS: percentileFloat(cp, 95),
		MaxMS: cp[len(cp)-1],
	}
}

func percentileFloat(sorted []float64, percentile int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted)*percentile + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}

func p95(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	cp := append([]int64(nil), values...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := (len(cp)*95 + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > len(cp) {
		idx = len(cp)
	}
	return cp[idx-1]
}

func makeDeepBenchRepo(ctx context.Context, packages int) (string, func(), error) {
	dir, err := os.MkdirTemp("", "squire-kernel-deep-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	for _, argv := range [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "squire@example.invalid"},
		{"git", "config", "user.name", "Squire Deep Bench"},
		{"git", "remote", "add", "origin", "https://github.com/example/deepbench.git"},
	} {
		if res := runNative(ctx, dir, argv); res.ExitCode != 0 {
			cleanup()
			return "", func() {}, fmt.Errorf("%s failed: %s", displayCommand(argv), string(res.Stderr))
		}
	}
	if err := writeBenchFile(filepath.Join(dir, "go.mod"), "module example.com/deepbench\n\ngo 1.22\n"); err != nil {
		cleanup()
		return "", func() {}, err
	}
	_ = writeBenchFile(filepath.Join(dir, ".gitignore"), "*.tmp\nignored/\ncache/\n")
	_ = writeBenchFile(filepath.Join(dir, "README.md"), "# Deep Bench\n")
	_ = writeBenchFile(filepath.Join(dir, "docs", "branches.md"), "# Branches\n")
	for p := 0; p < packages; p++ {
		pkg := fmt.Sprintf("pkg%03d", p)
		dirPath := filepath.Join(dir, "internal", pkg)
		for f := 0; f < 4; f++ {
			src := fmt.Sprintf("package %s\n\nfunc Value%d(seed int) int { return seed + %d + %d }\n", pkg, f, p, f)
			if err := writeBenchFile(filepath.Join(dirPath, fmt.Sprintf("value_%02d.go", f)), src); err != nil {
				cleanup()
				return "", func() {}, err
			}
		}
		test := fmt.Sprintf("package %s\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value0(10) != %d { t.Fatalf(\"bad value\") } }\n", pkg, 10+p)
		_ = writeBenchFile(filepath.Join(dirPath, "value_test.go"), test)
		_ = writeBenchFile(filepath.Join(dirPath, "fixture.json"), fmt.Sprintf("{\"pkg\":%q}\n", pkg))
		_ = writeBenchFile(filepath.Join(dir, "configs", fmt.Sprintf("service-%03d.yaml", p)), fmt.Sprintf("name: service-%03d\n", p))
	}
	_ = writeBenchFile(filepath.Join(dir, "cmd", "app", "main.go"), "package main\n\nfunc main() { println(\"deep bench\") }\n")
	if res := runNative(ctx, dir, []string{"go", "test", "./..."}); res.ExitCode != 0 {
		cleanup()
		return "", func() {}, fmt.Errorf("go test failed: %s", string(res.Stderr))
	}
	benchCommitAll(ctx, dir, "initial deep bench")
	return dir, cleanup, nil
}

func applyDeepTurn(ctx context.Context, repo string, turn, packages int) error {
	pkg := fmt.Sprintf("pkg%03d", turn%packages)
	dir := filepath.Join(repo, "internal", pkg)
	if err := appendBenchFile(filepath.Join(dir, "value_00.go"), fmt.Sprintf("\n// turn %03d\n", turn)); err != nil {
		return err
	}
	src := fmt.Sprintf("package %s\n\nfunc Turn%03d() int { return Value0(%d) }\n", pkg, turn, turn)
	if err := writeBenchFile(filepath.Join(dir, fmt.Sprintf("turn_%03d.go", turn)), src); err != nil {
		return err
	}
	_ = writeBenchFile(filepath.Join(repo, "configs", fmt.Sprintf("turn-%03d.yaml", turn)), fmt.Sprintf("turn: %d\n", turn))
	benchCommitAll(ctx, repo, fmt.Sprintf("turn %03d update", turn))
	return nil
}

func addDeepDirtySurface(repo string) error {
	if err := writeBenchFile(filepath.Join(repo, ".hidden-local"), "hidden\n"); err != nil {
		return err
	}
	if err := writeBenchFile(filepath.Join(repo, "untracked-notes.md"), "notes\n"); err != nil {
		return err
	}
	if err := writeBenchFile(filepath.Join(repo, "ignored", "artifact.tmp"), "ignored\n"); err != nil {
		return err
	}
	_ = os.Symlink(filepath.Join("configs", "service-000.yaml"), filepath.Join(repo, "service-link.yaml"))
	return nil
}

func benchCommitAll(ctx context.Context, repo, message string) {
	_ = runNative(ctx, repo, []string{"git", "add", "."})
	_ = runNative(ctx, repo, []string{"git", "commit", "-m", message})
}

func writeBenchFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func appendBenchFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}

func countBenchTracked(ctx context.Context, repo string) int {
	res := runNative(ctx, repo, []string{"git", "ls-files"})
	if res.ExitCode != 0 || len(res.Stdout) == 0 {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(string(res.Stdout)), "\n"))
}

func countBenchCommits(ctx context.Context, repo string) int {
	res := runNative(ctx, repo, []string{"git", "rev-list", "--count", "--all"})
	if res.ExitCode != 0 {
		return 0
	}
	var n int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(res.Stdout)), "%d", &n)
	return n
}

func countBenchBranches(ctx context.Context, repo string) int {
	res := runNative(ctx, repo, []string{"git", "branch", "--format=%(refname:short)"})
	if res.ExitCode != 0 || len(res.Stdout) == 0 {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(string(res.Stdout)), "\n"))
}
