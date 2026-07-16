package proofcache

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	repoSearchCorpusVersion        = 2
	repoSearchCorpusHeaderBytes    = 48
	repoSearchCorpusRecordBytes    = 24
	repoSearchCorpusLineBytes      = 24
	repoSearchCorpusMaxFiles       = 10000
	repoSearchCorpusMaxFileBytes   = 8 * 1024 * 1024
	repoSearchCorpusFoldUnsafeFlag = uint32(1 << 31)
)

var repoSearchCorpusMagic = [8]byte{'S', 'Q', 'R', 'G', 'C', '0', '0', '1'}

type preparedRepoSearchCorpus struct {
	Entry        PreparedEntry
	Content      []byte
	NativeWallMS int64
}

type repoSearchCorpusFile struct {
	path       string
	content    []byte
	lines      []repoSearchCorpusLine
	foldUnsafe bool
}

type repoSearchCorpusLine struct {
	offset      uint32
	length      uint32
	bloom       uint64
	foldedBloom uint64
}

func (k *Engine) prewarmRepoSearchCorpus(ctx context.Context, cwd string, ws WorldState, ledger *ValidityLedger, phases *PhaseTimings) bool {
	if k == nil || k.Store == nil || ledger == nil || !ws.OracleAvailable || ws.RepoRoot == "" {
		return false
	}
	// A user ripgrep config can change parsing and output semantics. Exact-query
	// replay still supports it; the generalized evaluator deliberately does not.
	if os.Getenv("RIPGREP_CONFIG_PATH") != "" {
		return false
	}
	fingerprints, epoch, key, root, ok := repoSearchCorpusProof(cwd, ws)
	if !ok {
		return false
	}
	buildStart := time.Now()
	corpus, ok := buildRepoSearchCorpus(ctx, root)
	phases.OutputMaterializeMS += elapsedMS(buildStart)
	if !ok {
		return false
	}
	refreshedFingerprints, refreshedEpoch, refreshedKey, _, proofOK := repoSearchCorpusProof(root, ws)
	if !proofOK || refreshedEpoch != epoch || refreshedKey != key || !mapsEqual(refreshedFingerprints, fingerprints) {
		return false
	}
	writeStart := time.Now()
	ref, err := k.Store.StoreRepoSearchCorpus(key, corpus)
	phases.DBOrFileWriteMS += elapsedMS(writeStart)
	if err != nil {
		return false
	}
	ledger.UpsertPrepared(PreparedEntry{
		PreparedID:           hashString("prepared:repo-search-corpus:" + key + ":" + epoch),
		Kind:                 PreparedKindRepoSearchCorpus,
		OperatorFamily:       FamilySearchList,
		NormalizedCommand:    "bounded repository search corpus",
		InputFingerprints:    cloneStringMap(fingerprints),
		HotFingerprints:      cloneStringMap(fingerprints),
		OutputFingerprints:   map[string]string{"repo_search_corpus": hashBytes(corpus)},
		InvalidationEpoch:    epoch,
		HotInvalidationEpoch: epoch,
		EvidenceQuality:      EvidenceStrong,
		ReplayEligible:       true,
		OutputRef:            ref,
		Privacy:              "eligible local repository paths and bytes stored locally for proof-bound bounded search",
		PreparedAt:           time.Now(),
		Notes:                []string{"native rg selected the complete default and guarded-hidden file sets; unsupported search semantics remain native"},
	})
	return true
}

func repoSearchCorpusProof(cwd string, ws WorldState) (map[string]string, string, string, string, bool) {
	root := ws.RepoRoot
	gitDir := ws.GitDirAbs
	if root == "" || gitDir == "" {
		var ok bool
		root, gitDir, ok = discoverGitDir(cwd)
		if !ok {
			return nil, "", "", "", false
		}
	}
	root = absPath(root)
	tree, content, complete := exactWorkspaceEpochs(root, repoSearchCorpusMaxFiles, true)
	if !complete {
		return nil, "", "", "", false
	}
	tool, ok := executableSignal(root, "rg")
	if !ok {
		return nil, "", "", "", false
	}
	ignore := workspaceIgnoreFingerprint(root, gitDir)
	config := ripgrepConfigFingerprint()
	environment := ripgrepEnvironmentFingerprint()
	key := hashString(root)
	fingerprints := map[string]string{
		"repo_search_corpus":  key,
		"repo_root":           hashString(root),
		"ignore_rules":        ignore,
		"ripgrep_config":      config,
		"ripgrep_environment": environment,
		"file_tree_epoch":     tree,
		"file_content_epoch":  content,
		"tool_path":           tool.PathHash,
		"tool_executable":     tool.FileHash,
	}
	epoch := "hot-repo-search-corpus:" + hashString(strings.Join([]string{
		root, ignore, config, environment, tree, content, tool.FileHash,
	}, "|"))
	return fingerprints, epoch, key, root, true
}

func buildRepoSearchCorpus(ctx context.Context, root string) ([]byte, bool) {
	defaultPaths, ok := nativeRepoSearchPaths(ctx, root, false)
	if !ok {
		return nil, false
	}
	hiddenPaths, ok := nativeRepoSearchPaths(ctx, root, true)
	if !ok {
		return nil, false
	}
	files := make([]repoSearchCorpusFile, 0, len(hiddenPaths))
	indices := make(map[string]uint32, len(hiddenPaths))
	addPaths := func(paths []string) ([]uint32, bool) {
		order := make([]uint32, 0, len(paths))
		for _, rel := range paths {
			if index, found := indices[rel]; found {
				order = append(order, index)
				continue
			}
			path, valid := safeRepoSearchCorpusPath(root, rel)
			if !valid {
				return nil, false
			}
			content, err := os.ReadFile(path)
			if err != nil || len(content) > repoSearchCorpusMaxFileBytes {
				return nil, false
			}
			lines, foldUnsafe := indexRepoSearchCorpusLines(content)
			index := uint32(len(files))
			indices[rel] = index
			files = append(files, repoSearchCorpusFile{
				path:       rel,
				content:    content,
				lines:      lines,
				foldUnsafe: foldUnsafe,
			})
			order = append(order, index)
			if len(files) > repoSearchCorpusMaxFiles {
				return nil, false
			}
		}
		return order, true
	}
	defaultOrder, ok := addPaths(defaultPaths)
	if !ok {
		return nil, false
	}
	hiddenOrder, ok := addPaths(hiddenPaths)
	if !ok || len(files) == 0 {
		return nil, false
	}
	return encodeRepoSearchCorpus(files, defaultOrder, hiddenOrder)
}

func indexRepoSearchCorpusLines(content []byte) ([]repoSearchCorpusLine, bool) {
	lines := make([]repoSearchCorpusLine, 0, 1+len(content)/80)
	foldUnsafeFile := false
	for offset := 0; offset < len(content); {
		end := offset
		for end < len(content) && content[end] != '\n' {
			end++
		}
		bloom, folded, foldUnsafe := repoSearchLineBlooms(content[offset:end])
		lines = append(lines, repoSearchCorpusLine{
			offset:      uint32(offset),
			length:      uint32(end - offset),
			bloom:       bloom,
			foldedBloom: folded,
		})
		foldUnsafeFile = foldUnsafeFile || foldUnsafe
		if end == len(content) {
			break
		}
		offset = end + 1
	}
	return lines, foldUnsafeFile
}

func repoSearchLineBlooms(line []byte) (uint64, uint64, bool) {
	var bloom uint64
	var foldedBloom uint64
	foldUnsafe := !utf8.Valid(line)
	for _, value := range string(line) {
		if value >= utf8.RuneSelf &&
			(unicode.ToLower(value) < utf8.RuneSelf || unicode.ToUpper(value) < utf8.RuneSelf) {
			foldUnsafe = true
		}
	}
	for width := 1; width <= 3; width++ {
		for start := 0; start+width <= len(line); start++ {
			window := line[start : start+width]
			bloom |= repoSearchBloomBits(window)
			var folded [3]byte
			for i, value := range window {
				if value >= 'A' && value <= 'Z' {
					value += 'a' - 'A'
				}
				folded[i] = value
			}
			foldedBloom |= repoSearchBloomBits(folded[:width])
		}
	}
	return bloom, foldedBloom, foldUnsafe
}

func repoSearchBloomBits(value []byte) uint64 {
	hash := uint64(1469598103934665603)
	hash ^= uint64(len(value))
	hash *= 1099511628211
	for _, b := range value {
		hash ^= uint64(b)
		hash *= 1099511628211
	}
	return uint64(1)<<(hash&63) | uint64(1)<<((hash>>6)&63)
}

func nativeRepoSearchPaths(ctx context.Context, root string, hidden bool) ([]string, bool) {
	argv := []string{"rg", "--files", "-0"}
	if hidden {
		argv = []string{"rg", "--files", "--hidden", "--glob", "!**/.git/**", "-0"}
	}
	result := runNative(ctx, root, argv)
	if result.ExitCode != 0 || len(result.Stderr) != 0 || len(result.Stdout) == 0 || result.Stdout[len(result.Stdout)-1] != 0 {
		return nil, false
	}
	raw := strings.Split(string(result.Stdout[:len(result.Stdout)-1]), "\x00")
	paths := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, rel := range raw {
		rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
		rel = strings.TrimPrefix(rel, "./")
		if rel == "" || rel == "." || len(rel) >= 4096 {
			return nil, false
		}
		if _, duplicate := seen[rel]; duplicate {
			return nil, false
		}
		seen[rel] = struct{}{}
		paths = append(paths, rel)
	}
	return paths, len(paths) <= repoSearchCorpusMaxFiles
}

func safeRepoSearchCorpusPath(root, rel string) (string, bool) {
	if rel == "" || filepath.IsAbs(rel) || strings.ContainsRune(rel, '\x00') {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	path := filepath.Join(root, clean)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > repoSearchCorpusMaxFileBytes {
		return "", false
	}
	return path, pathWithinRoot(path, root)
}

func encodeRepoSearchCorpus(files []repoSearchCorpusFile, defaultOrder, hiddenOrder []uint32) ([]byte, bool) {
	if len(files) == 0 || len(files) > repoSearchCorpusMaxFiles {
		return nil, false
	}
	recordsOffset := repoSearchCorpusHeaderBytes
	defaultOffset := recordsOffset + len(files)*repoSearchCorpusRecordBytes
	hiddenOffset := defaultOffset + len(defaultOrder)*4
	lineRecordsOffset := hiddenOffset + len(hiddenOrder)*4
	lineCount := 0
	for _, file := range files {
		lineCount += len(file.lines)
	}
	payloadOffset := lineRecordsOffset + lineCount*repoSearchCorpusLineBytes
	if payloadOffset >= maxRepoSearchCorpusBytes {
		return nil, false
	}
	frame := make([]byte, payloadOffset)
	copy(frame[:8], repoSearchCorpusMagic[:])
	binary.LittleEndian.PutUint32(frame[8:12], repoSearchCorpusVersion)
	binary.LittleEndian.PutUint32(frame[12:16], uint32(len(files)))
	binary.LittleEndian.PutUint32(frame[16:20], uint32(len(defaultOrder)))
	binary.LittleEndian.PutUint32(frame[20:24], uint32(len(hiddenOrder)))
	binary.LittleEndian.PutUint32(frame[24:28], uint32(recordsOffset))
	binary.LittleEndian.PutUint32(frame[28:32], uint32(defaultOffset))
	binary.LittleEndian.PutUint32(frame[32:36], uint32(hiddenOffset))
	binary.LittleEndian.PutUint32(frame[36:40], uint32(payloadOffset))
	binary.LittleEndian.PutUint32(frame[44:48], uint32(lineRecordsOffset))
	for i, index := range defaultOrder {
		if int(index) >= len(files) {
			return nil, false
		}
		binary.LittleEndian.PutUint32(frame[defaultOffset+i*4:defaultOffset+(i+1)*4], index)
	}
	for i, index := range hiddenOrder {
		if int(index) >= len(files) {
			return nil, false
		}
		binary.LittleEndian.PutUint32(frame[hiddenOffset+i*4:hiddenOffset+(i+1)*4], index)
	}
	lineIndex := 0
	for i, file := range files {
		pathOffset := len(frame)
		frame = append(frame, file.path...)
		contentOffset := len(frame)
		frame = append(frame, file.content...)
		if len(frame) > maxRepoSearchCorpusBytes {
			return nil, false
		}
		record := frame[recordsOffset+i*repoSearchCorpusRecordBytes : recordsOffset+(i+1)*repoSearchCorpusRecordBytes]
		binary.LittleEndian.PutUint32(record[0:4], uint32(pathOffset))
		binary.LittleEndian.PutUint32(record[4:8], uint32(len(file.path)))
		binary.LittleEndian.PutUint32(record[8:12], uint32(contentOffset))
		binary.LittleEndian.PutUint32(record[12:16], uint32(len(file.content)))
		binary.LittleEndian.PutUint32(record[16:20], uint32(lineIndex))
		encodedLineCount := uint32(len(file.lines))
		if file.foldUnsafe {
			encodedLineCount |= repoSearchCorpusFoldUnsafeFlag
		}
		binary.LittleEndian.PutUint32(record[20:24], encodedLineCount)
		for _, line := range file.lines {
			lineRecord := frame[lineRecordsOffset+lineIndex*repoSearchCorpusLineBytes : lineRecordsOffset+(lineIndex+1)*repoSearchCorpusLineBytes]
			binary.LittleEndian.PutUint32(lineRecord[0:4], line.offset)
			binary.LittleEndian.PutUint32(lineRecord[4:8], line.length)
			binary.LittleEndian.PutUint64(lineRecord[8:16], line.bloom)
			binary.LittleEndian.PutUint64(lineRecord[16:24], line.foldedBloom)
			lineIndex++
		}
	}
	binary.LittleEndian.PutUint32(frame[40:44], uint32(len(frame)))
	return frame, true
}
