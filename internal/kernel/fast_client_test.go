package kernel

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFastHotClientReplayUsesHotSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	storeRoot := DefaultStoreRoot(repo)
	k := New(storeRoot)
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}

	res, ok := FastHotClientReplay(ctx, "test", repo, storeRoot, []string{"git", "rev-parse", "HEAD"})
	if !ok {
		t.Fatal("fast hot client did not replay warmed metadata snapshot")
	}
	if res.Mode != ModeReplay {
		t.Fatalf("mode = %s, want replay", res.Mode)
	}
	if res.Proof == nil || res.Proof.OperationKey != "cli-mmap-hot-snapshot" {
		t.Fatalf("proof = %+v, want cli mmap hot snapshot", res.Proof)
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"git", "rev-parse", "HEAD"}))
	if res.Phases.NativeExecWaitMS != 0 || res.Phases.LedgerLookupMS != 0 || res.Phases.DBOrFileWriteMS != 0 || res.Phases.LockWaitMS != 0 {
		t.Fatalf("fast client used heavyweight phases: %+v", res.Phases)
	}
}

func TestFastHotClientReplayMissesWithoutSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	storeRoot := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(storeRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	res, ok := FastHotClientReplay(ctx, "test", repo, storeRoot, []string{"git", "rev-parse", "HEAD"})
	if ok || res != nil {
		t.Fatalf("fast client replayed without snapshot: ok=%t res=%+v", ok, res)
	}

	k := New(storeRoot)
	native := k.Run(ctx, "test", repo, []string{"git", "rev-parse", "HEAD"})
	if native.ExitCode != 0 {
		t.Fatalf("native fallback failed: %s", native.Stderr)
	}
	if native.Mode != ModeNative {
		t.Fatalf("mode = %s, want native fallback", native.Mode)
	}
	assertSameResult(t, native.Stdout, native.Stderr, native.ExitCode, runNative(ctx, repo, []string{"git", "rev-parse", "HEAD"}))
}
