package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxFastPathOutputBytes = 64 * 1024
const maxReplayableInspectionFileBytes = 256 * 1024

type ValidityLedger struct {
	Version  int             `json:"version"`
	Entries  []LedgerEntry   `json:"entries"`
	Prepared []PreparedEntry `json:"prepared,omitempty"`
}

type LedgerStore struct {
	Root string
}

func DefaultStoreRoot(cwd string) string {
	if res := runNative(context.Background(), cwd, []string{"git", "rev-parse", "--absolute-git-dir"}); res.ExitCode == 0 {
		return filepath.Join(strings.TrimSpace(string(res.Stdout)), "squire", "kernel")
	}
	return filepath.Join(cwd, ".squire", "kernel")
}

func NewLedgerStore(root string) *LedgerStore {
	return &LedgerStore{Root: root}
}

func (s *LedgerStore) Init() error {
	if err := os.MkdirAll(filepath.Join(s.Root, "outputs"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(s.Root, "warm_files"), 0o700); err != nil {
		return err
	}
	cfg := filepath.Join(s.Root, "config.json")
	if _, err := os.Stat(cfg); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(cfg, []byte("{\n  \"privacy_mode\": \"standard\"\n}\n"), 0o600); err != nil {
			return err
		}
	}
	ledgerPath := filepath.Join(s.Root, "ledger.json")
	if _, err := os.Stat(ledgerPath); errors.Is(err, os.ErrNotExist) {
		return s.Save(&ValidityLedger{Version: 1})
	}
	return nil
}

func (s *LedgerStore) Load() (*ValidityLedger, error) {
	b, err := os.ReadFile(filepath.Join(s.Root, "ledger.json"))
	if err != nil {
		return nil, err
	}
	var ledger ValidityLedger
	if err := json.Unmarshal(b, &ledger); err != nil {
		return nil, err
	}
	if ledger.Version == 0 {
		ledger.Version = 1
	}
	return &ledger, nil
}

func (s *LedgerStore) Save(ledger *ValidityLedger) error {
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return err
	}
	return s.withLock(func() error {
		b, err := json.MarshalIndent(mergeWithCurrentLedger(s, ledger), "", "  ")
		if err != nil {
			return err
		}
		path := filepath.Join(s.Root, "ledger.json")
		tmp := filepath.Join(s.Root, fmt.Sprintf("ledger.%d.%d.tmp", os.Getpid(), time.Now().UnixNano()))
		if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return nil
	})
}

func (s *LedgerStore) Signal() string {
	info, err := os.Stat(filepath.Join(s.Root, "ledger.json"))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

func (s *LedgerStore) withLock(fn func() error) error {
	lockDir := filepath.Join(s.Root, "ledger.lock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.Mkdir(lockDir, 0o700)
		if err == nil {
			defer os.Remove(lockDir)
			return fn()
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if info, statErr := os.Stat(lockDir); statErr == nil && time.Since(info.ModTime()) > 30*time.Second {
			_ = os.Remove(lockDir)
			continue
		}
		if time.Now().After(deadline) {
			return errors.New("validity ledger lock timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func mergeWithCurrentLedger(s *LedgerStore, incoming *ValidityLedger) *ValidityLedger {
	base := &ValidityLedger{Version: 1}
	if b, err := os.ReadFile(filepath.Join(s.Root, "ledger.json")); err == nil {
		var current ValidityLedger
		if json.Unmarshal(b, &current) == nil {
			base = current.CloneForSave()
		}
	}
	mergeLedgerInto(base, incoming)
	return base.CloneForSave()
}

func mergeLedgerInto(base, incoming *ValidityLedger) {
	if incoming == nil {
		return
	}
	if base.Version == 0 {
		base.Version = 1
	}
	for _, entry := range incoming.CloneForSave().Entries {
		mergeEntryInto(base, entry)
	}
	for _, prepared := range incoming.CloneForSave().Prepared {
		mergePreparedInto(base, prepared)
	}
}

func mergeEntryInto(base *ValidityLedger, incoming LedgerEntry) {
	for i := range base.Entries {
		current := &base.Entries[i]
		if current.OperationKey != incoming.OperationKey || current.InvalidationEpoch != incoming.InvalidationEpoch || !mapsEqual(current.InputFingerprints, incoming.InputFingerprints) {
			continue
		}
		current.OutputFingerprints = preferStringMap(current.OutputFingerprints, incoming.OutputFingerprints)
		if incoming.Observation.OutputRef != "" {
			current.Observation = incoming.Observation
		}
		current.ShadowMatchCount = maxInt(current.ShadowMatchCount, incoming.ShadowMatchCount)
		current.ShadowMismatchCount = maxInt(current.ShadowMismatchCount, incoming.ShadowMismatchCount)
		current.ShadowSkipCount = maxInt(current.ShadowSkipCount, incoming.ShadowSkipCount)
		current.WarmObservationCount = maxInt(current.WarmObservationCount, incoming.WarmObservationCount)
		current.ReplacementCount = maxInt(current.ReplacementCount, incoming.ReplacementCount)
		current.FallbackCount = maxInt(current.FallbackCount, incoming.FallbackCount)
		current.ShadowMismatchCategories = mergeIntMaps(current.ShadowMismatchCategories, incoming.ShadowMismatchCategories)
		current.NetROIHistoryMS = append(current.NetROIHistoryMS, incoming.NetROIHistoryMS...)
		current.MismatchExamples = appendLimited(current.MismatchExamples, incoming.MismatchExamples, 5)
		if incoming.LastValidatedAt.After(current.LastValidatedAt) {
			current.LastDecision = incoming.LastDecision
			current.LastValidatedAt = incoming.LastValidatedAt
		}
		return
	}
	base.Entries = append(base.Entries, incoming)
}

func mergePreparedInto(base *ValidityLedger, incoming PreparedEntry) {
	for i := range base.Prepared {
		if base.Prepared[i].PreparedID == incoming.PreparedID {
			if incoming.PreparedAt.After(base.Prepared[i].PreparedAt) || base.Prepared[i].PreparedAt.IsZero() {
				base.Prepared[i] = incoming
			}
			return
		}
	}
	base.Prepared = append(base.Prepared, incoming)
}

func preferStringMap(current, incoming map[string]string) map[string]string {
	if len(incoming) == 0 {
		return cloneStringMap(current)
	}
	return cloneStringMap(incoming)
}

func appendLimited(current, incoming []string, limit int) []string {
	out := append([]string(nil), current...)
	for _, item := range incoming {
		if len(out) >= limit {
			return out
		}
		out = append(out, item)
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *LedgerStore) StoreOutput(key string, stdout, stderr []byte) (string, error) {
	if len(stdout)+len(stderr) > maxFastPathOutputBytes {
		return "", errors.New("fast-path output exceeds bounded store limit")
	}
	ref := hashString(key + "|" + hashBytes(stdout) + "|" + hashBytes(stderr))
	dir := filepath.Join(s.Root, "outputs", ref)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout"), stdout, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "stderr"), stderr, 0o600); err != nil {
		return "", err
	}
	return ref, nil
}

func (s *LedgerStore) LoadOutput(ref string) ([]byte, []byte, error) {
	if ref == "" || strings.Contains(ref, "..") || strings.ContainsRune(ref, filepath.Separator) {
		return nil, nil, errors.New("invalid output ref")
	}
	dir := filepath.Join(s.Root, "outputs", ref)
	stdout, err := os.ReadFile(filepath.Join(dir, "stdout"))
	if err != nil {
		return nil, nil, err
	}
	stderr, err := os.ReadFile(filepath.Join(dir, "stderr"))
	if err != nil {
		return nil, nil, err
	}
	return stdout, stderr, nil
}

func (s *LedgerStore) StoreWarmFile(key string, content []byte) (string, error) {
	if len(content) > maxReplayableInspectionFileBytes {
		return "", errors.New("warm file exceeds bounded store limit")
	}
	ref := hashString("warm-file|" + key + "|" + hashBytes(content))
	dir := filepath.Join(s.Root, "warm_files", ref)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "content"), content, 0o600); err != nil {
		return "", err
	}
	return ref, nil
}

func (s *LedgerStore) LoadWarmFile(ref string) ([]byte, error) {
	if ref == "" || strings.Contains(ref, "..") || strings.ContainsRune(ref, filepath.Separator) {
		return nil, errors.New("invalid warm file ref")
	}
	return os.ReadFile(filepath.Join(s.Root, "warm_files", ref, "content"))
}

func (l *ValidityLedger) FindValid(key string, fingerprints map[string]string, epoch string) (*LedgerEntry, bool) {
	for i := range l.Entries {
		entry := &l.Entries[i]
		if entry.OperationKey != key || entry.InvalidationEpoch != epoch {
			continue
		}
		if mapsEqual(entry.InputFingerprints, fingerprints) {
			return entry, true
		}
	}
	return nil, false
}

func (l *ValidityLedger) CloneForSave() *ValidityLedger {
	if l == nil {
		return &ValidityLedger{Version: 1}
	}
	cp := &ValidityLedger{
		Version:  l.Version,
		Entries:  make([]LedgerEntry, len(l.Entries)),
		Prepared: make([]PreparedEntry, len(l.Prepared)),
	}
	for i, entry := range l.Entries {
		cp.Entries[i] = entry
		cp.Entries[i].StdoutBytes = nil
		cp.Entries[i].StderrBytes = nil
		cp.Entries[i].InputFingerprints = cloneStringMap(entry.InputFingerprints)
		cp.Entries[i].OutputFingerprints = cloneStringMap(entry.OutputFingerprints)
		cp.Entries[i].ShadowMismatchCategories = cloneIntMap(entry.ShadowMismatchCategories)
		cp.Entries[i].NetROIHistoryMS = append([]int64(nil), entry.NetROIHistoryMS...)
		cp.Entries[i].MismatchExamples = append([]string(nil), entry.MismatchExamples...)
	}
	for i, entry := range l.Prepared {
		cp.Prepared[i] = entry
		cp.Prepared[i].NormalizedCommand = ""
		cp.Prepared[i].InputFingerprints = cloneStringMap(entry.InputFingerprints)
		cp.Prepared[i].HotFingerprints = cloneStringMap(entry.HotFingerprints)
		cp.Prepared[i].OutputFingerprints = cloneStringMap(entry.OutputFingerprints)
		cp.Prepared[i].Notes = append([]string(nil), entry.Notes...)
	}
	if cp.Version == 0 {
		cp.Version = 1
	}
	return cp
}

func (l *ValidityLedger) UpsertObservation(entry LedgerEntry) {
	for i := range l.Entries {
		if l.Entries[i].OperationKey == entry.OperationKey && l.Entries[i].InvalidationEpoch == entry.InvalidationEpoch && mapsEqual(l.Entries[i].InputFingerprints, entry.InputFingerprints) {
			old := l.Entries[i]
			entry.ShadowMatchCount += old.ShadowMatchCount
			entry.ShadowMismatchCount += old.ShadowMismatchCount
			entry.ShadowSkipCount += old.ShadowSkipCount
			entry.ShadowMismatchCategories = mergeIntMaps(old.ShadowMismatchCategories, entry.ShadowMismatchCategories)
			entry.WarmObservationCount += old.WarmObservationCount
			entry.ReplacementCount += old.ReplacementCount
			entry.FallbackCount += old.FallbackCount
			entry.NetROIHistoryMS = append(old.NetROIHistoryMS, entry.NetROIHistoryMS...)
			entry.MismatchExamples = append(old.MismatchExamples, entry.MismatchExamples...)
			l.Entries[i] = entry
			return
		}
	}
	l.Entries = append(l.Entries, entry)
}

func (l *ValidityLedger) UpsertPrepared(entry PreparedEntry) {
	for i := range l.Prepared {
		if l.Prepared[i].PreparedID == entry.PreparedID {
			l.Prepared[i] = entry
			return
		}
	}
	l.Prepared = append(l.Prepared, entry)
}

func (l *ValidityLedger) IncrementReplacement(key string, roiMS int64) {
	for i := range l.Entries {
		if l.Entries[i].OperationKey == key {
			l.Entries[i].ReplacementCount++
			l.Entries[i].LastDecision = ModeReplay
			l.Entries[i].LastValidatedAt = time.Now()
			l.Entries[i].NetROIHistoryMS = append(l.Entries[i].NetROIHistoryMS, roiMS)
			return
		}
	}
}

func (l *ValidityLedger) IncrementFallback(key string) {
	for i := range l.Entries {
		if l.Entries[i].OperationKey == key {
			l.Entries[i].FallbackCount++
			l.Entries[i].LastDecision = ModeNative
			l.Entries[i].LastValidatedAt = time.Now()
			return
		}
	}
}

func (l *ValidityLedger) FindEntry(key, epoch string) (*LedgerEntry, bool) {
	for i := range l.Entries {
		entry := &l.Entries[i]
		if entry.OperationKey == key && entry.InvalidationEpoch == epoch {
			return entry, true
		}
	}
	return nil, false
}

func (l *ValidityLedger) RecordWarmObservation(key string, family OperatorFamily, fingerprints map[string]string, epoch string) {
	if entry, ok := l.FindEntry(key, epoch); ok {
		entry.WarmObservationCount++
		entry.LastDecision = ModeNative
		entry.LastValidatedAt = time.Now()
		return
	}
	l.Entries = append(l.Entries, LedgerEntry{
		OperationKey:         key,
		OperatorFamily:       family,
		InputFingerprints:    fingerprints,
		OutputFingerprints:   map[string]string{},
		InvalidationEpoch:    epoch,
		WarmObservationCount: 1,
		LastDecision:         ModeNative,
		LastValidatedAt:      time.Now(),
	})
}

func operationKey(op Operation, ws WorldState) string {
	return hashString(strings.Join([]string{
		ws.RepoRoot,
		op.CWD,
		normalizeArgv(op.Argv),
	}, "\x00"))
}

func inputFingerprints(op Operation, ws WorldState) map[string]string {
	fp := map[string]string{
		"repo_root": hashString(ws.RepoRoot),
		"cwd":       hashString(op.CWD),
		"tool":      hashString(toolForOperation(op, ws)),
	}
	if IsFastPathAllowed(op.Argv) {
		switch normalizedFastPath(op.Argv) {
		case "git rev-parse HEAD":
			fp["head"] = hashString(ws.Head)
		case "git rev-parse --abbrev-ref HEAD":
			fp["branch"] = hashString(ws.Branch)
			fp["head"] = hashString(ws.Head)
		case "git rev-parse --git-dir":
			fp["git_dir"] = hashString(ws.GitDir)
			fp["git_dir_abs"] = hashString(ws.GitDirAbs)
		case "git rev-parse --show-toplevel":
			fp["repo_root_abs"] = hashString(ws.RepoRoot)
			fp["git_dir_abs"] = hashString(ws.GitDirAbs)
		case "git rev-parse --is-inside-work-tree":
			fp["repo_root_abs"] = hashString(ws.RepoRoot)
			fp["git_dir_abs"] = hashString(ws.GitDirAbs)
		}
	}
	if IsProofGatedReplayCandidate(op.Argv) {
		for k, v := range proofGatedFingerprints(op, ws) {
			fp[k] = v
		}
	}
	return fp
}

func invalidationEpoch(op Operation, ws WorldState) string {
	if IsFastPathAllowed(op.Argv) {
		switch normalizedFastPath(op.Argv) {
		case "git rev-parse HEAD":
			return "head:" + ws.HeadEpoch
		case "git rev-parse --abbrev-ref HEAD":
			return "branch:" + hashString(ws.Branch) + ":head:" + ws.HeadEpoch
		case "git rev-parse --git-dir":
			return "gitdir:" + hashString(ws.RepoRoot+"|"+ws.GitDir+"|"+ws.GitDirAbs)
		case "git rev-parse --show-toplevel":
			return "repo-root:" + hashString(ws.RepoRoot+"|"+ws.GitDirAbs)
		case "git rev-parse --is-inside-work-tree":
			return "worktree:" + hashString(ws.RepoRoot+"|"+ws.GitDirAbs)
		}
	}
	if IsProofGatedReplayCandidate(op.Argv) {
		if isRepoSummaryReplayCandidate(op.Argv) {
			return "workspace:" + ws.WorkspaceEpoch
		}
		if epoch, ok := proofGatedEpoch(op, ws); ok {
			return epoch
		}
		return "proof-gated:missing"
	}
	return "none"
}

func proofGatedCandidateUsable(op Operation, ws WorldState) bool {
	_, ok := proofGatedEpoch(op, ws)
	return ok
}

func proofGatedFingerprints(op Operation, ws WorldState) map[string]string {
	if fp, _, ok := repoSummaryProof(op.CWD, op.Argv, ws); ok {
		return fp
	}
	if fp, _, ok := fileInspectionProof(op, ws); ok {
		return fp
	}
	if fp, _, ok := toolDiscoveryProof(op); ok {
		return fp
	}
	if fp, _, ok := staticEnvironmentProof(op.CWD, op.Argv); ok {
		return fp
	}
	if fp, _, ok := printenvProof(op.CWD, op.Argv); ok {
		return fp
	}
	if fp, _, ok := directoryListingProof(op.CWD, op.Argv, ws); ok {
		return fp
	}
	return map[string]string{"proof": "missing"}
}

func proofGatedEpoch(op Operation, ws WorldState) (string, bool) {
	if _, epoch, ok := repoSummaryProof(op.CWD, op.Argv, ws); ok {
		return epoch, true
	}
	if _, epoch, ok := fileInspectionProof(op, ws); ok {
		return epoch, true
	}
	if _, epoch, ok := toolDiscoveryProof(op); ok {
		return epoch, true
	}
	if _, epoch, ok := staticEnvironmentProof(op.CWD, op.Argv); ok {
		return epoch, true
	}
	if _, epoch, ok := printenvProof(op.CWD, op.Argv); ok {
		return epoch, true
	}
	if _, epoch, ok := directoryListingProof(op.CWD, op.Argv, ws); ok {
		return epoch, true
	}
	return "", false
}

func repoSummaryProof(cwd string, argv []string, ws WorldState) (map[string]string, string, bool) {
	argv = normalizeArgvForPolicy(argv)
	if !isRepoSummaryReplayCandidate(argv) {
		return nil, "", false
	}
	root := ws.RepoRoot
	gitDirAbs := ws.GitDirAbs
	if root == "" || (filepath.Base(argv[0]) == "git" && gitDirAbs == "") {
		discoveredRoot, discoveredGitDir, ok := discoverGitDir(cwd)
		if !ok && filepath.Base(argv[0]) == "git" {
			return nil, "", false
		}
		if root == "" {
			root = discoveredRoot
		}
		if gitDirAbs == "" {
			gitDirAbs = discoveredGitDir
		}
	}
	if root == "" {
		root = absPath(cwd)
	}
	toolName := filepath.Base(argv[0])
	toolSignal, ok := executableSignal(cwd, toolName)
	if !ok {
		return nil, "", false
	}
	fp := map[string]string{
		"summary_command": hashString(normalizeArgv(argv)),
		"repo_root":       hashString(root),
		"cwd":             hashString(absPath(cwd)),
		"tool_name":       hashString(toolName),
		"tool_path":       toolSignal.PathHash,
		"tool_executable": toolSignal.FileHash,
	}
	switch {
	case isGitLsFiles(argv):
		if gitDirAbs == "" {
			return nil, "", false
		}
		indexFP := fileHashOrMissing(filepath.Join(gitDirAbs, "index"))
		configFP := gitConfigSummaryFingerprint(root, gitDirAbs)
		fp["index"] = indexFP
		fp["git_config"] = configFP
		epoch := "repo-summary:git-ls-files:" + hashString(root+"|"+normalizeArgv(argv)+"|"+indexFP+"|"+configFP+"|"+toolSignal.FileHash)
		return fp, epoch, true
	case isGitStatusState(argv):
		if gitDirAbs == "" {
			return nil, "", false
		}
		head, branch, ok := readHeadAndBranch(gitDirAbs)
		if !ok {
			return nil, "", false
		}
		tree, content, complete := exactWorkspaceEpochs(root, 10000, true)
		if !complete {
			return nil, "", false
		}
		indexFP := fileHashOrMissing(filepath.Join(gitDirAbs, "index"))
		configFP := gitConfigSummaryFingerprint(root, gitDirAbs)
		ignoreFP := workspaceIgnoreFingerprint(root, gitDirAbs)
		fp["head"] = hashString(head)
		fp["branch"] = hashString(branch)
		fp["index"] = indexFP
		fp["git_config"] = configFP
		fp["ignore_rules"] = ignoreFP
		fp["file_tree_epoch"] = tree
		fp["file_content_epoch"] = content
		epoch := "repo-summary:git-status:" + hashString(root+"|"+normalizeArgv(argv)+"|"+head+"|"+branch+"|"+indexFP+"|"+configFP+"|"+ignoreFP+"|"+tree+"|"+content+"|"+toolSignal.FileHash)
		return fp, epoch, true
	case isGitReadOnlyDiff(argv):
		if gitDirAbs == "" {
			return nil, "", false
		}
		tree, content, complete := exactWorkspaceEpochs(root, 10000, true)
		if !complete {
			return nil, "", false
		}
		indexFP := fileHashOrMissing(filepath.Join(gitDirAbs, "index"))
		configFP := gitConfigSummaryFingerprint(root, gitDirAbs)
		attrFP := gitAttributeFingerprint(root, gitDirAbs)
		fp["index"] = indexFP
		fp["git_config"] = configFP
		fp["git_attributes"] = attrFP
		fp["file_tree_epoch"] = tree
		fp["file_content_epoch"] = content
		epoch := "repo-summary:git-diff:" + hashString(root+"|"+normalizeArgv(argv)+"|"+indexFP+"|"+configFP+"|"+attrFP+"|"+tree+"|"+content+"|"+toolSignal.FileHash)
		return fp, epoch, true
	case isRgFiles(argv):
		tree, _, complete := exactWorkspaceEpochs(root, 10000, false)
		if !complete {
			return nil, "", false
		}
		ignoreFP := workspaceIgnoreFingerprint(root, gitDirAbs)
		fp["file_tree_epoch"] = tree
		fp["ignore_rules"] = ignoreFP
		epoch := "repo-summary:rg-files:" + hashString(root+"|"+normalizeArgv(argv)+"|"+ignoreFP+"|"+tree+"|"+toolSignal.FileHash)
		return fp, epoch, true
	default:
		return nil, "", false
	}
}

func fileInspectionProof(op Operation, ws WorldState) (map[string]string, string, bool) {
	path, ok := replayableInspectionPath(op.CWD, op.Argv, ws)
	if !ok {
		return nil, "", false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() || info.Size() > maxReplayableInspectionFileBytes {
		return nil, "", false
	}
	if isReplayableCatFileRead(op.Argv) && info.Size() > maxFastPathOutputBytes {
		return nil, "", false
	}
	contentHash, ok := hashFile(path)
	if !ok {
		return nil, "", false
	}
	rel := path
	if ws.RepoRoot != "" {
		if r, err := filepath.Rel(ws.RepoRoot, path); err == nil {
			rel = filepath.ToSlash(r)
		}
	}
	fp := map[string]string{
		"inspection_command": hashString(normalizeArgv(op.Argv)),
		"file_path":          hashString(rel),
		"file_name":          hashString(filepath.Base(path)),
		"file_content":       contentHash,
		"file_size":          hashString(strconv.FormatInt(info.Size(), 10)),
		"file_mode":          hashString(info.Mode().String()),
	}
	if isBoundedSedPrint(op.Argv) {
		fp["sed_range"] = hashString(op.Argv[2])
	}
	if isBoundedHeadPrint(op.Argv) {
		_, n, _ := parseHeadTailArgs(op.Argv, false)
		fp["head_lines"] = hashString(strconv.Itoa(n))
	}
	if isBoundedTailPrint(op.Argv) {
		_, n, _ := parseHeadTailArgs(op.Argv, true)
		fp["tail_lines"] = hashString(strconv.Itoa(n))
	}
	epochInput := rel + "|" + contentHash + "|" + strconv.FormatInt(info.Size(), 10) + "|" + info.Mode().String() + "|" + normalizeArgv(op.Argv)
	if isFixedGrepFileSearch(op.Argv) {
		pattern, _, quiet, _ := parseFixedGrepArgs(op.Argv)
		fp["grep_pattern"] = hashString(pattern)
		fp["grep_quiet"] = hashString(strconv.FormatBool(quiet))
	}
	if isFixedRgFileSearch(op.Argv) {
		pattern, _, quiet, lineNumber, _ := parseFixedRgArgs(op.Argv)
		fp["rg_pattern"] = hashString(pattern)
		fp["rg_quiet"] = hashString(strconv.FormatBool(quiet))
		fp["rg_line_number"] = hashString(strconv.FormatBool(lineNumber))
	}
	if isReplayableFileType(op.Argv) {
		signal, ok := executableSignal(op.CWD, "file")
		if !ok {
			return nil, "", false
		}
		envHash := fileCommandEnvHash()
		fp["tool_path"] = signal.PathHash
		fp["tool_executable"] = signal.FileHash
		fp["file_env"] = envHash
		epochInput += "|" + signal.PathHash + "|" + signal.FileHash + "|" + envHash
	}
	epoch := "file-inspection:" + hashString(epochInput)
	return fp, epoch, true
}

func replayableInspectionPath(cwd string, argv []string, ws WorldState) (string, bool) {
	if !isReplayableFileInspection(argv) {
		return "", false
	}
	root := ws.RepoRoot
	if root == "" {
		root = cwd
	}
	argPath := ""
	argPath = replayableInspectionArgPath(argv)
	if argPath == "" {
		return "", false
	}
	path := filepath.Clean(filepath.Join(cwd, argPath))
	realRoot := root
	if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = resolvedRoot
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil || !pathWithinRoot(realPath, realRoot) {
		return "", false
	}
	if !isReplayableInspectionName(filepath.Base(path)) {
		return "", false
	}
	return absPath(realPath), true
}

func toolDiscoveryProof(op Operation) (map[string]string, string, bool) {
	if isToolVersionProbe(op.Argv) {
		name := filepath.Base(op.Argv[0])
		signal, ok := executableSignal(op.CWD, name)
		if !ok {
			return nil, "", false
		}
		fp := map[string]string{
			"tool_name":          hashString(name),
			"tool_path":          signal.PathHash,
			"tool_executable":    signal.FileHash,
			"path_env":           hashString(os.Getenv("PATH")),
			"version_env":        deterministicVersionEnvHash(),
			"version_probe_argv": hashString(normalizeArgv(op.Argv)),
		}
		epoch := "tool-version:" + hashString(name+"|"+signal.PathHash+"|"+signal.FileHash+"|"+fp["path_env"]+"|"+fp["version_env"])
		return fp, epoch, true
	}
	if isCommandPathLookup(op.Argv) {
		target := commandPathLookupTarget(op.Argv)
		if target == "" {
			return nil, "", false
		}
		whichSignal, whichOK := executableSignal(op.CWD, filepath.Base(op.Argv[0]))
		if filepath.Base(op.Argv[0]) == "command" {
			whichSignal = executableProofSignal{PathHash: hashString("shell-builtin:command"), FileHash: hashString("shell-builtin:command-v")}
			whichOK = true
		}
		targetSignal, targetOK := executableSignal(op.CWD, target)
		if !whichOK || !targetOK {
			return nil, "", false
		}
		fp := map[string]string{
			"lookup_tool":       hashString(target),
			"which_path":        whichSignal.PathHash,
			"which_executable":  whichSignal.FileHash,
			"target_path":       targetSignal.PathHash,
			"target_executable": targetSignal.FileHash,
			"path_env":          hashString(os.Getenv("PATH")),
			"version_env":       deterministicVersionEnvHash(),
			"path_lookup_argv":  hashString(normalizeArgv(op.Argv)),
		}
		epoch := "command-path:" + hashString(target+"|"+whichSignal.PathHash+"|"+whichSignal.FileHash+"|"+targetSignal.PathHash+"|"+targetSignal.FileHash+"|"+fp["path_env"]+"|"+fp["version_env"])
		return fp, epoch, true
	}
	return nil, "", false
}

type executableProofSignal struct {
	PathHash string
	FileHash string
}

func executableSignal(cwd, name string) (executableProofSignal, bool) {
	path, ok := resolveExecutable(cwd, name)
	if !ok {
		return executableProofSignal{}, false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return executableProofSignal{}, false
	}
	contentHash, ok := hashFile(path)
	if !ok {
		return executableProofSignal{}, false
	}
	signal := strings.Join([]string{
		filepath.Base(path),
		contentHash,
		fileHashStatSignal(info),
	}, "|")
	return executableProofSignal{
		PathHash: hashString(path),
		FileHash: hashString(signal),
	}, true
}

func resolveExecutable(cwd, name string) (string, bool) {
	if name == "" || strings.ContainsRune(name, filepath.Separator) {
		return "", false
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			return filepath.Clean(resolved), true
		}
		return filepath.Clean(candidate), true
	}
	return "", false
}

func deterministicVersionEnvHash() string {
	keys := []string{
		"GOROOT", "GOTOOLCHAIN", "GOENV",
		"NODE_OPTIONS", "NVM_BIN", "NVM_DIR",
		"PYENV_VERSION", "VIRTUAL_ENV", "CONDA_PREFIX",
		"CARGO_HOME", "RUSTUP_HOME", "RUSTUP_TOOLCHAIN",
	}
	var parts []string
	for _, key := range keys {
		parts = append(parts, key+"="+hashString(os.Getenv(key)))
	}
	return hashString(strings.Join(parts, "\n"))
}

func pathWithinRoot(path, root string) bool {
	pathAbs := absPath(path)
	rootAbs := absPath(root)
	rel, err := filepath.Rel(rootAbs, pathAbs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func fileHashOrMissing(path string) string {
	if fp, ok := hashFile(path); ok {
		return fp
	}
	return "missing:" + hashString(path)
}

func gitConfigSummaryFingerprint(repoRoot, gitDirAbs string) string {
	var parts []string
	parts = append(parts, gitConfigFileFingerprints("", filepath.Join(gitDirAbs, "config"))...)
	parts = append(parts, filepath.Join(gitDirAbs, "info", "sparse-checkout")+"\x00"+fileHashOrMissing(filepath.Join(gitDirAbs, "info", "sparse-checkout")))
	parts = append(parts, externalGitConfigFingerprints()...)
	sort.Strings(parts)
	return hashString(strings.Join(parts, "\n"))
}

func gitConfigFileFingerprints(label, path string) []string {
	if path == "" {
		return nil
	}
	var parts []string
	if label == "" {
		parts = append(parts, path+"\x00"+fileHashOrMissing(path))
	} else {
		parts = append(parts, "config:"+label+":"+path+"\x00"+fileHashOrMissing(path))
	}
	for _, includePath := range gitConfigIncludePaths(path, map[string]bool{}, 0) {
		parts = append(parts, "config-include:"+includePath+"\x00"+fileHashOrMissing(includePath))
	}
	return parts
}

func externalGitConfigFingerprints() []string {
	var parts []string
	for _, key := range []string{
		"GIT_CONFIG_NOSYSTEM",
		"GIT_CONFIG_SYSTEM",
		"GIT_CONFIG_GLOBAL",
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_PARAMETERS",
		"HOME",
		"XDG_CONFIG_HOME",
	} {
		parts = append(parts, "env:"+key+"\x00"+hashString(os.Getenv(key)))
	}
	if count, err := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT")); err == nil && count > 0 && count < 128 {
		for i := 0; i < count; i++ {
			key := fmt.Sprintf("GIT_CONFIG_KEY_%d", i)
			value := fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)
			parts = append(parts, "env:"+key+"\x00"+hashString(os.Getenv(key)))
			parts = append(parts, "env:"+value+"\x00"+hashString(os.Getenv(value)))
		}
	}
	addConfigPath := func(label, path string) {
		if path == "" {
			return
		}
		parts = append(parts, gitConfigFileFingerprints(label, path)...)
	}
	if global := os.Getenv("GIT_CONFIG_GLOBAL"); global != "" {
		addConfigPath("global-env", global)
	} else if home := os.Getenv("HOME"); home != "" {
		addConfigPath("global-home", filepath.Join(home, ".gitconfig"))
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			addConfigPath("global-xdg", filepath.Join(xdg, "git", "config"))
		} else {
			addConfigPath("global-xdg", filepath.Join(home, ".config", "git", "config"))
		}
	}
	if system := os.Getenv("GIT_CONFIG_SYSTEM"); system != "" {
		addConfigPath("system-env", system)
	} else if os.Getenv("GIT_CONFIG_NOSYSTEM") == "" {
		for _, path := range []string{"/etc/gitconfig", "/usr/local/etc/gitconfig", "/opt/homebrew/etc/gitconfig"} {
			addConfigPath("system", path)
		}
	}
	return parts
}

func gitConfigIncludePaths(configPath string, seen map[string]bool, depth int) []string {
	if configPath == "" || depth > 8 {
		return nil
	}
	configPath = filepath.Clean(configPath)
	if seen[configPath] {
		return nil
	}
	seen[configPath] = true
	content, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var includes []string
	section := ""
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.Index(line, "]")
			if end < 0 {
				section = ""
				continue
			}
			section = strings.ToLower(strings.TrimSpace(line[1:end]))
			continue
		}
		if section != "include" && !strings.HasPrefix(section, "includeif ") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "path") {
			continue
		}
		includePath := resolveGitConfigIncludePath(strings.TrimSpace(value), filepath.Dir(configPath))
		if includePath == "" {
			continue
		}
		includes = append(includes, includePath)
		includes = append(includes, gitConfigIncludePaths(includePath, seen, depth+1)...)
	}
	return includes
}

func resolveGitConfigIncludePath(value, baseDir string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, `"'`)
	if value == "" {
		return ""
	}
	if value == "~" {
		if home := os.Getenv("HOME"); home != "" {
			return filepath.Clean(home)
		}
		return ""
	}
	if strings.HasPrefix(value, "~/") {
		if home := os.Getenv("HOME"); home != "" {
			return filepath.Clean(filepath.Join(home, value[2:]))
		}
		return ""
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(baseDir, value))
}

func gitAttributeFingerprint(repoRoot, gitDirAbs string) string {
	var parts []string
	for _, path := range []string{
		filepath.Join(repoRoot, ".gitattributes"),
		filepath.Join(gitDirAbs, "info", "attributes"),
	} {
		parts = append(parts, path+"\x00"+fileHashOrMissing(path))
	}
	if gitDirAbs != "" {
		parts = append(parts, configuredGitCorePathFingerprints(gitDirAbs, "attributesfile", "attributes:core-attributes")...)
	}
	parts = append(parts, externalGitAttributeFingerprints()...)
	return hashString(strings.Join(parts, "\n"))
}

func externalGitAttributeFingerprints() []string {
	var parts []string
	addAttributesPath := func(label, path string) {
		if path == "" {
			return
		}
		parts = append(parts, "attributes:"+label+":"+path+"\x00"+fileHashOrMissing(path))
	}
	if home := os.Getenv("HOME"); home != "" {
		parts = append(parts, "env:HOME\x00"+hashString(home))
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		parts = append(parts, "env:XDG_CONFIG_HOME\x00"+hashString(xdg))
		addAttributesPath("global-xdg", filepath.Join(xdg, "git", "attributes"))
	} else if home := os.Getenv("HOME"); home != "" {
		addAttributesPath("global-xdg", filepath.Join(home, ".config", "git", "attributes"))
	}
	return parts
}

func configuredGitCorePathFingerprints(gitDirAbs, key, label string) []string {
	var parts []string
	seenValues := map[string]bool{}
	for _, configPath := range gitConfigProofPaths(gitDirAbs) {
		if configPath == "" {
			continue
		}
		allConfigs := append([]string{configPath}, gitConfigIncludePaths(configPath, map[string]bool{}, 0)...)
		for _, path := range allConfigs {
			for _, value := range gitConfigSectionPathValues(path, "core", key) {
				target := resolveGitConfigIncludePath(value, filepath.Dir(path))
				if target == "" || seenValues[target] {
					continue
				}
				seenValues[target] = true
				parts = append(parts, label+":"+target+"\x00"+fileHashOrMissing(target))
			}
		}
	}
	return parts
}

func gitConfigProofPaths(gitDirAbs string) []string {
	var paths []string
	if gitDirAbs != "" {
		paths = append(paths, filepath.Join(gitDirAbs, "config"))
	}
	if global := os.Getenv("GIT_CONFIG_GLOBAL"); global != "" {
		paths = append(paths, global)
	} else if home := os.Getenv("HOME"); home != "" {
		paths = append(paths, filepath.Join(home, ".gitconfig"))
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			paths = append(paths, filepath.Join(xdg, "git", "config"))
		} else {
			paths = append(paths, filepath.Join(home, ".config", "git", "config"))
		}
	}
	if system := os.Getenv("GIT_CONFIG_SYSTEM"); system != "" {
		paths = append(paths, system)
	} else if os.Getenv("GIT_CONFIG_NOSYSTEM") == "" {
		paths = append(paths, "/etc/gitconfig", "/usr/local/etc/gitconfig", "/opt/homebrew/etc/gitconfig")
	}
	return paths
}

func gitConfigSectionPathValues(configPath, wantSection, wantKey string) []string {
	content, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var values []string
	section := ""
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.Index(line, "]")
			if end < 0 {
				section = ""
				continue
			}
			section = strings.ToLower(strings.TrimSpace(line[1:end]))
			continue
		}
		if !strings.EqualFold(section, wantSection) {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), wantKey) {
			continue
		}
		values = append(values, strings.TrimSpace(value))
	}
	return values
}

func workspaceIgnoreFingerprint(repoRoot, gitDirAbs string) string {
	var parts []string
	for _, name := range []string{".gitignore", ".ignore", ".rgignore"} {
		collectNamedFileFingerprints(repoRoot, name, &parts)
	}
	if gitDirAbs != "" {
		parts = append(parts, filepath.Join(gitDirAbs, "info", "exclude")+"\x00"+fileHashOrMissing(filepath.Join(gitDirAbs, "info", "exclude")))
		parts = append(parts, configuredGitCorePathFingerprints(gitDirAbs, "excludesfile", "ignore:core-excludes")...)
	}
	parts = append(parts, externalGitIgnoreFingerprints()...)
	sort.Strings(parts)
	return hashString(strings.Join(parts, "\n"))
}

func externalGitIgnoreFingerprints() []string {
	var parts []string
	addIgnorePath := func(label, path string) {
		if path == "" {
			return
		}
		parts = append(parts, "ignore:"+label+":"+path+"\x00"+fileHashOrMissing(path))
	}
	if home := os.Getenv("HOME"); home != "" {
		parts = append(parts, "env:HOME\x00"+hashString(home))
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		parts = append(parts, "env:XDG_CONFIG_HOME\x00"+hashString(xdg))
		addIgnorePath("global-xdg", filepath.Join(xdg, "git", "ignore"))
	} else if home := os.Getenv("HOME"); home != "" {
		addIgnorePath("global-xdg", filepath.Join(home, ".config", "git", "ignore"))
	}
	return parts
}

func collectNamedFileFingerprints(root, name string, parts *[]string) {
	if root == "" {
		return
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == ".squire" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != name {
			return nil
		}
		rel := path
		if r, err := filepath.Rel(root, path); err == nil {
			rel = filepath.ToSlash(r)
		}
		*parts = append(*parts, rel+"\x00"+fileHashOrMissing(path))
		return nil
	})
}

type exactWorkspaceEpochCacheEntry struct {
	statSignal string
	tree       string
	content    string
	complete   bool
}

var exactWorkspaceEpochCache = struct {
	sync.Mutex
	entries map[string]exactWorkspaceEpochCacheEntry
}{
	entries: map[string]exactWorkspaceEpochCacheEntry{},
}

func exactWorkspaceEpochs(root string, maxContentFiles int, needContent bool) (string, string, bool) {
	if root == "" {
		return "", "", false
	}
	root = absPath(root)
	statSignal, statOK := exactWorkspaceStatSignal(root)
	cacheKey := root + "\x00" + strconv.Itoa(maxContentFiles) + "\x00" + strconv.FormatBool(needContent)
	if statOK {
		exactWorkspaceEpochCache.Lock()
		cached, ok := exactWorkspaceEpochCache.entries[cacheKey]
		exactWorkspaceEpochCache.Unlock()
		if ok && cached.statSignal == statSignal {
			return cached.tree, cached.content, cached.complete
		}
	}

	tree, content, complete := computeExactWorkspaceEpochs(root, maxContentFiles, needContent)
	if statOK {
		exactWorkspaceEpochCache.Lock()
		exactWorkspaceEpochCache.entries[cacheKey] = exactWorkspaceEpochCacheEntry{
			statSignal: statSignal,
			tree:       tree,
			content:    content,
			complete:   complete,
		}
		exactWorkspaceEpochCache.Unlock()
	}
	return tree, content, complete
}

func exactWorkspaceStatSignal(root string) (string, bool) {
	if root == "" {
		return "", false
	}
	var parts []string
	complete := true
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			complete = false
			return nil
		}
		name := d.Name()
		if d.IsDir() && (name == ".git" || name == ".squire") {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			complete = false
			return nil
		}
		signal, ok := fileHashStatCacheSignal(info)
		if !ok {
			complete = false
			return nil
		}
		rel = filepath.ToSlash(rel)
		parts = append(parts, rel+"\x00"+signal)
		return nil
	})
	if !complete {
		return "", false
	}
	sort.Strings(parts)
	return hashString(strings.Join(parts, "\n")), true
}

func computeExactWorkspaceEpochs(root string, maxContentFiles int, needContent bool) (string, string, bool) {
	var treeParts []string
	var contentParts []string
	contentCount := 0
	complete := true
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() && (name == ".git" || name == ".squire") {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		treeParts = append(treeParts, fmt.Sprintf("%s\x00%s\x00%d", rel, info.Mode().String(), info.Size()))
		if d.IsDir() || !info.Mode().IsRegular() || !needContent {
			return nil
		}
		contentCount++
		if maxContentFiles > 0 && contentCount > maxContentFiles {
			complete = false
			return nil
		}
		if fp, ok := hashFile(path); ok {
			contentParts = append(contentParts, rel+"\x00"+fp)
		} else {
			complete = false
		}
		return nil
	})
	sort.Strings(treeParts)
	sort.Strings(contentParts)
	return hashString(strings.Join(treeParts, "\n")), hashString(strings.Join(contentParts, "\n")), complete
}

func normalizedFastPath(argv []string) string {
	argv = normalizeArgvForPolicy(argv)
	if isGitAbbrevRefHead(argv) {
		return "git rev-parse --abbrev-ref HEAD"
	}
	if len(argv) == 3 && filepath.Base(argv[0]) == "git" && argv[1] == "rev-parse" {
		return "git rev-parse " + argv[2]
	}
	return displayCommand(argv)
}

func toolForOperation(op Operation, ws WorldState) string {
	if len(op.Argv) == 0 {
		return ""
	}
	name := filepath.Base(op.Argv[0])
	if ws.ToolIdentity != nil && ws.ToolIdentity[name] != "" {
		return ws.ToolIdentity[name]
	}
	return name
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func cloneStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func cloneIntMap(src map[string]int) map[string]int {
	if src == nil {
		return nil
	}
	dst := make(map[string]int, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func mergeIntMaps(a, b map[string]int) map[string]int {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := cloneIntMap(a)
	if out == nil {
		out = map[string]int{}
	}
	for k, v := range b {
		out[k] += v
	}
	return out
}

func incrementIntMap(target *map[string]int, key string) {
	if *target == nil {
		*target = map[string]int{}
	}
	(*target)[key]++
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
