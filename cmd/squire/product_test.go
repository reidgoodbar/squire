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
