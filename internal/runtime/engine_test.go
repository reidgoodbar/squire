package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"squire.run/internal/kernel"
)

func TestEngineReplaysPreparedGitMetadata(t *testing.T) {
	repo := initRepo(t)
	storeRoot := kernel.DefaultStoreRoot(repo)
	if _, err := kernel.WarmMetadata(context.Background(), repo, storeRoot); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(NewRegistry(func(context.Context, *Workspace) error { return nil }))
	result := engine.TryExecute(context.Background(), Request{
		SessionID: "test",
		CWD:       repo,
		Argv:      []string{"git", "rev-parse", "HEAD"},
	})
	if result.Outcome != OutcomeHit {
		t.Fatalf("outcome = %q, reason = %q", result.Outcome, result.Reason)
	}
	want := commandOutput(t, repo, "git", "rev-parse", "HEAD")
	if string(result.Stdout) != string(want) || result.ExitCode != 0 {
		t.Fatalf("result = (%q, %d), want (%q, 0)", result.Stdout, result.ExitCode, want)
	}
	if result.Provider != "validated_workspace" || result.ProofID == "" {
		t.Fatalf("provider/proof = %q/%q", result.Provider, result.ProofID)
	}
}

func TestEngineColdMissPreparesOnceWithoutBlockingFallback(t *testing.T) {
	repo := initRepo(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	registry := NewRegistry(func(context.Context, *Workspace) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return nil
	})
	engine := NewEngine(registry)
	request := Request{SessionID: "test", CWD: repo, Argv: []string{"git", "status", "--short"}}

	start := time.Now()
	first := engine.TryExecute(context.Background(), request)
	if first.Outcome != OutcomeMiss {
		t.Fatalf("first outcome = %q", first.Outcome)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("cold miss waited for workspace preparation")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("preparation did not start")
	}
	second := engine.TryExecute(context.Background(), request)
	if second.Outcome != OutcomeMiss {
		t.Fatalf("second outcome = %q", second.Outcome)
	}
	if calls.Load() != 1 {
		t.Fatalf("prepare calls = %d, want 1", calls.Load())
	}
	close(release)
}

func TestRegistrySharesRepoAcrossSubdirectoryAndGitC(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initRepoAt(t, repo)
	subdir := filepath.Join(repo, "nested")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(func(context.Context, *Workspace) error { return nil })
	direct, ok := registry.Resolve(subdir, []string{"git", "rev-parse", "HEAD"})
	if !ok {
		t.Fatal("subdirectory workspace was not resolved")
	}
	viaGitC, ok := registry.Resolve(parent, []string{"git", "-C", "repo", "rev-parse", "HEAD"})
	if !ok {
		t.Fatal("git -C workspace was not resolved")
	}
	wantRoot, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if direct != viaGitC || direct.Root != wantRoot {
		t.Fatalf("workspace mismatch: direct=%+v git-c=%+v", direct, viaGitC)
	}
}

func TestRegistryRetriesPreparationAfterReadyWorkspaceMissesAgain(t *testing.T) {
	repo := initRepo(t)
	completed := make(chan struct{}, 2)
	var calls atomic.Int32
	registry := NewRegistry(func(context.Context, *Workspace) error {
		calls.Add(1)
		completed <- struct{}{}
		return nil
	})
	workspace, ok := registry.Resolve(repo, []string{"git", "status", "--short"})
	if !ok {
		t.Fatal("workspace was not resolved")
	}
	if !registry.EnsurePrepared(workspace) {
		t.Fatal("initial preparation did not start")
	}
	waitForIdle := func(label string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for registry.Status(workspace.ID).Running && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if registry.Status(workspace.ID).Running {
			t.Fatalf("%s preparation remained running", label)
		}
	}
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("initial preparation did not finish")
	}
	waitForIdle("initial")

	registry.mu.Lock()
	entry := registry.workspaces[workspace.ID]
	entry.prepare.LastAttempt = time.Now().Add(-preparationRetryAfter)
	registry.mu.Unlock()
	if !registry.EnsurePrepared(workspace) {
		t.Fatal("preparation did not retry after cooldown")
	}
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("retry preparation did not finish")
	}
	waitForIdle("retry")
	if calls.Load() != 2 {
		t.Fatalf("prepare calls = %d, want 2", calls.Load())
	}
}

func TestEngineDoesNotPrepareUnsupportedCommands(t *testing.T) {
	repo := initRepo(t)
	var calls atomic.Int32
	registry := NewRegistry(func(context.Context, *Workspace) error {
		calls.Add(1)
		return nil
	})
	engine := NewEngine(registry)
	result := engine.TryExecute(context.Background(), Request{
		SessionID: "test",
		CWD:       repo,
		Argv:      []string{"go", "test", "./..."},
	})
	if result.Outcome != OutcomeMiss {
		t.Fatalf("outcome = %q", result.Outcome)
	}
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("prepare calls = %d, want 0", calls.Load())
	}
}

func TestEngineRejectsDifferentExecutionEnvironmentWithoutPreparing(t *testing.T) {
	repo := initRepo(t)
	storeRoot := kernel.DefaultStoreRoot(repo)
	if _, err := kernel.WarmMetadata(context.Background(), repo, storeRoot); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	registry := NewRegistry(func(context.Context, *Workspace) error {
		calls.Add(1)
		return nil
	})
	engine := NewEngine(registry)
	env := environmentMap(os.Environ())
	env["PATH"] = "/different/path"
	result := engine.TryExecute(context.Background(), Request{
		SessionID: "test",
		CWD:       repo,
		Argv:      []string{"git", "rev-parse", "HEAD"},
		Env:       env,
	})
	if result.Outcome != OutcomeMiss || !strings.Contains(result.Reason, "environment differs") {
		t.Fatalf("result = %+v", result)
	}
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("prepare calls = %d, want 0", calls.Load())
	}
}

func TestEngineAcceptsExactExecutionEnvironment(t *testing.T) {
	repo := initRepo(t)
	storeRoot := kernel.DefaultStoreRoot(repo)
	if _, err := kernel.WarmMetadata(context.Background(), repo, storeRoot); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(NewRegistry(func(context.Context, *Workspace) error { return nil }))
	result := engine.TryExecute(context.Background(), Request{
		SessionID: "test",
		CWD:       repo,
		Argv:      []string{"git", "rev-parse", "HEAD"},
		Env:       environmentMap(os.Environ()),
	})
	if result.Outcome != OutcomeHit {
		t.Fatalf("result = %+v", result)
	}
}

func environmentMap(entries []string) map[string]string {
	env := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	return env
}

func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	initRepoAt(t, repo)
	return repo
}

func initRepoAt(t *testing.T, repo string) {
	t.Helper()
	commandOutput(t, repo, "git", "init", "-q")
	commandOutput(t, repo, "git", "config", "user.email", "squire@example.test")
	commandOutput(t, repo, "git", "config", "user.name", "Squire Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Runtime Test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commandOutput(t, repo, "git", "add", "README.md")
	commandOutput(t, repo, "git", "commit", "-qm", "initial")
}

func commandOutput(t *testing.T, dir, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return out
}
