package kernel

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

const (
	hotSnapshotMagic        uint64 = 0x3150535148535153 // SQSHSQP1, little-endian marker for hot snapshot files.
	hotSnapshotHeaderBytes         = 64
	hotSnapshotEntryBytes          = 320 // Five 64-byte cache lines.
	hotSnapshotMaxEntries          = 8192
	hotSnapshotMaxBytes            = 64 << 20
	hotSnapshotKindExact    uint32 = 1
	hotSnapshotKindWarmFile uint32 = 2
)

type hotSnapshotCandidate struct {
	commandKey   string
	epoch        string
	stdout       []byte
	stderr       []byte
	exitCode     int
	nativeWallMS int64
	stdoutHash   string
	stderrHash   string
	kind         uint32
}

type HotSnapshotStats struct {
	Available        bool
	Path             string
	SharedMemoryMode string
	Version          int
	SizeBytes        int64
	Entries          int
	ExactEntries     int
	WarmFileEntries  int
	PayloadBytes     int64
	Diagnostic       string
}

func hotCacheSnapshotPath(storeRoot string) string {
	if storeRoot == "" {
		return ""
	}
	return filepath.Join(storeRoot, "hot_snapshot.bin")
}

func HotSnapshotStatsForStore(storeRoot string) HotSnapshotStats {
	path := hotCacheSnapshotPath(storeRoot)
	stats := HotSnapshotStats{Path: path, SharedMemoryMode: "mmap-file-backed"}
	if path == "" {
		stats.Diagnostic = "hot snapshot path unavailable"
		return stats
	}
	info, err := os.Stat(path)
	if err != nil {
		stats.Diagnostic = err.Error()
		return stats
	}
	stats.SizeBytes = info.Size()
	data, cleanup, err := mapHotSnapshotFile(path)
	if err != nil {
		stats.Diagnostic = err.Error()
		return stats
	}
	defer cleanup()
	if len(data) < hotSnapshotHeaderBytes || len(data) > hotSnapshotMaxBytes {
		stats.Diagnostic = "invalid hot snapshot size"
		return stats
	}
	if binary.LittleEndian.Uint64(data[0:8]) != hotSnapshotMagic {
		stats.Diagnostic = "invalid hot snapshot magic"
		return stats
	}
	entrySize := int(binary.LittleEndian.Uint16(data[10:12]))
	count := int(binary.LittleEndian.Uint32(data[12:16]))
	headerSize := int(binary.LittleEndian.Uint32(data[16:20]))
	payloadOffset := int(binary.LittleEndian.Uint32(data[20:24]))
	totalSize := int(binary.LittleEndian.Uint32(data[24:28]))
	if entrySize != hotSnapshotEntryBytes || count < 0 || count > hotSnapshotMaxEntries || headerSize != hotSnapshotHeaderBytes || payloadOffset != hotSnapshotHeaderBytes+count*hotSnapshotEntryBytes || totalSize != len(data) {
		stats.Diagnostic = "invalid hot snapshot header"
		return stats
	}
	stats.Available = true
	stats.Version = int(binary.LittleEndian.Uint16(data[8:10]))
	stats.Entries = count
	stats.PayloadBytes = int64(totalSize - payloadOffset)
	for i := 0; i < count; i++ {
		entry := data[hotSnapshotHeaderBytes+i*hotSnapshotEntryBytes : hotSnapshotHeaderBytes+(i+1)*hotSnapshotEntryBytes]
		switch binary.LittleEndian.Uint32(entry[276:280]) {
		case hotSnapshotKindExact:
			stats.ExactEntries++
		case hotSnapshotKindWarmFile:
			stats.WarmFileEntries++
		}
	}
	return stats
}

func (k *Kernel) publishHotSnapshot(replays map[string][]preparedReplay, warmFiles map[string][]preparedWarmFile) {
	if k == nil || k.Store == nil {
		return
	}
	path := hotCacheSnapshotPath(k.Store.Root)
	if path == "" {
		return
	}
	candidates := hotSnapshotCandidates(replays, warmFiles)
	if len(candidates) == 0 {
		_ = os.Remove(path)
		return
	}
	frame, ok := encodeHotSnapshot(candidates)
	if !ok {
		_ = os.Remove(path)
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp." + strconv.FormatInt(int64(os.Getpid()), 10) + "." + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(tmp, frame, 0o600); err != nil {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return
	}
	_ = os.Chmod(path, 0o600)
}

func hotSnapshotCandidates(replays map[string][]preparedReplay, warmFiles map[string][]preparedWarmFile) []hotSnapshotCandidate {
	var candidates []hotSnapshotCandidate
	for commandKey, items := range replays {
		for _, replay := range items {
			if !validHotSnapshotHash(commandKey) || replay.Entry.HotInvalidationEpoch == "" {
				continue
			}
			if len(replay.Stdout)+len(replay.Stderr) > maxFastPathOutputBytes {
				continue
			}
			if hashBytes(replay.Stdout) != replay.Observation.StdoutHash || hashBytes(replay.Stderr) != replay.Observation.StderrHash {
				continue
			}
			candidates = append(candidates, hotSnapshotCandidate{
				commandKey:   commandKey,
				epoch:        replay.Entry.HotInvalidationEpoch,
				stdout:       replay.Stdout,
				stderr:       replay.Stderr,
				exitCode:     replay.Observation.ExitCode,
				nativeWallMS: replay.Observation.NativeWallMS,
				stdoutHash:   replay.Observation.StdoutHash,
				stderrHash:   replay.Observation.StderrHash,
				kind:         hotSnapshotKindExact,
			})
		}
	}
	for key, items := range warmFiles {
		commandKey := hashString("warm-file:" + key)
		for _, replay := range items {
			if replay.Entry.HotInvalidationEpoch == "" || len(replay.Content) > maxReplayableInspectionFileBytes {
				continue
			}
			contentHash := replay.Entry.OutputFingerprints["file_content"]
			if contentHash == "" || hashBytes(replay.Content) != contentHash {
				continue
			}
			candidates = append(candidates, hotSnapshotCandidate{
				commandKey:   commandKey,
				epoch:        replay.Entry.HotInvalidationEpoch,
				stdout:       replay.Content,
				stderr:       nil,
				exitCode:     0,
				nativeWallMS: replay.NativeWallMS,
				stdoutHash:   contentHash,
				stderrHash:   hashBytes(nil),
				kind:         hotSnapshotKindWarmFile,
			})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].commandKey == candidates[j].commandKey {
			return hashString(candidates[i].epoch) < hashString(candidates[j].epoch)
		}
		return candidates[i].commandKey < candidates[j].commandKey
	})
	if len(candidates) > hotSnapshotMaxEntries {
		candidates = candidates[:hotSnapshotMaxEntries]
	}
	return candidates
}

func encodeHotSnapshot(candidates []hotSnapshotCandidate) ([]byte, bool) {
	if len(candidates) == 0 || len(candidates) > hotSnapshotMaxEntries {
		return nil, false
	}
	payloadOffset := hotSnapshotHeaderBytes + len(candidates)*hotSnapshotEntryBytes
	frame := make([]byte, payloadOffset)
	var payload []byte
	for i, candidate := range candidates {
		stdout := candidate.stdout
		stderr := candidate.stderr
		if len(stdout)+len(stderr) > maxFastPathOutputBytes {
			if candidate.kind != hotSnapshotKindWarmFile || len(stdout)+len(stderr) > maxReplayableInspectionFileBytes {
				continue
			}
		}
		if candidate.kind != hotSnapshotKindExact && candidate.kind != hotSnapshotKindWarmFile {
			continue
		}
		stdoutOffset := payloadOffset + len(payload)
		payload = append(payload, stdout...)
		stderrOffset := payloadOffset + len(payload)
		payload = append(payload, stderr...)
		if payloadOffset+len(payload) > hotSnapshotMaxBytes {
			return nil, false
		}
		entry := frame[hotSnapshotHeaderBytes+i*hotSnapshotEntryBytes : hotSnapshotHeaderBytes+(i+1)*hotSnapshotEntryBytes]
		copy(entry[0:64], candidate.commandKey)
		copy(entry[64:128], hashString(candidate.epoch))
		copy(entry[128:192], candidate.stdoutHash)
		copy(entry[192:256], candidate.stderrHash)
		binary.LittleEndian.PutUint32(entry[256:260], uint32(stdoutOffset))
		binary.LittleEndian.PutUint32(entry[260:264], uint32(len(stdout)))
		binary.LittleEndian.PutUint32(entry[264:268], uint32(stderrOffset))
		binary.LittleEndian.PutUint32(entry[268:272], uint32(len(stderr)))
		binary.LittleEndian.PutUint32(entry[272:276], uint32(int32(candidate.exitCode)))
		binary.LittleEndian.PutUint32(entry[276:280], candidate.kind)
		binary.LittleEndian.PutUint64(entry[280:288], uint64(candidate.nativeWallMS))
	}
	frame = append(frame, payload...)
	binary.LittleEndian.PutUint64(frame[0:8], hotSnapshotMagic)
	binary.LittleEndian.PutUint16(frame[8:10], hotCacheIPCVersion)
	binary.LittleEndian.PutUint16(frame[10:12], hotSnapshotEntryBytes)
	binary.LittleEndian.PutUint32(frame[12:16], uint32(len(candidates)))
	binary.LittleEndian.PutUint32(frame[16:20], hotSnapshotHeaderBytes)
	binary.LittleEndian.PutUint32(frame[20:24], uint32(payloadOffset))
	binary.LittleEndian.PutUint32(frame[24:28], uint32(len(frame)))
	return frame, true
}

func (k *Kernel) tryHotSnapshotReplay(inv CommandInvocation, family OperatorFamily, phases *PhaseTimings) (RunResult, bool) {
	if k == nil || k.Store == nil {
		return RunResult{}, false
	}
	start := time.Now()
	resp, ok := k.readHotSnapshotResponse(hotCacheSnapshotPath(k.Store.Root), inv)
	materializeMS := elapsedMS(start)
	phases.OutputMaterializeMS += materializeMS
	if !ok {
		return RunResult{}, false
	}
	k.hotReplayRing.Record(resp.NativeWallMS, materializeMS, len(resp.Stdout), len(resp.Stderr))
	return RunResult{
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
		ExitCode: resp.ExitCode,
		Mode:     ModeReplay,
		Family:   family,
		Observation: Observation{
			StdoutHash:   resp.StdoutHash,
			StderrHash:   resp.StderrHash,
			StdoutSize:   len(resp.Stdout),
			StderrSize:   len(resp.Stderr),
			ExitCode:     resp.ExitCode,
			NativeWallMS: resp.NativeWallMS,
		},
		Proof: &ProofRecord{
			OperationKeyMatched:        true,
			InputFingerprintsMatched:   true,
			InvalidationEpochUnchanged: true,
			OperatorAllowlisted:        IsReplayAllowed(inv.PolicyArgv),
			OutputAvailable:            true,
			OutputExact:                true,
			PolicyAllowedReplay:        true,
			NativeFallbackAvailable:    true,
			OperationKey:               "mmap-hot-snapshot",
		},
		Phases: *phases,
	}, true
}

func (k *Kernel) PreloadHotSnapshot() bool {
	if k == nil || k.Store == nil {
		return false
	}
	path := hotCacheSnapshotPath(k.Store.Root)
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > hotSnapshotMaxBytes {
		return false
	}
	modTime := info.ModTime().UnixNano()

	k.hotCacheMu.Lock()
	defer k.hotCacheMu.Unlock()
	if k.hotSnapshotData != nil && k.hotSnapshotPath == path && k.hotSnapshotSize == info.Size() && k.hotSnapshotModTime == modTime {
		return true
	}
	if k.hotSnapshotCleanup != nil {
		k.hotSnapshotCleanup()
	}
	data, cleanup, err := mapHotSnapshotFile(path)
	if err != nil {
		k.hotSnapshotPath = ""
		k.hotSnapshotSize = 0
		k.hotSnapshotModTime = 0
		k.hotSnapshotData = nil
		k.hotSnapshotCleanup = nil
		return false
	}
	k.hotSnapshotPath = path
	k.hotSnapshotSize = info.Size()
	k.hotSnapshotModTime = modTime
	k.hotSnapshotData = data
	k.hotSnapshotCleanup = cleanup
	return true
}

func (k *Kernel) readHotSnapshotResponse(path string, inv CommandInvocation) (hotCacheResponse, bool) {
	if path == "" {
		return hotCacheResponse{}, false
	}
	_, hotEpoch, ok := preparedHotProof(inv.PolicyCWD, inv.PolicyArgv)
	if !ok {
		return hotCacheResponse{}, false
	}
	commandKey := preparedReplayLookupKey(inv.PolicyCWD, inv.PolicyArgv)
	epochHash := hashString(hotEpoch)

	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > hotSnapshotMaxBytes {
		return hotCacheResponse{}, false
	}
	modTime := info.ModTime().UnixNano()

	k.hotCacheMu.Lock()
	defer k.hotCacheMu.Unlock()
	if k.hotSnapshotData == nil || k.hotSnapshotPath != path || k.hotSnapshotSize != info.Size() || k.hotSnapshotModTime != modTime {
		if k.hotSnapshotCleanup != nil {
			k.hotSnapshotCleanup()
		}
		data, cleanup, err := mapHotSnapshotFile(path)
		if err != nil {
			k.hotSnapshotPath = ""
			k.hotSnapshotSize = 0
			k.hotSnapshotModTime = 0
			k.hotSnapshotData = nil
			k.hotSnapshotCleanup = nil
			return hotCacheResponse{}, false
		}
		k.hotSnapshotPath = path
		k.hotSnapshotSize = info.Size()
		k.hotSnapshotModTime = modTime
		k.hotSnapshotData = data
		k.hotSnapshotCleanup = cleanup
	}
	resp, err := decodeHotSnapshotResponseNoCopy(k.hotSnapshotData, commandKey, epochHash)
	if err == nil {
		return resp, true
	}
	if key, _, warmEpoch, _, ok := warmFileHotProof(inv.PolicyCWD, inv.PolicyArgv); ok {
		warmKey := hashString("warm-file:" + key)
		warmResp, warmErr := decodeHotSnapshotWarmFileResponse(k.hotSnapshotData, warmKey, hashString(warmEpoch), inv.PolicyArgv)
		if warmErr == nil {
			return warmResp, true
		}
	}
	return hotCacheResponse{}, false
}

func readHotSnapshotResponse(path string, inv CommandInvocation) (hotCacheResponse, bool) {
	if path == "" {
		return hotCacheResponse{}, false
	}
	_, hotEpoch, ok := preparedHotProof(inv.PolicyCWD, inv.PolicyArgv)
	if !ok {
		return hotCacheResponse{}, false
	}
	commandKey := preparedReplayLookupKey(inv.PolicyCWD, inv.PolicyArgv)
	epochHash := hashString(hotEpoch)
	data, cleanup, err := mapHotSnapshotFile(path)
	if err != nil {
		return hotCacheResponse{}, false
	}
	defer cleanup()
	resp, err := decodeHotSnapshotResponse(data, commandKey, epochHash)
	if err == nil {
		return resp, true
	}
	if key, _, warmEpoch, _, ok := warmFileHotProof(inv.PolicyCWD, inv.PolicyArgv); ok {
		warmKey := hashString("warm-file:" + key)
		warmResp, warmErr := decodeHotSnapshotWarmFileResponse(data, warmKey, hashString(warmEpoch), inv.PolicyArgv)
		if warmErr == nil {
			return warmResp, true
		}
	}
	return hotCacheResponse{}, false
}

func decodeHotSnapshotResponse(data []byte, commandKey, epochHash string) (hotCacheResponse, error) {
	return decodeHotSnapshotResponsePayload(data, commandKey, epochHash, true)
}

func decodeHotSnapshotResponseNoCopy(data []byte, commandKey, epochHash string) (hotCacheResponse, error) {
	return decodeHotSnapshotResponsePayload(data, commandKey, epochHash, false)
}

func decodeHotSnapshotResponsePayload(data []byte, commandKey, epochHash string, copyPayload bool) (hotCacheResponse, error) {
	if len(data) < hotSnapshotHeaderBytes || len(data) > hotSnapshotMaxBytes {
		return hotCacheResponse{}, errors.New("invalid hot snapshot size")
	}
	if binary.LittleEndian.Uint64(data[0:8]) != hotSnapshotMagic {
		return hotCacheResponse{}, errors.New("invalid hot snapshot magic")
	}
	if int(binary.LittleEndian.Uint16(data[8:10])) != hotCacheIPCVersion {
		return hotCacheResponse{}, errors.New("invalid hot snapshot version")
	}
	entrySize := int(binary.LittleEndian.Uint16(data[10:12]))
	count := int(binary.LittleEndian.Uint32(data[12:16]))
	headerSize := int(binary.LittleEndian.Uint32(data[16:20]))
	payloadOffset := int(binary.LittleEndian.Uint32(data[20:24]))
	totalSize := int(binary.LittleEndian.Uint32(data[24:28]))
	if entrySize != hotSnapshotEntryBytes || count < 0 || count > hotSnapshotMaxEntries || headerSize != hotSnapshotHeaderBytes || totalSize != len(data) {
		return hotCacheResponse{}, errors.New("invalid hot snapshot header")
	}
	if payloadOffset != hotSnapshotHeaderBytes+count*hotSnapshotEntryBytes || payloadOffset > len(data) {
		return hotCacheResponse{}, errors.New("invalid hot snapshot payload offset")
	}
	if !validHotSnapshotHash(commandKey) || !validHotSnapshotHash(epochHash) {
		return hotCacheResponse{}, errors.New("invalid hot snapshot lookup key")
	}
	start, found := hotSnapshotCommandRange(data, count, commandKey)
	if !found {
		return hotCacheResponse{}, errors.New("hot snapshot miss")
	}
	for i := start; i < count; i++ {
		entry := data[hotSnapshotHeaderBytes+i*hotSnapshotEntryBytes : hotSnapshotHeaderBytes+(i+1)*hotSnapshotEntryBytes]
		if hotSnapshotKeyCompare(entry[0:64], commandKey) != 0 {
			break
		}
		if string(entry[64:128]) != epochHash {
			continue
		}
		stdoutHash := string(entry[128:192])
		stderrHash := string(entry[192:256])
		if !validHotSnapshotHash(stdoutHash) || !validHotSnapshotHash(stderrHash) {
			return hotCacheResponse{}, errors.New("invalid hot snapshot output hash")
		}
		stdoutOffset := int(binary.LittleEndian.Uint32(entry[256:260]))
		stdoutLen := int(binary.LittleEndian.Uint32(entry[260:264]))
		stderrOffset := int(binary.LittleEndian.Uint32(entry[264:268]))
		stderrLen := int(binary.LittleEndian.Uint32(entry[268:272]))
		exitCode := int(int32(binary.LittleEndian.Uint32(entry[272:276])))
		kind := binary.LittleEndian.Uint32(entry[276:280])
		nativeWallMS := int64(binary.LittleEndian.Uint64(entry[280:288]))
		if kind != hotSnapshotKindExact {
			return hotCacheResponse{}, errors.New("hot snapshot kind mismatch")
		}
		if stdoutLen < 0 || stderrLen < 0 || stdoutLen+stderrLen > maxFastPathOutputBytes {
			return hotCacheResponse{}, errors.New("invalid hot snapshot output size")
		}
		if stdoutOffset < payloadOffset || stderrOffset < payloadOffset || stdoutOffset+stdoutLen > len(data) || stderrOffset+stderrLen > len(data) {
			return hotCacheResponse{}, errors.New("truncated hot snapshot output")
		}
		stdout := data[stdoutOffset : stdoutOffset+stdoutLen]
		stderr := data[stderrOffset : stderrOffset+stderrLen]
		if hashBytes(stdout) != stdoutHash || hashBytes(stderr) != stderrHash {
			return hotCacheResponse{}, errors.New("hot snapshot hash mismatch")
		}
		if copyPayload {
			stdout = append([]byte(nil), stdout...)
			stderr = append([]byte(nil), stderr...)
		}
		return hotCacheResponse{
			Version:      hotCacheIPCVersion,
			Hit:          true,
			Stdout:       stdout,
			Stderr:       stderr,
			ExitCode:     exitCode,
			StdoutHash:   stdoutHash,
			StderrHash:   stderrHash,
			NativeWallMS: nativeWallMS,
		}, nil
	}
	return hotCacheResponse{}, errors.New("hot snapshot miss")
}

func decodeHotSnapshotWarmFileResponse(data []byte, commandKey, epochHash string, argv []string) (hotCacheResponse, error) {
	resp, err := decodeHotSnapshotPayload(data, commandKey, epochHash, hotSnapshotKindWarmFile, maxReplayableInspectionFileBytes)
	if err != nil {
		return hotCacheResponse{}, err
	}
	stdout, ok := warmFileCommandOutput(resp.Stdout, argv)
	if !ok || len(stdout) > maxFastPathOutputBytes {
		return hotCacheResponse{}, errors.New("hot snapshot warm-file command output unavailable")
	}
	stderr := []byte(nil)
	return hotCacheResponse{
		Version:      hotCacheIPCVersion,
		Hit:          true,
		Stdout:       stdout,
		Stderr:       stderr,
		ExitCode:     0,
		NativeWallMS: resp.NativeWallMS,
		StdoutHash:   hashBytes(stdout),
		StderrHash:   hashBytes(stderr),
	}, nil
}

func decodeHotSnapshotPayload(data []byte, commandKey, epochHash string, wantKind uint32, maxOutputBytes int) (hotCacheResponse, error) {
	if len(data) < hotSnapshotHeaderBytes || len(data) > hotSnapshotMaxBytes {
		return hotCacheResponse{}, errors.New("invalid hot snapshot size")
	}
	if binary.LittleEndian.Uint64(data[0:8]) != hotSnapshotMagic {
		return hotCacheResponse{}, errors.New("invalid hot snapshot magic")
	}
	if int(binary.LittleEndian.Uint16(data[8:10])) != hotCacheIPCVersion {
		return hotCacheResponse{}, errors.New("invalid hot snapshot version")
	}
	entrySize := int(binary.LittleEndian.Uint16(data[10:12]))
	count := int(binary.LittleEndian.Uint32(data[12:16]))
	headerSize := int(binary.LittleEndian.Uint32(data[16:20]))
	payloadOffset := int(binary.LittleEndian.Uint32(data[20:24]))
	totalSize := int(binary.LittleEndian.Uint32(data[24:28]))
	if entrySize != hotSnapshotEntryBytes || count < 0 || count > hotSnapshotMaxEntries || headerSize != hotSnapshotHeaderBytes || totalSize != len(data) {
		return hotCacheResponse{}, errors.New("invalid hot snapshot header")
	}
	if payloadOffset != hotSnapshotHeaderBytes+count*hotSnapshotEntryBytes || payloadOffset > len(data) {
		return hotCacheResponse{}, errors.New("invalid hot snapshot payload offset")
	}
	if !validHotSnapshotHash(commandKey) || !validHotSnapshotHash(epochHash) {
		return hotCacheResponse{}, errors.New("invalid hot snapshot lookup key")
	}
	start, found := hotSnapshotCommandRange(data, count, commandKey)
	if !found {
		return hotCacheResponse{}, errors.New("hot snapshot miss")
	}
	for i := start; i < count; i++ {
		entry := data[hotSnapshotHeaderBytes+i*hotSnapshotEntryBytes : hotSnapshotHeaderBytes+(i+1)*hotSnapshotEntryBytes]
		if hotSnapshotKeyCompare(entry[0:64], commandKey) != 0 {
			break
		}
		if string(entry[64:128]) != epochHash {
			continue
		}
		stdoutHash := string(entry[128:192])
		stderrHash := string(entry[192:256])
		if !validHotSnapshotHash(stdoutHash) || !validHotSnapshotHash(stderrHash) {
			return hotCacheResponse{}, errors.New("invalid hot snapshot output hash")
		}
		stdoutOffset := int(binary.LittleEndian.Uint32(entry[256:260]))
		stdoutLen := int(binary.LittleEndian.Uint32(entry[260:264]))
		stderrOffset := int(binary.LittleEndian.Uint32(entry[264:268]))
		stderrLen := int(binary.LittleEndian.Uint32(entry[268:272]))
		exitCode := int(int32(binary.LittleEndian.Uint32(entry[272:276])))
		kind := binary.LittleEndian.Uint32(entry[276:280])
		nativeWallMS := int64(binary.LittleEndian.Uint64(entry[280:288]))
		if kind != wantKind {
			return hotCacheResponse{}, errors.New("hot snapshot kind mismatch")
		}
		if stdoutLen < 0 || stderrLen < 0 || stdoutLen+stderrLen > maxOutputBytes {
			return hotCacheResponse{}, errors.New("invalid hot snapshot output size")
		}
		if stdoutOffset < payloadOffset || stderrOffset < payloadOffset || stdoutOffset+stdoutLen > len(data) || stderrOffset+stderrLen > len(data) {
			return hotCacheResponse{}, errors.New("truncated hot snapshot output")
		}
		stdout := data[stdoutOffset : stdoutOffset+stdoutLen]
		stderr := data[stderrOffset : stderrOffset+stderrLen]
		if hashBytes(stdout) != stdoutHash || hashBytes(stderr) != stderrHash {
			return hotCacheResponse{}, errors.New("hot snapshot hash mismatch")
		}
		return hotCacheResponse{
			Version:      hotCacheIPCVersion,
			Hit:          true,
			Stdout:       append([]byte(nil), stdout...),
			Stderr:       append([]byte(nil), stderr...),
			ExitCode:     exitCode,
			StdoutHash:   stdoutHash,
			StderrHash:   stderrHash,
			NativeWallMS: nativeWallMS,
		}, nil
	}
	return hotCacheResponse{}, errors.New("hot snapshot miss")
}

func hotSnapshotCommandRange(data []byte, count int, commandKey string) (int, bool) {
	lo, hi := 0, count
	for lo < hi {
		mid := lo + (hi-lo)/2
		entry := data[hotSnapshotHeaderBytes+mid*hotSnapshotEntryBytes : hotSnapshotHeaderBytes+(mid+1)*hotSnapshotEntryBytes]
		if hotSnapshotKeyCompare(entry[0:64], commandKey) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= count {
		return 0, false
	}
	entry := data[hotSnapshotHeaderBytes+lo*hotSnapshotEntryBytes : hotSnapshotHeaderBytes+(lo+1)*hotSnapshotEntryBytes]
	return lo, hotSnapshotKeyCompare(entry[0:64], commandKey) == 0
}

func hotSnapshotKeyCompare(entryKey []byte, key string) int {
	for i := 0; i < 64; i++ {
		left := entryKey[i]
		right := key[i]
		if left < right {
			return -1
		}
		if left > right {
			return 1
		}
	}
	return 0
}

func validHotSnapshotHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
