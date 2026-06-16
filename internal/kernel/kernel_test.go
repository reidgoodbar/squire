package kernel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFastPathReplayExactAndHeadInvalidates(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	k := New(DefaultStoreRoot(repo))

	first := k.Run(ctx, "test", repo, []string{"git", "rev-parse", "HEAD"})
	if first.ExitCode != 0 {
		t.Fatalf("first native failed: %s", first.Stderr)
	}
	if first.Mode != ModeNative {
		t.Fatalf("first run mode = %s, want native", first.Mode)
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

	first := k.Run(ctx, "test", repo, []string{"git", "rev-parse", "--abbrev-ref", "HEAD"})
	if first.ExitCode != 0 {
		t.Fatalf("branch native failed: %s", first.Stderr)
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

	first := k.Run(ctx, "test", repo, []string{"git", "rev-parse", "--git-dir"})
	if first.ExitCode != 0 {
		t.Fatalf("git-dir native failed: %s", first.Stderr)
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
		{"shadow", []string{"git", "status", "--short"}, ModeShadow},
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

func TestNonInterferenceForNativeShadowAndNeverModes(t *testing.T) {
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

func TestShadowRunsNativeAndRecordsExactness(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := New(DefaultStoreRoot(repo))
	res := k.Run(ctx, "test", repo, []string{"git", "status", "--porcelain"})
	if res.Mode != ModeShadow {
		t.Fatalf("mode = %s, want shadow", res.Mode)
	}
	native := runNative(ctx, repo, []string{"git", "status", "--porcelain"})
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, native)
	ledger, err := k.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	var matches int
	for _, entry := range ledger.Entries {
		matches += entry.ShadowMatchCount
		if entry.ReplacementCount != 0 {
			t.Fatalf("shadow entry replaced command: %+v", entry)
		}
	}
	if matches == 0 {
		t.Fatalf("shadow match was not recorded")
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
	if len(res.Diagnostics) == 0 {
		t.Fatalf("missing ledger-unavailable diagnostic")
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, native)
}

func TestCorruptOutputFaultsOpenToNative(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	k := New(DefaultStoreRoot(repo))
	first := k.Run(ctx, "test", repo, []string{"git", "rev-parse", "HEAD"})
	if first.ExitCode != 0 {
		t.Fatalf("native failed: %s", first.Stderr)
	}
	ledger, err := k.Store.Load()
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
	if err := os.WriteFile(filepath.Join(k.Store.Root, "outputs", ref, "stdout"), []byte("corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := k.Run(ctx, "test", repo, []string{"git", "rev-parse", "HEAD"})
	if res.Mode != ModeNative {
		t.Fatalf("mode = %s, want native fallback", res.Mode)
	}
	if res.Proof == nil || res.Proof.OutputExact {
		t.Fatalf("missing failed proof: %+v", res.Proof)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"git", "rev-parse", "HEAD"}))
}

func TestPrivacyLedgerAvoidsRawSensitiveContent(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	k := New(DefaultStoreRoot(repo))
	_ = k.Run(ctx, "session-secret@example.com", repo, []string{"git", "rev-parse", "HEAD"})
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
	if len(parsed.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(parsed.Entries))
	}
	if parsed.Entries[0].OperationKey == "" || parsed.Entries[0].OutputFingerprints["stdout"] == "" {
		t.Fatalf("ledger did not preserve operation/output hashes: %+v", parsed.Entries[0])
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
	for _, want := range []string{"enabled_fast_paths", "shadow_candidates", "never_replay_policy", "file_tree_epoch"} {
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
