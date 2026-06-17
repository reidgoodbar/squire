package kernel

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type preparedWarmFile struct {
	Entry        PreparedEntry
	Content      []byte
	NativeWallMS int64
}

func (k *Kernel) prewarmWarmFiles(ctx context.Context, cwd string, ws WorldState, ledger *ValidityLedger, phases *PhaseTimings) (int, []WarmPreparedReport) {
	_ = ctx
	if ws.RepoRoot == "" || k.Store == nil || ledger == nil {
		return 0, nil
	}
	var count int
	var reports []WarmPreparedReport
	for _, rel := range replayableInspectionPrewarmFiles(ws.RepoRoot, workspaceImagePrewarmFileLimit) {
		argv := []string{"sed", "-n", "1,1p", rel}
		if k.prepareWarmFileFromCommand(cwd, argv, ws, ledger, phases, "Level 3 workspace image bytes prepared for arbitrary bounded sed/cat replay; native fallback still available") {
			count++
			reports = append(reports, WarmPreparedReport{
				Kind:              PreparedKindWarmFile,
				OperatorFamily:    FamilyFileInspection,
				NormalizedCommand: "warm eligible workspace file",
				ReplayEligible:    true,
				EvidenceQuality:   ws.EvidenceQuality,
				Privacy:           "eligible local workspace file bytes stored locally for exact bounded cat/sed replay",
			})
		}
	}
	return count, reports
}

func (k *Kernel) prepareWarmFileFromCommand(cwd string, argv []string, ws WorldState, ledger *ValidityLedger, phases *PhaseTimings, note string) bool {
	if k.Store == nil || ledger == nil || !isReplayableFileInspection(argv) {
		return false
	}
	key, hotFPS, hotEpoch, path, ok := warmFileHotProof(cwd, argv)
	if !ok {
		return false
	}
	readStart := time.Now()
	content, err := os.ReadFile(path)
	phases.OutputMaterializeMS += elapsedMS(readStart)
	if err != nil || len(content) > maxReplayableInspectionFileBytes {
		return false
	}
	if hashBytes(content) != hotFPS["file_content"] {
		return false
	}
	writeStart := time.Now()
	ref, err := k.Store.StoreWarmFile(key, content)
	phases.DBOrFileWriteMS += elapsedMS(writeStart)
	if err != nil {
		return false
	}
	ledger.UpsertPrepared(PreparedEntry{
		PreparedID:           hashString("prepared:warm-file:" + key + ":" + hotEpoch),
		Kind:                 PreparedKindWarmFile,
		OperatorFamily:       FamilyFileInspection,
		NormalizedCommand:    "warm eligible workspace file",
		InputFingerprints:    cloneStringMap(hotFPS),
		HotFingerprints:      hotFPS,
		OutputFingerprints:   map[string]string{"file_content": hashBytes(content), "file_size": hashString(strconv.Itoa(len(content)))},
		InvalidationEpoch:    hotEpoch,
		HotInvalidationEpoch: hotEpoch,
		EvidenceQuality:      ws.EvidenceQuality,
		ReplayEligible:       true,
		OutputRef:            ref,
		Privacy:              "eligible local workspace file bytes stored locally for exact bounded cat/sed replay",
		PreparedAt:           time.Now(),
		Notes:                []string{note},
	})
	return true
}

func (k *Kernel) findPreparedWarmFileReplay(inv CommandInvocation, diagnostics []string, phases *PhaseTimings) (preparedReplay, []string, bool) {
	key, hotFPS, hotEpoch, _, ok := warmFileHotProof(inv.PolicyCWD, inv.PolicyArgv)
	if !ok {
		return preparedReplay{}, diagnostics, false
	}
	candidates, warmDiagnostics := k.residentPreparedWarmFileCandidates(key, phases)
	diagnostics = append(diagnostics, warmDiagnostics...)
	if len(candidates) == 0 {
		return preparedReplay{}, diagnostics, false
	}
	for _, candidate := range candidates {
		if candidate.Entry.HotInvalidationEpoch != hotEpoch || !mapsEqual(candidate.Entry.HotFingerprints, hotFPS) {
			continue
		}
		stdout, ok := warmFileCommandOutput(candidate.Content, inv.PolicyArgv)
		if !ok || len(stdout) > maxFastPathOutputBytes {
			return preparedReplay{}, diagnostics, false
		}
		stderr := []byte(nil)
		obs := Observation{
			OperationID:  hashString("warm-file-replay:" + key + ":" + normalizeArgv(inv.PolicyArgv)),
			StdoutHash:   hashBytes(stdout),
			StderrHash:   hashBytes(stderr),
			StdoutSize:   len(stdout),
			StderrSize:   len(stderr),
			ExitCode:     0,
			NativeWallMS: candidate.NativeWallMS,
			Timestamp:    candidate.Entry.PreparedAt,
			OutputRef:    candidate.Entry.OutputRef,
		}
		return preparedReplay{
			Entry:         candidate.Entry,
			Observation:   obs,
			Stdout:        stdout,
			Stderr:        stderr,
			HotCacheFrame: encodeHotCacheHitFrame(stdout, stderr, 0),
		}, diagnostics, true
	}
	return preparedReplay{}, diagnostics, false
}

func (k *Kernel) residentPreparedWarmFileCandidates(key string, phases *PhaseTimings) ([]preparedWarmFile, []string) {
	lockStart := time.Now()
	k.mu.Lock()
	phases.LockWaitMS += elapsedMS(lockStart)
	if !k.preparedLoaded || !k.preparedAvailable {
		k.mu.Unlock()
		return nil, nil
	}
	candidates := append([]preparedWarmFile(nil), k.preparedWarmFiles[key]...)
	diagnostics := append([]string(nil), k.preparedDiag...)
	k.mu.Unlock()
	return candidates, diagnostics
}

func warmFileHotProof(cwd string, argv []string) (string, map[string]string, string, string, bool) {
	argv = normalizeArgvForPolicy(argv)
	if !isReplayableFileInspection(argv) {
		return "", nil, "", "", false
	}
	root := absPath(cwd)
	argPath := replayableInspectionArgPath(argv)
	if argPath == "" {
		return "", nil, "", "", false
	}
	path := filepath.Clean(filepath.Join(root, argPath))
	if !pathWithinRoot(path, root) || !isReplayableInspectionName(filepath.Base(path)) {
		return "", nil, "", "", false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() || info.Size() > maxReplayableInspectionFileBytes {
		return "", nil, "", "", false
	}
	if isReplayableCatFileRead(argv) && info.Size() > maxFastPathOutputBytes {
		return "", nil, "", "", false
	}
	contentHash, ok := hashFile(path)
	if !ok {
		return "", nil, "", "", false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", nil, "", "", false
	}
	rel = filepath.ToSlash(rel)
	key := hashString(root + "\x00" + rel)
	fp := map[string]string{
		"hot_cwd":      hashString(root),
		"warm_file":    key,
		"file_path":    hashString(rel),
		"file_name":    hashString(filepath.Base(path)),
		"file_content": contentHash,
		"file_size":    hashString(strconv.FormatInt(info.Size(), 10)),
		"file_mode":    hashString(info.Mode().String()),
	}
	epoch := "hot-warm-file:" + hashString(root+"|"+rel+"|"+contentHash+"|"+strconv.FormatInt(info.Size(), 10)+"|"+info.Mode().String())
	return key, fp, epoch, path, true
}

func replayableInspectionArgPath(argv []string) string {
	if isReplayableCatFileRead(argv) {
		return argv[1]
	}
	if isBoundedSedPrint(argv) {
		return argv[3]
	}
	return ""
}

func warmFileCommandOutput(content []byte, argv []string) ([]byte, bool) {
	argv = normalizeArgvForPolicy(argv)
	if isReplayableCatFileRead(argv) {
		return append([]byte(nil), content...), true
	}
	if !isBoundedSedPrint(argv) {
		return nil, false
	}
	start, end, ok := parseSedPrintRange(argv[2])
	if !ok {
		return nil, false
	}
	return sedPrintRangeBytes(content, start, end), true
}

func sedPrintRangeBytes(content []byte, start, end int) []byte {
	if start < 1 || end < start || len(content) == 0 {
		return nil
	}
	var out bytes.Buffer
	lineNo := 1
	offset := 0
	for offset < len(content) && lineNo <= end {
		next := bytes.IndexByte(content[offset:], '\n')
		lineEnd := len(content)
		if next >= 0 {
			lineEnd = offset + next + 1
		}
		if lineNo >= start {
			out.Write(content[offset:lineEnd])
		}
		offset = lineEnd
		lineNo++
	}
	return out.Bytes()
}
