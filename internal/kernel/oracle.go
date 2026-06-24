package kernel

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type RepoOracle struct {
	mu            sync.Mutex
	cache         map[string]WorldState
	shadowSignals map[string]string
}

func NewRepoOracle() *RepoOracle {
	return &RepoOracle{cache: map[string]WorldState{}, shadowSignals: map[string]string{}}
}

func (o *RepoOracle) Snapshot(ctx context.Context, cwd string) WorldState {
	ws := o.fullSnapshot(ctx, cwd)
	o.storeCached(cwd, ws)
	return ws
}

func (o *RepoOracle) FastSnapshot(ctx context.Context, cwd string) WorldState {
	abs := absPath(cwd)
	o.mu.Lock()
	cached, ok := o.cache[abs]
	o.mu.Unlock()
	if ok {
		if refreshed, refreshedOK := refreshFastMetadataWorld(cached); refreshedOK {
			o.storeCached(cwd, refreshed)
			return refreshed
		}
	}
	return o.MetadataSnapshot(ctx, cwd)
}

func (o *RepoOracle) MetadataSnapshot(ctx context.Context, cwd string) WorldState {
	ws := o.metadataSnapshot(ctx, cwd)
	o.storeCached(cwd, ws)
	return ws
}

func (o *RepoOracle) ShadowSnapshot(ctx context.Context, cwd string) WorldState {
	abs := absPath(cwd)
	o.mu.Lock()
	cached, ok := o.cache[abs]
	previousSignal := o.shadowSignals[abs]
	o.mu.Unlock()
	if !ok {
		return o.Snapshot(ctx, cwd)
	}
	refreshed, refreshedOK := refreshFastMetadataWorld(cached)
	if !refreshedOK {
		return o.Snapshot(ctx, cwd)
	}
	if fp, ok := hashFile(filepath.Join(refreshed.GitDirAbs, "config")); ok {
		refreshed.ConfigFingerprint = fp
		refreshed.ConfigEpoch = fp
	}
	if fp, ok := hashFile(filepath.Join(refreshed.GitDirAbs, "index")); ok {
		refreshed.IndexFingerprint = fp
		refreshed.IndexEpoch = fp
	}
	refreshed.IgnoreRuleFingerprint = ignoreRuleFingerprint(refreshed.RepoRoot, refreshed.GitDirAbs)
	currentSignal := cheapWorkspaceSignal(refreshed.RepoRoot, refreshed.GitDirAbs)
	if previousSignal != "" && currentSignal != previousSignal {
		refreshed.FileTreeEpoch = "cheap:" + hashString(currentSignal)
		refreshed.FileContentEpoch = ""
		refreshed.DirtyState = "unknown"
		refreshed.UntrackedSummary = ""
		refreshed.EvidenceQuality = EvidencePartial
		refreshed.WorkspaceEpoch = hashString(strings.Join([]string{
			refreshed.HeadEpoch,
			refreshed.ConfigEpoch,
			refreshed.IndexEpoch,
			refreshed.IgnoreRuleFingerprint,
			refreshed.FileTreeEpoch,
			refreshed.FileContentEpoch,
			refreshed.DirtyState,
			refreshed.UntrackedSummary,
		}, "|"))
	}
	refreshed.CollectedAtUnixNano = time.Now().UnixNano()
	o.storeCached(cwd, refreshed)
	return refreshed
}

func (o *RepoOracle) storeCached(cwd string, ws WorldState) {
	abs := absPath(cwd)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cache[abs] = ws
	if ws.OracleAvailable && ws.RepoRoot != "" && ws.GitDirAbs != "" {
		o.shadowSignals[abs] = cheapWorkspaceSignal(ws.RepoRoot, ws.GitDirAbs)
	}
}

func (o *RepoOracle) fullSnapshot(ctx context.Context, cwd string) WorldState {
	ws := WorldState{
		RepoRoot:            cwd,
		DirtyState:          "unknown",
		ToolIdentity:        map[string]string{},
		EvidenceQuality:     EvidenceMissing,
		OracleAvailable:     false,
		CollectedAtUnixNano: time.Now().UnixNano(),
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		ws.RepoRoot = abs
	}

	root := runNative(ctx, cwd, []string{"git", "rev-parse", "--show-toplevel"})
	if root.ExitCode != 0 {
		ws.OracleDiagnostics = append(ws.OracleDiagnostics, "not a git worktree")
		ws.FileTreeEpoch, ws.FileContentEpoch = workspaceEpochs(ws.RepoRoot)
		ws.WorkspaceEpoch = hashString(ws.FileTreeEpoch + "|" + ws.FileContentEpoch)
		ws.ToolIdentity = toolIdentity(ctx, cwd)
		return ws
	}
	ws.RepoRoot = strings.TrimSpace(string(root.Stdout))
	ws.OracleAvailable = true
	ws.EvidenceQuality = EvidenceStrong

	if res := runNative(ctx, cwd, []string{"git", "rev-parse", "HEAD"}); res.ExitCode == 0 {
		ws.Head = strings.TrimSpace(string(res.Stdout))
		ws.HeadEpoch = hashString(ws.Head)
	} else {
		ws.OracleDiagnostics = append(ws.OracleDiagnostics, "HEAD unavailable")
		ws.EvidenceQuality = EvidencePartial
	}
	if res := runNative(ctx, cwd, []string{"git", "rev-parse", "--abbrev-ref", "HEAD"}); res.ExitCode == 0 {
		ws.Branch = strings.TrimSpace(string(res.Stdout))
	}
	if res := runNative(ctx, cwd, []string{"git", "rev-parse", "--git-dir"}); res.ExitCode == 0 {
		ws.GitDir = strings.TrimSpace(string(res.Stdout))
	}
	if res := runNative(ctx, cwd, []string{"git", "rev-parse", "--absolute-git-dir"}); res.ExitCode == 0 {
		ws.GitDirAbs = strings.TrimSpace(string(res.Stdout))
	}
	if res := runNative(ctx, cwd, []string{"git", "remote", "get-url", "origin"}); res.ExitCode == 0 {
		remote := strings.TrimSpace(string(res.Stdout))
		ws.RemoteURLFingerprint = hashString(remote)
		if isSafeRemoteURL(remote) {
			ws.RemoteURL = remote
		}
	}

	configPath := filepath.Join(ws.GitDirAbs, "config")
	if fp, ok := hashFile(configPath); ok {
		ws.ConfigFingerprint = fp
		ws.ConfigEpoch = fp
	}
	indexPath := filepath.Join(ws.GitDirAbs, "index")
	if fp, ok := hashFile(indexPath); ok {
		ws.IndexFingerprint = fp
		ws.IndexEpoch = fp
	}
	ws.IgnoreRuleFingerprint = ignoreRuleFingerprint(ws.RepoRoot, ws.GitDirAbs)
	if status := runNative(ctx, cwd, []string{"git", "status", "--porcelain"}); status.ExitCode == 0 {
		if len(status.Stdout) == 0 {
			ws.DirtyState = "clean"
		} else {
			ws.DirtyState = "dirty"
			ws.UntrackedSummary = summarizeUntracked(status.Stdout)
		}
	} else {
		ws.EvidenceQuality = EvidencePartial
		ws.OracleDiagnostics = append(ws.OracleDiagnostics, "status unavailable")
	}
	ws.FileTreeEpoch, ws.FileContentEpoch = workspaceEpochs(ws.RepoRoot)
	ws.WorkspaceEpoch = hashString(strings.Join([]string{
		ws.HeadEpoch,
		ws.ConfigEpoch,
		ws.IndexEpoch,
		ws.IgnoreRuleFingerprint,
		ws.FileTreeEpoch,
		ws.FileContentEpoch,
		ws.DirtyState,
		ws.UntrackedSummary,
	}, "|"))
	ws.ToolIdentity = toolIdentity(ctx, cwd)
	return ws
}

func (o *RepoOracle) metadataSnapshot(ctx context.Context, cwd string) WorldState {
	_ = ctx
	ws := WorldState{
		RepoRoot:            cwd,
		DirtyState:          "unknown",
		ToolIdentity:        map[string]string{},
		EvidenceQuality:     EvidenceMissing,
		OracleAvailable:     false,
		CollectedAtUnixNano: time.Now().UnixNano(),
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		ws.RepoRoot = abs
	}
	repoRoot, gitDirAbs, ok := discoverGitDir(cwd)
	if !ok {
		ws.OracleDiagnostics = append(ws.OracleDiagnostics, "not a git worktree")
		ws.WorkspaceEpoch = hashString(ws.RepoRoot)
		return ws
	}
	ws.RepoRoot = repoRoot
	ws.GitDirAbs = gitDirAbs
	ws.OracleAvailable = true
	ws.EvidenceQuality = EvidenceStrong
	if rel, err := filepath.Rel(absPath(cwd), gitDirAbs); err == nil {
		ws.GitDir = filepath.ToSlash(rel)
	} else {
		ws.GitDir = gitDirAbs
	}
	if head, branch, ok := readHeadAndBranch(gitDirAbs); ok {
		ws.Head = head
		ws.Branch = branch
		ws.HeadEpoch = hashString(head)
	} else {
		ws.OracleDiagnostics = append(ws.OracleDiagnostics, "HEAD unavailable")
		ws.EvidenceQuality = EvidencePartial
	}
	if fp, ok := hashFile(filepath.Join(gitDirAbs, "config")); ok {
		ws.ConfigFingerprint = fp
		ws.ConfigEpoch = fp
	}
	if fp, ok := hashFile(filepath.Join(gitDirAbs, "index")); ok {
		ws.IndexFingerprint = fp
		ws.IndexEpoch = fp
	}
	ws.IgnoreRuleFingerprint = ignoreRuleFingerprint(ws.RepoRoot, ws.GitDirAbs)
	ws.WorkspaceEpoch = hashString(strings.Join([]string{
		ws.HeadEpoch,
		ws.ConfigEpoch,
		ws.IndexEpoch,
		ws.IgnoreRuleFingerprint,
		ws.DirtyState,
	}, "|"))
	return ws
}

func refreshFastMetadataWorld(ws WorldState) (WorldState, bool) {
	if !ws.OracleAvailable || ws.GitDirAbs == "" || ws.RepoRoot == "" {
		return ws, false
	}
	refreshed := ws
	head, branch, ok := readHeadAndBranch(ws.GitDirAbs)
	if !ok {
		return ws, false
	}
	refreshed.Head = head
	refreshed.Branch = branch
	refreshed.HeadEpoch = hashString(head)
	refreshed.WorkspaceEpoch = hashString(strings.Join([]string{
		refreshed.HeadEpoch,
		refreshed.ConfigEpoch,
		refreshed.IndexEpoch,
		refreshed.IgnoreRuleFingerprint,
		refreshed.FileTreeEpoch,
		refreshed.FileContentEpoch,
		refreshed.DirtyState,
		refreshed.UntrackedSummary,
	}, "|"))
	refreshed.CollectedAtUnixNano = time.Now().UnixNano()
	return refreshed, true
}

func readHeadAndBranch(gitDirAbs string) (string, string, bool) {
	b, err := os.ReadFile(filepath.Join(gitDirAbs, "HEAD"))
	if err != nil {
		return "", "", false
	}
	headText := strings.TrimSpace(string(b))
	if strings.HasPrefix(headText, "ref: ") {
		ref := strings.TrimSpace(strings.TrimPrefix(headText, "ref: "))
		headBytes, err := os.ReadFile(filepath.Join(gitDirAbs, filepath.FromSlash(ref)))
		if err != nil {
			if packed, ok := readPackedRef(gitDirAbs, ref); ok {
				return packed, strings.TrimPrefix(ref, "refs/heads/"), true
			}
			return "", "", false
		}
		return strings.TrimSpace(string(headBytes)), strings.TrimPrefix(ref, "refs/heads/"), true
	}
	if headText == "" {
		return "", "", false
	}
	return headText, "HEAD", true
}

func readPackedRef(gitDirAbs, ref string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(gitDirAbs, "packed-refs"))
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == ref {
			return parts[0], true
		}
	}
	return "", false
}

func isSafeRemoteURL(remote string) bool {
	if remote == "" {
		return false
	}
	if strings.Contains(remote, "://") {
		beforeHost := strings.SplitN(remote, "://", 2)[1]
		hostPart := beforeHost
		if slash := strings.IndexByte(beforeHost, '/'); slash >= 0 {
			hostPart = beforeHost[:slash]
		}
		return !strings.Contains(hostPart, "@")
	}
	if strings.Contains(remote, " ") || strings.Contains(remote, "\n") {
		return false
	}
	return !strings.Contains(remote, "://")
}

func ignoreRuleFingerprint(repoRoot, gitDirAbs string) string {
	var parts []string
	for _, path := range []string{
		filepath.Join(repoRoot, ".gitignore"),
		filepath.Join(gitDirAbs, "info", "exclude"),
	} {
		if fp, ok := hashFile(path); ok {
			parts = append(parts, path+"\x00"+fp)
		}
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return ""
	}
	return hashString(strings.Join(parts, "\n"))
}

func summarizeUntracked(stdout []byte) string {
	lines := bytes.Split(stdout, []byte{'\n'})
	var hashed []string
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("?? ")) {
			hashed = append(hashed, hashBytes(bytes.TrimSpace(line[3:])))
		}
	}
	sort.Strings(hashed)
	if len(hashed) == 0 {
		return ""
	}
	return fmt.Sprintf("count=%d hash=%s", len(hashed), hashString(strings.Join(hashed, "\n")))
}

func toolIdentity(ctx context.Context, cwd string) map[string]string {
	out := map[string]string{}
	if res := runNative(ctx, cwd, []string{"git", "--version"}); res.ExitCode == 0 {
		out["git"] = strings.TrimSpace(string(res.Stdout))
	}
	if res := runNative(ctx, cwd, []string{"rg", "--version"}); res.ExitCode == 0 {
		first := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")[0]
		out["rg"] = first
	}
	return out
}

func workspaceEpochs(root string) (string, string) {
	var treeParts []string
	var contentParts []string
	maxContentFiles := 2048
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
		mode := info.Mode().String()
		treeParts = append(treeParts, fmt.Sprintf("%s\x00%s\x00%d", rel, mode, info.Size()))
		if !d.IsDir() && info.Mode().IsRegular() && len(contentParts) < maxContentFiles {
			if fp, ok := hashFile(path); ok {
				contentParts = append(contentParts, rel+"\x00"+fp)
			}
		}
		return nil
	})
	sort.Strings(treeParts)
	sort.Strings(contentParts)
	return hashString(strings.Join(treeParts, "\n")), hashString(strings.Join(contentParts, "\n"))
}

func cheapWorkspaceSignal(repoRoot, gitDirAbs string) string {
	var parts []string
	for _, path := range []string{
		repoRoot,
		filepath.Join(gitDirAbs, "HEAD"),
		filepath.Join(gitDirAbs, "config"),
		filepath.Join(repoRoot, ".gitignore"),
		filepath.Join(gitDirAbs, "info", "exclude"),
	} {
		if info, err := os.Stat(path); err == nil {
			parts = append(parts, fmt.Sprintf("%s\x00%d\x00%d\x00%s", path, info.Size(), info.ModTime().UnixNano(), info.Mode().String()))
		} else {
			parts = append(parts, path+"\x00missing")
		}
	}
	return hashString(strings.Join(parts, "\n"))
}

func absPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
			return resolved
		}
		return abs
	}
	return path
}
