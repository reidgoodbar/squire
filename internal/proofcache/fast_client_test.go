package proofcache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestEngineFastReplayUsesCachedHotSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	storeRoot := DefaultStoreRoot(repo)
	k := New(storeRoot)
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		res, ok := k.FastReplay(ctx, "test", repo, []string{"git", "rev-parse", "HEAD"})
		if !ok {
			t.Fatalf("fast replay miss on iteration %d", i)
		}
		if res.Mode != ModeReplay {
			t.Fatalf("mode = %s, want replay", res.Mode)
		}
		if res.Proof == nil || res.Proof.OperationKey != "mmap-hot-snapshot" {
			t.Fatalf("proof = %+v, want mmap hot snapshot", res.Proof)
		}
		assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"git", "rev-parse", "HEAD"}))
		if res.Phases.NativeExecWaitMS != 0 || res.Phases.LedgerLookupMS != 0 || res.Phases.DBOrFileWriteMS != 0 || res.Phases.LockWaitMS != 0 {
			t.Fatalf("long-lived fast replay used heavyweight phases: %+v", res.Phases)
		}
	}
}

func TestEnginePreloadHotSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t, ctx)
	storeRoot := DefaultStoreRoot(repo)
	k := New(storeRoot)
	if _, err := k.Warm(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if !k.PreloadHotSnapshot() {
		t.Fatal("preload did not map warmed hot snapshot")
	}
	res, ok := k.FastReplay(ctx, "test", repo, []string{"git", "rev-parse", "HEAD"})
	if !ok {
		t.Fatal("fast replay missed after preload")
	}
	assertSameResult(t, res.Stdout, res.Stderr, res.ExitCode, runNative(ctx, repo, []string{"git", "rev-parse", "HEAD"}))
}

func TestHotSnapshotCandidatesPrioritizeHighValueCommandsUnderPressure(t *testing.T) {
	replays := make(map[string][]preparedReplay)
	now := time.Now()
	for i := 0; i < hotSnapshotMaxEntries+10; i++ {
		key := hashString(fmt.Sprintf("low-value-%05d", i))
		replays[key] = []preparedReplay{testPreparedReplayForHotSnapshot(
			"python3 --version",
			key,
			hashString(fmt.Sprintf("epoch-low-%05d", i)),
			[]byte("Python 3\n"),
			now.Add(-time.Hour),
		)}
	}
	statusKey := hashString("high-value-status")
	statusEpoch := hashString("epoch-status")
	replays[statusKey] = []preparedReplay{testPreparedReplayForHotSnapshot(
		"git status --short",
		statusKey,
		statusEpoch,
		[]byte(" M README.md\n"),
		now,
	)}

	candidates := hotSnapshotCandidates(replays, nil, nil)
	if len(candidates) != hotSnapshotMaxEntries {
		t.Fatalf("candidate count = %d, want %d", len(candidates), hotSnapshotMaxEntries)
	}
	foundStatus := false
	for i, candidate := range candidates {
		if i > 0 && hotSnapshotKeyCompare([]byte(candidates[i-1].commandKey), candidate.commandKey) > 0 {
			t.Fatalf("candidates not sorted by command key at %d", i)
		}
		if candidate.commandKey == statusKey && candidate.epoch == statusEpoch {
			foundStatus = true
		}
	}
	if !foundStatus {
		t.Fatal("high-value git status candidate was evicted under snapshot pressure")
	}
}

func TestHotSnapshotCandidatesRetainForegroundDemandUnderPressure(t *testing.T) {
	replays := make(map[string][]preparedReplay)
	now := time.Now()
	for i := 0; i < hotSnapshotMaxEntries+10; i++ {
		key := hashString(fmt.Sprintf("broad-warm-%05d", i))
		replays[key] = []preparedReplay{testPreparedReplayForHotSnapshot(
			"head -n 20 src/file.go",
			key,
			hashString(fmt.Sprintf("epoch-broad-%05d", i)),
			[]byte("line\n"),
			now,
		)}
	}
	demandKey := hashString("foreground-demand")
	demandEpoch := hashString("foreground-demand-epoch")
	demand := testPreparedReplayForHotSnapshot(
		"rg -F demanded-literal .",
		demandKey,
		demandEpoch,
		[]byte("src/file.go:demanded-literal\n"),
		now.Add(-time.Hour),
	)
	demand.Entry.Notes = []string{
		"foreground-safe miss requested exact background preparation; native fallback remains available",
	}
	replays[demandKey] = []preparedReplay{demand}

	candidates := hotSnapshotCandidates(replays, nil, nil)
	for _, candidate := range candidates {
		if candidate.commandKey == demandKey && candidate.epoch == demandEpoch {
			return
		}
	}
	t.Fatal("foreground-demanded candidate was evicted by broad warming")
}

func testPreparedReplayForHotSnapshot(command, key, epoch string, stdout []byte, preparedAt time.Time) preparedReplay {
	return preparedReplay{
		Entry: PreparedEntry{
			PreparedID:           hashString("prepared:" + command + ":" + key + ":" + epoch),
			Kind:                 PreparedKindProofGatedOutput,
			OperatorFamily:       FamilyRepoState,
			NormalizedCommand:    command,
			HotInvalidationEpoch: epoch,
			OutputFingerprints: map[string]string{
				"stdout": hashBytes(stdout),
				"stderr": hashBytes(nil),
			},
			ReplayEligible: true,
			PreparedAt:     preparedAt,
		},
		Observation: Observation{
			StdoutHash:   hashBytes(stdout),
			StderrHash:   hashBytes(nil),
			ExitCode:     0,
			NativeWallMS: 10,
		},
		Stdout: stdout,
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
