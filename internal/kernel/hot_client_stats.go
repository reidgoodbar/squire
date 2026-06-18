package kernel

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const hotClientStatsMaxBytes = 1024 * 1024

type HotClientStats struct {
	Replays         int `json:"replays"`
	NativeFallbacks int `json:"native_fallbacks"`
}

func RecordHotClientResult(storeRoot string, res RunResult) {
	if storeRoot == "" || res.Mode != ModeReplay || res.Proof == nil || res.Proof.OperationKey != "cli-mmap-hot-snapshot" {
		return
	}
	if err := os.MkdirAll(storeRoot, 0o700); err != nil {
		return
	}
	path := hotClientStatsPath(storeRoot)
	if info, err := os.Stat(path); err == nil && info.Size() >= hotClientStatsMaxBytes {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%d replay %s %d\n", time.Now().UnixNano(), res.Family, res.Observation.NativeWallMS)
}

func LoadHotClientStats(storeRoot string) HotClientStats {
	var stats HotClientStats
	if storeRoot == "" {
		return stats
	}
	b, err := os.ReadFile(hotClientStatsPath(storeRoot))
	if err != nil {
		return stats
	}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		fields := bytes.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch string(fields[1]) {
		case "replay":
			stats.Replays++
		case "native":
			stats.NativeFallbacks++
		}
	}
	return stats
}

func hotClientStatsPath(storeRoot string) string {
	return filepath.Join(storeRoot, "hot_client_events.log")
}
