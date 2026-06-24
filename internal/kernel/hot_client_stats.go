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
	GoClientReplays        int   `json:"go_client_replays"`
	PreparedChildReplays   int   `json:"prepared_child_replays"`
	SyntheticReplays       int   `json:"synthetic_replays"`
	NativeFallbacks        int   `json:"native_fallbacks"`
	NativeWallAvoidedMS    int64 `json:"native_wall_avoided_ms"`
	ReplayWallUS           int64 `json:"replay_wall_us"`
	ReplayWallMeasured     int   `json:"replay_wall_measured"`
	NetWallSavedMeasuredMS int64 `json:"net_wall_saved_measured_ms"`
	LastEventUnixNano      int64 `json:"last_event_unix_nano"`
	LastReplayUnixNano     int64 `json:"last_replay_unix_nano"`
}

func RecordHotClientResult(storeRoot string, res RunResult, replayWall time.Duration) {
	if storeRoot == "" || res.Mode != ModeReplay || res.Proof == nil || !isHotClientProof(res.Proof.OperationKey) {
		return
	}
	line := fmt.Sprintf("%d replay %s %d %d", time.Now().UnixNano(), res.Proof.OperationKey, res.Observation.NativeWallMS, replayWall.Microseconds())
	_ = AppendHotClientEventLine(storeRoot, []byte(line))
}

func AppendHotClientEventLine(storeRoot string, line []byte) error {
	line = bytes.TrimSpace(line)
	if storeRoot == "" || !validHotClientEventLine(line) {
		return nil
	}
	if err := os.MkdirAll(storeRoot, 0o700); err != nil {
		return err
	}
	path := hotClientStatsPath(storeRoot)
	if info, err := os.Stat(path); err == nil && info.Size() >= hotClientStatsMaxBytes {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

func validHotClientEventLine(line []byte) bool {
	fields := bytes.Fields(line)
	if len(fields) != 5 || string(fields[1]) != "replay" || !isHotClientProof(string(fields[2])) {
		return false
	}
	if _, err := strconv.ParseInt(string(fields[0]), 10, 64); err != nil {
		return false
	}
	if _, err := strconv.ParseInt(string(fields[3]), 10, 64); err != nil {
		return false
	}
	if _, err := strconv.ParseInt(string(fields[4]), 10, 64); err != nil {
		return false
	}
	return true
}

func isHotClientProof(proof string) bool {
	return proof == "cli-mmap-hot-snapshot" ||
		proof == "mmap-hot-snapshot" ||
		proof == "c-mmap-hot-snapshot" ||
		proof == "c-mmap-hot-synthetic"
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
		if ts, err := strconv.ParseInt(string(fields[0]), 10, 64); err == nil {
			if ts > stats.LastEventUnixNano {
				stats.LastEventUnixNano = ts
			}
			if string(fields[1]) == "replay" && ts > stats.LastReplayUnixNano {
				stats.LastReplayUnixNano = ts
			}
		}
		switch string(fields[1]) {
		case "replay":
			stats.Replays++
			if len(fields) >= 3 {
				switch string(fields[2]) {
				case "cli-mmap-hot-snapshot", "mmap-hot-snapshot":
					stats.GoClientReplays++
				case "c-mmap-hot-snapshot":
					stats.PreparedChildReplays++
				case "c-mmap-hot-synthetic":
					stats.SyntheticReplays++
				}
			}
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
