package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
		"squire kernel adapter --stdio",
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
		{name: "kernel adapter topic", args: []string{"help", "kernel", "adapter"}, want: "model still"},
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
		{name: "bad adapter usage", args: []string{"adapter"}, want: "invalid kernel adapter usage"},
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

func TestKernelAdapterServesMultipleRequestsOverOneProcess(t *testing.T) {
	ctx := context.Background()
	repo := initAdapterGitRepo(t)
	t.Setenv("SQUIRE_KERNEL_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		kernels:          make(map[string]*kernel.Kernel),
		states:           make(map[string]adapterCWDState),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	requests := []adapterRequest{
		{ID: "head", CWD: repo, Argv: []string{"git", "rev-parse", "HEAD"}},
		{ID: "branch", CWD: repo, Argv: []string{"git", "rev-parse", "--abbrev-ref", "HEAD"}},
	}
	var in bytes.Buffer
	for _, req := range requests {
		if err := json.NewEncoder(&in).Encode(req); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := server.serve(ctx, &in, &out); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("adapter returned %d response lines, want 2:\n%s", len(lines), out.String())
	}
	got := make(map[string]adapterResponse)
	for _, line := range lines {
		var resp adapterResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("response is not JSON: %s\n%v", line, err)
		}
		if !resp.OK {
			t.Fatalf("adapter response failed: %+v", resp)
		}
		got[resp.ID] = resp
	}
	assertAdapterStdout(t, got["head"], runGit(t, repo, "rev-parse", "HEAD"))
	assertAdapterStdout(t, got["branch"], runGit(t, repo, "rev-parse", "--abbrev-ref", "HEAD"))
	if len(server.kernels) != 1 {
		t.Fatalf("adapter constructed %d kernel instances, want one reused instance", len(server.kernels))
	}
}

func TestAdapterEnvOverlayRestoresProcessEnvironment(t *testing.T) {
	const key = "SQUIRE_ADAPTER_TEST_ENV"
	t.Setenv(key, "before")
	resp := withAdapterEnv(map[string]string{key: "during"}, false, func() adapterResponse {
		if got := os.Getenv(key); got != "during" {
			return adapterResponse{OK: false, Error: "env during callback = " + got}
		}
		return adapterResponse{OK: true}
	})
	if !resp.OK {
		t.Fatal(resp.Error)
	}
	if got := os.Getenv(key); got != "before" {
		t.Fatalf("env after callback = %q, want before", got)
	}
}

func TestAdapterFastResponseWriterMatchesJSONSemantics(t *testing.T) {
	resp := adapterResponse{
		ID:                "id-with-quote-\"",
		OK:                true,
		StdoutB64:         base64.StdEncoding.EncodeToString([]byte("hello\n")),
		StderrB64:         base64.StdEncoding.EncodeToString([]byte("warn\n")),
		ExitCode:          7,
		Mode:              kernel.ModeReplay,
		Family:            kernel.FamilyRepoState,
		Proof:             "mmap-hot-snapshot",
		NativeWallMS:      11,
		Diagnostics:       []string{"line\nbreak", "quote\""},
		MaintainerRunning: true,
		MaintainerAlready: true,
	}
	var fast bytes.Buffer
	writeAdapterResponseFast(&fast, resp)
	var got adapterResponse
	if err := json.Unmarshal(bytes.TrimSpace(fast.Bytes()), &got); err != nil {
		t.Fatalf("fast response is invalid JSON: %s\n%v", fast.String(), err)
	}
	if !reflect.DeepEqual(got, resp) {
		t.Fatalf("decoded fast response mismatch\ngot:  %+v\nwant: %+v\njson: %s", got, resp, fast.String())
	}

	var slow bytes.Buffer
	if err := writeAdapterResponseSlow(&slow, resp); err != nil {
		t.Fatal(err)
	}
	var slowMap, fastMap map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(slow.Bytes()), &slowMap); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bytes.TrimSpace(fast.Bytes()), &fastMap); err != nil {
		t.Fatal(err)
	}
	if !jsonMapsEqual(slowMap, fastMap) {
		t.Fatalf("fast JSON differs from standard encoder\nfast=%s\nslow=%s", fast.String(), slow.String())
	}
}

func TestAdapterResponseWriterKeepsDebugPhases(t *testing.T) {
	resp := adapterResponse{
		ID:       "debug",
		OK:       true,
		ExitCode: 0,
		Mode:     kernel.ModeReplay,
		Family:   kernel.FamilyLocalRepoMetadata,
		Phases:   &kernel.PhaseTimings{ClassifyMS: 1.25},
	}
	var out bytes.Buffer
	if err := writeAdapterResponse(&out, resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"phases"`) {
		t.Fatalf("debug response missing phases: %s", out.String())
	}
	var got adapterResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &got); err != nil {
		t.Fatal(err)
	}
	if got.Phases == nil || got.Phases.ClassifyMS != resp.Phases.ClassifyMS {
		t.Fatalf("decoded phases = %+v, want %+v", got.Phases, resp.Phases)
	}
}

func TestAdapterPlanCacheCopiesArgv(t *testing.T) {
	server := &adapterServer{plans: make(map[string]adapterCommandPlan)}
	cwd := t.TempDir()
	argv := []string{"git", "status", "--short"}
	shortPlan := server.planFor(cwd, argv)
	if shortPlan.key == "" {
		t.Fatal("short plan has empty key")
	}
	argv[2] = "--porcelain"
	porcelainPlan := server.planFor(cwd, argv)
	if porcelainPlan.key == shortPlan.key {
		t.Fatalf("mutated argv reused stale plan key %q", porcelainPlan.key)
	}
	argv[2] = "--short"
	again := server.planFor(cwd, argv)
	if again.key != shortPlan.key {
		t.Fatalf("short plan key = %q, want cached %q", again.key, shortPlan.key)
	}
	if len(server.plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(server.plans))
	}
}

func TestKernelAdapterNativeDirectSkipsMaintainerAndKernel(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	t.Setenv("SQUIRE_KERNEL_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		ensureMaintainer: true,
		kernels:          make(map[string]*kernel.Kernel),
		states:           make(map[string]adapterCWDState),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	req := adapterRequest{
		ID:   "never",
		CWD:  repo,
		Argv: []string{"python3", "-m", "unittest", "-h"},
	}
	resp := server.handleRequest(ctx, req)
	if !resp.OK {
		t.Fatalf("adapter response failed: %+v", resp)
	}
	if resp.Mode != kernel.ModeNever {
		t.Fatalf("mode = %s, want never", resp.Mode)
	}
	if resp.Family != kernel.FamilyValidation {
		t.Fatalf("family = %s, want validation", resp.Family)
	}
	if len(server.kernels) != 0 {
		t.Fatalf("native-direct path constructed %d kernel instances", len(server.kernels))
	}
	if len(server.maintainers) != 0 {
		t.Fatalf("native-direct path touched maintainer state: %+v", server.maintainers)
	}
	assertAdapterStdout(t, resp, runCommand(t, repo, "python3", "-m", "unittest", "-h"))
}

func TestKernelAdapterCachesPlanAndHotMiss(t *testing.T) {
	ctx := context.Background()
	repo := initAdapterGitRepo(t)
	t.Setenv("SQUIRE_KERNEL_STORE_ROOT", filepath.Join(t.TempDir(), "store"))
	server := &adapterServer{
		defaultCWD:       repo,
		defaultSessionID: "test-adapter",
		kernels:          make(map[string]*kernel.Kernel),
		states:           make(map[string]adapterCWDState),
		plans:            make(map[string]adapterCommandPlan),
		hotMisses:        make(map[string]time.Time),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	req := adapterRequest{
		ID:   "status",
		CWD:  repo,
		Argv: []string{"git", "status", "--short"},
	}
	first := server.handleRequest(ctx, req)
	if !first.OK {
		t.Fatalf("first response failed: %+v", first)
	}
	if first.Mode != kernel.ModeNative {
		t.Fatalf("first mode = %s, want native cold miss", first.Mode)
	}
	assertAdapterStdout(t, first, runGit(t, repo, "status", "--short"))
	if len(server.plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(server.plans))
	}
	if len(server.hotMisses) != 1 {
		t.Fatalf("hot misses = %d, want 1", len(server.hotMisses))
	}
	second := server.handleRequest(ctx, req)
	if !second.OK {
		t.Fatalf("second response failed: %+v", second)
	}
	if second.Mode != kernel.ModeNative {
		t.Fatalf("second mode = %s, want native hot miss memo", second.Mode)
	}
	assertAdapterStdout(t, second, runGit(t, repo, "status", "--short"))
	if len(server.plans) != 1 {
		t.Fatalf("plans after second request = %d, want 1", len(server.plans))
	}
	if len(server.hotMisses) != 1 {
		t.Fatalf("hot misses after second request = %d, want 1", len(server.hotMisses))
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

func initAdapterGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "squire@example.invalid"},
		{"config", "user.name", "Squire Kernel"},
	} {
		stdout, stderr, code := runGitRaw(repo, args...)
		if code != 0 {
			t.Fatalf("git %s failed with code %d\nstdout=%s\nstderr=%s", strings.Join(args, " "), code, stdout, stderr)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Adapter Test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-m", "initial"},
	} {
		stdout, stderr, code := runGitRaw(repo, args...)
		if code != 0 {
			t.Fatalf("git %s failed with code %d\nstdout=%s\nstderr=%s", strings.Join(args, " "), code, stdout, stderr)
		}
	}
	return repo
}

func runGit(t *testing.T, repo string, args ...string) []byte {
	t.Helper()
	stdout, stderr, code := runGitRaw(repo, args...)
	if code != 0 {
		t.Fatalf("git %s failed with code %d\nstdout=%s\nstderr=%s", strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout
}

func runCommand(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s failed: %v\nstdout=%s\nstderr=%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.Bytes()
}

func runGitRaw(repo string, args ...string) ([]byte, []byte, int) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			code = 1
			stderr.WriteString(err.Error())
		}
	}
	return stdout.Bytes(), stderr.Bytes(), code
}

func assertAdapterStdout(t *testing.T, resp adapterResponse, want []byte) {
	t.Helper()
	got, err := base64.StdEncoding.DecodeString(resp.StdoutB64)
	if err != nil {
		t.Fatalf("stdout is not base64: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("adapter stdout mismatch for %s\ngot:  %q\nwant: %q", resp.ID, got, want)
	}
	stderr, err := base64.StdEncoding.DecodeString(resp.StderrB64)
	if err != nil {
		t.Fatalf("stderr is not base64: %v", err)
	}
	if len(stderr) != 0 {
		t.Fatalf("adapter stderr for %s = %q, want empty", resp.ID, stderr)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("adapter exit code for %s = %d, want 0", resp.ID, resp.ExitCode)
	}
}

func jsonMapsEqual(left, right map[string]any) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}
