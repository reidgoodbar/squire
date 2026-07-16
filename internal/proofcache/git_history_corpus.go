package proofcache

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	gitHistoryCorpusVersion     = 1
	gitHistoryCorpusHeaderBytes = 32
	gitHistoryCorpusRecordBytes = 32
	gitHistoryCorpusPathBytes   = 8
	gitHistoryCorpusMaxCommits  = 512
	gitHistoryCorpusMaxBytes    = 8 * 1024 * 1024
)

var gitHistoryCorpusMagic = [8]byte{'S', 'Q', 'G', 'I', 'T', 'H', '0', '1'}

type gitHistoryCorpusCommit struct {
	hash        string
	oneline     []byte
	parentCount uint32
	paths       []string
}

func (k *Engine) prewarmGitHistoryCorpus(ctx context.Context, cwd string, ws WorldState, ledger *ValidityLedger, phases *PhaseTimings) bool {
	if k == nil || k.Store == nil || ledger == nil || !ws.OracleAvailable || ws.RepoRoot == "" {
		return false
	}
	fingerprints, epoch, key, root, ok := gitHistoryCorpusProof(cwd, ws)
	if !ok {
		return false
	}
	buildStart := time.Now()
	corpus, ok := buildGitHistoryCorpus(ctx, root)
	phases.OutputMaterializeMS += elapsedMS(buildStart)
	if !ok {
		return false
	}
	refreshedFingerprints, refreshedEpoch, refreshedKey, _, proofOK := gitHistoryCorpusProof(root, ws)
	if !proofOK || refreshedEpoch != epoch || refreshedKey != key || !mapsEqual(refreshedFingerprints, fingerprints) {
		return false
	}
	writeStart := time.Now()
	ref, err := k.Store.StoreRepoSearchCorpus("git-history-"+key, corpus)
	phases.DBOrFileWriteMS += elapsedMS(writeStart)
	if err != nil {
		return false
	}
	ledger.UpsertPrepared(PreparedEntry{
		PreparedID:           hashString("prepared:git-history-corpus:" + key + ":" + epoch),
		Kind:                 PreparedKindGitHistoryCorpus,
		OperatorFamily:       FamilyLocalRepoMetadata,
		NormalizedCommand:    "bounded git history corpus",
		InputFingerprints:    cloneStringMap(fingerprints),
		HotFingerprints:      cloneStringMap(fingerprints),
		OutputFingerprints:   map[string]string{"git_history_corpus": hashBytes(corpus)},
		InvalidationEpoch:    epoch,
		HotInvalidationEpoch: epoch,
		EvidenceQuality:      EvidenceStrong,
		ReplayEligible:       true,
		OutputRef:            ref,
		Privacy:              "recent commit oneline output and changed paths stored locally for bounded proof-gated history queries",
		PreparedAt:           time.Now(),
		Notes:                []string{"bounded path history replays stop before merge ambiguity; unsupported history semantics remain native"},
	})
	return true
}

func gitHistoryCorpusProof(cwd string, ws WorldState) (map[string]string, string, string, string, bool) {
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
	head, branch, ok := readHeadAndBranch(gitDir)
	if !ok || len(head) != 40 {
		return nil, "", "", "", false
	}
	tool, ok := executableSignal(root, "git")
	if !ok {
		return nil, "", "", "", false
	}
	objects, ok := gitObjectNamespaceFingerprint(gitDir)
	if !ok {
		return nil, "", "", "", false
	}
	config := gitConfigSummaryFingerprint(root, gitDir)
	view := gitLogViewFingerprint(gitDir)
	key := hashString(root)
	fingerprints := map[string]string{
		"git_history_corpus":   key,
		"repo_root":            hashString(root),
		"head":                 hashString(head),
		"branch":               hashString(branch),
		"git_config":           config,
		"git_log_view":         view,
		"git_object_namespace": objects,
		"tool_path":            tool.PathHash,
		"tool_executable":      tool.FileHash,
	}
	epoch := "hot-git-history-corpus:" + hashString(strings.Join([]string{
		root, head, branch, config, view, objects, tool.FileHash,
	}, "|"))
	return fingerprints, epoch, key, root, true
}

func gitObjectNamespaceFingerprint(gitDir string) (string, bool) {
	for _, key := range []string{
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_COMMON_DIR",
		"GIT_NAMESPACE",
		"GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_NO_REPLACE_OBJECTS",
		"GIT_REPLACE_REF_BASE",
		"GIT_SHALLOW_FILE",
		"GIT_LITERAL_PATHSPECS",
		"GIT_GLOB_PATHSPECS",
		"GIT_NOGLOB_PATHSPECS",
		"GIT_ICASE_PATHSPECS",
		"GIT_CEILING_DIRECTORIES",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM",
	} {
		if os.Getenv(key) != "" {
			return "", false
		}
	}
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		if strings.TrimSpace(string(data)) != "" {
			return "", false
		}
	} else if !os.IsNotExist(err) {
		return "", false
	}
	objectsDir := filepath.Join(gitDir, "objects")
	info, err := os.Stat(objectsDir)
	if err != nil || !info.IsDir() {
		return "", false
	}
	alternates := filepath.Join(objectsDir, "info", "alternates")
	if data, err := os.ReadFile(alternates); err == nil {
		if strings.TrimSpace(string(data)) != "" {
			return "", false
		}
	} else if !os.IsNotExist(err) {
		return "", false
	}

	parts := []string{"format:sha1"}
	entries, err := os.ReadDir(objectsDir)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(objectsDir, name)
		if len(name) == 2 && lowerHex(name) {
			loose, err := os.ReadDir(path)
			if err != nil {
				return "", false
			}
			for _, object := range loose {
				objectName := object.Name()
				objectInfo, err := os.Stat(filepath.Join(path, objectName))
				if err != nil {
					return "", false
				}
				if objectInfo.Mode().IsRegular() && len(objectName) == 38 && lowerHex(objectName) {
					parts = append(parts, "loose:"+name+objectName)
				}
			}
			continue
		}
		if name != "pack" {
			continue
		}
		packed, err := os.ReadDir(path)
		if err != nil {
			return "", false
		}
		for _, item := range packed {
			itemName := item.Name()
			if !strings.HasSuffix(itemName, ".idx") && itemName != "multi-pack-index" {
				continue
			}
			itemPath := filepath.Join(path, itemName)
			itemInfo, err := os.Stat(itemPath)
			if err != nil || !itemInfo.Mode().IsRegular() {
				return "", false
			}
			label := "pack-index:" + itemName
			if itemName == "multi-pack-index" {
				label = "multi-pack-index"
			}
			parts = append(parts, label+"\x00"+fileHashOrMissing(itemPath))
		}
	}
	sort.Strings(parts)
	return hashString(strings.Join(parts, "\n")), true
}

func lowerHex(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if !((value[i] >= '0' && value[i] <= '9') || (value[i] >= 'a' && value[i] <= 'f')) {
			return false
		}
	}
	return true
}

func buildGitHistoryCorpus(ctx context.Context, root string) ([]byte, bool) {
	limit := gitHistoryCorpusMaxCommits + 1
	metadata := runNative(ctx, root, []string{
		"git", "log", "-" + strconv.Itoa(limit), "-z", "--format=%H%x00%P",
	})
	oneline := runNative(ctx, root, []string{
		"git", "log", "-" + strconv.Itoa(limit), "--oneline", "-z",
	})
	if metadata.ExitCode != 0 || len(metadata.Stderr) != 0 || oneline.ExitCode != 0 || len(oneline.Stderr) != 0 {
		return nil, false
	}
	metaFields, ok := splitNULTerminated(metadata.Stdout)
	if !ok || len(metaFields)%2 != 0 {
		return nil, false
	}
	onelineFields, ok := splitNULTerminated(oneline.Stdout)
	if !ok || len(onelineFields) != len(metaFields)/2 || len(onelineFields) == 0 {
		return nil, false
	}
	complete := len(onelineFields) <= gitHistoryCorpusMaxCommits
	count := len(onelineFields)
	if count > gitHistoryCorpusMaxCommits {
		count = gitHistoryCorpusMaxCommits
	}
	commits := make([]gitHistoryCorpusCommit, count)
	for i := 0; i < count; i++ {
		hash := string(metaFields[i*2])
		parents := string(metaFields[i*2+1])
		line := onelineFields[i]
		if !validFullGitHash(hash) || len(line) < 6 {
			return nil, false
		}
		space := bytes.IndexByte(line, ' ')
		if space < 4 || space > 40 || !strings.HasPrefix(hash, string(line[:space])) {
			return nil, false
		}
		parentCount := uint32(0)
		if parents != "" {
			for _, parent := range strings.Fields(parents) {
				if !validFullGitHash(parent) {
					return nil, false
				}
				parentCount++
			}
		}
		commits[i] = gitHistoryCorpusCommit{
			hash:        hash,
			oneline:     append([]byte(nil), line...),
			parentCount: parentCount,
		}
	}
	if !populateGitHistoryPaths(ctx, root, commits) {
		return nil, false
	}
	return encodeGitHistoryCorpus(commits, complete)
}

func splitNULTerminated(value []byte) ([][]byte, bool) {
	if len(value) == 0 || value[len(value)-1] != 0 {
		return nil, false
	}
	return bytes.Split(value[:len(value)-1], []byte{0}), true
}

func validFullGitHash(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, b := range []byte(value) {
		if !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')) {
			return false
		}
	}
	return true
}

func populateGitHistoryPaths(ctx context.Context, root string, commits []gitHistoryCorpusCommit) bool {
	if len(commits) == 0 {
		return false
	}
	var input strings.Builder
	for _, commit := range commits {
		input.WriteString(commit.hash)
		input.WriteByte('\n')
	}
	cmd := exec.CommandContext(ctx, "git", "diff-tree", "--stdin", "--root", "--name-only", "--no-renames", "-r", "-z")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(input.String())
	configureNativeCommandCleanup(cmd)
	stdout, err := cmd.Output()
	if err != nil {
		return false
	}
	fields, ok := splitNULTerminated(stdout)
	if !ok {
		return false
	}
	position := 0
	for i := range commits {
		if position >= len(fields) || string(fields[position]) != commits[i].hash {
			return false
		}
		position++
		nextHash := ""
		if i+1 < len(commits) {
			nextHash = commits[i+1].hash
		}
		for position < len(fields) && (nextHash == "" || string(fields[position]) != nextHash) {
			path := string(fields[position])
			if validFullGitHash(path) || !safeGitHistoryPath(path) {
				return false
			}
			commits[i].paths = append(commits[i].paths, filepath.ToSlash(filepath.Clean(filepath.FromSlash(path))))
			position++
		}
	}
	return position == len(fields)
}

func safeGitHistoryPath(path string) bool {
	if path == "" || filepath.IsAbs(filepath.FromSlash(path)) || strings.ContainsRune(path, '\x00') || len(path) >= 4096 {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func encodeGitHistoryCorpus(commits []gitHistoryCorpusCommit, complete bool) ([]byte, bool) {
	if len(commits) == 0 || len(commits) > gitHistoryCorpusMaxCommits {
		return nil, false
	}
	pathCount := 0
	for _, commit := range commits {
		pathCount += len(commit.paths)
	}
	recordsOffset := gitHistoryCorpusHeaderBytes
	pathRecordsOffset := recordsOffset + len(commits)*gitHistoryCorpusRecordBytes
	payloadOffset := pathRecordsOffset + pathCount*gitHistoryCorpusPathBytes
	if payloadOffset >= gitHistoryCorpusMaxBytes {
		return nil, false
	}
	frame := make([]byte, payloadOffset)
	copy(frame[:8], gitHistoryCorpusMagic[:])
	binary.LittleEndian.PutUint32(frame[8:12], gitHistoryCorpusVersion)
	binary.LittleEndian.PutUint32(frame[12:16], uint32(len(commits)))
	binary.LittleEndian.PutUint32(frame[16:20], uint32(recordsOffset))
	binary.LittleEndian.PutUint32(frame[20:24], uint32(pathRecordsOffset))
	binary.LittleEndian.PutUint32(frame[24:28], uint32(payloadOffset))
	if complete {
		binary.LittleEndian.PutUint32(frame[28:32], 1)
	}
	pathIndex := 0
	for i, commit := range commits {
		hashOffset := len(frame)
		frame = append(frame, commit.hash...)
		lineOffset := len(frame)
		frame = append(frame, commit.oneline...)
		record := frame[recordsOffset+i*gitHistoryCorpusRecordBytes : recordsOffset+(i+1)*gitHistoryCorpusRecordBytes]
		binary.LittleEndian.PutUint32(record[0:4], uint32(hashOffset))
		binary.LittleEndian.PutUint32(record[4:8], uint32(len(commit.hash)))
		binary.LittleEndian.PutUint32(record[8:12], uint32(lineOffset))
		binary.LittleEndian.PutUint32(record[12:16], uint32(len(commit.oneline)))
		binary.LittleEndian.PutUint32(record[16:20], uint32(pathIndex))
		binary.LittleEndian.PutUint32(record[20:24], uint32(len(commit.paths)))
		binary.LittleEndian.PutUint32(record[24:28], commit.parentCount)
		for _, path := range commit.paths {
			pathOffset := len(frame)
			frame = append(frame, path...)
			pathRecord := frame[pathRecordsOffset+pathIndex*gitHistoryCorpusPathBytes : pathRecordsOffset+(pathIndex+1)*gitHistoryCorpusPathBytes]
			binary.LittleEndian.PutUint32(pathRecord[0:4], uint32(pathOffset))
			binary.LittleEndian.PutUint32(pathRecord[4:8], uint32(len(path)))
			pathIndex++
		}
		if len(frame) > gitHistoryCorpusMaxBytes {
			return nil, false
		}
	}
	return frame, true
}
