package kernel

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type preparedReplay struct {
	Entry         PreparedEntry
	Observation   Observation
	Stdout        []byte
	Stderr        []byte
	HotCacheFrame []byte
}

func (k *Kernel) tryPreparedReplay(inv CommandInvocation, family OperatorFamily, phases *PhaseTimings) (RunResult, bool) {
	candidate, diagnostics, ok := k.findPreparedReplay(inv, phases)
	if !ok {
		return RunResult{}, false
	}
	k.recordPreparedReplay(candidate)
	stdout := append([]byte(nil), candidate.Stdout...)
	stderr := append([]byte(nil), candidate.Stderr...)
	return RunResult{
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: candidate.Observation.ExitCode,
		Mode:     ModeReplay,
		Family:   family,
		Observation: Observation{
			OperationID:  candidate.Observation.OperationID,
			StdoutHash:   candidate.Observation.StdoutHash,
			StderrHash:   candidate.Observation.StderrHash,
			StdoutSize:   candidate.Observation.StdoutSize,
			StderrSize:   candidate.Observation.StderrSize,
			ExitCode:     candidate.Observation.ExitCode,
			NativeWallMS: candidate.Observation.NativeWallMS,
			Timestamp:    candidate.Observation.Timestamp,
			OutputRef:    candidate.Observation.OutputRef,
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
			OperationKey:               "prepared:" + candidate.Entry.PreparedID,
		},
		Diagnostics: diagnostics,
		Phases:      *phases,
	}, true
}

func (k *Kernel) recordPreparedReplay(candidate preparedReplay) {
	if k.Store == nil || candidate.Observation.OutputRef == "" {
		return
	}
	k.mu.Lock()
	if k.ledger != nil {
		for i := range k.ledger.Entries {
			entry := &k.ledger.Entries[i]
			if entry.Observation.OutputRef != candidate.Observation.OutputRef {
				continue
			}
			entry.ReplacementCount++
			entry.LastDecision = ModeReplay
			entry.LastValidatedAt = time.Now()
			entry.NetROIHistoryMS = append(entry.NetROIHistoryMS, int64(entry.Observation.NativeWallMS))
			break
		}
	}
	k.mu.Unlock()
}

func (k *Kernel) tryDiskPreparedReplay(inv CommandInvocation, family OperatorFamily, phases *PhaseTimings) (RunResult, bool) {
	if os.Getenv("SQUIRE_KERNEL_ENABLE_FOREGROUND_DISK_REPLAY") != "1" {
		return RunResult{}, false
	}
	if !isHotPreparedReplayCandidate(inv.PolicyArgv) {
		return RunResult{}, false
	}
	if IsFastPathAllowed(inv.PolicyArgv) {
		return RunResult{}, false
	}
	if k.Store == nil || k.Store.Signal() == "" {
		return RunResult{}, false
	}
	k.ensurePreparedReplayCacheLoaded(phases)
	return k.tryPreparedReplay(inv, family, phases)
}

func (k *Kernel) ensurePreparedReplayCacheLoaded(phases *PhaseTimings) {
	if k.Store == nil {
		return
	}
	signal := k.Store.Signal()
	lockStart := time.Now()
	k.mu.Lock()
	loaded := k.preparedLoaded && (signal == "" || signal == k.preparedSignal)
	k.mu.Unlock()
	phases.LockWaitMS += elapsedMS(lockStart)
	if loaded {
		return
	}
	ledger, available, diagnostics := k.loadLedger(phases)
	if !available || ledger == nil {
		k.mu.Lock()
		k.preparedLoaded = true
		k.preparedAvailable = false
		k.preparedSignal = signal
		k.preparedDiag = diagnostics
		k.preparedReplays = map[string][]preparedReplay{}
		k.preparedWarmFiles = map[string][]preparedWarmFile{}
		k.mu.Unlock()
		return
	}
	k.hydratePreparedReplayCache(ledger, k.Store.Signal(), phases)
}

func (k *Kernel) findPreparedReplay(inv CommandInvocation, phases *PhaseTimings) (preparedReplay, []string, bool) {
	if !isHotPreparedReplayCandidate(inv.PolicyArgv) {
		return preparedReplay{}, nil, false
	}
	candidates, diagnostics := k.residentPreparedReplayCandidates(preparedReplayLookupKey(inv.PolicyArgv), phases)
	if len(candidates) == 0 {
		return k.findPreparedWarmFileReplay(inv, diagnostics, phases)
	}
	hotFPS, hotEpoch, ok := preparedHotProof(inv.PolicyCWD, inv.PolicyArgv)
	if !ok {
		return preparedReplay{}, nil, false
	}
	for _, candidate := range candidates {
		if candidate.Entry.HotInvalidationEpoch != hotEpoch || !mapsEqual(candidate.Entry.HotFingerprints, hotFPS) {
			continue
		}
		return candidate, diagnostics, true
	}
	return k.findPreparedWarmFileReplay(inv, diagnostics, phases)
}

func preparedReplayLookupKey(argv []string) string {
	return hashString(normalizeArgv(normalizeArgvForPolicy(argv)))
}

func (k *Kernel) residentPreparedReplayCandidates(command string, phases *PhaseTimings) ([]preparedReplay, []string) {
	lockStart := time.Now()
	k.mu.Lock()
	phases.LockWaitMS += elapsedMS(lockStart)
	if !k.preparedLoaded || !k.preparedAvailable {
		k.mu.Unlock()
		return nil, nil
	}
	candidates := append([]preparedReplay(nil), k.preparedReplays[command]...)
	diagnostics := append([]string(nil), k.preparedDiag...)
	k.mu.Unlock()
	return candidates, diagnostics
}

func (k *Kernel) hydratePreparedReplayCache(ledger *ValidityLedger, signal string, phases *PhaseTimings) {
	replays, warmFiles, available, diagnostics := k.buildPreparedReplayCache(ledger, phases)
	if available {
		k.publishHotSnapshot(replays, warmFiles)
	}
	k.mu.Lock()
	k.preparedLoaded = true
	k.preparedSignal = signal
	k.preparedAvailable = available
	k.preparedDiag = diagnostics
	k.preparedReplays = replays
	k.preparedWarmFiles = warmFiles
	k.mu.Unlock()
}

func (k *Kernel) buildPreparedReplayCache(ledger *ValidityLedger, phases *PhaseTimings) (map[string][]preparedReplay, map[string][]preparedWarmFile, bool, []string) {
	replays := map[string][]preparedReplay{}
	warmFiles := map[string][]preparedWarmFile{}
	if k.Store == nil {
		return replays, warmFiles, false, []string{"prepared replay cache unavailable"}
	}
	if ledger == nil {
		return replays, warmFiles, false, nil
	}
	for _, prepared := range ledger.Prepared {
		if !prepared.ReplayEligible || prepared.OutputRef == "" || len(prepared.HotFingerprints) == 0 || prepared.HotInvalidationEpoch == "" {
			continue
		}
		if prepared.Kind == PreparedKindWarmFile {
			key := prepared.HotFingerprints["warm_file"]
			if key == "" {
				continue
			}
			outputStart := time.Now()
			content, err := k.Store.LoadWarmFile(prepared.OutputRef)
			phases.OutputMaterializeMS += elapsedMS(outputStart)
			if err != nil {
				continue
			}
			if hashBytes(content) != prepared.OutputFingerprints["file_content"] {
				continue
			}
			warmFiles[key] = append(warmFiles[key], preparedWarmFile{
				Entry:        prepared,
				Content:      content,
				NativeWallMS: 1,
			})
			continue
		}
		obs, ok := observationForPrepared(ledger, prepared)
		if !ok {
			continue
		}
		stdout, stderr, ok := outputForPrepared(ledger, prepared)
		if hashBytes(stdout) != obs.StdoutHash || hashBytes(stderr) != obs.StderrHash {
			outputStart := time.Now()
			var err error
			stdout, stderr, err = k.Store.LoadOutput(prepared.OutputRef)
			phases.OutputMaterializeMS += elapsedMS(outputStart)
			if err != nil {
				continue
			}
			if hashBytes(stdout) != obs.StdoutHash || hashBytes(stderr) != obs.StderrHash {
				continue
			}
		}
		command := prepared.HotFingerprints["hot_command"]
		if command == "" {
			continue
		}
		replays[command] = append(replays[command], preparedReplay{
			Entry:         prepared,
			Observation:   obs,
			Stdout:        stdout,
			Stderr:        stderr,
			HotCacheFrame: encodeHotCacheHitFrame(stdout, stderr, obs.ExitCode),
		})
	}
	return replays, warmFiles, true, nil
}

func observationForPrepared(ledger *ValidityLedger, prepared PreparedEntry) (Observation, bool) {
	for _, entry := range ledger.Entries {
		if entry.Observation.OutputRef != prepared.OutputRef {
			continue
		}
		if prepared.OutputFingerprints != nil && !mapsEqual(entry.OutputFingerprints, prepared.OutputFingerprints) {
			continue
		}
		return entry.Observation, true
	}
	return Observation{}, false
}

func outputForPrepared(ledger *ValidityLedger, prepared PreparedEntry) ([]byte, []byte, bool) {
	for _, entry := range ledger.Entries {
		if entry.Observation.OutputRef != prepared.OutputRef {
			continue
		}
		if entry.StdoutBytes == nil && entry.StderrBytes == nil {
			return nil, nil, false
		}
		return append([]byte(nil), entry.StdoutBytes...), append([]byte(nil), entry.StderrBytes...), true
	}
	return nil, nil, false
}

func preparedHotProof(cwd string, argv []string) (map[string]string, string, bool) {
	argv = normalizeArgvForPolicy(argv)
	switch {
	case IsFastPathAllowed(argv):
		return fastPathHotProof(cwd, argv)
	case isRepoSummaryReplayCandidate(argv):
		return repoSummaryHotProof(cwd, argv)
	case isReplayableFileInspection(argv):
		return fileInspectionHotProof(cwd, argv)
	case isToolVersionProbe(argv):
		return toolVersionHotProof(cwd, argv)
	case isCommandPathLookup(argv):
		return commandPathHotProof(cwd, argv)
	default:
		return nil, "", false
	}
}

func isHotPreparedReplayCandidate(argv []string) bool {
	argv = normalizeArgvForPolicy(argv)
	return IsFastPathAllowed(argv) || isRepoSummaryReplayCandidate(argv) || isReplayableFileInspection(argv) || isToolVersionProbe(argv) || isCommandPathLookup(argv)
}

func repoSummaryHotProof(cwd string, argv []string) (map[string]string, string, bool) {
	fp, epoch, ok := repoSummaryProof(cwd, argv, WorldState{})
	if !ok {
		return nil, "", false
	}
	hot := map[string]string{
		"hot_cwd":     hashString(absPath(cwd)),
		"hot_command": hashString(normalizeArgv(normalizeArgvForPolicy(argv))),
	}
	for k, v := range fp {
		hot[k] = v
	}
	return hot, "hot-" + epoch, true
}

func fastPathHotProof(cwd string, argv []string) (map[string]string, string, bool) {
	repoRoot, gitDirAbs, ok := discoverGitDir(cwd)
	if !ok {
		return nil, "", false
	}
	head, branch, ok := readHeadAndBranch(gitDirAbs)
	if !ok {
		return nil, "", false
	}
	gitDir := ".git"
	if rel, err := filepath.Rel(absPath(cwd), gitDirAbs); err == nil {
		gitDir = filepath.ToSlash(rel)
	}
	fp := map[string]string{
		"hot_cwd":     hashString(absPath(cwd)),
		"hot_command": hashString(normalizeArgv(argv)),
		"repo_root":   hashString(repoRoot),
	}
	var epoch string
	switch normalizedFastPath(argv) {
	case "git rev-parse HEAD":
		fp["head"] = hashString(head)
		epoch = "hot-head:" + hashString(repoRoot+"|"+head)
	case "git rev-parse --abbrev-ref HEAD":
		fp["head"] = hashString(head)
		fp["branch"] = hashString(branch)
		epoch = "hot-branch:" + hashString(repoRoot+"|"+branch+"|"+head)
	case "git rev-parse --git-dir":
		fp["git_dir"] = hashString(gitDir)
		fp["git_dir_abs"] = hashString(gitDirAbs)
		epoch = "hot-gitdir:" + hashString(repoRoot+"|"+gitDir+"|"+gitDirAbs)
	case "git rev-parse --show-toplevel":
		fp["repo_root_abs"] = hashString(repoRoot)
		fp["git_dir_abs"] = hashString(gitDirAbs)
		epoch = "hot-repo-root:" + hashString(repoRoot+"|"+gitDirAbs)
	case "git rev-parse --is-inside-work-tree":
		fp["repo_root_abs"] = hashString(repoRoot)
		fp["git_dir_abs"] = hashString(gitDirAbs)
		epoch = "hot-worktree:" + hashString(repoRoot+"|"+gitDirAbs)
	default:
		return nil, "", false
	}
	return fp, epoch, true
}

func fastPathHotProofFromWorld(ws WorldState, argv []string) (map[string]string, string, bool) {
	if !ws.OracleAvailable || ws.RepoRoot == "" || ws.GitDirAbs == "" {
		return nil, "", false
	}
	argv = normalizeArgvForPolicy(argv)
	fp := map[string]string{
		"hot_cwd":     hashString(ws.RepoRoot),
		"hot_command": hashString(normalizeArgv(argv)),
		"repo_root":   hashString(ws.RepoRoot),
	}
	var epoch string
	switch normalizedFastPath(argv) {
	case "git rev-parse HEAD":
		if ws.Head == "" {
			return nil, "", false
		}
		fp["head"] = hashString(ws.Head)
		epoch = "hot-head:" + hashString(ws.RepoRoot+"|"+ws.Head)
	case "git rev-parse --abbrev-ref HEAD":
		if ws.Head == "" || ws.Branch == "" {
			return nil, "", false
		}
		fp["head"] = hashString(ws.Head)
		fp["branch"] = hashString(ws.Branch)
		epoch = "hot-branch:" + hashString(ws.RepoRoot+"|"+ws.Branch+"|"+ws.Head)
	case "git rev-parse --git-dir":
		if ws.GitDir == "" {
			return nil, "", false
		}
		fp["git_dir"] = hashString(ws.GitDir)
		fp["git_dir_abs"] = hashString(ws.GitDirAbs)
		epoch = "hot-gitdir:" + hashString(ws.RepoRoot+"|"+ws.GitDir+"|"+ws.GitDirAbs)
	case "git rev-parse --show-toplevel":
		fp["repo_root_abs"] = hashString(ws.RepoRoot)
		fp["git_dir_abs"] = hashString(ws.GitDirAbs)
		epoch = "hot-repo-root:" + hashString(ws.RepoRoot+"|"+ws.GitDirAbs)
	case "git rev-parse --is-inside-work-tree":
		fp["repo_root_abs"] = hashString(ws.RepoRoot)
		fp["git_dir_abs"] = hashString(ws.GitDirAbs)
		epoch = "hot-worktree:" + hashString(ws.RepoRoot+"|"+ws.GitDirAbs)
	default:
		return nil, "", false
	}
	return fp, epoch, true
}

func discoverGitDir(cwd string) (string, string, bool) {
	dir := absPath(cwd)
	for {
		gitPath := filepath.Join(dir, ".git")
		if info, err := os.Stat(gitPath); err == nil {
			if info.IsDir() {
				return dir, gitPath, true
			}
			if b, err := os.ReadFile(gitPath); err == nil {
				text := strings.TrimSpace(string(b))
				if strings.HasPrefix(text, "gitdir:") {
					target := strings.TrimSpace(strings.TrimPrefix(text, "gitdir:"))
					if !filepath.IsAbs(target) {
						target = filepath.Join(dir, target)
					}
					return dir, filepath.Clean(target), true
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
}

func fileInspectionHotProof(cwd string, argv []string) (map[string]string, string, bool) {
	root := absPath(cwd)
	argPath := ""
	if isReplayableCatFileRead(argv) {
		argPath = argv[1]
	} else if isBoundedSedPrint(argv) {
		argPath = argv[3]
	}
	if argPath == "" {
		return nil, "", false
	}
	path := filepath.Clean(filepath.Join(root, argPath))
	if !pathWithinRoot(path, root) || !isReplayableInspectionName(filepath.Base(path)) {
		return nil, "", false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() || info.Size() > maxReplayableInspectionFileBytes {
		return nil, "", false
	}
	if isReplayableCatFileRead(argv) && info.Size() > maxFastPathOutputBytes {
		return nil, "", false
	}
	contentHash, ok := hashFile(path)
	if !ok {
		return nil, "", false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, "", false
	}
	rel = filepath.ToSlash(rel)
	fp := map[string]string{
		"hot_cwd":            hashString(root),
		"hot_command":        hashString(normalizeArgv(argv)),
		"file_path":          hashString(rel),
		"file_name":          hashString(filepath.Base(path)),
		"file_content":       contentHash,
		"file_size":          hashString(strconv.FormatInt(info.Size(), 10)),
		"file_mode":          hashString(info.Mode().String()),
		"inspection_command": hashString(normalizeArgv(argv)),
	}
	if isBoundedSedPrint(argv) {
		fp["sed_range"] = hashString(argv[2])
	}
	epoch := "hot-file-inspection:" + hashString(root+"|"+rel+"|"+contentHash+"|"+strconv.FormatInt(info.Size(), 10)+"|"+info.Mode().String()+"|"+normalizeArgv(argv))
	return fp, epoch, true
}

func toolVersionHotProof(cwd string, argv []string) (map[string]string, string, bool) {
	name := filepath.Base(argv[0])
	signal, ok := executableSignal(cwd, name)
	if !ok {
		return nil, "", false
	}
	fp := map[string]string{
		"hot_cwd":            hashString(absPath(cwd)),
		"hot_command":        hashString(normalizeArgv(argv)),
		"tool_name":          hashString(name),
		"tool_path":          signal.PathHash,
		"tool_executable":    signal.FileHash,
		"path_env":           hashString(os.Getenv("PATH")),
		"version_env":        deterministicVersionEnvHash(),
		"version_probe_argv": hashString(normalizeArgv(argv)),
	}
	epoch := "hot-tool-version:" + hashString(fmt.Sprintf("%s|%s|%s|%s|%s", name, signal.PathHash, signal.FileHash, fp["path_env"], fp["version_env"]))
	return fp, epoch, true
}

func commandPathHotProof(cwd string, argv []string) (map[string]string, string, bool) {
	target := commandPathLookupTarget(argv)
	if target == "" {
		return nil, "", false
	}
	whichSignal, whichOK := executableSignal(cwd, filepath.Base(argv[0]))
	if filepath.Base(argv[0]) == "command" {
		whichSignal = executableProofSignal{PathHash: hashString("shell-builtin:command"), FileHash: hashString("shell-builtin:command-v")}
		whichOK = true
	}
	targetSignal, targetOK := executableSignal(cwd, target)
	if !whichOK || !targetOK {
		return nil, "", false
	}
	fp := map[string]string{
		"hot_cwd":           hashString(absPath(cwd)),
		"hot_command":       hashString(normalizeArgv(argv)),
		"lookup_tool":       hashString(target),
		"which_path":        whichSignal.PathHash,
		"which_executable":  whichSignal.FileHash,
		"target_path":       targetSignal.PathHash,
		"target_executable": targetSignal.FileHash,
		"path_env":          hashString(os.Getenv("PATH")),
		"version_env":       deterministicVersionEnvHash(),
		"path_lookup_argv":  hashString(normalizeArgv(argv)),
	}
	epoch := "hot-command-path:" + hashString(fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s", target, whichSignal.PathHash, whichSignal.FileHash, targetSignal.PathHash, targetSignal.FileHash, fp["path_env"], fp["version_env"]))
	return fp, epoch, true
}
