package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxFastPathOutputBytes = 64 * 1024

type ValidityLedger struct {
	Version int           `json:"version"`
	Entries []LedgerEntry `json:"entries"`
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
	b, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.Root, "ledger.json"), append(b, '\n'), 0o600)
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

func (l *ValidityLedger) UpsertObservation(entry LedgerEntry) {
	for i := range l.Entries {
		if l.Entries[i].OperationKey == entry.OperationKey && l.Entries[i].InvalidationEpoch == entry.InvalidationEpoch && mapsEqual(l.Entries[i].InputFingerprints, entry.InputFingerprints) {
			old := l.Entries[i]
			entry.ShadowMatchCount += old.ShadowMatchCount
			entry.ShadowMismatchCount += old.ShadowMismatchCount
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

func (l *ValidityLedger) RecordShadow(key string, family OperatorFamily, fingerprints map[string]string, epoch string, matched bool, example string) {
	for i := range l.Entries {
		if l.Entries[i].OperationKey == key && l.Entries[i].InvalidationEpoch == epoch {
			if matched {
				l.Entries[i].ShadowMatchCount++
			} else {
				l.Entries[i].ShadowMismatchCount++
				if len(l.Entries[i].MismatchExamples) < 5 {
					l.Entries[i].MismatchExamples = append(l.Entries[i].MismatchExamples, example)
				}
			}
			l.Entries[i].LastDecision = ModeShadow
			l.Entries[i].LastValidatedAt = time.Now()
			return
		}
	}
	entry := LedgerEntry{
		OperationKey:       key,
		OperatorFamily:     family,
		InputFingerprints:  fingerprints,
		OutputFingerprints: map[string]string{},
		InvalidationEpoch:  epoch,
		LastDecision:       ModeShadow,
		LastValidatedAt:    time.Now(),
	}
	if matched {
		entry.ShadowMatchCount = 1
	} else {
		entry.ShadowMismatchCount = 1
		entry.MismatchExamples = []string{example}
	}
	l.Entries = append(l.Entries, entry)
}

func (l *ValidityLedger) HasShadowMismatch(key string) bool {
	for _, entry := range l.Entries {
		if entry.OperationKey == key && entry.ShadowMismatchCount > 0 {
			return true
		}
	}
	return false
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
		}
	}
	if IsShadowCandidate(op.Argv) {
		fp["index"] = ws.IndexFingerprint
		fp["file_tree_epoch"] = ws.FileTreeEpoch
		fp["file_content_epoch"] = ws.FileContentEpoch
		fp["dirty_state"] = hashString(ws.DirtyState + "|" + ws.UntrackedSummary)
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
		}
	}
	if IsShadowCandidate(op.Argv) {
		return "workspace:" + ws.WorkspaceEpoch
	}
	return "none"
}

func normalizedFastPath(argv []string) string {
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

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
