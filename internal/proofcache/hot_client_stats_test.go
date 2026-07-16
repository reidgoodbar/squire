package proofcache

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func TestHotClientStatsRecordAggregateOnly(t *testing.T) {
	storeRoot := t.TempDir()
	replay := RunResult{
		Stdout: []byte("secret-output"),
		Mode:   ModeReplay,
		Family: FamilyLocalRepoMetadata,
		Observation: Observation{
			NativeWallMS: 7,
		},
		Proof: &ProofRecord{OperationKey: "cli-mmap-hot-snapshot"},
	}

	RecordHotClientResult(storeRoot, replay, 2500*time.Microsecond)
	RecordHotClientResult(storeRoot, replay, 1500*time.Microsecond)
	reusedCacheReplay := replay
	reusedCacheReplay.Observation.NativeWallMS = 3
	reusedCacheReplay.Proof = &ProofRecord{OperationKey: "mmap-hot-snapshot"}
	RecordHotClientResult(storeRoot, reusedCacheReplay, 500*time.Microsecond)
	composedReplay := replay
	composedReplay.Observation.NativeWallMS = 11
	composedReplay.Proof = &ProofRecord{OperationKey: "composed-shell-adapter"}
	RecordHotClientResult(storeRoot, composedReplay, 700*time.Microsecond)
	RecordHotClientResult(storeRoot, RunResult{Mode: ModeNative}, 0)

	stats := LoadHotClientStats(storeRoot)
	if stats.Replays != 4 {
		t.Fatalf("replays = %d, want 4", stats.Replays)
	}
	if stats.NativeFallbacks != 0 {
		t.Fatalf("native fallbacks = %d, want 0", stats.NativeFallbacks)
	}
	if stats.NativeWallAvoidedMS != 28 {
		t.Fatalf("native wall avoided = %d, want 28", stats.NativeWallAvoidedMS)
	}
	if stats.ReplayWallUS != 5200 {
		t.Fatalf("replay wall us = %d, want 5200", stats.ReplayWallUS)
	}
	if stats.ReplayWallMeasured != 4 {
		t.Fatalf("replay wall measured = %d, want 4", stats.ReplayWallMeasured)
	}
	if stats.NetWallSavedMeasuredMS != 23 {
		t.Fatalf("net wall saved measured = %d, want 23", stats.NetWallSavedMeasuredMS)
	}
	if stats.LastEventUnixNano == 0 || stats.LastReplayUnixNano == 0 {
		t.Fatalf("last event/replay timestamps were not recorded: %+v", stats)
	}

	b, err := os.ReadFile(hotClientStatsPath(storeRoot))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("secret-output")) {
		t.Fatalf("hot client stats persisted stdout bytes:\n%s", b)
	}
}

func TestAppendHotClientEventLineValidatesInput(t *testing.T) {
	storeRoot := t.TempDir()
	if err := AppendHotClientEventLine(storeRoot, []byte("123 replay c-mmap-hot-snapshot 9 42\n")); err != nil {
		t.Fatal(err)
	}
	if err := AppendHotClientEventLine(storeRoot, []byte("124 replay c-current-file 0 17\n")); err != nil {
		t.Fatal(err)
	}
	if err := AppendHotClientEventLine(storeRoot, []byte("not an event\n")); err != nil {
		t.Fatal(err)
	}
	stats := LoadHotClientStats(storeRoot)
	if stats.Replays != 2 {
		t.Fatalf("replays = %d, want 2", stats.Replays)
	}
	if stats.CurrentFileReplays != 1 {
		t.Fatalf("current-file replays = %d, want 1", stats.CurrentFileReplays)
	}
	if stats.NativeWallAvoidedMS != 9 {
		t.Fatalf("native avoided = %d, want 9", stats.NativeWallAvoidedMS)
	}
	if stats.ReplayWallUS != 59 {
		t.Fatalf("replay wall = %d, want 59", stats.ReplayWallUS)
	}
}
