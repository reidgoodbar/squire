package kernel

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
	RecordHotClientResult(storeRoot, RunResult{Mode: ModeNative}, 0)

	stats := LoadHotClientStats(storeRoot)
	if stats.Replays != 2 {
		t.Fatalf("replays = %d, want 2", stats.Replays)
	}
	if stats.NativeFallbacks != 0 {
		t.Fatalf("native fallbacks = %d, want 0", stats.NativeFallbacks)
	}
	if stats.NativeWallAvoidedMS != 14 {
		t.Fatalf("native wall avoided = %d, want 14", stats.NativeWallAvoidedMS)
	}
	if stats.ReplayWallUS != 4000 {
		t.Fatalf("replay wall us = %d, want 4000", stats.ReplayWallUS)
	}
	if stats.ReplayWallMeasured != 2 {
		t.Fatalf("replay wall measured = %d, want 2", stats.ReplayWallMeasured)
	}
	if stats.NetWallSavedMeasuredMS != 10 {
		t.Fatalf("net wall saved measured = %d, want 10", stats.NetWallSavedMeasuredMS)
	}

	b, err := os.ReadFile(hotClientStatsPath(storeRoot))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("secret-output")) {
		t.Fatalf("hot client stats persisted stdout bytes:\n%s", b)
	}
}
