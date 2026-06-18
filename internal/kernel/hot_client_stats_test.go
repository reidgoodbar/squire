package kernel

import (
	"bytes"
	"os"
	"testing"
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

	RecordHotClientResult(storeRoot, replay)
	RecordHotClientResult(storeRoot, replay)
	RecordHotClientResult(storeRoot, RunResult{Mode: ModeNative})

	stats := LoadHotClientStats(storeRoot)
	if stats.Replays != 2 {
		t.Fatalf("replays = %d, want 2", stats.Replays)
	}
	if stats.NativeFallbacks != 0 {
		t.Fatalf("native fallbacks = %d, want 0", stats.NativeFallbacks)
	}

	b, err := os.ReadFile(hotClientStatsPath(storeRoot))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("secret-output")) {
		t.Fatalf("hot client stats persisted stdout bytes:\n%s", b)
	}
}
