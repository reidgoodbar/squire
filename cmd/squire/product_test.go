package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"squire.run/internal/green"
	"squire.run/internal/proofcache"
	squireruntime "squire.run/internal/runtime"
)

func TestEnvironmentWithOverridesReplacesExistingValues(t *testing.T) {
	base := []string{"PATH=/older", "HOME=/home", "PATH=/old", "EMPTY="}
	original := append([]string(nil), base...)
	overrides := []string{"PATH=/new", "SQUIRE_CODEX_BRIDGE=1", "invalid"}
	want := []string{"PATH=/new", "HOME=/home", "EMPTY=", "SQUIRE_CODEX_BRIDGE=1"}
	if got := environmentWithOverrides(base, overrides); !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(base, original) {
		t.Fatalf("base environment was mutated: %#v", base)
	}
}

func TestRunnableFileRequiresExecutableRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tool")
	if err := os.WriteFile(path, []byte("tool\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && runnableFile(path) {
		t.Fatal("non-executable file reported runnable")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if !runnableFile(path) {
		t.Fatal("executable regular file reported unavailable")
	}
	if runnableFile(dir) {
		t.Fatal("directory reported runnable")
	}
}

func TestProductStatusReportsProductionRuntimeLanes(t *testing.T) {
	repo := initAdapterGitRepo(t)
	storeRoot := proofcache.DefaultStoreRoot(repo)
	if _, err := proofcache.WarmMetadata(context.Background(), repo, storeRoot); err != nil {
		t.Fatal(err)
	}
	out, err := runProductStatus(context.Background(), repo, storeRoot, outputJSON)
	if err != nil {
		t.Fatal(err)
	}
	var report productStatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Product != "Squire" || report.Runtime.ABI != 1 || !report.Runtime.NativeFallback {
		t.Fatalf("status = %+v", report)
	}
	if report.Workspace.State != "ready" || report.Workspace.Root == "" {
		t.Fatalf("workspace = %+v", report.Workspace)
	}
	encoded, err := json.Marshal(report.Lanes)
	if err != nil {
		t.Fatal(err)
	}
	lanes := string(encoded)
	for _, want := range []string{"git rev-parse", "cat, sed -n", "git status"} {
		if !strings.Contains(lanes, want) {
			t.Fatalf("production lanes missing %q: %s", want, lanes)
		}
	}
	for _, removed := range []string{"--version", "command -v", "which"} {
		if strings.Contains(lanes, removed) {
			t.Fatalf("production lanes include disabled slow probe %q: %s", removed, lanes)
		}
	}
}

func TestProductVerifyReportsGreenAndStatusIncludesChecks(t *testing.T) {
	repo := initAdapterGitRepo(t)
	_, storeRoot, ok := proofcache.FastWorkspace(repo)
	if !ok {
		t.Fatal("test repository was not discovered")
	}
	configDir := filepath.Join(repo, ".squire")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := `version = 1

[[check]]
name = "tests"
command = ["git", "diff", "--check"]
inputs = ["README.md"]
`
	if err := os.WriteFile(filepath.Join(configDir, "checks.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := green.TrustConfig(repo, storeRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := green.RunPending(context.Background(), repo, storeRoot); err != nil {
		t.Fatal(err)
	}
	out, verified, err := runProductVerify(context.Background(), repo, storeRoot, outputShort)
	if err != nil {
		t.Fatal(err)
	}
	if !verified || !strings.Contains(out, "SQUIRE GREEN") || !strings.Contains(out, "tests: PASS current") {
		t.Fatalf("verify output = %q, verified=%t", out, verified)
	}
	statusJSON, err := runProductStatus(context.Background(), repo, storeRoot, outputJSON)
	if err != nil {
		t.Fatal(err)
	}
	var status productStatusReport
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Verification.Green || len(status.Verification.Checks) != 1 {
		t.Fatalf("product status verification = %+v", status.Verification)
	}
}

func TestProductVerifyUnconfiguredIsNonGreen(t *testing.T) {
	repo := initAdapterGitRepo(t)
	out, verified, err := runProductVerify(context.Background(), repo, proofcache.DefaultStoreRoot(repo), outputShort)
	if err != nil {
		t.Fatal(err)
	}
	if verified || !strings.Contains(out, "SQUIRE UNCONFIGURED") || !strings.Contains(out, green.ConfigRelativePath) {
		t.Fatalf("unconfigured verify output = %q, verified=%t", out, verified)
	}
}

func TestProductGreenTrustAndRevokeExactConfig(t *testing.T) {
	repo := initAdapterGitRepo(t)
	_, storeRoot, ok := proofcache.FastWorkspace(repo)
	if !ok {
		t.Fatal("test repository was not discovered")
	}
	configDir := filepath.Join(repo, ".squire")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "checks.toml")
	config := "[[check]]\nname = \"tests\"\ncommand = [\"git\", \"diff\", \"--check\"]\ninputs = [\"README.md\"]\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runProductGreenTrust(context.Background(), repo, storeRoot, outputShort)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "config trusted") || !strings.Contains(out, "git") {
		t.Fatalf("trust output = %q", out)
	}
	loaded, err := green.LoadConfig(repo)
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := green.ConfigTrusted(repo, storeRoot, loaded.Digest)
	if err != nil || !trusted {
		t.Fatalf("trusted=%t err=%v", trusted, err)
	}
	if _, err := runProductGreenRevoke(context.Background(), repo, storeRoot, outputShort); err != nil {
		t.Fatal(err)
	}
	trusted, err = green.ConfigTrusted(repo, storeRoot, loaded.Digest)
	if err != nil || trusted {
		t.Fatalf("trusted after revoke=%t err=%v", trusted, err)
	}
}

func TestResidentMaintainerRunsGreenAutomatically(t *testing.T) {
	repo := initAdapterGitRepo(t)
	storeRoot := proofcache.DefaultStoreRoot(repo)
	configDir := filepath.Join(repo, ".squire")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := `version = 1
quiescence = "100ms"
poll_interval = "100ms"

[[check]]
name = "tests"
command = ["git", "diff", "--check"]
inputs = ["README.md"]
`
	if err := os.WriteFile(filepath.Join(configDir, "checks.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := green.TrustConfig(repo, storeRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := runMaintain(context.Background(), repo, storeRoot, []string{"--duration", "1s", "--poll-interval", "100ms", "--short"}); err != nil {
		t.Fatal(err)
	}
	status := green.Inspect(context.Background(), repo, storeRoot)
	if !status.Green {
		t.Fatalf("resident maintainer did not publish Green: %+v", status)
	}
}

func TestProductExplainUsesProductionPolicy(t *testing.T) {
	repo := initAdapterGitRepo(t)
	out, err := runProductExplain(
		context.Background(),
		repo,
		[]string{"--", "git", "--version"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var explanation squireruntime.Explanation
	if err := json.Unmarshal([]byte(out), &explanation); err != nil {
		t.Fatal(err)
	}
	if explanation.Eligible || explanation.Outcome != squireruntime.OutcomeMiss {
		t.Fatalf("explanation = %+v", explanation)
	}
}

func TestProductPrepareRejectsDirectoryOutsideGitWorktree(t *testing.T) {
	_, err := runProductPrepare(context.Background(), t.TempDir(), "", nil)
	if err == nil || !strings.Contains(err.Error(), "Git worktree") {
		t.Fatalf("prepare error = %v, want unsupported worktree error", err)
	}
}

func TestProductDoctorReportsMissingInstalledComponentsAsNotReady(t *testing.T) {
	_, ready, err := runProductDoctor(context.Background(), t.TempDir(), outputJSON)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("doctor reported a test binary without installed runtime components as ready")
	}
}
