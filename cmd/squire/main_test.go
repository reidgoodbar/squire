package main

import (
	"strings"
	"testing"
	"time"

	"squire.run/kernel/internal/kernel"
)

func TestUsageTextDocumentsKernelContract(t *testing.T) {
	text := usageText()
	for _, want := range []string{
		"Squire Kernel v1",
		"squire version",
		"Agent chooses. Squire serves.",
		"Native fallback always exists.",
		"Runtime decisions are replay or native.",
		"squire kernel maintain --background",
		"squire kernel run -- git rev-parse HEAD",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("usage text missing %q:\n%s", want, text)
		}
	}
}

func TestHelpTextForArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "global long help", args: []string{"--help"}, want: "usage:"},
		{name: "global help topic", args: []string{"help"}, want: "usage:"},
		{name: "version topic", args: []string{"help", "version"}, want: "build identity"},
		{name: "kernel run topic", args: []string{"kernel", "run", "--help"}, want: "The \"--\" delimiter is"},
		{name: "kernel maintain topic", args: []string{"help", "kernel", "maintain"}, want: "resident maintainer"},
		{name: "boost topic", args: []string{"boost", "-h"}, want: "no broad Codex speedup claim"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, ok := helpTextForArgs(tt.args)
			if !ok {
				t.Fatalf("helpTextForArgs(%v) did not detect help", tt.args)
			}
			if !strings.Contains(text, tt.want) {
				t.Fatalf("help text missing %q:\n%s", tt.want, text)
			}
		})
	}
}

func TestVersionOutput(t *testing.T) {
	oldVersion, oldCommit, oldDate := buildVersion, buildCommit, buildDate
	t.Cleanup(func() {
		buildVersion, buildCommit, buildDate = oldVersion, oldCommit, oldDate
	})
	buildVersion = "1.2.3"
	buildCommit = "abc123"
	buildDate = "2026-06-18"

	text := versionOut(outputShort)
	for _, want := range []string{
		"Squire Kernel v1",
		"version: 1.2.3",
		"commit: abc123",
		"date: 2026-06-18",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("version short output missing %q:\n%s", want, text)
		}
	}

	json := versionOut(outputJSON)
	for _, want := range []string{
		`"product": "Squire Kernel"`,
		`"kernel_contract": "v1"`,
		`"version": "1.2.3"`,
		`"commit": "abc123"`,
		`"date": "2026-06-18"`,
	} {
		if !strings.Contains(json, want) {
			t.Fatalf("version json output missing %q:\n%s", want, json)
		}
	}
}

func TestHelpTextDoesNotInterceptCommandHelpAfterDelimiter(t *testing.T) {
	if text, ok := helpTextForArgs([]string{"kernel", "run", "--", "git", "--help"}); ok {
		t.Fatalf("help intercepted command argv after --:\n%s", text)
	}
}

func TestKernelUsageError(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing subcommand", args: nil, want: "missing kernel subcommand"},
		{name: "unknown subcommand", args: []string{"stats"}, want: `unknown kernel subcommand "stats"`},
		{name: "bad status option", args: []string{"status", "--json"}, want: `unknown kernel status option "--json"`},
		{name: "missing run delimiter", args: []string{"run"}, want: "requires -- before the command"},
		{name: "bad warm option", args: []string{"warm", "--short"}, want: `does not accept option "--short"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kernelUsageError(tt.args)
			if !strings.Contains(got, tt.want) {
				t.Fatalf("kernelUsageError(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestBoostUsageError(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing subcommand", args: nil, want: "missing boost subcommand"},
		{name: "unknown subcommand", args: []string{"stats"}, want: `unknown boost subcommand "stats"`},
		{name: "bad status option", args: []string{"status", "--bogus"}, want: `does not accept option "--bogus"`},
		{name: "missing bench target", args: []string{"bench"}, want: "missing boost bench target"},
		{name: "bad bench target", args: []string{"bench", "all"}, want: `unknown boost bench target "all"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := boostUsageError(tt.args)
			if !strings.Contains(got, tt.want) {
				t.Fatalf("boostUsageError(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestCommandAfterDelimiter(t *testing.T) {
	argv, err := commandAfterDelimiter("squire kernel run", []string{"--", "git", "status", "--short"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(argv, " ") != "git status --short" {
		t.Fatalf("argv = %q", strings.Join(argv, " "))
	}

	for _, args := range [][]string{
		nil,
		{"git", "status"},
		{"--"},
	} {
		if _, err := commandAfterDelimiter("squire kernel run", args); err == nil {
			t.Fatalf("commandAfterDelimiter(%v) returned nil error", args)
		}
	}
}

func TestSplitOutputFormatFlag(t *testing.T) {
	args, format, err := splitOutputFormatFlag([]string{"--background-status", "--short"})
	if err != nil {
		t.Fatal(err)
	}
	if format != outputShort {
		t.Fatalf("format = %v, want outputShort", format)
	}
	if strings.Join(args, " ") != "--background-status" {
		t.Fatalf("args = %q", strings.Join(args, " "))
	}

	args, format, err = splitOutputFormatFlag([]string{"--once", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if format != outputJSON {
		t.Fatalf("format = %v, want outputJSON", format)
	}
	if strings.Join(args, " ") != "--once" {
		t.Fatalf("args = %q", strings.Join(args, " "))
	}

	if _, _, err := splitOutputFormatFlag([]string{"--json", "--short"}); err == nil {
		t.Fatalf("splitOutputFormatFlag accepted conflicting output flags")
	}
}

func TestOutputFormatFromTrailingArgs(t *testing.T) {
	format, err := outputFormatFromTrailingArgs([]string{"--short"})
	if err != nil {
		t.Fatal(err)
	}
	if format != outputShort {
		t.Fatalf("format = %v, want outputShort", format)
	}
	if _, err := outputFormatFromTrailingArgs([]string{"--bogus"}); err == nil {
		t.Fatalf("outputFormatFromTrailingArgs accepted unknown option")
	}

	format, err = outputFormatFromTrailingArgsDefault(nil, outputShort)
	if err != nil {
		t.Fatal(err)
	}
	if format != outputShort {
		t.Fatalf("default format = %v, want outputShort", format)
	}
}

func TestBoostStatusOutputFormats(t *testing.T) {
	report := kernel.BoostStatusReport{
		Claim:                        "scoped",
		EnabledFastPaths:             []string{"git rev-parse HEAD"},
		ProofGatedReplayCandidates:   []string{"cat <bounded workspace source/config file>"},
		Replays:                      3,
		NativeFallbacks:              2,
		HotClientReplays:             1,
		HotClientNativeFallbacks:     0,
		HotClientNativeAvoidedMS:     9,
		HotClientReplayWallUS:        1200,
		HotClientReplayWallMeasured:  1,
		HotClientReplayWallAvgUS:     1200,
		HotClientNetSavedMeasuredMS:  8,
		DiagnosticMismatches:         1,
		DiagnosticMismatchCategories: map[string]int{"ordering": 1},
		DiagnosticSampleSkips:        4,
		Invalidations:                "derived from epoch mismatch",
		ROIHistoryMS:                 []int64{5, 6},
		NativeFallbackAvailable:      true,
		RuntimeDecisions:             "replay_or_native",
	}
	text := boostStatusOut(report, outputShort)
	for _, want := range []string{
		"Squire Kernel acceleration status",
		"git rev-parse HEAD",
		"replays: 3",
		"native_fallbacks: 2",
		"hot_client_replays: 1",
		"hot_client_replay_wall_avg_us: 1200",
		"hot_client_net_saved_measured_ms: 8",
		"native_fallback_available: true",
		"runtime_decisions: replay_or_native",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("boost short output missing %q:\n%s", want, text)
		}
	}
	json := boostStatusOut(report, outputJSON)
	for _, want := range []string{
		`"claim": "scoped"`,
		`"replays": 3`,
		`"native_fallback_available": true`,
		`"runtime_decisions": "replay_or_native"`,
	} {
		if !strings.Contains(json, want) {
			t.Fatalf("boost json output missing %q:\n%s", want, json)
		}
	}
}

func TestRepoMetadataBenchShortOutput(t *testing.T) {
	report := kernel.BenchReport{
		Exactness:                    true,
		Mismatches:                   0,
		MutationBoundaryInvalidation: true,
		WorkloadOnlyWallDeltaMS:      12,
		NetROIMS:                     10,
		NoBroadCodexSpeedupClaim:     true,
		Commands:                     []string{"git rev-parse HEAD"},
	}
	text := repoMetadataBenchOut(report, outputShort)
	for _, want := range []string{
		"Squire Kernel repo-metadata benchmark",
		"exactness: true",
		"mismatches: 0",
		"mutation_boundary_invalidation: true",
		"workload_only_wall_delta_ms: 12",
		"no_broad_codex_speedup_claim: true",
		"  - git rev-parse HEAD",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("repo metadata short output missing %q:\n%s", want, text)
		}
	}
}

func TestWarmReportShortOutput(t *testing.T) {
	report := kernel.WarmReport{
		RepoRoot:                "/repo",
		OracleAvailable:         true,
		FastPathPrepared:        5,
		ProofGatedPrewarmed:     7,
		WarmFilesPrepared:       11,
		FileTreeIndexesPrepared: 1,
		ProjectMetadataPrepared: 2,
		CommandPathPrepared:     1,
		EcosystemPrepared:       3,
		DependencyPrepared:      4,
		SourceSymbolPrepared:    6,
		Prepared:                []kernel.WarmPreparedReport{{Kind: "fast_path_output"}},
		PrivacyMode:             "standard",
		ReplaySetUnchanged:      true,
		AgentVisibleSuggestions: false,
		Notes:                   []string{"no prompt changes"},
	}
	text := warmReportOut(report, outputShort)
	for _, want := range []string{
		"Squire Kernel warm",
		"repo_oracle: available",
		"repo_root: /repo",
		"fast_path_prepared: 5",
		"proof_gated_prewarmed: 7",
		"warm_files_prepared: 11",
		"prepared_entries: 1",
		"privacy_mode: standard",
		"replay_set_unchanged: true",
		"agent_visible_suggestions: false",
		"note: no prompt changes",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("warm short output missing %q:\n%s", want, text)
		}
	}
}

func TestDeepLocalBenchShortOutput(t *testing.T) {
	report := kernel.DeepBenchReport{
		EnabledFastPathExactness:      true,
		EnabledFastPathMismatches:     0,
		NativeOnlyCandidateExactness:  true,
		NativeOnlyCandidateMismatches: 0,
		NoBroadCodexSpeedupClaim:      true,
		SafetyGates:                   kernel.GateReport{Status: "pass", Passed: true, Required: true},
		PerformanceGates:              kernel.GateReport{Status: "needs_optimization", Violations: []string{"native fallback overhead p95 over budget"}},
		NeverReplayDiagnostics:        kernel.NeverReplayDiagnostics{ValidationReplays: 0},
		Performance: kernel.PerformanceBudgetReport{
			MetadataFastPathP95US:       95,
			ProofGatedReplayP95US:       120,
			NativeFallbackOverheadP95US: 4000,
			NativeOnlyBookkeepingP95US:  9000,
		},
	}
	text := deepLocalBenchOut(report, outputShort)
	for _, want := range []string{
		"Squire Kernel deep-local benchmark",
		"safety_gates: pass",
		"performance_gates: needs_optimization",
		"enabled_fast_path_exactness: true",
		"validation_replays: 0",
		"metadata_fast_path_p95_us: 95",
		"native_only_bookkeeping_p95_us: 9000",
		"performance_violation: native fallback overhead p95 over budget",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("deep-local short output missing %q:\n%s", want, text)
		}
	}
}

func TestBackgroundStatusShortOutput(t *testing.T) {
	status := kernel.BackgroundMaintainerStatus{
		Mode:                    "background_process",
		RepoRoot:                "/repo",
		StoreRoot:               "/store",
		HotCacheSocket:          "/store/hot.sock",
		PID:                     123,
		Running:                 true,
		AlreadyRunning:          true,
		Duration:                "30m0s",
		PollInterval:            "2s",
		LogPath:                 "/store/maintainer.log",
		StatusPath:              "/store/maintainer_status.json",
		AgentVisibleSuggestions: false,
		NativeFallbackAvailable: true,
		Diagnostics:             []string{"ready"},
	}
	text := formatBackgroundStatusShort(status)
	for _, want := range []string{
		"Squire Kernel maintainer",
		"status: already_running",
		"running: true",
		"pid: 123",
		"repo_root: /repo",
		"native_fallback: true",
		"agent_visible_suggestions: false",
		"diagnostic: ready",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("short background status missing %q:\n%s", want, text)
		}
	}
}

func TestBackgroundStatusShortPrefersStopState(t *testing.T) {
	status := kernel.BackgroundMaintainerStatus{
		StoreRoot:               "/store",
		StatusPath:              "/store/maintainer_status.json",
		PID:                     123,
		Started:                 true,
		StopRequested:           true,
		Running:                 false,
		AgentVisibleSuggestions: false,
		NativeFallbackAvailable: true,
	}
	text := formatBackgroundStatusShort(status)
	if !strings.Contains(text, "status: stopped") {
		t.Fatalf("stopped status hidden by stale started flag:\n%s", text)
	}

	status.Running = true
	text = formatBackgroundStatusShort(status)
	if !strings.Contains(text, "status: stop_failed") {
		t.Fatalf("stop failure status hidden by stale started flag:\n%s", text)
	}
}

func TestMaintainerReportShortOutput(t *testing.T) {
	report := kernel.MaintainerReport{
		Mode:                    "resident_bounded",
		RepoRoot:                "/repo",
		OracleAvailable:         true,
		PollCycles:              2,
		WarmCycles:              1,
		InvalidationsObserved:   1,
		FastPathPrepared:        5,
		ProofGatedPrewarmed:     7,
		PreparedEntriesObserved: 12,
		LastMaintainedAt:        time.Now(),
		AgentVisibleSuggestions: false,
		NativeFallbackAvailable: true,
	}
	text := formatMaintainerReportShort(report)
	for _, want := range []string{
		"Squire Kernel maintainer",
		"mode: resident_bounded",
		"repo_oracle: available",
		"poll_cycles: 2",
		"warm_cycles: 1",
		"fast_path_prepared: 5",
		"proof_gated_prewarmed: 7",
		"native_fallback: true",
		"agent_visible_suggestions: false",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("short maintainer report missing %q:\n%s", want, text)
		}
	}
}
