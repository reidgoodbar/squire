package proofcache

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
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
		if k.prepareWarmFileFromCommand(cwd, argv, ws, ledger, phases, "Level 3 workspace image bytes prepared for arbitrary bounded cat/sed/head/tail/nl/grep/rg replay; native fallback still available") {
			count++
			reports = append(reports, WarmPreparedReport{
				Kind:              PreparedKindWarmFile,
				OperatorFamily:    FamilyFileInspection,
				NormalizedCommand: "warm eligible workspace file",
				ReplayEligible:    true,
				EvidenceQuality:   ws.EvidenceQuality,
				Privacy:           "eligible local workspace file bytes stored locally for exact bounded cat/sed/head/tail/nl/grep/rg replay",
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
		Privacy:              "eligible local workspace file bytes stored locally for exact bounded cat/sed/head/tail/nl/grep/rg replay",
		PreparedAt:           time.Now(),
		Notes:                []string{note},
	})
	return true
}

func (k *Engine) findPreparedWarmFileReplay(inv CommandInvocation, diagnostics []string, phases *PhaseTimings) (preparedReplay, []string, bool) {
	candidate, diagnostics, ok := k.findCurrentPreparedWarmFile(inv, diagnostics, phases)
	if !ok {
		return preparedReplay{}, diagnostics, false
	}
	stdout, exitCode, ok := warmFileCommandOutputIndexed(candidate.Content, candidate.LineStarts, inv.PolicyArgv)
	if !ok || len(stdout) > maxFileInspectionOutputBytes {
		return preparedReplay{}, diagnostics, false
	}
	stderr := []byte(nil)
	obs := Observation{
		OperationID:  hashString("warm-file-replay:" + candidate.Entry.PreparedID + ":" + normalizeArgv(inv.PolicyArgv)),
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

func (k *Engine) findCurrentPreparedWarmFile(inv CommandInvocation, diagnostics []string, phases *PhaseTimings) (preparedWarmFile, []string, bool) {
	key, hotFPS, hotEpoch, _, ok := warmFileHotProof(inv.PolicyCWD, inv.PolicyArgv)
	if !ok {
		return preparedWarmFile{}, diagnostics, false
	}
	candidates, warmDiagnostics := k.residentPreparedWarmFileCandidates(key, phases)
	diagnostics = append(diagnostics, warmDiagnostics...)
	if len(candidates) == 0 {
		return preparedWarmFile{}, diagnostics, false
	}
	for _, candidate := range candidates {
		if candidate.Entry.HotInvalidationEpoch != hotEpoch || !mapsEqual(candidate.Entry.HotFingerprints, hotFPS) {
			continue
		}
		return candidate, diagnostics, true
	}
	return preparedWarmFile{}, diagnostics, false
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
	operation, ok := parseWarmFileOperation(argv)
	if !ok {
		return "", nil, "", "", false
	}
	root := absPath(cwd)
	argPath := operation.path
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
	if operation.kind == warmFileOperationCat && info.Size() > maxFileInspectionOutputBytes {
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
	if operation, ok := parseWarmFileOperation(argv); ok {
		return operation.path
	}
	if isReplayableFileType(argv) {
		return argv[1]
	}
	return ""
}

func warmFileCommandOutput(content []byte, argv []string) ([]byte, int, bool) {
	return warmFileCommandOutputIndexed(content, nil, argv)
}

func warmFileCommandOutputIndexed(content []byte, lineStarts []int, argv []string) ([]byte, int, bool) {
	operation, ok := parseWarmFileOperation(argv)
	if !ok {
		return nil, 0, false
	}
	switch operation.kind {
	case warmFileOperationCat:
		return append([]byte(nil), content...), 0, true
	case warmFileOperationSed:
		stdout, ok := sedPrintSelectionBytesIndexed(content, lineStarts, operation.selection, maxFileInspectionOutputBytes)
		return stdout, 0, ok
	case warmFileOperationHead:
		return sedPrintRangeBytesIndexed(content, lineStarts, 1, operation.lineCount), 0, true
	case warmFileOperationTail:
		return tailLineBytesIndexed(content, lineStarts, operation.lineCount), 0, true
	case warmFileOperationNL:
		stdout, ok := numberedAllLinesBytes(content, maxFileInspectionOutputBytes)
		return stdout, 0, ok
	case warmFileOperationGrep:
		if bytes.IndexByte(content, 0) >= 0 {
			return nil, 0, false
		}
		stdout, matched := fixedGrepOutput(content, []byte(operation.pattern), operation.quiet)
		if !matched {
			return nil, 1, true
		}
		return stdout, 0, true
	case warmFileOperationRg:
		if bytes.IndexByte(content, 0) >= 0 {
			return nil, 0, false
		}
		stdout, matched := fixedRgOutput(content, []byte(operation.pattern), operation.quiet, operation.lineNumber)
		if !matched {
			return nil, 1, true
		}
		return stdout, 0, true
	default:
		return nil, 0, false
	}
}

func numberedAllLinesBytes(content []byte, maxOutput int) ([]byte, bool) {
	return numberedLineSelectionBytes(content, singleLineSelection(1, int(^uint(0)>>1)), maxOutput)
}

func numberedLineRangeBytes(content []byte, start, end, maxOutput int) ([]byte, bool) {
	return numberedLineSelectionBytes(content, singleLineSelection(start, end), maxOutput)
}

func numberedLineSelectionBytes(content []byte, selection lineSelection, maxOutput int) ([]byte, bool) {
	if !selection.valid() {
		return nil, false
	}
	if len(content) == 0 {
		return nil, true
	}
	for offset := 0; offset < len(content); {
		next := bytes.IndexByte(content[offset:], '\n')
		lineEnd := len(content)
		if next >= 0 {
			lineEnd = offset + next + 1
		}
		logical := content[offset:lineEnd]
		if len(logical) > 0 && logical[len(logical)-1] == '\n' {
			logical = logical[:len(logical)-1]
		}
		if isDefaultNLLogicalPageDelimiter(logical) {
			return nil, false
		}
		offset = lineEnd
	}
	var out bytes.Buffer
	lineNo := 1
	maxEnd := selection.maxEnd()
	for offset := 0; offset < len(content); lineNo++ {
		next := bytes.IndexByte(content[offset:], '\n')
		lineEnd := len(content)
		if next >= 0 {
			lineEnd = offset + next + 1
		}
		line := content[offset:lineEnd]
		matches := selection.matchCount(lineNo)
		for match := 0; match < matches; match++ {
			digits := strconv.Itoa(lineNo)
			for padding := 6 - len(digits); padding > 0; padding-- {
				out.WriteByte(' ')
			}
			out.WriteString(digits)
			out.WriteByte('\t')
			out.Write(line)
			if runtime.GOOS == "linux" && (len(line) == 0 || line[len(line)-1] != '\n') {
				out.WriteByte('\n')
			}
			if maxOutput > 0 && out.Len() > maxOutput {
				return nil, false
			}
		}
		if lineNo >= maxEnd {
			break
		}
		offset = lineEnd
	}
	return out.Bytes(), true
}

func isDefaultNLLogicalPageDelimiter(line []byte) bool {
	if len(line) != 2 && len(line) != 4 && len(line) != 6 {
		return false
	}
	for i := 0; i < len(line); i += 2 {
		if line[i] != '\\' || line[i+1] != ':' {
			return false
		}
	}
	return true
}

func sedPrintRangeBytes(content []byte, start, end int) []byte {
	return sedPrintRangeBytesIndexed(content, nil, start, end)
}

func sedPrintRangeBytesIndexed(content []byte, lineStarts []int, start, end int) []byte {
	stdout, _ := sedPrintSelectionBytesIndexed(content, lineStarts, singleLineSelection(start, end), 0)
	return stdout
}

func sedPrintSelectionBytesIndexed(content []byte, lineStarts []int, selection lineSelection, maxOutput int) ([]byte, bool) {
	if !selection.valid() || len(content) == 0 {
		return nil, selection.valid()
	}
	var out bytes.Buffer
	maxEnd := selection.maxEnd()
	if len(lineStarts) > 0 {
		for lineNo := 1; lineNo <= maxEnd && lineNo <= len(lineStarts); lineNo++ {
			matches := selection.matchCount(lineNo)
			if matches == 0 {
				continue
			}
			start := lineStarts[lineNo-1]
			end := lineEndOffset(content, lineStarts, lineNo-1)
			for match := 0; match < matches; match++ {
				if maxOutput > 0 && out.Len()+end-start > maxOutput {
					return nil, false
				}
				out.Write(content[start:end])
			}
		}
		return out.Bytes(), true
	}
	lineNo := 1
	offset := 0
	for offset < len(content) && lineNo <= maxEnd {
		next := bytes.IndexByte(content[offset:], '\n')
		lineEnd := len(content)
		if next >= 0 {
			lineEnd = offset + next + 1
		}
		for match := selection.matchCount(lineNo); match > 0; match-- {
			if maxOutput > 0 && out.Len()+lineEnd-offset > maxOutput {
				return nil, false
			}
			out.Write(content[offset:lineEnd])
		}
		offset = lineEnd
		lineNo++
	}
	return out.Bytes(), true
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
