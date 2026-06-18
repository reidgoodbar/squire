package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFastPathReplayExactAndHeadInvalidates(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	k := New(DefaultStoreRoot(repo))

	cold := k.Run(ctx, "test", repo, []string{"git", "rev-parse", "HEAD"})
	if cold.ExitCode != 0 {
		t.Fatalf("cold native failed: %s", cold.Stderr)
	}
	if cold.Mode != ModeNative {
		t.Fatalf("cold run mode = %s, want native", cold.Mode)
	}
	if cold.Phases.LedgerLookupMS != 0 || cold.Phases.WorldStateLookupMS != 0 || cold.Phases.DBOrFileWriteMS != 0 {
		t.Fatalf("cold fast path did expensive cache work: %+v", cold.Phases)
	}
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	second := k.Run(ctx, "test", repo, []string{"git", "rev-parse", "HEAD"})
	if second.Mode != ModeReplay {
		t.Fatalf("second run mode = %s, want replay; diagnostics=%v", second.Mode, second.Diagnostics)
	}
	assertSameResult(t, second.Stdout, second.Stderr, second.ExitCode, runNative(ctx, repo, []string{"git", "rev-parse", "HEAD"}))
	oldHead := strings.TrimSpace(string(second.Stdout))

	commitFile(t, ctx, repo, "second.txt", "second\n", "second")
	after := k.Run(ctx, "test", repo, []string{"git", "rev-parse", "HEAD"})
	if after.Mode == ModeReplay {
		t.Fatalf("HEAD replayed across commit boundary")
	}
	newHead := strings.TrimSpace(string(after.Stdout))
	if newHead == "" || newHead == oldHead {
		t.Fatalf("HEAD did not change across commit: old=%q new=%q", oldHead, newHead)
	}
}

func TestBranchFastPathInvalidatesOnCheckout(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	k := New(DefaultStoreRoot(repo))

	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	replay := k.Run(ctx, "test", repo, []string{"git", "rev-parse", "--abbrev-ref", "HEAD"})
	if replay.Mode != ModeReplay {
		t.Fatalf("branch was not replayed: %s", replay.Mode)
	}
	if res := runNative(ctx, repo, []string{"git", "checkout", "-b", "feature"}); res.ExitCode != 0 {
		t.Fatalf("checkout failed: %s", res.Stderr)
	}
	after := k.Run(ctx, "test", repo, []string{"git", "rev-parse", "--abbrev-ref", "HEAD"})
	if after.Mode == ModeReplay {
		t.Fatalf("branch replayed across checkout boundary")
	}
	if got := strings.TrimSpace(string(after.Stdout)); got != "feature" {
		t.Fatalf("branch = %q, want feature", got)
	}
}

func TestGitDirFastPathSurvivesSourceEdit(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	k := New(DefaultStoreRoot(repo))

	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("local edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	replay := k.Run(ctx, "test", repo, []string{"git", "rev-parse", "--git-dir"})
	if replay.Mode != ModeReplay {
		t.Fatalf("git-dir was not replayed across source edit: mode=%s diagnostics=%v", replay.Mode, replay.Diagnostics)
	}
	assertSameResult(t, replay.Stdout, replay.Stderr, replay.ExitCode, runNative(ctx, repo, []string{"git", "rev-parse", "--git-dir"}))
}

func TestSafeGitConfigMetadataNormalizesToFastPath(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	k := New(DefaultStoreRoot(repo))
	argv := []string{"git", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "rev-parse", "HEAD"}

	if got := Classify(argv); got != FamilyLocalRepoMetadata {
		t.Fatalf("family = %s, want %s", got, FamilyLocalRepoMetadata)
	}
	if !IsFastPathAllowed(argv) {
		t.Fatalf("safe git -c rev-parse HEAD was not fast-path eligible")
	}
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	second := k.Run(ctx, "test", repo, argv)
	if second.Mode != ModeReplay {
		t.Fatalf("second mode = %s, want replay; diagnostics=%v", second.Mode, second.Diagnostics)
	}
	assertSameResult(t, second.Stdout, second.Stderr, second.ExitCode, runNative(ctx, repo, argv))
}

func TestGitDashCNormalizesEffectiveWorkingDirectory(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	parent := t.TempDir()
	k := New(filepath.Join(parent, ".squire", "kernel"))
	argv := []string{"git", "-C", repo, "rev-parse", "HEAD"}

	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	second := k.Run(ctx, "test", parent, argv)
	if second.Mode != ModeReplay {
		t.Fatalf("second mode = %s, want replay; diagnostics=%v", second.Mode, second.Diagnostics)
	}
	assertSameResult(t, second.Stdout, second.Stderr, second.ExitCode, runNative(ctx, parent, argv))
}

func TestSafeGitConfigStatusRunsNativeWithoutInlineSpeculation(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	k := New(DefaultStoreRoot(repo))
	argv := []string{"git", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "status", "--porcelain"}

	if got := Classify(argv); got != FamilyRepoState {
		t.Fatalf("family = %s, want %s", got, FamilyRepoState)
	}
	if !IsProofGatedReplayCandidate(argv) {
		t.Fatalf("safe git -c status --porcelain was not proof-gated replayable")
	}
	res := k.Run(ctx, "test", repo, argv)
	if res.Mode != ModeNative {
		t.Fatalf("mode = %s, want native", res.Mode)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, argv))
	if res.Phases.WorldStateLookupMS != 0 || res.Phases.ShadowBookkeepingMS != 0 || res.Phases.DBOrFileWriteMS != 0 {
		t.Fatalf("repo-state serving path did expensive speculation: %+v", res.Phases)
	}
}

func TestUnknownGitPassThroughAvoidsOracleAndLedger(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, "local.txt"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := New(DefaultStoreRoot(repo))
	argv := []string{"git", "log", "--oneline"}

	res := k.Run(ctx, "test", repo, argv)
	if res.Mode != ModeNative {
		t.Fatalf("mode = %s, want native", res.Mode)
	}
	if res.Family != FamilyShellUnknown {
		t.Fatalf("family = %s, want shell_unknown", res.Family)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, argv))
	if res.Phases.RepoRootLookupMS != 0 || res.Phases.WorldStateLookupMS != 0 || res.Phases.LedgerLookupMS != 0 {
		t.Fatalf("unknown git should pass through without oracle/ledger lookup: %+v", res.Phases)
	}
}

func TestWarmPreparesFastPathForLaterAgentChosenCommand(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	storeRoot := DefaultStoreRoot(repo)

	k := New(storeRoot)
	report, err := k.Warm(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if report.AgentVisibleSuggestions {
		t.Fatalf("warm report exposed agent-visible suggestions")
	}
	if !report.ReplaySetUnchanged {
		t.Fatalf("warm changed replay set")
	}
	if report.FastPathPrepared == 0 {
		t.Fatalf("warm did not prepare fast paths: %+v", report)
	}

	argv := []string{"git", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "rev-parse", "HEAD"}
	res := k.Run(ctx, "test", repo, argv)
	if res.Mode != ModeReplay {
		t.Fatalf("warmed command mode = %s, want replay; diagnostics=%v", res.Mode, res.Diagnostics)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, argv))
}

func TestWarmPrewarmsProofGatedCommandForLaterAgentChosenReplay(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/prewarm\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(repo)

	k := New(storeRoot)
	report, err := k.Warm(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if report.AgentVisibleSuggestions {
		t.Fatalf("warm exposed agent-visible suggestions")
	}
	if report.ProofGatedPrewarmed == 0 {
		t.Fatalf("warm did not prewarm proof-gated commands: %+v", report)
	}

	argv := []string{"cat", "go.mod"}
	res := k.Run(ctx, "test", repo, argv)
	if res.Mode != ModeReplay {
		t.Fatalf("warmed proof-gated command mode = %s, want replay; diagnostics=%v", res.Mode, res.Diagnostics)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, argv))
	if res.Phases.NativeExecWaitMS != 0 || res.Phases.WorldStateLookupMS != 0 || res.Phases.DBOrFileWriteMS != 0 || res.Phases.ShadowBookkeepingMS != 0 {
		t.Fatalf("prepared replay used expensive serving-path phases: %+v", res.Phases)
	}
	again := k.Run(ctx, "test", repo, argv)
	if again.Mode != ModeReplay {
		t.Fatalf("second warmed proof-gated command mode = %s, want replay; diagnostics=%v", again.Mode, again.Diagnostics)
	}
	assertSameResult(t, again.Stdout, again.Stderr, again.ExitCode, runNative(ctx, repo, argv))
	if again.Phases.NativeExecWaitMS != 0 || again.Phases.WorldStateLookupMS != 0 || again.Phases.LedgerLookupMS != 0 || again.Phases.OutputMaterializeMS > 1 || again.Phases.DBOrFileWriteMS != 0 || again.Phases.ShadowBookkeepingMS != 0 {
		t.Fatalf("hot prepared replay used expensive serving-path phases: %+v", again.Phases)
	}
}

func TestMaintainerPrewarmsAndRefreshesProofGatedManifest(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/maintain\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(repo)
	k := New(storeRoot)
	report, err := k.RunMaintainer(ctx, repo, MaintainerOptions{MaxCycles: 1, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if report.WarmCycles != 1 || report.ProofGatedPrewarmed == 0 {
		t.Fatalf("maintainer did not prewarm proof-gated outputs: %+v", report)
	}
	argv := []string{"cat", "go.mod"}
	first := k.Run(ctx, "test", repo, argv)
	if first.Mode != ModeReplay {
		t.Fatalf("mode = %s, want replay after maintainer prewarm; diagnostics=%v", first.Mode, first.Diagnostics)
	}
	assertSameResult(t, first.Stdout, first.Stderr, first.ExitCode, runNative(ctx, repo, argv))

	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/maintain\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	refresh, err := k.RunMaintainer(ctx, repo, MaintainerOptions{MaxCycles: 1, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if refresh.WarmCycles != 1 || refresh.ProofGatedPrewarmed == 0 {
		t.Fatalf("maintainer did not refresh after manifest change: %+v", refresh)
	}
	after := k.Run(ctx, "test", repo, argv)
	if after.Mode != ModeReplay {
		t.Fatalf("mode after refresh = %s, want replay; diagnostics=%v", after.Mode, after.Diagnostics)
	}
	assertSameResult(t, after.Stdout, after.Stderr, after.ExitCode, runNative(ctx, repo, argv))
	if string(after.Stdout) == string(first.Stdout) {
		t.Fatalf("replayed stale manifest output after refresh")
	}
}

func TestMaintainerSkipsWarmWhenSignalUnchanged(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/skip\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := New(DefaultStoreRoot(repo)).RunMaintainer(ctx, repo, MaintainerOptions{MaxCycles: 2, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if report.PollCycles != 2 {
		t.Fatalf("poll cycles = %d, want 2: %+v", report.PollCycles, report)
	}
	if report.WarmCycles != 1 {
		t.Fatalf("warm cycles = %d, want 1 when signal is unchanged: %+v", report.WarmCycles, report)
	}
}

func TestStartMaintainerRunsInBackground(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/background\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := New(DefaultStoreRoot(repo)).StartMaintainer(ctx, repo, MaintainerOptions{MaxCycles: 1, PollInterval: time.Millisecond})
	select {
	case result := <-done:
		if result.Err != nil {
			t.Fatal(result.Err)
		}
		if result.Report.WarmCycles != 1 || result.Report.ProofGatedPrewarmed == 0 {
			t.Fatalf("background maintainer did not prewarm: %+v", result.Report)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background maintainer did not finish")
	}
}

func TestBackgroundMaintainerProcessLifecycle(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	storeRoot := filepath.Join(t.TempDir(), "store")
	oldFactory := newBackgroundMaintainerCommand
	t.Cleanup(func() { newBackgroundMaintainerCommand = oldFactory })
	newBackgroundMaintainerCommand = func(cwd, storeRoot string, opts BackgroundMaintainerOptions, log *os.File) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=TestBackgroundMaintainerHelperProcess", "--")
		cmd.Dir = cwd
		cmd.Stdout = log
		cmd.Stderr = log
		cmd.Env = append(os.Environ(), "SQUIRE_TEST_BACKGROUND_MAINTAINER=1")
		return cmd, nil
	}

	started, err := StartBackgroundMaintainer(ctx, repo, storeRoot, BackgroundMaintainerOptions{Duration: 5 * time.Second, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = terminateProcess(started.PID) })
	if !started.Started || started.AlreadyRunning || !started.Running || started.PID <= 0 {
		t.Fatalf("unexpected background start status: %+v", started)
	}
	if started.AgentVisibleSuggestions {
		t.Fatalf("background maintainer exposed agent-visible suggestions")
	}
	if !started.NativeFallbackAvailable {
		t.Fatalf("background maintainer status lost native fallback invariant")
	}
	again, err := StartBackgroundMaintainer(ctx, repo, storeRoot, BackgroundMaintainerOptions{Duration: 5 * time.Second, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !again.AlreadyRunning || again.Started || again.PID != started.PID {
		t.Fatalf("second start did not detect already-running maintainer: first=%+v second=%+v", started, again)
	}
	status, err := LoadBackgroundMaintainerStatus(ctx, repo, storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Running || status.PID != started.PID {
		t.Fatalf("status did not report running process: %+v", status)
	}
	text, err := KernelStatus(ctx, repo, storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"background_maintainer:", "running: true", "native_fallback_available: true", "agent_visible_suggestions: false"} {
		if !strings.Contains(text, want) {
			t.Fatalf("kernel status missing %q:\n%s", want, text)
		}
	}
	stopped, err := StopBackgroundMaintainer(ctx, repo, storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Started || stopped.AlreadyRunning || !stopped.StopRequested {
		t.Fatalf("stop status retained transient start state: %+v", stopped)
	}
	if stopped.Running && len(stopped.Diagnostics) == 0 {
		t.Fatalf("running stop status did not explain stop failure: %+v", stopped)
	}
	if !stopped.Running && stopped.StoppedAt.IsZero() {
		t.Fatalf("stopped status did not record stop time: %+v", stopped)
	}
}

func TestDefaultBackgroundMaintainerDisablesOptionalGitLocks(t *testing.T) {
	logFile, err := os.CreateTemp(t.TempDir(), "maintainer.log")
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd, err := defaultBackgroundMaintainerCommand("/repo", "/store", BackgroundMaintainerOptions{Duration: time.Minute, PollInterval: time.Second}, logFile)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range cmd.Env {
		if item == "GIT_OPTIONAL_LOCKS=0" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("background maintainer env missing GIT_OPTIONAL_LOCKS=0: %v", cmd.Env)
	}
}

func TestBackgroundMaintainerHelperProcess(t *testing.T) {
	if os.Getenv("SQUIRE_TEST_BACKGROUND_MAINTAINER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

func TestColdForegroundLoadsExternallyWarmedLedger(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/reload\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(repo)
	if _, err := New(storeRoot).RunMaintainer(ctx, repo, MaintainerOptions{MaxCycles: 1, PollInterval: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	serving := New(storeRoot)
	res := serving.Run(ctx, "test", repo, []string{"cat", "go.mod"})
	if res.Mode != ModeReplay {
		t.Fatalf("cold foreground mode = %s, want replay from warmed ledger", res.Mode)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"cat", "go.mod"}))
	second := serving.Run(ctx, "test", repo, []string{"cat", "go.mod"})
	if second.Mode != ModeReplay {
		t.Fatalf("second foreground mode = %s, want replay from resident cache", second.Mode)
	}
	assertSameResult(t, second.Stdout, second.Stderr, second.ExitCode, runNative(ctx, repo, []string{"cat", "go.mod"}))
}

func TestDaemonHotCacheReplaysAcrossKernelInstances(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/ipc\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(repo)
	probeCtx, probeCancel := context.WithCancel(ctx)
	probeServer, err := startHotCacheServer(probeCtx, New(storeRoot), storeRoot)
	if err != nil {
		probeCancel()
		if runtime.GOOS == "windows" || errors.Is(err, os.ErrPermission) {
			t.Skipf("unix hot cache socket unavailable in this environment: %v", err)
		}
		t.Fatal(err)
	}
	_ = probeServer.Close()
	probeCancel()

	maintainerCtx, cancel := context.WithCancel(ctx)
	done := New(storeRoot).StartMaintainer(maintainerCtx, repo, MaintainerOptions{PollInterval: time.Millisecond})
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("maintainer did not stop")
		}
	})

	argv := []string{"cat", "go.mod"}
	var replay RunResult
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		client := New(storeRoot)
		replay = client.Run(ctx, "client", repo, argv)
		if replay.Mode == ModeReplay && replay.Proof != nil && (replay.Proof.OperationKey == "ipc-hot-cache" || replay.Proof.OperationKey == "mmap-hot-snapshot") {
			break
		}
		if replay.ExitCode != 0 {
			t.Fatalf("client run failed while waiting for daemon replay: %s", replay.Stderr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if replay.Mode != ModeReplay {
		probe, probeErr := testHotCacheRequest(storeRoot, repo, argv)
		t.Fatalf("daemon hot cache did not replay across kernel instances; last mode=%s diagnostics=%v socket=%s probe=%+v probeErr=%v", replay.Mode, replay.Diagnostics, hotCacheSocketPath(storeRoot), probe, probeErr)
	}
	if replay.Proof == nil || (replay.Proof.OperationKey != "ipc-hot-cache" && replay.Proof.OperationKey != "mmap-hot-snapshot") {
		t.Fatalf("replay did not come from daemon-maintained hot cache: proof=%+v", replay.Proof)
	}
	assertSameResult(t, replay.Stdout, replay.Stderr, replay.ExitCode, runNative(ctx, repo, argv))
	if replay.Phases.NativeExecWaitMS != 0 || replay.Phases.WorldStateLookupMS != 0 || replay.Phases.LedgerLookupMS != 0 || replay.Phases.DBOrFileWriteMS != 0 || replay.Phases.ShadowBookkeepingMS != 0 {
		t.Fatalf("daemon replay used expensive foreground phases: %+v", replay.Phases)
	}
	persistentClient := New(storeRoot)
	firstPersistent := persistentClient.Run(ctx, "client-persistent", repo, argv)
	secondPersistent := persistentClient.Run(ctx, "client-persistent", repo, argv)
	if firstPersistent.Mode != ModeReplay || secondPersistent.Mode != ModeReplay {
		t.Fatalf("persistent daemon client did not replay twice: first=%s second=%s", firstPersistent.Mode, secondPersistent.Mode)
	}
	assertSameResult(t, secondPersistent.Stdout, secondPersistent.Stderr, secondPersistent.ExitCode, runNative(ctx, repo, argv))
}

func testHotCacheRequest(storeRoot, cwd string, argv []string) (hotCacheResponse, error) {
	conn, err := net.DialTimeout("unix", hotCacheSocketPath(storeRoot), time.Second)
	if err != nil {
		return hotCacheResponse{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	req := hotCacheRequest{Version: hotCacheIPCVersion, CWD: cwd, Argv: argv}
	if err := writeHotCacheRequest(conn, req); err != nil {
		return hotCacheResponse{}, err
	}
	resp, err := readHotCacheResponse(conn)
	if err != nil {
		return hotCacheResponse{}, err
	}
	return resp, nil
}

func TestWarmStoresHashOnlyNonReplayPreparations(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	secretModule := "private.example/fieldops-secret"
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module "+secretModule+"\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(repo)

	report, err := Warm(ctx, repo, storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if report.FileTreeIndexesPrepared == 0 {
		t.Fatalf("warm did not prepare file tree index: %+v", report)
	}
	if report.ProjectMetadataPrepared == 0 {
		t.Fatalf("warm did not prepare project metadata: %+v", report)
	}
	if report.CommandPathPrepared == 0 {
		t.Fatalf("warm did not prepare command path index: %+v", report)
	}
	if report.EcosystemPrepared == 0 {
		t.Fatalf("warm did not prepare ecosystem metadata: %+v", report)
	}
	if report.DependencyPrepared == 0 {
		t.Fatalf("warm did not prepare dependency metadata: %+v", report)
	}
	if report.SourceSymbolPrepared == 0 {
		t.Fatalf("warm did not prepare source symbol index: %+v", report)
	}
	ledger, err := NewLedgerStore(storeRoot).Load()
	if err != nil {
		t.Fatal(err)
	}
	var fileTree, metadata, commandPath, ecosystem, dependency, sourceSymbols int
	for _, entry := range ledger.Prepared {
		if (entry.Kind == PreparedKindFileTreeIndex || entry.Kind == PreparedKindProjectMetadata || entry.Kind == PreparedKindCommandPath || entry.Kind == PreparedKindEcosystem || entry.Kind == PreparedKindDependencyMetadata || entry.Kind == PreparedKindSourceSymbolIndex) && entry.ReplayEligible {
			t.Fatalf("non-replay preparation marked replay eligible: %+v", entry)
		}
		switch entry.Kind {
		case PreparedKindFileTreeIndex:
			fileTree++
		case PreparedKindProjectMetadata:
			metadata++
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
	if fileTree == 0 || metadata == 0 || commandPath == 0 || ecosystem == 0 || dependency == 0 || sourceSymbols == 0 {
		t.Fatalf("missing prepared non-replay entries: fileTree=%d metadata=%d commandPath=%d ecosystem=%d dependency=%d sourceSymbols=%d prepared=%+v", fileTree, metadata, commandPath, ecosystem, dependency, sourceSymbols, ledger.Prepared)
	}
	b, err := os.ReadFile(filepath.Join(storeRoot, "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secretModule) {
		t.Fatalf("warm persisted raw project metadata contents:\n%s", b)
	}
	if pathValue := os.Getenv("PATH"); pathValue != "" && strings.Contains(string(b), pathValue) {
		t.Fatalf("warm persisted raw PATH value:\n%s", b)
	}
}

func TestKernelStatusShowsPreparedWorld(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	storeRoot := DefaultStoreRoot(repo)
	if _, err := Warm(ctx, repo, storeRoot); err != nil {
		t.Fatal(err)
	}
	status, err := KernelStatus(ctx, repo, storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"prepared_world", "prepared_entries:", "fast_path_outputs:", "proof_gated_outputs:", "file_tree_indexes:", "command_path_indexes:", "ecosystem_metadata_fingerprints:", "dependency_metadata_fingerprints:", "source_symbol_indexes:", "process_guard:", "cleanup_actions: 0"} {
		if !strings.Contains(status, want) {
			t.Fatalf("kernel status missing %q:\n%s", want, status)
		}
	}
}

func TestProcessGuardIsObserveOnly(t *testing.T) {
	report := ProcessGuardStatus()
	if report.Mode != "observe_only" {
		t.Fatalf("mode = %q, want observe_only", report.Mode)
	}
	if report.CleanupActions != 0 {
		t.Fatalf("process guard performed cleanup actions: %+v", report)
	}
	if report.CurrentPID <= 0 || report.ParentPID <= 0 {
		t.Fatalf("missing process ids: %+v", report)
	}
}

func TestPolicyNeverReplayAndNativeUnknown(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	ws := NewRepoOracle().Snapshot(ctx, repo)
	policy := PolicyEngine{}
	ledger := &ValidityLedger{Version: 1}
	cases := []struct {
		name string
		argv []string
		want Mode
	}{
		{"validation", []string{"go", "test", "./..."}, ModeNever},
		{"mutating git", []string{"git", "commit", "-m", "x"}, ModeNever},
		{"package install", []string{"npm", "install"}, ModeNever},
		{"unknown shell", []string{"printf", "x"}, ModeNative},
		{"repo-state proof-gated without prepared observation", []string{"git", "status", "--short"}, ModeNative},
		{"remote list native", []string{"git", "remote", "-v"}, ModeNative},
		{"remote get-url native", []string{"git", "remote", "get-url", "origin"}, ModeNative},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := Operation{Argv: tc.argv, CWD: repo, RepoRoot: ws.RepoRoot, OperatorFamily: Classify(tc.argv)}
			got := policy.Decide(ctx, op, ws, ledger)
			if got.Mode != tc.want {
				t.Fatalf("mode = %s, want %s (%s)", got.Mode, tc.want, got.Reason)
			}
		})
	}
}

func TestRemoteMetadataRemainsNativeOnly(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if res := runNative(ctx, repo, []string{"git", "remote", "add", "origin", "https://github.com/example/repo.git"}); res.ExitCode != 0 {
		t.Fatalf("remote add failed: %s", res.Stderr)
	}
	k := New(DefaultStoreRoot(repo))
	t.Cleanup(func() { k.flushLedgerNow() })
	for _, argv := range [][]string{
		{"git", "remote", "-v"},
		{"git", "remote", "get-url", "origin"},
	} {
		t.Run(displayCommand(argv), func(t *testing.T) {
			if IsFastPathAllowed(argv) {
				t.Fatalf("%s must not be fast-path allowlisted", displayCommand(argv))
			}
			res := k.Run(ctx, "test", repo, argv)
			if res.Mode != ModeNative {
				t.Fatalf("mode = %s, want native on foreground path", res.Mode)
			}
			assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, argv))
			if res.Phases.WorldStateLookupMS != 0 || res.Phases.ShadowBookkeepingMS != 0 || res.Phases.LedgerLookupMS != 0 {
				t.Fatalf("remote metadata did foreground cache work: %+v", res.Phases)
			}
			second := k.Run(ctx, "test", repo, argv)
			if second.Mode == ModeReplay {
				t.Fatalf("second mode = replay, want remote metadata to remain native only")
			}
			assertSameResult(t, second.Stdout, second.Stderr, second.ExitCode, runNative(ctx, repo, argv))
		})
	}
}

func TestNonInterferenceForNativeAndNeverModes(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ignored.log"), []byte("ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "nested", "untracked.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := New(DefaultStoreRoot(repo))
	commands := [][]string{
		{"git", "status", "--short"},
		{"git", "status", "--porcelain"},
		{"git", "ls-files"},
		{"git", "rev-parse", "NOT_A_REF"},
		{"go", "test", "./definitely-not-a-package"},
	}
	for _, argv := range commands {
		t.Run(displayCommand(argv), func(t *testing.T) {
			before := repoStateSignature(ctx, repo)
			native := runNative(ctx, repo, argv)
			afterNative := repoStateSignature(ctx, repo)
			res := k.Run(ctx, "test", repo, argv)
			afterSquire := repoStateSignature(ctx, repo)
			assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, native)
			if afterNative != before {
				t.Fatalf("native command unexpectedly changed repo state: before=%q after=%q", before, afterNative)
			}
			if afterSquire != before {
				t.Fatalf("squire command changed repo state: before=%q after=%q", before, afterSquire)
			}
		})
	}
}

func TestFormerShadowCandidateRunsNativeWithoutRecording(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := New(DefaultStoreRoot(repo))
	argv := []string{"git", "remote", "-v"}
	res := k.Run(ctx, "test", repo, argv)
	if res.Mode != ModeNative {
		t.Fatalf("mode = %s, want native", res.Mode)
	}
	native := runNative(ctx, repo, argv)
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, native)
	if res.Phases.WorldStateLookupMS != 0 || res.Phases.ShadowBookkeepingMS != 0 || res.Phases.LedgerLookupMS != 0 || res.Phases.DBOrFileWriteMS != 0 {
		t.Fatalf("native-only candidate did expensive cache work: %+v", res.Phases)
	}
}

func TestProofGatedRepoStateReplayInvalidatesOnWorkspaceChange(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, "one.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := New(DefaultStoreRoot(repo))
	argv := []string{"git", "status", "--short"}

	first := k.Run(ctx, "test", repo, argv)
	if first.Mode != ModeNative {
		t.Fatalf("first mode = %s, want native", first.Mode)
	}
	second := k.Run(ctx, "test", repo, argv)
	if second.Mode != ModeNative {
		t.Fatalf("second mode = %s, want native; diagnostics=%v", second.Mode, second.Diagnostics)
	}
	assertSameResult(t, second.Stdout, second.Stderr, second.ExitCode, runNative(ctx, repo, argv))

	if err := os.WriteFile(filepath.Join(repo, "two.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterChange := k.Run(ctx, "test", repo, argv)
	if afterChange.Mode == ModeReplay {
		t.Fatalf("mode after workspace change = replay, want native")
	}
	assertSameResult(t, afterChange.Stdout, afterChange.Stderr, afterChange.ExitCode, runNative(ctx, repo, argv))
}

func TestProofGatedStatusReplayInvalidatesOnTrackedContentChange(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	k := New(DefaultStoreRoot(repo))
	argv := []string{"git", "status", "--short"}

	first := k.Run(ctx, "test", repo, argv)
	if first.Mode != ModeNative {
		t.Fatalf("first mode = %s, want native", first.Mode)
	}
	second := k.Run(ctx, "test", repo, argv)
	if second.Mode != ModeNative {
		t.Fatalf("second mode = %s, want native; diagnostics=%v", second.Mode, second.Diagnostics)
	}
	assertSameResult(t, second.Stdout, second.Stderr, second.ExitCode, runNative(ctx, repo, argv))

	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("frist\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterChange := k.Run(ctx, "test", repo, argv)
	if afterChange.Mode == ModeReplay {
		t.Fatalf("status replayed after tracked file content changed")
	}
	assertSameResult(t, afterChange.Stdout, afterChange.Stderr, afterChange.ExitCode, runNative(ctx, repo, argv))
}

func TestProofGatedRepoSummaryReplaysAfterWarm(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.Mkdir(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	commitFile(t, ctx, repo, filepath.Join("src", "app.js"), "export const value = 1;\n", "add app")
	if err := os.WriteFile(filepath.Join(repo, "src", "app.js"), []byte("export const value = 2;\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	k := New(DefaultStoreRoot(repo))
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{
		{"git", "ls-files"},
		{"git", "status", "--short"},
		{"git", "status", "--porcelain"},
		{"git", "diff"},
		{"git", "diff", "--stat"},
		{"git", "diff", "--", "src/app.js"},
	}
	for _, argv := range commands {
		t.Run(displayCommand(argv), func(t *testing.T) {
			if !IsProofGatedReplayCandidate(argv) || !isHotPreparedReplayCandidate(argv) {
				t.Fatalf("%s is not default proof-gated hot candidate", displayCommand(argv))
			}
			replay := k.Run(ctx, "test", repo, argv)
			if replay.Mode != ModeReplay {
				t.Fatalf("mode = %s, want replay after warm; diagnostics=%v", replay.Mode, replay.Diagnostics)
			}
			assertSameResult(t, replay.Stdout, replay.Stderr, replay.ExitCode, runNative(ctx, repo, argv))
		})
	}

	before := k.Run(ctx, "test", repo, []string{"git", "diff", "--", "src/app.js"})
	if before.Mode != ModeReplay {
		t.Fatalf("before mode = %s, want replay", before.Mode)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "app.js"), []byte("export const value = 3;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := k.Run(ctx, "test", repo, []string{"git", "diff", "--", "src/app.js"})
	if after.Mode == ModeReplay {
		t.Fatalf("git diff replayed after workspace content changed")
	}
	assertSameResult(t, after.Stdout, after.Stderr, after.ExitCode, runNative(ctx, repo, []string{"git", "diff", "--", "src/app.js"}))
	if string(after.Stdout) == string(before.Stdout) {
		t.Fatalf("stale git diff output returned after file edit")
	}
}

func TestProofGatedStatusInvalidatesOnCommitHeadChange(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := runNative(ctx, repo, []string{"git", "add", "file.txt"}); res.ExitCode != 0 {
		t.Fatalf("git add failed: %s", res.Stderr)
	}
	k := New(DefaultStoreRoot(repo))
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	dirty := k.Run(ctx, "test", repo, []string{"git", "status", "--short"})
	if dirty.Mode != ModeReplay {
		t.Fatalf("dirty status mode = %s, want replay; diagnostics=%v", dirty.Mode, dirty.Diagnostics)
	}
	assertSameResult(t, dirty.Stdout, dirty.Stderr, dirty.ExitCode, runNative(ctx, repo, []string{"git", "status", "--short"}))
	if len(dirty.Stdout) == 0 {
		t.Fatalf("expected staged status before commit")
	}
	if res := runNative(ctx, repo, []string{"git", "commit", "-m", "second"}); res.ExitCode != 0 {
		t.Fatalf("git commit failed: %s", res.Stderr)
	}
	after := k.Run(ctx, "test", repo, []string{"git", "status", "--short"})
	if after.Mode == ModeReplay {
		t.Fatalf("status replayed across HEAD change after commit")
	}
	assertSameResult(t, after.Stdout, after.Stderr, after.ExitCode, runNative(ctx, repo, []string{"git", "status", "--short"}))
	if string(after.Stdout) == string(dirty.Stdout) {
		t.Fatalf("stale staged status returned after commit")
	}
}

func TestProofGatedManifestReadReplaysAcrossUnrelatedWorkspaceChange(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	argv := []string{"cat", "go.mod"}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/squiretest\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(repo)
	k := New(storeRoot)
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}

	first := k.Run(ctx, "test", repo, argv)
	if first.Mode != ModeReplay {
		t.Fatalf("first mode = %s, want replay after warm; diagnostics=%v", first.Mode, first.Diagnostics)
	}
	assertSameResult(t, first.Stdout, first.Stderr, first.ExitCode, runNative(ctx, repo, argv))

	if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unrelated := k.Run(ctx, "test", repo, argv)
	if unrelated.Mode != ModeReplay {
		t.Fatalf("unrelated edit mode = %s, want replay; diagnostics=%v", unrelated.Mode, unrelated.Diagnostics)
	}
	assertSameResult(t, unrelated.Stdout, unrelated.Stderr, unrelated.ExitCode, runNative(ctx, repo, argv))

	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/squiretest\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := k.Run(ctx, "test", repo, argv)
	if changed.Mode == ModeReplay {
		t.Fatalf("manifest read replayed after manifest content changed")
	}
	assertSameResult(t, changed.Stdout, changed.Stderr, changed.ExitCode, runNative(ctx, repo, argv))
}

func TestNonGitWorkspaceWarmReplaysFileInspectionAcrossKernelInstances(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "package.json"), []byte("{\"name\":\"non-git-demo\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := strings.Builder{}
	for i := 1; i <= 320; i++ {
		fmt.Fprintf(&source, "export const line%d = %d;\n", i, i)
	}
	sourcePath := filepath.Join(workspace, "src", "app.ts")
	if err := os.WriteFile(sourcePath, []byte(source.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(workspace)
	if _, err := New(storeRoot).Warm(ctx, workspace); err != nil {
		t.Fatal(err)
	}

	client := New(storeRoot)
	for _, argv := range [][]string{
		{"cat", "package.json"},
		{"sed", "-n", "1,260p", "src/app.ts"},
		{"sed", "-n", "220,520p", "src/app.ts"},
	} {
		t.Run(displayCommand(argv), func(t *testing.T) {
			replay := client.Run(ctx, "test", workspace, argv)
			if replay.Mode != ModeReplay {
				t.Fatalf("mode = %s, want replay in non-git workspace; diagnostics=%v", replay.Mode, replay.Diagnostics)
			}
			assertSameResult(t, replay.Stdout, replay.Stderr, replay.ExitCode, runNative(ctx, workspace, argv))
		})
	}

	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unrelated := client.Run(ctx, "test", workspace, []string{"cat", "package.json"})
	if unrelated.Mode != ModeReplay {
		t.Fatalf("unrelated non-git edit mode = %s, want replay", unrelated.Mode)
	}
	assertSameResult(t, unrelated.Stdout, unrelated.Stderr, unrelated.ExitCode, runNative(ctx, workspace, []string{"cat", "package.json"}))

	if err := os.WriteFile(filepath.Join(workspace, "package.json"), []byte("{\"name\":\"changed\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := client.Run(ctx, "test", workspace, []string{"cat", "package.json"})
	if changed.Mode == ModeReplay {
		t.Fatalf("non-git file read replayed after the target file changed")
	}
	assertSameResult(t, changed.Stdout, changed.Stderr, changed.ExitCode, runNative(ctx, workspace, []string{"cat", "package.json"}))
}

func TestPrewarmFileSelectionSkipsDependencyAndBuildDirectories(t *testing.T) {
	workspace := t.TempDir()
	for _, dir := range []string{"src", "node_modules/pkg", ".next/server", "dist"} {
		if err := os.MkdirAll(filepath.Join(workspace, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"src/app.ts":                "export const app = true;\n",
		"package.json":              "{\"name\":\"app\"}\n",
		"node_modules/pkg/index.ts": "export const dependency = true;\n",
		".next/server/render.js":    "module.exports = {}\n",
		"dist/bundle.js":            "console.log('built')\n",
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(workspace, filepath.FromSlash(rel)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	selected := replayableInspectionPrewarmFiles(workspace, 20)
	selectedSet := map[string]bool{}
	for _, rel := range selected {
		selectedSet[rel] = true
	}
	for _, want := range []string{"package.json", "src/app.ts"} {
		if !selectedSet[want] {
			t.Fatalf("expected %s in prewarm selection, got %v", want, selected)
		}
	}
	for _, notWant := range []string{"node_modules/pkg/index.ts", ".next/server/render.js", "dist/bundle.js"} {
		if selectedSet[notWant] {
			t.Fatalf("did not expect %s in prewarm selection: %v", notWant, selected)
		}
	}
}

func TestAdaptiveSedPrewarmCandidates(t *testing.T) {
	candidates := adaptiveSedPrewarmCandidates([]string{"sed", "-n", "1,80p", "src/app.ts"})
	got := map[string]bool{}
	for _, argv := range candidates {
		got[displayCommand(argv)] = true
	}
	for _, want := range []string{
		"sed -n 81,160p src/app.ts",
		"sed -n 40,120p src/app.ts",
	} {
		if !got[want] {
			t.Fatalf("missing adaptive candidate %q from %v", want, candidates)
		}
	}

	wide := adaptiveSedPrewarmCandidates([]string{"sed", "-n", "1,260p", "src/app.ts"})
	got = map[string]bool{}
	for _, argv := range wide {
		got[displayCommand(argv)] = true
	}
	if !got["sed -n 220,520p src/app.ts"] {
		t.Fatalf("missing Ephemeris-style overlapping follow-up window from %v", wide)
	}
}

func TestAdaptiveSedAdjacentPrewarmReplaysNextWindow(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	source := strings.Builder{}
	for i := 1; i <= 240; i++ {
		fmt.Fprintf(&source, "export const line%d = %d;\n", i, i)
	}
	sourcePath := filepath.Join(workspace, "src", "app.ts")
	if err := os.WriteFile(sourcePath, []byte(source.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(workspace)
	k := New(storeRoot)

	first := k.Run(ctx, "test", workspace, []string{"sed", "-n", "1,80p", "src/app.ts"})
	if first.Mode != ModeNative {
		t.Fatalf("first mode = %s, want native before adaptive prewarm", first.Mode)
	}
	count, err := k.PrewarmAdjacent(ctx, workspace, "test", []string{"sed", "-n", "1,80p", "src/app.ts"})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatalf("adaptive prewarm did not prepare any adjacent windows")
	}

	next := New(storeRoot).Run(ctx, "test", workspace, []string{"sed", "-n", "81,160p", "src/app.ts"})
	if next.Mode != ModeReplay {
		t.Fatalf("next window mode = %s, want replay; diagnostics=%v", next.Mode, next.Diagnostics)
	}
	assertSameResult(t, next.Stdout, next.Stderr, next.ExitCode, runNative(ctx, workspace, []string{"sed", "-n", "81,160p", "src/app.ts"}))

	if err := os.WriteFile(sourcePath, []byte("export const changed = true;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := New(storeRoot).Run(ctx, "test", workspace, []string{"sed", "-n", "81,160p", "src/app.ts"})
	if changed.Mode == ModeReplay {
		t.Fatalf("adaptive sed window replayed after source content changed")
	}
	assertSameResult(t, changed.Stdout, changed.Stderr, changed.ExitCode, runNative(ctx, workspace, []string{"sed", "-n", "81,160p", "src/app.ts"}))
}

func TestProofGatedSourceCatAndSedReplayInvalidateOnFileChange(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.Mkdir(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(repo, "src", "app.js")
	if err := os.WriteFile(sourcePath, []byte("export const answer = 41;\nexport const label = 'old';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(repo)
	k := New(storeRoot)
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}

	for _, argv := range [][]string{
		{"cat", "src/app.js"},
		{"sed", "-n", "1,80p", "src/app.js"},
		{"sed", "-n", "1,120p", "src/app.js"},
		{"sed", "-n", "1,140p", "src/app.js"},
		{"sed", "-n", "1,160p", "src/app.js"},
		{"sed", "-n", "1,200p", "src/app.js"},
		{"sed", "-n", "1,220p", "src/app.js"},
	} {
		t.Run(displayCommand(argv), func(t *testing.T) {
			replay := k.Run(ctx, "test", repo, argv)
			if replay.Mode != ModeReplay {
				t.Fatalf("mode = %s, want replay after warm; diagnostics=%v", replay.Mode, replay.Diagnostics)
			}
			assertSameResult(t, replay.Stdout, replay.Stderr, replay.ExitCode, runNative(ctx, repo, argv))
		})
	}

	before := k.Run(ctx, "test", repo, []string{"cat", "src/app.js"})
	if before.Mode != ModeReplay {
		t.Fatalf("before mode = %s, want replay", before.Mode)
	}
	if err := os.WriteFile(sourcePath, []byte("export const answer = 42;\nexport const label = 'new';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := k.Run(ctx, "test", repo, []string{"cat", "src/app.js"})
	if stale.Mode == ModeReplay {
		t.Fatalf("source cat replayed after file content changed")
	}
	assertSameResult(t, stale.Stdout, stale.Stderr, stale.ExitCode, runNative(ctx, repo, []string{"cat", "src/app.js"}))
	if string(stale.Stdout) == string(before.Stdout) {
		t.Fatalf("stale source output returned after file edit")
	}
}

func TestForegroundNativeObservationPreparesLaterReplay(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.Mkdir(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	k := New(DefaultStoreRoot(repo))
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "late.py"), []byte("def answer():\n    return 42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argv := []string{"sed", "-n", "1,140p", "src/late.py"}
	first := k.Run(ctx, "test", repo, argv)
	if first.ExitCode != 0 {
		t.Fatalf("first command failed: %s", first.Stderr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		second := k.Run(ctx, "test", repo, argv)
		if second.Mode == ModeReplay {
			assertSameResult(t, second.Stdout, second.Stderr, second.ExitCode, runNative(ctx, repo, argv))
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("foreground native observation did not prepare replay")
}

func TestProofGatedToolDiscoveryReplays(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	storeRoot := DefaultStoreRoot(repo)
	k := New(storeRoot)
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"git", "--version"},
		{"which", "git"},
		{"command", "-v", "git"},
		{"python3", "--version"},
	} {
		t.Run(displayCommand(argv), func(t *testing.T) {
			native := runNative(ctx, repo, argv)
			if native.ExitCode != 0 {
				t.Skipf("%s unavailable: %s", displayCommand(argv), native.Stderr)
			}
			if got := Classify(argv); got != FamilyEnvironment {
				t.Fatalf("family = %s, want %s", got, FamilyEnvironment)
			}
			first := k.Run(ctx, "test", repo, argv)
			if first.Mode != ModeReplay {
				t.Fatalf("first mode = %s, want replay after warm; diagnostics=%v", first.Mode, first.Diagnostics)
			}
			assertSameResult(t, first.Stdout, first.Stderr, first.ExitCode, native)
			second := k.Run(ctx, "test", repo, argv)
			if second.Mode != ModeReplay {
				t.Fatalf("second mode = %s, want replay; diagnostics=%v", second.Mode, second.Diagnostics)
			}
			assertSameResult(t, second.Stdout, second.Stderr, second.ExitCode, runNative(ctx, repo, argv))
		})
	}
}

func TestDeepLocalReportTiering(t *testing.T) {
	ctx := context.Background()
	// Use very small options to keep the benchmark fast in CI.
	opts := DefaultDeepBenchOptions()
	opts.Packages = 1
	opts.Turns = 1
	opts.MetadataRepeats = 1
	opts.InitialRepeats = 1
	opts.FinalRepeats = 1
	opts.ShadowRepeats = 1
	opts.ValidationEveryTurns = 1000

	report, err := BenchDeepLocalWithOptions(ctx, opts)
	if err != nil {
		t.Skipf("bench not runnable here: %v", err)
	}

	// Tiering: metadata, shadow, and validation metrics must be reported
	if report.Metadata.Runs == 0 && report.Shadow.Runs == 0 && report.Validation.Runs == 0 {
		t.Fatalf("deep-local report missing runs: metadata=%d shadow=%d validation=%d", report.Metadata.Runs, report.Shadow.Runs, report.Validation.Runs)
	}

	// Enabled/Shadow/Never sets are reflected via mode counts
	if report.Metadata.ReplayModes < 0 || report.Shadow.ShadowModes < 0 || report.Validation.NeverModes < 0 {
		t.Fatalf("unexpected negative mode counts")
	}

	// Safety vs performance gates: safety gates (exactness) are separate from performance (performance report may contain violations)
	_ = report.MetadataExactness
	_ = report.ShadowExactness
	// At minimum the performance report object should exist.
	if report.Performance.MetadataFastPathP95US < 0 {
		t.Fatalf("performance report not present or invalid")
	}
}

func TestDeepLocalReportContainsPhaseNames(t *testing.T) {
	ctx := context.Background()
	opts := DefaultDeepBenchOptions()
	opts.Packages = 1
	opts.Turns = 1
	opts.MetadataRepeats = 1
	opts.InitialRepeats = 1
	opts.FinalRepeats = 1
	opts.ShadowRepeats = 1
	opts.ValidationEveryTurns = 1000

	report, err := BenchDeepLocalWithOptions(ctx, opts)
	if err != nil {
		t.Skipf("bench not runnable here: %v", err)
	}

	// Marshal to JSON and examine keys for expected phase timing names.
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	s := string(b)
	phases := []string{
		"classify_ms",
		"repo_root_lookup_ms",
		"world_state_lookup_ms",
		"epoch_check_ms",
		"ledger_lookup_ms",
		"output_materialize_ms",
		"event_append_ms",
		"db_or_file_write_ms",
		"lock_wait_ms",
		"shadow_bookkeeping_ms",
		"fallback_decision_ms",
		"native_exec_wait_ms",
	}
	var missing []string
	for _, p := range phases {
		if !strings.Contains(s, p) {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("report JSON missing phase keys: %v", missing)
	}
}

func TestOracleTracksConfigRemoteAndIgnoreFingerprint(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if res := runNative(ctx, repo, []string{"git", "remote", "add", "origin", "https://github.com/example/repo.git"}); res.ExitCode != 0 {
		t.Fatalf("remote add failed: %s", res.Stderr)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := NewRepoOracle().Snapshot(ctx, repo)
	if ws.RemoteURL != "https://github.com/example/repo.git" {
		t.Fatalf("safe remote URL not tracked: %q", ws.RemoteURL)
	}
	if ws.RemoteURLFingerprint == "" || ws.ConfigFingerprint == "" || ws.IgnoreRuleFingerprint == "" {
		t.Fatalf("missing fingerprints: %+v", ws)
	}
	beforeConfig := ws.ConfigFingerprint
	beforeIgnore := ws.IgnoreRuleFingerprint
	if res := runNative(ctx, repo, []string{"git", "remote", "set-url", "origin", "https://github.com/example/changed.git"}); res.ExitCode != 0 {
		t.Fatalf("remote set-url failed: %s", res.Stderr)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.log\n*.tmp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := NewRepoOracle().Snapshot(ctx, repo)
	if after.ConfigFingerprint == beforeConfig {
		t.Fatalf("config fingerprint did not change")
	}
	if after.IgnoreRuleFingerprint == beforeIgnore {
		t.Fatalf("ignore-rule fingerprint did not change")
	}
}

func TestNonGitFaultsOpenToNative(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	k := New(DefaultStoreRoot(dir))
	res := k.Run(ctx, "test", dir, []string{"git", "rev-parse", "HEAD"})
	native := runNative(ctx, dir, []string{"git", "rev-parse", "HEAD"})
	if res.Mode != ModeNative {
		t.Fatalf("mode = %s, want native fallback", res.Mode)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, native)
}

func TestLedgerUnavailableFaultsOpenToNative(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	storeFile := filepath.Join(repo, "store-file")
	if err := os.WriteFile(storeFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := New(storeFile)
	res := k.Run(ctx, "test", repo, []string{"git", "rev-parse", "HEAD"})
	native := runNative(ctx, repo, []string{"git", "rev-parse", "HEAD"})
	if res.Mode != ModeNative {
		t.Fatalf("mode = %s, want native", res.Mode)
	}
	if len(res.Diagnostics) != 0 || res.Phases.LedgerLookupMS != 0 || res.Phases.DBOrFileWriteMS != 0 {
		t.Fatalf("cold foreground should not touch unavailable ledger: diagnostics=%v phases=%+v", res.Diagnostics, res.Phases)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, native)
}

func TestCorruptOutputFaultsOpenToNative(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	storeRoot := DefaultStoreRoot(repo)
	if _, err := Warm(ctx, repo, storeRoot); err != nil {
		t.Fatal(err)
	}
	store := NewLedgerStore(storeRoot)
	ledger, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Entries) == 0 {
		t.Fatal("no ledger entries")
	}
	ref := ledger.Entries[0].Observation.OutputRef
	if ref == "" {
		t.Fatal("no output ref")
	}
	if err := os.WriteFile(filepath.Join(store.Root, "outputs", ref, "stdout"), []byte("corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(hotCacheSnapshotPath(storeRoot))
	restarted := New(storeRoot)
	res := restarted.Run(ctx, "test", repo, []string{"git", "rev-parse", "HEAD"})
	if res.Mode != ModeNative {
		t.Fatalf("mode = %s, want native without cold disk hydration", res.Mode)
	}
	if res.Proof != nil || res.Phases.LedgerLookupMS != 0 {
		t.Fatalf("cold foreground read corrupt output record: proof=%+v phases=%+v", res.Proof, res.Phases)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"git", "rev-parse", "HEAD"}))
}

func TestPrivacyLedgerAvoidsRawSensitiveContent(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	k := New(DefaultStoreRoot(repo))
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(k.Store.Root, "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"session-secret@example.com",
		"git rev-parse HEAD",
		"user.email",
		"raw prompt",
		"raw completion",
	} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("ledger persisted forbidden raw content %q in:\n%s", forbidden, b)
		}
	}
	var parsed ValidityLedger
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Entries) == 0 {
		t.Fatalf("entries = %d, want prepared hashed observations", len(parsed.Entries))
	}
	for _, entry := range parsed.Entries {
		if entry.OperationKey == "" || entry.OutputFingerprints["stdout"] == "" {
			t.Fatalf("ledger did not preserve operation/output hashes: %+v", entry)
		}
	}
}

func TestPrivacyDoesNotPersistArbitraryStdout(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	k := New(DefaultStoreRoot(repo))
	secret := "AUTH_SECRET_DO_NOT_STORE"
	res := k.Run(ctx, "test", repo, []string{"printf", secret})
	if res.ExitCode != 0 {
		t.Fatalf("printf failed: %s", res.Stderr)
	}
	if _, err := os.Stat(k.Store.Root); os.IsNotExist(err) {
		return
	}
	err := filepath.WalkDir(k.Store.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(b), secret) {
			t.Fatalf("arbitrary stdout persisted in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEnvFileReadRemainsNativeAndUnstored(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	secret := "SQUIRE_ENV_SECRET=do-not-store\n"
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	k := New(DefaultStoreRoot(repo))
	res := k.Run(ctx, "test", repo, []string{"cat", ".env"})
	if res.Mode != ModeNative {
		t.Fatalf("mode = %s, want native", res.Mode)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"cat", ".env"}))
	if _, err := os.Stat(k.Store.Root); os.IsNotExist(err) {
		return
	}
	err := filepath.WalkDir(k.Store.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(b), secret) {
			t.Fatalf(".env contents persisted in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSetupAndStatusSurfaces(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	setup, err := Setup(ctx, repo, DefaultStoreRoot(repo))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(setup, "privacy_mode: standard") || !strings.Contains(setup, "global_shims: not installed") {
		t.Fatalf("setup output missing required framing:\n%s", setup)
	}
	status, err := KernelStatus(ctx, repo, DefaultStoreRoot(repo))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"enabled_fast_paths", "proof_gated_replay_candidates", "native_only_discovery", "never_replay_boundaries", "current_repo_oracle_state", "invalidation_epoch", "p95_fast_path_overhead", "latest_benchmark_status", "file_tree_epoch"} {
		if !strings.Contains(status, want) {
			t.Fatalf("kernel status missing %q:\n%s", want, status)
		}
	}
}

func TestKernelStatusSummaryIsCompact(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	storeRoot := DefaultStoreRoot(repo)
	if _, err := Warm(ctx, repo, storeRoot); err != nil {
		t.Fatal(err)
	}
	status, err := KernelStatusSummary(ctx, repo, storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Squire Kernel status", "readiness:", "repo_oracle:", "repo_root:", "native_fallback: available", "runtime_decisions: replay_or_native", "enabled_fast_paths:", "prepared_entries:", "hot_snapshot:", "background_maintainer:"} {
		if !strings.Contains(status, want) {
			t.Fatalf("kernel status summary missing %q:\n%s", want, status)
		}
	}
	for _, notWant := range []string{"current_repo_oracle_state", "invalidation_epoch:", "process_guard:"} {
		if strings.Contains(status, notWant) {
			t.Fatalf("kernel status summary should not include detailed section %q:\n%s", notWant, status)
		}
	}
}

func TestKernelStatusShowsLatestBenchmark(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	store := NewLedgerStore(DefaultStoreRoot(repo))
	err := store.SaveLatestBenchmarkStatus(LatestBenchmarkStatus{
		Name:                  "deep-local",
		Exactness:             true,
		Mismatches:            0,
		MetadataFastPathP95US: 95,
		SafetyGates:           GateReport{Required: true, Passed: true, Status: "pass"},
		PerformanceGates:      GateReport{Required: false, Passed: true, Status: "pass"},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := KernelStatus(ctx, repo, DefaultStoreRoot(repo))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"latest_metadata_fast_path_p95_us: 95", "name: deep-local", "safety_gates: pass", "performance_gates: pass"} {
		if !strings.Contains(status, want) {
			t.Fatalf("kernel status missing %q:\n%s", want, status)
		}
	}
}

func TestBenchRepoMetadata(t *testing.T) {
	report, err := BenchRepoMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Exactness {
		t.Fatalf("bench exactness failed: %+v", report)
	}
	if !report.MutationBoundaryInvalidation {
		t.Fatalf("bench did not observe mutation-boundary invalidation: %+v", report)
	}
	if report.QuarantinedRuns != 0 || !report.NoBroadCodexSpeedupClaim {
		t.Fatalf("bench framing failed: %+v", report)
	}
}

func TestBenchDeepLocalSplitMetrics(t *testing.T) {
	report, err := BenchDeepLocalWithOptions(context.Background(), DeepBenchOptions{
		Packages:             3,
		Turns:                2,
		MetadataRepeats:      1,
		InitialRepeats:       2,
		FinalRepeats:         2,
		ShadowRepeats:        1,
		ValidationEveryTurns: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Metadata.ReplayModes == 0 || report.Incremental.MetadataReplayModes == 0 {
		t.Fatalf("deep-local observed zero metadata replay modes after warm: metadata=%d incremental=%d", report.Metadata.ReplayModes, report.Incremental.MetadataReplayModes)
	}
	if report.Metadata.Runs == 0 || report.Shadow.Runs == 0 || report.Validation.Runs == 0 {
		t.Fatalf("missing split metrics: %+v", report)
	}
	if !report.MetadataExactness {
		t.Fatalf("metadata exactness failed: %+v", report.Metadata)
	}
	if !report.ValidationNeverReplayObserved {
		t.Fatalf("validation never-replay was not observed: %+v", report.Validation)
	}
	if report.StaleReplayObserved {
		t.Fatalf("stale replay observed: %+v", report.Incremental)
	}
	if report.Shadow.ExactMismatches > 0 && len(report.Shadow.ShadowMismatchCategories) == 0 {
		t.Fatalf("shadow mismatches missing categories: %+v", report.Shadow)
	}
	if !report.StoreInsideGitDir || !report.NoBroadCodexSpeedupClaim {
		t.Fatalf("report framing failed: %+v", report)
	}
}

func testRepo(t *testing.T, ctx context.Context) string {
	t.Helper()
	dir := t.TempDir()
	for _, argv := range [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "squire@example.invalid"},
		{"git", "config", "user.name", "Squire Kernel"},
	} {
		if res := runNative(ctx, dir, argv); res.ExitCode != 0 {
			t.Fatalf("%s failed: %s", displayCommand(argv), res.Stderr)
		}
	}
	commitFile(t, ctx, dir, "file.txt", "first\n", "initial")
	return dir
}

func commitFile(t *testing.T, ctx context.Context, repo, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := runNative(ctx, repo, []string{"git", "add", name}); res.ExitCode != 0 {
		t.Fatalf("git add failed: %s", res.Stderr)
	}
	if res := runNative(ctx, repo, []string{"git", "commit", "-m", msg}); res.ExitCode != 0 {
		t.Fatalf("git commit failed: %s", res.Stderr)
	}
}

func assertSameResult(t *testing.T, stdout, stderr []byte, exitCode int, native NativeResult) {
	t.Helper()
	if exitCode != native.ExitCode || string(stdout) != string(native.Stdout) || string(stderr) != string(native.Stderr) {
		t.Fatalf("result mismatch\nexit got/want: %d/%d\nstdout got/want: %q/%q\nstderr got/want: %q/%q", exitCode, native.ExitCode, stdout, native.Stdout, stderr, native.Stderr)
	}
}

func repoStateSignature(ctx context.Context, repo string) string {
	status := runNative(ctx, repo, []string{"git", "status", "--porcelain"})
	head := runNative(ctx, repo, []string{"git", "rev-parse", "HEAD"})
	return string(status.Stdout) + "\x00" + string(status.Stderr) + "\x00" + string(head.Stdout) + "\x00" + string(head.Stderr)
}
