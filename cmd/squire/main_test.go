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

func TestHelpTextDoesNotInterceptCommandHelpAfterDelimiter(t *testing.T) {
	if text, ok := helpTextForArgs([]string{"kernel", "run", "--", "git", "--help"}); ok {
		t.Fatalf("help intercepted command argv after --:\n%s", text)
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
