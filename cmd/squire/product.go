package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"squire.run/internal/green"
	"squire.run/internal/proofcache"
	squireruntime "squire.run/internal/runtime"
)

type doctorCheck struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Required bool   `json:"required"`
}

type doctorReport struct {
	Ready     bool          `json:"ready"`
	CWD       string        `json:"cwd"`
	RepoRoot  string        `json:"repo_root,omitempty"`
	StoreRoot string        `json:"store_root,omitempty"`
	Checks    []doctorCheck `json:"checks"`
}

type productStatusWorkspace struct {
	State           string `json:"state"`
	Root            string `json:"root,omitempty"`
	StoreRoot       string `json:"store_root,omitempty"`
	SnapshotEntries int    `json:"snapshot_entries,omitempty"`
	SnapshotBytes   int64  `json:"snapshot_bytes,omitempty"`
}

type productStatusRuntime struct {
	ABI            int    `json:"abi"`
	Decision       string `json:"decision"`
	NativeFallback bool   `json:"native_fallback"`
}

type productStatusLane struct {
	Tier       string   `json:"tier"`
	Scope      string   `json:"scope"`
	Operations []string `json:"operations"`
}

type productStatusMetrics struct {
	Replays                 int   `json:"replays"`
	MeasuredReplays         int   `json:"measured_replays"`
	ReplayWallAverageUS     int64 `json:"replay_wall_average_us"`
	EstimatedNetSavedMS     int64 `json:"estimated_net_saved_ms"`
	LastReplayUnixNano      int64 `json:"last_replay_unix_nano"`
	DiagnosticMismatches    int   `json:"diagnostic_mismatches"`
	DiagnosticSampleSkips   int   `json:"diagnostic_sample_skips"`
	NativeFallbacksRecorded int   `json:"native_fallbacks_recorded"`
}

type productStatusReport struct {
	Product      string                 `json:"product"`
	Workspace    productStatusWorkspace `json:"workspace"`
	Verification green.Report           `json:"verification"`
	Runtime      productStatusRuntime   `json:"runtime"`
	Lanes        []productStatusLane    `json:"lanes"`
	Metrics      productStatusMetrics   `json:"metrics"`
}

type productGreenTrustReport struct {
	Trusted      bool     `json:"trusted"`
	RepoRoot     string   `json:"repo_root"`
	ConfigPath   string   `json:"config_path"`
	ConfigDigest string   `json:"config_digest,omitempty"`
	Checks       []string `json:"checks,omitempty"`
}

func runProductStatus(ctx context.Context, cwd, storeRoot string, format outputFormat) (string, error) {
	workspace := productStatusWorkspace{State: "outside_workspace"}
	if root, resolvedStore, ok := proofcache.FastWorkspace(cwd); ok {
		storeRoot = resolvedStore
		snapshot := proofcache.HotSnapshotStatsForStore(storeRoot)
		workspace = productStatusWorkspace{
			State:           "cold",
			Root:            root,
			StoreRoot:       storeRoot,
			SnapshotEntries: snapshot.Entries,
			SnapshotBytes:   snapshot.SizeBytes,
		}
		if snapshot.Available {
			workspace.State = "ready"
		}
	}
	verification := green.Report{State: "unconfigured"}
	if workspace.Root != "" {
		verification = green.Inspect(ctx, workspace.Root, storeRoot)
	}
	metrics, err := proofcache.BoostStatusReportForStore(ctx, cwd, storeRoot)
	if err != nil {
		return "", err
	}
	report := productStatusReport{
		Product:      "Squire",
		Workspace:    workspace,
		Verification: verification,
		Runtime: productStatusRuntime{
			ABI:            1,
			Decision:       "exact_hit_or_native",
			NativeFallback: true,
		},
		Lanes: productRuntimeLanes(),
		Metrics: productStatusMetrics{
			Replays:                 metrics.HotClientReplays,
			MeasuredReplays:         metrics.HotClientReplayWallMeasured,
			ReplayWallAverageUS:     metrics.HotClientReplayWallAvgUS,
			EstimatedNetSavedMS:     metrics.HotClientNetSavedMeasuredMS,
			LastReplayUnixNano:      metrics.HotClientLastReplayUnixNano,
			DiagnosticMismatches:    metrics.DiagnosticMismatches,
			DiagnosticSampleSkips:   metrics.DiagnosticSampleSkips,
			NativeFallbacksRecorded: metrics.HotClientNativeFallbacks,
		},
	}
	if format == outputJSON {
		return jsonOut(report), nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Squire status: %s\n", report.Workspace.State)
	if report.Workspace.Root != "" {
		fmt.Fprintf(&out, "  workspace: %s\n", report.Workspace.Root)
		fmt.Fprintf(&out, "  prepared entries: %d\n", report.Workspace.SnapshotEntries)
	}
	fmt.Fprintf(&out, "  runtime: ABI %d, exact hit or native\n", report.Runtime.ABI)
	fmt.Fprintf(&out, "  replays: %d\n", report.Metrics.Replays)
	if report.Metrics.MeasuredReplays > 0 {
		fmt.Fprintf(&out, "  replay wall average: %dus (%d measured)\n", report.Metrics.ReplayWallAverageUS, report.Metrics.MeasuredReplays)
	}
	fmt.Fprintf(&out, "  diagnostic mismatches: %d\n", report.Metrics.DiagnosticMismatches)
	if report.Verification.Configured {
		fmt.Fprintf(&out, "  verification: %s (%d/%d current)\n", report.Verification.State, report.Verification.CurrentChecks, len(report.Verification.Checks))
		for _, check := range report.Verification.Checks {
			fmt.Fprintf(&out, "    %s: %s\n", check.Name, green.StateLabel(check.State))
		}
	} else {
		fmt.Fprintln(&out, "  verification: unconfigured")
	}
	return out.String(), nil
}

func runProductVerify(ctx context.Context, cwd, storeRoot string, format outputFormat) (string, bool, error) {
	repoRoot, resolvedStore, ok := proofcache.FastWorkspace(cwd)
	if !ok {
		report := green.Report{State: "outside_workspace", Diagnostics: []string{"current directory is not inside a supported Git worktree"}}
		if format == outputJSON {
			return jsonOut(report), false, nil
		}
		return formatGreenReport(report), false, nil
	}
	if resolvedStore != "" {
		storeRoot = resolvedStore
	}
	report := green.Inspect(ctx, repoRoot, storeRoot)
	if format == outputJSON {
		return jsonOut(report), report.Green, nil
	}
	return formatGreenReport(report), report.Green, nil
}

func runProductGreenTrust(ctx context.Context, cwd, storeRoot string, format outputFormat) (string, error) {
	_ = ctx
	repoRoot, resolvedStore, ok := proofcache.FastWorkspace(cwd)
	if !ok {
		return "", fmt.Errorf("current directory is not inside a supported Git worktree")
	}
	if resolvedStore != "" {
		storeRoot = resolvedStore
	}
	trust, err := green.TrustConfig(repoRoot, storeRoot)
	if err != nil {
		return "", err
	}
	config, err := green.LoadConfig(repoRoot)
	if err != nil || config.Digest != trust.ConfigDigest {
		_ = green.RevokeConfigTrust(storeRoot)
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("Green config changed while trust was being recorded; review and trust it again")
	}
	report := productGreenTrustReport{
		Trusted:      true,
		RepoRoot:     repoRoot,
		ConfigPath:   config.Path,
		ConfigDigest: trust.ConfigDigest,
	}
	for _, check := range config.Checks {
		report.Checks = append(report.Checks, check.Name+": "+displayArgv(check.Command))
	}
	if format == outputJSON {
		return jsonOut(report), nil
	}
	var out strings.Builder
	fmt.Fprintln(&out, "Squire Green config trusted")
	fmt.Fprintf(&out, "Workspace: %s\n", report.RepoRoot)
	fmt.Fprintf(&out, "Config: %s\n", report.ConfigPath)
	fmt.Fprintf(&out, "Digest: %s\n", shortDigest(report.ConfigDigest))
	for _, check := range report.Checks {
		fmt.Fprintf(&out, "Check: %s\n", check)
	}
	fmt.Fprintln(&out, "Validation runs in the background while squire codex is active.")
	return out.String(), nil
}

func runProductGreenRevoke(ctx context.Context, cwd, storeRoot string, format outputFormat) (string, error) {
	_ = ctx
	repoRoot, resolvedStore, ok := proofcache.FastWorkspace(cwd)
	if !ok {
		return "", fmt.Errorf("current directory is not inside a supported Git worktree")
	}
	if resolvedStore != "" {
		storeRoot = resolvedStore
	}
	if err := green.RevokeConfigTrust(storeRoot); err != nil {
		return "", err
	}
	report := productGreenTrustReport{Trusted: false, RepoRoot: repoRoot, ConfigPath: filepath.Join(repoRoot, filepath.FromSlash(green.ConfigRelativePath))}
	if format == outputJSON {
		return jsonOut(report), nil
	}
	return fmt.Sprintf("Squire Green trust revoked\nWorkspace: %s\n", repoRoot), nil
}

func formatGreenReport(report green.Report) string {
	var out strings.Builder
	switch {
	case report.Green:
		fmt.Fprintln(&out, "SQUIRE GREEN")
	case report.Configured && !report.Trusted:
		fmt.Fprintln(&out, "SQUIRE UNTRUSTED")
	case !report.Configured && report.State == "unconfigured":
		fmt.Fprintln(&out, "SQUIRE UNCONFIGURED")
	default:
		fmt.Fprintln(&out, "SQUIRE NOT GREEN")
	}
	if report.RepoRoot != "" {
		fmt.Fprintf(&out, "Workspace: %s\n", report.RepoRoot)
	}
	if report.WorkspaceState != "" {
		fmt.Fprintf(&out, "Workspace state: %s\n", report.WorkspaceState)
	}
	if report.ObservedWorkspaceID != "" {
		fmt.Fprintf(&out, "Observed workspace epoch: %s\n", shortDigest(report.ObservedWorkspaceID))
	}
	for _, check := range report.Checks {
		label := green.StateLabel(check.State)
		if check.Current {
			label += " current"
		}
		if check.Duration > 0 {
			label += " " + check.Duration.Round(time.Millisecond).String()
		}
		fmt.Fprintf(&out, "%s: %s\n", check.Name, label)
		if check.Reason != "" {
			fmt.Fprintf(&out, "  %s\n", check.Reason)
		}
	}
	for _, diagnostic := range report.Diagnostics {
		fmt.Fprintf(&out, "Diagnostic: %s\n", diagnostic)
	}
	if report.Green {
		fmt.Fprintln(&out, "All required checks are current.")
	} else if report.Configured && !report.Trusted {
		fmt.Fprintln(&out, "Review .squire/checks.toml, then run: squire green trust")
	} else if !report.Configured && report.State == "unconfigured" {
		fmt.Fprintf(&out, "Configure checks in %s.\n", green.ConfigRelativePath)
	}
	return out.String()
}

func displayArgv(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		parts = append(parts, strconv.Quote(arg))
	}
	return strings.Join(parts, " ")
}

func shortDigest(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func productRuntimeLanes() []productStatusLane {
	return []productStatusLane{
		{
			Tier:  "metadata",
			Scope: "constant-size Git state proofs",
			Operations: []string{
				"git rev-parse HEAD|--git-dir|--show-toplevel|--is-inside-work-tree",
				"git rev-parse --abbrev-ref HEAD",
				"git branch --show-current",
			},
		},
		{
			Tier:  "bounded",
			Scope: "hash-validated workspace files and directories",
			Operations: []string{
				"cat, sed -n, head, tail, nl -ba windows",
				"grep -F and single-file rg -F",
				"tight ls, safe printenv, hostname, uname",
			},
		},
		{
			Tier:  "repository",
			Scope: "proof cost scales with relevant repository state",
			Operations: []string{
				"git status --short|--porcelain",
				"git ls-files and git diff",
				"fully supported read-only shell compositions",
			},
		},
	}
}

func runProductCodex(ctx context.Context, cwd, storeRoot string, args []string) (int, error) {
	binary, err := findSquireCodex()
	if err != nil {
		return 1, err
	}
	if repoRoot, resolvedStore, ok := proofcache.FastWorkspace(cwd); ok {
		if resolvedStore != "" {
			storeRoot = resolvedStore
		}
		go func() {
			_, _ = proofcache.StartBackgroundMaintainer(
				context.Background(),
				repoRoot,
				storeRoot,
				proofcache.DefaultBackgroundMaintainerOptions(),
			)
		}()
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = cwd
	cmd.Env = environmentWithOverrides(os.Environ(), squireCodexBridgeEnv())
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err == nil {
		return 0, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), nil
	}
	return 1, err
}

func findSquireCodex() (string, error) {
	name := "squire-codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), name)
		if runnableFile(candidate) {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("%s is not installed next to squire or on PATH", name)
}

func runnableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0
}

func environmentWithOverrides(base, overrides []string) []string {
	order := make([]string, 0, len(base)+len(overrides))
	values := make(map[string]string, len(base)+len(overrides))
	add := func(entry string) {
		if key, _, ok := strings.Cut(entry, "="); ok && key != "" {
			if _, exists := values[key]; !exists {
				order = append(order, key)
			}
			values[key] = entry
		}
	}
	for _, entry := range base {
		add(entry)
	}
	for _, entry := range overrides {
		add(entry)
	}
	result := make([]string, 0, len(order))
	for _, key := range order {
		result = append(result, values[key])
	}
	return result
}

func runProductPrepare(ctx context.Context, cwd, storeRoot string, args []string) (string, error) {
	format, err := outputFormatFromTrailingArgsDefault(args, outputShort)
	if err != nil {
		return "", err
	}
	repoRoot, resolvedStore, ok := proofcache.FastWorkspace(cwd)
	if !ok {
		return "", fmt.Errorf("current directory is not inside a supported Git worktree")
	}
	storeRoot = resolvedStore
	status, err := proofcache.StartBackgroundMaintainer(
		ctx,
		repoRoot,
		storeRoot,
		proofcache.DefaultBackgroundMaintainerOptions(),
	)
	if err != nil {
		return "", err
	}
	if format == outputJSON {
		return jsonOut(status), nil
	}
	return formatBackgroundStatusShort(status), nil
}

func runProductExplain(ctx context.Context, cwd string, args []string) (string, error) {
	argv, err := commandAfterDelimiter("squire explain", args)
	if err != nil {
		return "", err
	}
	engine := squireruntime.NewEngine(nil)
	explanation := engine.Explain(ctx, squireruntime.Request{
		SessionID: "explain",
		CWD:       cwd,
		Argv:      argv,
	})
	b, err := json.MarshalIndent(explanation, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

func runProductDoctor(ctx context.Context, cwd string, format outputFormat) (string, bool, error) {
	_ = ctx
	report := doctorReport{Ready: true, CWD: cwd}
	add := func(name string, required bool, ok bool, detail string) {
		status := "ok"
		if !ok {
			if required {
				status = "missing"
				report.Ready = false
			} else {
				status = "inactive"
			}
		}
		report.Checks = append(report.Checks, doctorCheck{Name: name, Status: status, Detail: detail, Required: required})
	}

	squireCodex, codexErr := findSquireCodex()
	add("squire-codex", true, codexErr == nil, squireCodex)
	hotLibrary := ""
	runtimeABI := ""
	if exe, err := os.Executable(); err == nil {
		hotLibrary = squireHotLibraryNextTo(exe)
		abiPath := filepath.Join(filepath.Dir(exe), ".squire-runtime-abi")
		abi, readErr := os.ReadFile(abiPath)
		if readErr == nil {
			runtimeABI = strings.TrimSpace(string(abi))
		}
	}
	add("runtime-abi", true, runtimeABI == "1", runtimeABI)
	add("runtime-library", true, hotLibrary != "", hotLibrary)
	helper := "codex-code-mode-host"
	if runtime.GOOS == "windows" {
		helper += ".exe"
	}
	helperPath := ""
	if squireCodex != "" {
		candidate := filepath.Join(filepath.Dir(squireCodex), helper)
		if runnableFile(candidate) {
			helperPath = candidate
		}
	}
	add("codex-runtime-helper", true, helperPath != "", helperPath)
	gitPath, gitErr := exec.LookPath("git")
	add("git", true, gitErr == nil, gitPath)
	if repoRoot, storeRoot, ok := proofcache.FastWorkspace(cwd); ok {
		report.RepoRoot = repoRoot
		report.StoreRoot = storeRoot
		add("workspace", false, true, repoRoot)
	} else {
		add("workspace", false, false, "current directory is not inside a Git worktree")
	}

	if format == outputJSON {
		return jsonOut(report), report.Ready, nil
	}
	var out strings.Builder
	state := "ready"
	if !report.Ready {
		state = "needs attention"
	}
	fmt.Fprintf(&out, "Squire doctor: %s\n", state)
	for _, check := range report.Checks {
		fmt.Fprintf(&out, "  %-24s %s", check.Name, check.Status)
		if check.Detail != "" {
			fmt.Fprintf(&out, " (%s)", check.Detail)
		}
		out.WriteByte('\n')
	}
	return out.String(), report.Ready, nil
}
