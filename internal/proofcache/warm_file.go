package proofcache

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
	LineStarts   []int
	NativeWallMS int64
}

func (k *Engine) prewarmWarmFiles(ctx context.Context, cwd string, ws WorldState, ledger *ValidityLedger, phases *PhaseTimings) (int, []WarmPreparedReport) {
	_ = ctx
	if ws.RepoRoot == "" || k.Store == nil || ledger == nil {
		return 0, nil
	}
	var count int
	var reports []WarmPreparedReport
	for _, rel := range replayableInspectionPrewarmFiles(ws.RepoRoot, workspaceImagePrewarmFileLimit) {
		argv := []string{"sed", "-n", "1,1p", rel}
		if k.prepareWarmFileFromCommand(cwd, argv, ws, ledger, phases, "Level 3 workspace image bytes prepared for arbitrary bounded sed/cat/head/tail/grep/rg replay; native fallback still available") {
			count++
			reports = append(reports, WarmPreparedReport{
				Kind:              PreparedKindWarmFile,
				OperatorFamily:    FamilyFileInspection,
				NormalizedCommand: "warm eligible workspace file",
				ReplayEligible:    true,
				EvidenceQuality:   ws.EvidenceQuality,
				Privacy:           "eligible local workspace file bytes stored locally for exact bounded cat/sed/head/tail/grep/rg replay",
			})
		}
	}
	return count, reports
}

func (k *Engine) prepareWarmFileFromCommand(cwd string, argv []string, ws WorldState, ledger *ValidityLedger, phases *PhaseTimings, note string) bool {
	if k.Store == nil || ledger == nil || !isWarmFileBackedInspection(argv) {
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
		Privacy:              "eligible local workspace file bytes stored locally for exact bounded cat/sed/head/tail/grep/rg replay",
		PreparedAt:           time.Now(),
		Notes:                []string{note},
	})
	return true
}

func (k *Engine) findPreparedWarmFileReplay(inv CommandInvocation, diagnostics []string, phases *PhaseTimings) (preparedReplay, []string, bool) {
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
		stdout, exitCode, ok := warmFileCommandOutputIndexed(candidate.Content, candidate.LineStarts, inv.PolicyArgv)
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
			ExitCode:     exitCode,
			NativeWallMS: candidate.NativeWallMS,
			Timestamp:    candidate.Entry.PreparedAt,
			OutputRef:    candidate.Entry.OutputRef,
		}
		return preparedReplay{
			Entry:         candidate.Entry,
			Observation:   obs,
			Stdout:        stdout,
			Stderr:        stderr,
			HotCacheFrame: encodeHotCacheHitFrame(stdout, stderr, exitCode),
		}, diagnostics, true
	}
	return preparedReplay{}, diagnostics, false
}

func (k *Engine) residentPreparedWarmFileCandidates(key string, phases *PhaseTimings) ([]preparedWarmFile, []string) {
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
	if !isWarmFileBackedInspection(argv) {
		return "", nil, "", "", false
	}
	root := absPath(cwd)
	argPath := replayableInspectionArgPath(argv)
	if argPath == "" {
		return "", nil, "", "", false
	}
	path := filepath.Clean(filepath.Join(root, argPath))
	realRoot := root
	if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = resolvedRoot
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil || !pathWithinRoot(realPath, realRoot) || !isReplayableInspectionName(filepath.Base(path)) {
		return "", nil, "", "", false
	}
	info, err := os.Stat(realPath)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() || info.Size() > maxReplayableInspectionFileBytes {
		return "", nil, "", "", false
	}
	if isReplayableCatFileRead(argv) && info.Size() > maxFastPathOutputBytes {
		return "", nil, "", "", false
	}
	contentHash, ok := hashFile(realPath)
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
	return key, fp, epoch, realPath, true
}

func replayableInspectionArgPath(argv []string) string {
	if isReplayableCatFileRead(argv) {
		return argv[1]
	}
	if isBoundedSedPrint(argv) {
		return argv[3]
	}
	if isBoundedHeadPrint(argv) {
		path, _, ok := parseHeadTailArgs(argv, false)
		if ok {
			return path
		}
	}
	if isBoundedTailPrint(argv) {
		path, _, ok := parseHeadTailArgs(argv, true)
		if ok {
			return path
		}
	}
	if isReplayableFileType(argv) {
		return argv[1]
	}
	if isFixedGrepFileSearch(argv) {
		_, path, _, ok := parseFixedGrepArgs(argv)
		if ok {
			return path
		}
	}
	if isFixedRgFileSearch(argv) {
		_, path, _, _, ok := parseFixedRgArgs(argv)
		if ok {
			return path
		}
	}
	return ""
}

func warmFileCommandOutput(content []byte, argv []string) ([]byte, int, bool) {
	return warmFileCommandOutputIndexed(content, nil, argv)
}

func warmFileCommandOutputIndexed(content []byte, lineStarts []int, argv []string) ([]byte, int, bool) {
	argv = normalizeArgvForPolicy(argv)
	if isReplayableCatFileRead(argv) {
		return append([]byte(nil), content...), 0, true
	}
	if isBoundedSedPrint(argv) {
		start, end, ok := parseSedPrintRange(argv[2])
		if !ok {
			return nil, 0, false
		}
		return sedPrintRangeBytesIndexed(content, lineStarts, start, end), 0, true
	}
	if isBoundedHeadPrint(argv) {
		_, n, ok := parseHeadTailArgs(argv, false)
		if !ok {
			return nil, 0, false
		}
		return sedPrintRangeBytesIndexed(content, lineStarts, 1, n), 0, true
	}
	if isBoundedTailPrint(argv) {
		_, n, ok := parseHeadTailArgs(argv, true)
		if !ok {
			return nil, 0, false
		}
		return tailLineBytesIndexed(content, lineStarts, n), 0, true
	}
	if isFixedGrepFileSearch(argv) {
		pattern, _, quiet, ok := parseFixedGrepArgs(argv)
		if !ok || bytes.IndexByte(content, 0) >= 0 {
			return nil, 0, false
		}
		stdout, matched := fixedGrepOutput(content, []byte(pattern), quiet)
		if !matched {
			return nil, 1, true
		}
		return stdout, 0, true
	}
	if isFixedRgFileSearch(argv) {
		pattern, _, quiet, lineNumber, ok := parseFixedRgArgs(argv)
		if !ok || bytes.IndexByte(content, 0) >= 0 {
			return nil, 0, false
		}
		stdout, matched := fixedRgOutput(content, []byte(pattern), quiet, lineNumber)
		if !matched {
			return nil, 1, true
		}
		return stdout, 0, true
	}
	return nil, 0, false
}

func sedPrintRangeBytes(content []byte, start, end int) []byte {
	return sedPrintRangeBytesIndexed(content, nil, start, end)
}

func sedPrintRangeBytesIndexed(content []byte, lineStarts []int, start, end int) []byte {
	if start < 1 || end < start || len(content) == 0 {
		return nil
	}
	if len(lineStarts) > 0 {
		startIdx := start - 1
		if startIdx >= len(lineStarts) {
			return nil
		}
		endIdx := end - 1
		if endIdx >= len(lineStarts) {
			endIdx = len(lineStarts) - 1
		}
		begin := lineStarts[startIdx]
		finish := lineEndOffset(content, lineStarts, endIdx)
		return append([]byte(nil), content[begin:finish]...)
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

func tailLineBytes(content []byte, count int) []byte {
	return tailLineBytesIndexed(content, nil, count)
}

func tailLineBytesIndexed(content []byte, lineStarts []int, count int) []byte {
	if count < 1 || len(content) == 0 {
		return nil
	}
	if len(lineStarts) > 0 {
		startIdx := len(lineStarts) - count
		if startIdx < 0 {
			startIdx = 0
		}
		begin := lineStarts[startIdx]
		finish := lineEndOffset(content, lineStarts, len(lineStarts)-1)
		return append([]byte(nil), content[begin:finish]...)
	}
	type lineRange struct {
		start int
		end   int
	}
	lines := make([]lineRange, 0, count)
	offset := 0
	for offset < len(content) {
		next := bytes.IndexByte(content[offset:], '\n')
		lineEnd := len(content)
		if next >= 0 {
			lineEnd = offset + next + 1
		}
		lines = append(lines, lineRange{start: offset, end: lineEnd})
		offset = lineEnd
	}
	start := len(lines) - count
	if start < 0 {
		start = 0
	}
	var out bytes.Buffer
	for _, line := range lines[start:] {
		out.Write(content[line.start:line.end])
	}
	return out.Bytes()
}

func lineStartsForContent(content []byte) []int {
	if len(content) == 0 {
		return nil
	}
	starts := make([]int, 0, bytes.Count(content, []byte{'\n'})+1)
	starts = append(starts, 0)
	for i, b := range content {
		if b == '\n' && i+1 < len(content) {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func lineEndOffset(content []byte, lineStarts []int, idx int) int {
	if idx < 0 || idx >= len(lineStarts) {
		return len(content)
	}
	if idx+1 < len(lineStarts) {
		return lineStarts[idx+1]
	}
	return len(content)
}

func fixedGrepOutput(content, pattern []byte, quiet bool) ([]byte, bool) {
	if len(pattern) == 0 {
		return nil, false
	}
	var out bytes.Buffer
	matched := false
	offset := 0
	for offset < len(content) {
		next := bytes.IndexByte(content[offset:], '\n')
		lineEnd := len(content)
		hasNewline := false
		if next >= 0 {
			lineEnd = offset + next + 1
			hasNewline = true
		}
		line := content[offset:lineEnd]
		lineForMatch := line
		if hasNewline {
			lineForMatch = line[:len(line)-1]
		}
		if bytes.Contains(lineForMatch, pattern) {
			matched = true
			if quiet {
				return nil, true
			}
			out.Write(lineForMatch)
			out.WriteByte('\n')
		}
		offset = lineEnd
	}
	return out.Bytes(), matched
}

func fixedRgOutput(content, pattern []byte, quiet, lineNumber bool) ([]byte, bool) {
	if len(pattern) == 0 {
		return nil, false
	}
	var out bytes.Buffer
	matched := false
	lineNo := 1
	offset := 0
	for offset < len(content) {
		next := bytes.IndexByte(content[offset:], '\n')
		lineEnd := len(content)
		hasNewline := false
		if next >= 0 {
			lineEnd = offset + next + 1
			hasNewline = true
		}
		line := content[offset:lineEnd]
		lineForMatch := line
		if hasNewline {
			lineForMatch = line[:len(line)-1]
		}
		if bytes.Contains(lineForMatch, pattern) {
			matched = true
			if quiet {
				return nil, true
			}
			if lineNumber {
				out.WriteString(strconv.Itoa(lineNo))
				out.WriteByte(':')
			}
			out.Write(lineForMatch)
			out.WriteByte('\n')
		}
		offset = lineEnd
		lineNo++
	}
	return out.Bytes(), matched
}
