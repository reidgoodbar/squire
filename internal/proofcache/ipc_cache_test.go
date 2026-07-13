package proofcache

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestHotCacheBinaryRequestRoundTrip(t *testing.T) {
	req := hotCacheRequest{
		Version: hotCacheIPCVersion,
		CWD:     "/tmp/example",
		Argv:    []string{"git", "rev-parse", "HEAD"},
	}
	var buf bytes.Buffer
	if err := writeHotCacheRequest(&buf, req); err != nil {
		t.Fatal(err)
	}
	got, err := readHotCacheRequest(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != req.Version || got.CWD != req.CWD || len(got.Argv) != len(req.Argv) {
		t.Fatalf("decoded request = %+v, want %+v", got, req)
	}
	for i := range req.Argv {
		if got.Argv[i] != req.Argv[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, got.Argv[i], req.Argv[i])
		}
	}
}

func TestHotCacheBinaryHitFrameRoundTrip(t *testing.T) {
	stdout := []byte("abc\n")
	stderr := []byte("warning\n")
	frame := encodeHotCacheHitFrame(stdout, stderr, 7)
	got, err := readHotCacheResponse(bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Hit || got.Version != hotCacheIPCVersion || got.ExitCode != 7 {
		t.Fatalf("decoded response metadata = %+v", got)
	}
	if !bytes.Equal(got.Stdout, stdout) || !bytes.Equal(got.Stderr, stderr) {
		t.Fatalf("decoded response bytes stdout=%q stderr=%q", got.Stdout, got.Stderr)
	}
	if got.StdoutHash != hashBytes(stdout) || got.StderrHash != hashBytes(stderr) {
		t.Fatalf("decoded hashes stdout=%q stderr=%q", got.StdoutHash, got.StderrHash)
	}
}

func TestHotCacheRejectsCorruptResponseFrame(t *testing.T) {
	frame := encodeHotCacheHitFrame([]byte("abc\n"), nil, 0)
	frame[0] = 0
	if _, err := readHotCacheResponse(bytes.NewReader(frame)); err == nil {
		t.Fatalf("expected corrupt frame to be rejected")
	}
}

func TestHotCacheUnavailableIsSessionCached(t *testing.T) {
	oldDial := hotCacheDialContext
	t.Cleanup(func() { hotCacheDialContext = oldDial })
	var dials int
	hotCacheDialContext = func(ctx context.Context, path string) (net.Conn, error) {
		dials++
		return nil, errors.New("daemon unavailable")
	}

	k := New(filepath.Join(t.TempDir(), "store"))
	inv := NormalizeInvocation(t.TempDir(), []string{"cat", "go.mod"})
	if _, ok := k.tryDaemonReplay(context.Background(), inv, Classify(inv.PolicyArgv), &PhaseTimings{}); ok {
		t.Fatalf("unexpected replay on unavailable daemon")
	}
	if _, ok := k.tryDaemonReplay(context.Background(), inv, Classify(inv.PolicyArgv), &PhaseTimings{}); ok {
		t.Fatalf("unexpected replay on cached unavailable daemon")
	}
	if dials != 1 {
		t.Fatalf("dials = %d, want 1 due to session-local unavailable cache", dials)
	}
}

func TestHotCacheMissIsSessionCached(t *testing.T) {
	oldDial := hotCacheDialContext
	t.Cleanup(func() { hotCacheDialContext = oldDial })
	var dials int
	hotCacheDialContext = func(ctx context.Context, path string) (net.Conn, error) {
		dials++
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			_, _ = readHotCacheRequest(server)
			_, _ = server.Write(encodeHotCacheMissFrame())
		}()
		return client, nil
	}

	k := New(filepath.Join(t.TempDir(), "store"))
	inv := NormalizeInvocation(t.TempDir(), []string{"cat", "go.mod"})
	if _, ok := k.tryDaemonReplay(context.Background(), inv, Classify(inv.PolicyArgv), &PhaseTimings{}); ok {
		t.Fatalf("unexpected replay on daemon miss")
	}
	if _, ok := k.tryDaemonReplay(context.Background(), inv, Classify(inv.PolicyArgv), &PhaseTimings{}); ok {
		t.Fatalf("unexpected replay on cached daemon miss")
	}
	if dials != 1 {
		t.Fatalf("dials = %d, want 1 due to session-local miss cache", dials)
	}
}

func TestHotSnapshotReplaysWithoutSocket(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/snapshot\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(repo)
	if _, err := New(storeRoot).Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hotCacheSnapshotPath(storeRoot)); err != nil {
		t.Fatalf("hot snapshot was not published: %v", err)
	}

	client := New(storeRoot)
	res := client.Run(ctx, "test", repo, []string{"cat", "go.mod"})
	if res.Mode != ModeReplay {
		t.Fatalf("mode = %s, want replay; diagnostics=%v", res.Mode, res.Diagnostics)
	}
	if res.Proof == nil || res.Proof.OperationKey != "mmap-hot-snapshot" {
		t.Fatalf("proof = %+v, want mmap-hot-snapshot", res.Proof)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"cat", "go.mod"}))
	if res.Phases.NativeExecWaitMS != 0 || res.Phases.LedgerLookupMS != 0 || res.Phases.DBOrFileWriteMS != 0 {
		t.Fatalf("mmap replay used expensive foreground phases: %+v", res.Phases)
	}
}

func TestHotSnapshotDoesNotReplayRepoSummaryAcrossCWD(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	subdir := filepath.Join(repo, "src")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	commitFile(t, ctx, repo, filepath.Join("src", "app.js"), "export const value = 1;\n", "add app")
	if err := os.WriteFile(filepath.Join(subdir, "app.js"), []byte("export const value = 2;\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	argv := []string{"git", "status", "--short"}
	rootNative := runNative(ctx, repo, argv)
	subdirNative := runNative(ctx, subdir, argv)
	if rootNative.ExitCode != 0 || subdirNative.ExitCode != 0 {
		t.Fatalf("native status failed: root=%q subdir=%q", rootNative.Stderr, subdirNative.Stderr)
	}
	if string(rootNative.Stdout) == string(subdirNative.Stdout) {
		t.Skipf("native status output is not cwd-sensitive in this environment: %q", rootNative.Stdout)
	}

	storeRoot := DefaultStoreRoot(repo)
	if _, err := New(storeRoot).Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	client := New(storeRoot)
	rootReplay := client.Run(ctx, "test", repo, argv)
	if rootReplay.Mode != ModeReplay {
		t.Fatalf("root mode = %s, want replay; diagnostics=%v", rootReplay.Mode, rootReplay.Diagnostics)
	}
	assertSameResult(t, rootReplay.Stdout, rootReplay.Stderr, rootReplay.ExitCode, rootNative)

	subdirResult := client.Run(ctx, "test", subdir, argv)
	assertSameResult(t, subdirResult.Stdout, subdirResult.Stderr, subdirResult.ExitCode, subdirNative)
	if subdirResult.Mode == ModeReplay && string(subdirResult.Stdout) == string(rootNative.Stdout) {
		t.Fatalf("hot snapshot replayed root-relative status in subdir: got %q want %q", subdirResult.Stdout, subdirNative.Stdout)
	}
}

func TestHotSnapshotStaleEpochFallsBackNative(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	path := filepath.Join(repo, "go.mod")
	if err := os.WriteFile(path, []byte("module example.com/snapshot\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(repo)
	if _, err := New(storeRoot).Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("module example.com/snapshot\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := New(storeRoot)
	res := client.Run(ctx, "test", repo, []string{"cat", "go.mod"})
	if res.Mode == ModeReplay {
		t.Fatalf("stale hot snapshot replayed after file change")
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"cat", "go.mod"}))
	if !bytes.Contains(res.Stdout, []byte("go 1.23")) {
		t.Fatalf("expected fresh native output after stale snapshot miss, got %q", res.Stdout)
	}
}

func TestHotSnapshotCorruptFallsBackToDaemonIPC(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/socketfallback\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(repo)
	daemon := New(storeRoot)
	if _, err := daemon.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	server, err := startHotCacheServer(ctx, daemon, storeRoot)
	if err != nil {
		t.Skipf("hot cache socket unavailable in this environment: %v", err)
	}
	defer server.Close()
	if err := os.WriteFile(hotCacheSnapshotPath(storeRoot), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := New(storeRoot)
	res := client.Run(ctx, "test", repo, []string{"cat", "go.mod"})
	if res.Mode != ModeReplay {
		t.Fatalf("mode = %s, want daemon replay after corrupt snapshot fallback; diagnostics=%v", res.Mode, res.Diagnostics)
	}
	if res.Proof == nil || res.Proof.OperationKey != "ipc-hot-cache" {
		t.Fatalf("proof = %+v, want ipc-hot-cache fallback", res.Proof)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"cat", "go.mod"}))
}

func TestHotSnapshotRejectsTruncatedFrame(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/truncated\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeRoot := DefaultStoreRoot(repo)
	if _, err := New(storeRoot).Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	path := hotCacheSnapshotPath(storeRoot)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < hotSnapshotHeaderBytes+1 {
		t.Fatalf("snapshot too small for truncation test: %d", len(data))
	}
	if err := os.WriteFile(path, data[:hotSnapshotHeaderBytes], 0o600); err != nil {
		t.Fatal(err)
	}
	inv := NormalizeInvocation(repo, []string{"cat", "go.mod"})
	if _, ok := readHotSnapshotResponse(path, inv); ok {
		t.Fatalf("truncated hot snapshot was accepted")
	}
}
