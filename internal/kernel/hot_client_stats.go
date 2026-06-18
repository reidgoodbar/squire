package kernel

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const hotClientStatsMaxBytes = 1024 * 1024

type HotClientStats struct {
	Replays                int   `json:"replays"`
	NativeFallbacks        int   `json:"native_fallbacks"`
	NativeWallAvoidedMS    int64 `json:"native_wall_avoided_ms"`
	ReplayWallUS           int64 `json:"replay_wall_us"`
	ReplayWallMeasured     int   `json:"replay_wall_measured"`
	NetWallSavedMeasuredMS int64 `json:"net_wall_saved_measured_ms"`
}

func RecordHotClientResult(storeRoot string, res RunResult, replayWall time.Duration) {
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
	_, _ = fmt.Fprintf(f, "%d replay %s %d %d\n", time.Now().UnixNano(), res.Family, res.Observation.NativeWallMS, replayWall.Microseconds())
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
	var measuredNativeWallMS int64
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		fields := bytes.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch string(fields[1]) {
		case "replay":
			stats.Replays++
			var nativeMS int64
			if len(fields) >= 4 {
				if parsed, err := strconv.ParseInt(string(fields[3]), 10, 64); err == nil {
					nativeMS = parsed
					stats.NativeWallAvoidedMS += nativeMS
				}
			}
			if len(fields) >= 5 {
				if replayUS, err := strconv.ParseInt(string(fields[4]), 10, 64); err == nil && replayUS > 0 {
					stats.ReplayWallUS += replayUS
					stats.ReplayWallMeasured++
					measuredNativeWallMS += nativeMS
				}
			}
		case "native":
			stats.NativeFallbacks++
		}
	}
	stats.NetWallSavedMeasuredMS = measuredNativeWallMS - stats.ReplayWallUS/1000
	return stats
}

func hotClientStatsPath(storeRoot string) string {
	return filepath.Join(storeRoot, "hot_client_events.log")
}
