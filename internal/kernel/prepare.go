package kernel

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	PreparedKindFastPathOutput     = "fast_path_output"
	PreparedKindFileTreeIndex      = "file_tree_index"
	PreparedKindProjectMetadata    = "project_metadata"
	PreparedKindCommandPath        = "command_path_index"
	PreparedKindEcosystem          = "ecosystem_metadata"
	PreparedKindDependencyMetadata = "dependency_metadata"
	PreparedKindSourceSymbolIndex  = "source_symbol_index"
	PreparedKindProofGatedOutput   = "proof_gated_output"
	PreparedKindWarmFile           = "warm_file"

	proofGatedPrewarmCommandFileLimit = 160
	workspaceImagePrewarmFileLimit    = 2048
)

type WarmReport struct {
	Claim                   string               `json:"claim"`
	RepoRoot                string               `json:"repo_root"`
	OracleAvailable         bool                 `json:"oracle_available"`
	FastPathPrepared        int                  `json:"fast_path_prepared"`
	ProofGatedPrewarmed     int                  `json:"proof_gated_prewarmed"`
	WarmFilesPrepared       int                  `json:"warm_files_prepared"`
	FileTreeIndexesPrepared int                  `json:"file_tree_indexes_prepared"`
	ProjectMetadataPrepared int                  `json:"project_metadata_prepared"`
	CommandPathPrepared     int                  `json:"command_path_prepared"`
	EcosystemPrepared       int                  `json:"ecosystem_prepared"`
	DependencyPrepared      int                  `json:"dependency_metadata_prepared"`
	SourceSymbolPrepared    int                  `json:"source_symbol_indexes_prepared"`
	Prepared                []WarmPreparedReport `json:"prepared"`
	PrivacyMode             string               `json:"privacy_mode"`
	AgentVisibleSuggestions bool                 `json:"agent_visible_suggestions"`
	ReplaySetUnchanged      bool                 `json:"replay_set_unchanged"`
	Notes                   []string             `json:"notes,omitempty"`
}

type WarmPreparedReport struct {
	Kind              string          `json:"kind"`
	OperatorFamily    OperatorFamily  `json:"operator_family"`
	NormalizedCommand string          `json:"normalized_command,omitempty"`
	ReplayEligible    bool            `json:"replay_eligible"`
	EvidenceQuality   EvidenceQuality `json:"evidence_quality"`
	Privacy           string          `json:"privacy"`
}

func Warm(ctx context.Context, cwd, storeRoot string) (WarmReport, error) {
	k := New(storeRoot)
	return k.Warm(ctx, cwd)
}

func WarmMetadata(ctx context.Context, cwd, storeRoot string) (WarmReport, error) {
	k := New(storeRoot)
	return k.WarmMetadata(ctx, cwd)
}

func (k *Kernel) Warm(ctx context.Context, cwd string) (WarmReport, error) {
	return k.warm(ctx, cwd, false)
}

func (k *Kernel) WarmMetadata(ctx context.Context, cwd string) (WarmReport, error) {
	return k.warm(ctx, cwd, true)
}

func (k *Kernel) warm(ctx context.Context, cwd string, metadataOnly bool) (WarmReport, error) {
	if k.Oracle == nil {
		k.Oracle = NewRepoOracle()
	}
	if err := k.Store.Init(); err != nil {
		return WarmReport{}, err
	}
	var ws WorldState
	if metadataOnly {
		ws = k.Oracle.MetadataSnapshot(ctx, cwd)
		if ws.OracleAvailable && ws.RepoRoot != "" && absPath(cwd) != ws.RepoRoot {
			ws = k.Oracle.MetadataSnapshot(ctx, ws.RepoRoot)
		}
	} else {
		ws = k.Oracle.Snapshot(ctx, cwd)
	}
	ledger, err := k.Store.Load()
	if err != nil {
		return WarmReport{}, err
	}

	report := WarmReport{
		Claim:                   scopedKernelClaim,
		RepoRoot:                ws.RepoRoot,
		OracleAvailable:         ws.OracleAvailable,
		PrivacyMode:             "standard",
		AgentVisibleSuggestions: false,
		ReplaySetUnchanged:      true,
		Notes: []string{
			"Level 3 read-only virtual workspace image warm; no CoW writes, no rollback mutation layer.",
			"Enabled fast paths and proof-gated read-only discovery outputs may be replay eligible.",
			"Level 5 observed-command speculation warms local follow-up reads/probes only; no token stream monitoring.",
			"Non-replay preparations store hashes and counts, not raw source or arbitrary stdout.",
		},
	}
	if !ws.OracleAvailable {
		report.Notes = append(report.Notes, "repo oracle unavailable; prepared only non-git workspace fingerprints")
	}

	var phases PhaseTimings
	metadataCWD := cwd
	if ws.OracleAvailable && ws.RepoRoot != "" {
		metadataCWD = ws.RepoRoot
	}
	if ws.OracleAvailable && ws.Head != "" {
		observedArgv := []string{"git", "rev-parse", "HEAD"}
		observed := runNative(ctx, metadataCWD, observedArgv)
		if observed.ExitCode == 0 {
			if err := k.precomputeFastPathOutputs(ctx, metadataCWD, "warm", observedArgv, observed, ws, ledger, &phases); err != nil {
				return WarmReport{}, err
			}
			for _, argv := range enabledFastPathArgv() {
				if addPreparedFastPath(ws, ledger, argv) {
					report.FastPathPrepared++
					report.Prepared = append(report.Prepared, WarmPreparedReport{
						Kind:              PreparedKindFastPathOutput,
						OperatorFamily:    FamilyLocalRepoMetadata,
						NormalizedCommand: normalizedFastPath(argv),
						ReplayEligible:    true,
						EvidenceQuality:   ws.EvidenceQuality,
						Privacy:           "allowlisted local output bytes stored for exact replay",
					})
				}
			}
		}
	}
	if metadataOnly {
		report.Notes = append(report.Notes, "metadata-only warm skipped workspace/speculative prewarming")
		if err := k.finishWarm(ledger, &phases); err != nil {
			return WarmReport{}, err
		}
		return report, nil
	}

	prewarmed, prewarmedReports := k.prewarmProofGatedOutputs(ctx, cwd, ws, ledger, &phases)
	report.ProofGatedPrewarmed += prewarmed
	report.Prepared = append(report.Prepared, prewarmedReports...)

	warmFiles, warmFileReports := k.prewarmWarmFiles(ctx, cwd, ws, ledger, &phases)
	report.WarmFilesPrepared += warmFiles
	report.Prepared = append(report.Prepared, warmFileReports...)

	if addPreparedFileTreeIndex(ws, ledger) {
		report.FileTreeIndexesPrepared++
		report.Prepared = append(report.Prepared, WarmPreparedReport{
			Kind:              PreparedKindFileTreeIndex,
			OperatorFamily:    FamilySearchList,
			NormalizedCommand: "workspace file tree index",
			ReplayEligible:    false,
			EvidenceQuality:   ws.EvidenceQuality,
			Privacy:           "hashes and counts only; no raw paths or stdout",
		})
	}

	projectPrepared := addPreparedProjectMetadata(ws, ledger)
	report.ProjectMetadataPrepared += projectPrepared
	for i := 0; i < projectPrepared; i++ {
		report.Prepared = append(report.Prepared, WarmPreparedReport{
			Kind:              PreparedKindProjectMetadata,
			OperatorFamily:    FamilyFileInspection,
			NormalizedCommand: "well-known project metadata fingerprint",
			ReplayEligible:    false,
			EvidenceQuality:   ws.EvidenceQuality,
			Privacy:           "well-known filename plus content hash only; no file contents",
		})
	}

	if addPreparedCommandPathIndex(ws, ledger) {
		report.CommandPathPrepared++
		report.Prepared = append(report.Prepared, WarmPreparedReport{
			Kind:              PreparedKindCommandPath,
			OperatorFamily:    FamilyEnvironment,
			NormalizedCommand: "PATH executable index",
			ReplayEligible:    false,
			EvidenceQuality:   EvidencePartial,
			Privacy:           "PATH, executable names, and directories are hashed; no raw env or paths",
		})
	}

	ecosystemPrepared := addPreparedEcosystemMetadata(ws, ledger)
	report.EcosystemPrepared += ecosystemPrepared
	for i := 0; i < ecosystemPrepared; i++ {
		report.Prepared = append(report.Prepared, WarmPreparedReport{
			Kind:              PreparedKindEcosystem,
			OperatorFamily:    FamilyFileInspection,
			NormalizedCommand: "ecosystem follow-up metadata fingerprint",
			ReplayEligible:    false,
			EvidenceQuality:   ws.EvidenceQuality,
			Privacy:           "manifest/lockfile names and contents are hashed; no file contents",
		})
	}

	dependencyPrepared := addPreparedDependencyMetadata(ws, ledger)
	report.DependencyPrepared += dependencyPrepared
	for i := 0; i < dependencyPrepared; i++ {
		report.Prepared = append(report.Prepared, WarmPreparedReport{
			Kind:              PreparedKindDependencyMetadata,
			OperatorFamily:    FamilyFileInspection,
			NormalizedCommand: "ecosystem dependency proof seed",
			ReplayEligible:    false,
			EvidenceQuality:   ws.EvidenceQuality,
			Privacy:           "manifest/lockfile/tool signals are hashed; dependency-list commands are not run",
		})
	}

	sourceSymbolPrepared := addPreparedSourceSymbolIndex(ws, ledger)
	report.SourceSymbolPrepared += sourceSymbolPrepared
	if sourceSymbolPrepared > 0 {
		report.Prepared = append(report.Prepared, WarmPreparedReport{
			Kind:              PreparedKindSourceSymbolIndex,
			OperatorFamily:    FamilyFileInspection,
			NormalizedCommand: "workspace top-level source symbol/import index",
			ReplayEligible:    false,
			EvidenceQuality:   ws.EvidenceQuality,
			Privacy:           "source lines and symbol names are hashed; no raw source content is persisted",
		})
	}

	if err := k.finishWarm(ledger, &phases); err != nil {
		return WarmReport{}, err
	}
	return report, nil
}

func (k *Kernel) finishWarm(ledger *ValidityLedger, phases *PhaseTimings) error {
	if err := k.Store.Save(ledger); err != nil {
		return err
	}
	k.hydratePreparedReplayCache(ledger, k.Store.Signal(), phases)
	k.mu.Lock()
	k.ledger = ledger
	k.ledgerLoaded = true
	k.ledgerAvailable = true
	k.ledgerDiag = nil
	k.ledgerSignal = k.Store.Signal()
	k.mu.Unlock()
	return nil
}

func enabledFastPathArgv() [][]string {
	return [][]string{
		{"git", "rev-parse", "HEAD"},
		{"git", "rev-parse", "--git-dir"},
		{"git", "rev-parse", "--abbrev-ref", "HEAD"},
		{"git", "branch", "--show-current"},
		{"git", "rev-parse", "--show-toplevel"},
		{"git", "rev-parse", "--is-inside-work-tree"},
	}
}

func addPreparedFastPath(ws WorldState, ledger *ValidityLedger, argv []string) bool {
	op := Operation{
		OperationID:       hashString("warm" + normalizeArgv(argv)),
		SessionID:         "warm",
		OperatorFamily:    Classify(argv),
		NormalizedCommand: normalizedFastPath(argv),
		Argv:              argv,
		CWD:               ws.RepoRoot,
		RepoRoot:          ws.RepoRoot,
		Mode:              ModeNative,
		EvidenceQuality:   ws.EvidenceQuality,
	}
	key := operationKey(op, ws)
	fps := inputFingerprints(op, ws)
	epoch := invalidationEpoch(op, ws)
	hotFPS, hotEpoch, hotOK := fastPathHotProofFromWorld(ws, argv)
	if !hotOK {
		return false
	}
	entry, ok := ledger.FindValid(key, fps, epoch)
	if !ok || entry.Observation.OutputRef == "" {
		return false
	}
	ledger.UpsertPrepared(PreparedEntry{
		PreparedID:           hashString("prepared:fast-path:" + key + ":" + epoch),
		Kind:                 PreparedKindFastPathOutput,
		OperatorFamily:       op.OperatorFamily,
		NormalizedCommand:    op.NormalizedCommand,
		InputFingerprints:    fps,
		HotFingerprints:      hotFPS,
		OutputFingerprints:   entry.OutputFingerprints,
		InvalidationEpoch:    epoch,
		HotInvalidationEpoch: hotEpoch,
		EvidenceQuality:      ws.EvidenceQuality,
		ReplayEligible:       true,
		OutputRef:            entry.Observation.OutputRef,
		Privacy:              "allowlisted local output bytes stored for exact replay",
		PreparedAt:           time.Now(),
		Notes:                []string{"existing enabled fast path; native fallback still available"},
	})
	return true
}

func (k *Kernel) prewarmProofGatedOutputs(ctx context.Context, cwd string, ws WorldState, ledger *ValidityLedger, phases *PhaseTimings) (int, []WarmPreparedReport) {
	candidates := proofGatedPrewarmCandidates(ws)
	if len(candidates) == 0 {
		return 0, nil
	}
	type result struct {
		argv     []string
		native   NativeResult
		hotFPS   map[string]string
		hotEpoch string
		hotOK    bool
	}
	jobs := make(chan []string)
	results := make(chan result, len(candidates))
	workers := 4
	if len(candidates) < workers {
		workers = len(candidates)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for argv := range jobs {
				if !IsProofGatedReplayCandidate(argv) {
					continue
				}
				if proofGatedWarmBlocked(cwd, argv) {
					continue
				}
				beforeFPS, beforeEpoch, beforeOK := preparedHotProof(cwd, argv)
				if !beforeOK {
					continue
				}
				native := runProofGatedNative(ctx, cwd, argv)
				if proofGatedWarmBlocked(cwd, argv) {
					continue
				}
				afterFPS, afterEpoch, afterOK := preparedHotProof(cwd, argv)
				if !afterOK || beforeEpoch != afterEpoch || !mapsEqual(beforeFPS, afterFPS) {
					continue
				}
				results <- result{argv: argv, native: native, hotFPS: afterFPS, hotEpoch: afterEpoch, hotOK: true}
			}
		}()
	}
	for _, argv := range candidates {
		jobs <- argv
	}
	close(jobs)
	wg.Wait()
	close(results)

	var count int
	var reports []WarmPreparedReport
	for res := range results {
		if res.native.ExitCode != 0 {
			continue
		}
		op := Operation{
			OperationID:       hashString("warm-prewarm" + normalizeArgv(res.argv)),
			SessionID:         "warm",
			OperatorFamily:    Classify(res.argv),
			NormalizedCommand: normalizedFastPath(res.argv),
			Argv:              res.argv,
			CWD:               cwd,
			RepoRoot:          ws.RepoRoot,
			Mode:              ModeNative,
			EvidenceQuality:   ws.EvidenceQuality,
		}
		if !proofGatedCandidateUsable(op, ws) {
			continue
		}
		key := operationKey(op, ws)
		fps := inputFingerprints(op, ws)
		epoch := invalidationEpoch(op, ws)
		if !res.hotOK {
			continue
		}
		if err := k.storeReplayableObservation(res.argv, "warm", cwd, res.native, ws, ledger, phases, ModeNative); err != nil {
			continue
		}
		k.mu.Lock()
		ledger.RecordWarmObservation(key, op.OperatorFamily, fps, epoch)
		entry, ok := ledger.FindValid(key, fps, epoch)
		k.mu.Unlock()
		if !ok || entry.Observation.OutputRef == "" {
			continue
		}
		ledger.UpsertPrepared(PreparedEntry{
			PreparedID:           hashString("prepared:proof-gated:" + key + ":" + epoch),
			Kind:                 PreparedKindProofGatedOutput,
			OperatorFamily:       op.OperatorFamily,
			NormalizedCommand:    op.NormalizedCommand,
			InputFingerprints:    fps,
			HotFingerprints:      res.hotFPS,
			OutputFingerprints:   entry.OutputFingerprints,
			InvalidationEpoch:    epoch,
			HotInvalidationEpoch: res.hotEpoch,
			EvidenceQuality:      ws.EvidenceQuality,
			ReplayEligible:       true,
			OutputRef:            entry.Observation.OutputRef,
			Privacy:              proofGatedOutputPrivacy(res.argv),
			PreparedAt:           time.Now(),
			Notes:                []string{"speculative warm observation; native fallback still available"},
		})
		count++
		reports = append(reports, WarmPreparedReport{
			Kind:              PreparedKindProofGatedOutput,
			OperatorFamily:    op.OperatorFamily,
			NormalizedCommand: op.NormalizedCommand,
			ReplayEligible:    true,
			EvidenceQuality:   ws.EvidenceQuality,
			Privacy:           proofGatedOutputPrivacy(res.argv),
		})
	}
	return count, reports
}

func proofGatedPrewarmCandidates(ws WorldState) [][]string {
	seen := map[string]bool{}
	var out [][]string
	add := func(argv []string) {
		key := normalizeArgv(argv)
		if seen[key] || !isHotPreparedReplayCandidate(argv) {
			return
		}
		seen[key] = true
		out = append(out, argv)
	}
	if ws.RepoRoot != "" {
		add([]string{"git", "ls-files"})
		add([]string{"git", "status", "--short"})
		add([]string{"git", "status", "--porcelain"})
		add([]string{"git", "diff"})
		add([]string{"git", "diff", "--stat"})
		add([]string{"git", "log", "-1", "--format=%H%n%s"})
		add([]string{"rg", "--files"})
		for _, rel := range replayableGitLsFilesPrewarmTargets(ws.RepoRoot, 64) {
			add([]string{"git", "ls-files", rel})
		}
		add([]string{"ls"})
		add([]string{"ls", "-p"})
		add([]string{"ls", "-la"})
		for _, rel := range replayableDirectoryPrewarmTargets(ws.RepoRoot, 24) {
			add([]string{"ls", rel})
			add([]string{"ls", "-p", rel})
		}
		for _, rel := range replayableInspectionPrewarmFiles(ws.RepoRoot, proofGatedPrewarmCommandFileLimit) {
			add([]string{"cat", rel})
			add([]string{"file", rel})
			add([]string{"head", "-n", "20", rel})
			add([]string{"tail", "-n", "50", rel})
			if isLikelySourceInspectionFile(rel) {
				for _, expr := range commonSedPrewarmRanges() {
					add([]string{"sed", "-n", expr, rel})
				}
				add([]string{"git", "diff", "--", rel})
			}
		}
	}
	for _, argv := range [][]string{
		{"git", "--version"},
		{"rg", "--version"},
		{"go", "version"},
		{"node", "--version"},
		{"npm", "--version"},
		{"python", "--version"},
		{"python3", "--version"},
		{"pip", "--version"},
		{"pip3", "--version"},
		{"whoami"},
		{"hostname"},
		{"id"},
		{"uname", "-m"},
		{"uname", "-s"},
		{"printenv", "PATH"},
		{"printenv", "HOME"},
		{"printenv", "USER"},
		{"printenv", "LANG"},
		{"printenv", "SHELL"},
	} {
		add(argv)
	}
	for _, tool := range []string{"git", "rg", "go", "node", "npm", "python", "python3", "pip", "pip3", "make"} {
		add([]string{"which", tool})
		add([]string{"command", "-v", tool})
	}
	return out
}

func proofGatedOutputPrivacy(argv []string) string {
	switch {
	case isManifestFileRead(argv):
		return "well-known manifest/config output bytes stored locally for exact replay"
	case isReplayableFileInspection(argv):
		return "bounded workspace file-inspection output bytes stored locally for exact replay"
	case isToolVersionProbe(argv):
		return "tool version stdout/stderr stored locally for exact replay"
	case isCommandPathLookup(argv):
		return "command path lookup output stored locally for exact replay"
	case isStaticEnvironmentProbe(argv):
		return "static environment probe output stored locally for exact session replay"
	case isPrintenvProbe(argv):
		return "selected non-sensitive environment variable output stored locally for exact session replay"
	case isDirectoryListing(argv):
		return "directory listing output stored locally for exact replay with directory/stat/env proof"
	case isGitHeadSubjectLog(argv):
		return "current HEAD commit metadata stored locally for exact replay"
	default:
		return "proof-gated output bytes stored locally for exact replay"
	}
}

func replayableDirectoryPrewarmTargets(root string, limit int) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var rels []string
	for _, entry := range entries {
		if len(rels) >= limit {
			break
		}
		if !entry.IsDir() || shouldSkipPrewarmDir(entry.Name()) {
			continue
		}
		if _, _, ok := parseDirectoryListing([]string{"ls", entry.Name()}); !ok {
			continue
		}
		rels = append(rels, entry.Name())
	}
	return rels
}

func replayableGitLsFilesPrewarmTargets(root string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	var rels []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || len(rels) >= limit {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path == root {
			return nil
		}
		if shouldSkipPrewarmDir(name) {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if safeRelativeInspectionPath(filepath.FromSlash(rel)) {
			rels = append(rels, rel)
		}
		return nil
	})
	return rels
}

func replayableInspectionPrewarmFiles(root string, limit int) []string {
	var rels []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || len(rels) >= limit {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if shouldSkipPrewarmDir(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isReplayableInspectionName(name) {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxReplayableInspectionFileBytes {
			return nil
		}
		if isReplayableCatFileRead([]string{"cat", name}) && info.Size() > maxFastPathOutputBytes {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !safeRelativeInspectionPath(filepath.FromSlash(rel)) {
			return nil
		}
		rels = append(rels, rel)
		return nil
	})
	return rels
}

func shouldSkipPrewarmDir(name string) bool {
	if name == ".git" || name == ".squire" || strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "vendor", "dist", "build", "coverage", "target", ".next", ".turbo", ".cache", ".venv", "venv":
		return true
	default:
		return false
	}
}

func isLikelySourceInspectionFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs",
		".py", ".rs", ".java", ".kt", ".kts", ".rb", ".php",
		".c", ".h", ".cc", ".cpp", ".hpp", ".cs", ".swift",
		".sh", ".bash", ".zsh", ".fish", ".sql",
		".css", ".scss", ".sass", ".html", ".htm", ".md", ".markdown":
		return true
	default:
		return false
	}
}

func commonSedPrewarmRanges() []string {
	return []string{
		"1,80p",
		"1,120p",
		"1,140p",
		"1,160p",
		"1,200p",
		"1,220p",
		"1,240p",
		"1,260p",
		"220,520p",
		"520,920p",
		"920,1220p",
	}
}

func runProofGatedNative(ctx context.Context, cwd string, argv []string) NativeResult {
	if isGitRepoState(argv) {
		start := time.Now()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
		configureNativeCommandCleanup(cmd)
		stdout, err := cmd.Output()
		var stderr []byte
		exitCode := 0
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				stderr = exit.Stderr
				exitCode = exit.ExitCode()
			} else {
				stderr = []byte(err.Error())
				exitCode = 127
			}
		}
		return NativeResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode, Wall: time.Since(start), Err: err}
	}
	return runNative(ctx, cwd, argv)
}

func proofGatedWarmBlocked(cwd string, argv []string) bool {
	if !isGitRepoState(argv) {
		return false
	}
	_, gitDir, ok := discoverGitDir(cwd)
	if !ok {
		return false
	}
	for _, rel := range []string{"index.lock", "HEAD.lock", "config.lock", "packed-refs.lock"} {
		if _, err := os.Stat(filepath.Join(gitDir, rel)); err == nil {
			return true
		}
	}
	return false
}

func wellKnownManifestNames() []string {
	return []string{
		"go.mod",
		"go.sum",
		"go.work",
		"go.work.sum",
		"package.json",
		"package-lock.json",
		"npm-shrinkwrap.json",
		"pnpm-lock.yaml",
		"yarn.lock",
		"tsconfig.json",
		"Cargo.toml",
		"Cargo.lock",
		"rust-toolchain",
		"rust-toolchain.toml",
		"pyproject.toml",
		"poetry.lock",
		"requirements.txt",
		"requirements-dev.txt",
		"setup.cfg",
		"tox.ini",
		"Makefile",
		"makefile",
		"Dockerfile",
		"docker-compose.yml",
		"compose.yml",
	}
}

func addPreparedFileTreeIndex(ws WorldState, ledger *ValidityLedger) bool {
	if ws.RepoRoot == "" {
		return false
	}
	count := workspaceFileCount(ws.RepoRoot)
	entry := PreparedEntry{
		PreparedID:        hashString("prepared:file-tree:" + ws.RepoRoot + ":" + ws.WorkspaceEpoch),
		Kind:              PreparedKindFileTreeIndex,
		OperatorFamily:    FamilySearchList,
		NormalizedCommand: "workspace file tree index",
		InputFingerprints: map[string]string{
			"repo_root":               hashString(ws.RepoRoot),
			"ignore_rule_fingerprint": ws.IgnoreRuleFingerprint,
			"workspace_epoch":         ws.WorkspaceEpoch,
		},
		OutputFingerprints: map[string]string{
			"file_tree_epoch":    ws.FileTreeEpoch,
			"file_content_epoch": ws.FileContentEpoch,
			"file_count_hash":    hashString(strconv.Itoa(count)),
		},
		InvalidationEpoch: ws.WorkspaceEpoch,
		EvidenceQuality:   ws.EvidenceQuality,
		ReplayEligible:    false,
		Privacy:           "hashes and counts only; no raw paths or stdout",
		PreparedAt:        time.Now(),
		Notes:             []string{"supports future file-list proof work; not replay eligible"},
	}
	ledger.UpsertPrepared(entry)
	return true
}

func addPreparedProjectMetadata(ws WorldState, ledger *ValidityLedger) int {
	if ws.RepoRoot == "" {
		return 0
	}
	names := []string{
		"go.mod",
		"go.sum",
		"package.json",
		"package-lock.json",
		"pnpm-lock.yaml",
		"yarn.lock",
		"Cargo.toml",
		"Cargo.lock",
		"pyproject.toml",
		"poetry.lock",
		"requirements.txt",
		"Makefile",
	}
	var count int
	for _, name := range names {
		path := filepath.Join(ws.RepoRoot, name)
		fp, ok := hashFile(path)
		if !ok {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		entry := PreparedEntry{
			PreparedID:        hashString("prepared:project-metadata:" + ws.RepoRoot + ":" + name + ":" + fp),
			Kind:              PreparedKindProjectMetadata,
			OperatorFamily:    FamilyFileInspection,
			NormalizedCommand: "well-known project metadata fingerprint",
			InputFingerprints: map[string]string{
				"repo_root": hashString(ws.RepoRoot),
				"file_name": hashString(name),
			},
			OutputFingerprints: map[string]string{
				"content_hash": fp,
				"size_hash":    hashString(fmt.Sprintf("%d", info.Size())),
			},
			InvalidationEpoch: hashString(name + ":" + fp),
			EvidenceQuality:   ws.EvidenceQuality,
			ReplayEligible:    false,
			Privacy:           "well-known filename plus content hash only; no file contents",
			PreparedAt:        time.Now(),
			Notes:             []string{"supports future local project metadata proof work; not replay eligible"},
		}
		ledger.UpsertPrepared(entry)
		count++
	}
	return count
}

func addPreparedCommandPathIndex(ws WorldState, ledger *ValidityLedger) bool {
	pathValue := os.Getenv("PATH")
	if pathValue == "" {
		return false
	}
	dirs := filepath.SplitList(pathValue)
	var dirSignals []string
	var executableSignals []string
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		cleanDir := filepath.Clean(dir)
		info, err := os.Stat(cleanDir)
		if err != nil || !info.IsDir() {
			dirSignals = append(dirSignals, hashString(cleanDir)+"\x00missing")
			continue
		}
		dirSignals = append(dirSignals, hashString(cleanDir)+"\x00"+fmt.Sprintf("%d\x00%d", info.Size(), info.ModTime().UnixNano()))
		entries, err := os.ReadDir(cleanDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			entryInfo, err := entry.Info()
			if err != nil || entryInfo.Mode().Perm()&0o111 == 0 {
				continue
			}
			executableSignals = append(executableSignals, hashString(entry.Name())+"\x00"+fmt.Sprintf("%d", entryInfo.Size()))
		}
	}
	if len(dirSignals) == 0 {
		return false
	}
	entry := PreparedEntry{
		PreparedID:        hashString("prepared:command-path:" + hashString(pathValue) + ":" + hashString(strings.Join(dirSignals, "\n"))),
		Kind:              PreparedKindCommandPath,
		OperatorFamily:    FamilyEnvironment,
		NormalizedCommand: "PATH executable index",
		InputFingerprints: map[string]string{
			"path_hash": hashString(pathValue),
		},
		OutputFingerprints: map[string]string{
			"path_dir_signal":       hashString(strings.Join(dirSignals, "\n")),
			"executable_signal":     hashString(strings.Join(executableSignals, "\n")),
			"path_dir_count_hash":   hashString(strconv.Itoa(len(dirSignals))),
			"executable_count_hash": hashString(strconv.Itoa(len(executableSignals))),
		},
		InvalidationEpoch: hashString(pathValue + "\x00" + strings.Join(dirSignals, "\n")),
		EvidenceQuality:   EvidencePartial,
		ReplayEligible:    false,
		Privacy:           "PATH, executable names, and directories are hashed; no raw env or paths",
		PreparedAt:        time.Now(),
		Notes:             []string{"supports future command-path proof work; aliases/functions are not replay eligible"},
	}
	ledger.UpsertPrepared(entry)
	return true
}

func addPreparedEcosystemMetadata(ws WorldState, ledger *ValidityLedger) int {
	if ws.RepoRoot == "" {
		return 0
	}
	ecosystems := map[string][]string{
		"go":         {"go.mod", "go.sum", "go.work", "go.work.sum"},
		"node":       {"package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "npm-shrinkwrap.json", "tsconfig.json"},
		"rust":       {"Cargo.toml", "Cargo.lock", "rust-toolchain", "rust-toolchain.toml"},
		"python":     {"pyproject.toml", "poetry.lock", "requirements.txt", "requirements-dev.txt", "setup.cfg", "tox.ini"},
		"make":       {"Makefile", "makefile"},
		"container":  {"Dockerfile", "docker-compose.yml", "compose.yml"},
		"terraform":  {"versions.tf", "providers.tf", ".terraform.lock.hcl"},
		"kubernetes": {"kustomization.yaml", "kustomization.yml"},
	}
	var count int
	for ecosystem, names := range ecosystems {
		var signals []string
		for _, name := range names {
			path := filepath.Join(ws.RepoRoot, name)
			fp, ok := hashFile(path)
			if !ok {
				continue
			}
			info, err := os.Stat(path)
			if err != nil || info.IsDir() {
				continue
			}
			signals = append(signals, hashString(name)+"\x00"+fp+"\x00"+hashString(fmt.Sprintf("%d", info.Size())))
		}
		if len(signals) == 0 {
			continue
		}
		entry := PreparedEntry{
			PreparedID:        hashString("prepared:ecosystem:" + ws.RepoRoot + ":" + ecosystem + ":" + hashString(strings.Join(signals, "\n"))),
			Kind:              PreparedKindEcosystem,
			OperatorFamily:    FamilyFileInspection,
			NormalizedCommand: "ecosystem follow-up metadata fingerprint",
			InputFingerprints: map[string]string{
				"repo_root": hashString(ws.RepoRoot),
				"ecosystem": hashString(ecosystem),
			},
			OutputFingerprints: map[string]string{
				"metadata_signal":     hashString(strings.Join(signals, "\n")),
				"metadata_count_hash": hashString(strconv.Itoa(len(signals))),
			},
			InvalidationEpoch: hashString(ecosystem + ":" + strings.Join(signals, "\n")),
			EvidenceQuality:   ws.EvidenceQuality,
			ReplayEligible:    false,
			Privacy:           "manifest/lockfile names and contents are hashed; no file contents",
			PreparedAt:        time.Now(),
			Notes:             []string{"supports future ecosystem-specific read-only proof work; not replay eligible"},
		}
		ledger.UpsertPrepared(entry)
		count++
	}
	return count
}

func addPreparedDependencyMetadata(ws WorldState, ledger *ValidityLedger) int {
	if ws.RepoRoot == "" {
		return 0
	}
	type dependencySeed struct {
		ecosystem string
		files     []string
		tools     []string
		commands  []string
	}
	seeds := []dependencySeed{
		{
			ecosystem: "node",
			files:     []string{"package.json", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock"},
			tools:     []string{"node", "npm", "pnpm", "yarn"},
			commands:  []string{"npm ls --json", "npm query . --json", "pnpm list --json", "yarn info --json"},
		},
		{
			ecosystem: "python",
			files:     []string{"pyproject.toml", "poetry.lock", "requirements.txt", "requirements-dev.txt", "setup.cfg", "tox.ini"},
			tools:     []string{"python", "python3", "pip", "pip3", "poetry"},
			commands:  []string{"pip list --format=json", "pip freeze", "python -m pip list --format=json"},
		},
		{
			ecosystem: "go",
			files:     []string{"go.mod", "go.sum", "go.work", "go.work.sum"},
			tools:     []string{"go"},
			commands:  []string{"go list -m -json all", "go env -json"},
		},
		{
			ecosystem: "rust",
			files:     []string{"Cargo.toml", "Cargo.lock", "rust-toolchain", "rust-toolchain.toml"},
			tools:     []string{"cargo", "rustc"},
			commands:  []string{"cargo metadata --format-version 1 --no-deps"},
		},
	}
	var count int
	for _, seed := range seeds {
		var fileSignals []string
		for _, name := range seed.files {
			path := filepath.Join(ws.RepoRoot, name)
			fp, ok := hashFile(path)
			if !ok {
				continue
			}
			info, err := os.Stat(path)
			if err != nil || info.IsDir() {
				continue
			}
			fileSignals = append(fileSignals, strings.Join([]string{
				hashString(name),
				fp,
				hashString(strconv.FormatInt(info.Size(), 10)),
				hashString(info.Mode().String()),
			}, "\x00"))
		}
		if len(fileSignals) == 0 {
			continue
		}
		var toolSignals []string
		for _, tool := range seed.tools {
			signal, ok := executableSignal(ws.RepoRoot, tool)
			if !ok {
				toolSignals = append(toolSignals, hashString(tool)+"\x00missing")
				continue
			}
			toolSignals = append(toolSignals, hashString(tool)+"\x00"+signal.PathHash+"\x00"+signal.FileHash)
		}
		fileSignalHash := hashString(strings.Join(fileSignals, "\n"))
		toolSignalHash := hashString(strings.Join(toolSignals, "\n"))
		commandSignalHash := hashString(strings.Join(seed.commands, "\n"))
		entry := PreparedEntry{
			PreparedID:        hashString("prepared:dependency:" + ws.RepoRoot + ":" + seed.ecosystem + ":" + fileSignalHash + ":" + toolSignalHash),
			Kind:              PreparedKindDependencyMetadata,
			OperatorFamily:    FamilyFileInspection,
			NormalizedCommand: "ecosystem dependency proof seed",
			InputFingerprints: map[string]string{
				"repo_root": hashString(ws.RepoRoot),
				"ecosystem": hashString(seed.ecosystem),
			},
			OutputFingerprints: map[string]string{
				"dependency_file_signal":   fileSignalHash,
				"dependency_tool_signal":   toolSignalHash,
				"candidate_command_signal": commandSignalHash,
				"dependency_file_count":    hashString(strconv.Itoa(len(fileSignals))),
				"dependency_tool_count":    hashString(strconv.Itoa(len(toolSignals))),
			},
			InvalidationEpoch: hashString(seed.ecosystem + ":" + fileSignalHash + ":" + toolSignalHash),
			EvidenceQuality:   ws.EvidenceQuality,
			ReplayEligible:    false,
			Privacy:           "manifest/lockfile/tool signals are hashed; dependency-list commands are not run",
			PreparedAt:        time.Now(),
			Notes:             []string{"seeds future deterministic dependency discovery proof; package manager commands are not replay eligible"},
		}
		ledger.UpsertPrepared(entry)
		count++
	}
	return count
}

func addPreparedSourceSymbolIndex(ws WorldState, ledger *ValidityLedger) int {
	if ws.RepoRoot == "" {
		return 0
	}
	const maxSymbolSignals = 8192
	var fileSignals []string
	var symbolSignals int
	for _, rel := range replayableInspectionPrewarmFiles(ws.RepoRoot, workspaceImagePrewarmFileLimit) {
		if symbolSignals >= maxSymbolSignals {
			break
		}
		if !isLikelySourceInspectionFile(rel) {
			continue
		}
		path := filepath.Join(ws.RepoRoot, filepath.FromSlash(rel))
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Size() > maxReplayableInspectionFileBytes {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		signals := sourceSymbolSignals(rel, content, maxSymbolSignals-symbolSignals)
		if len(signals) == 0 {
			continue
		}
		symbolSignals += len(signals)
		fileSignals = append(fileSignals, hashString(rel)+"\x00"+hashString(strings.Join(signals, "\n")))
	}
	if len(fileSignals) == 0 {
		return 0
	}
	signalHash := hashString(strings.Join(fileSignals, "\n"))
	entry := PreparedEntry{
		PreparedID:        hashString("prepared:source-symbol-index:" + ws.RepoRoot + ":" + ws.FileContentEpoch + ":" + signalHash),
		Kind:              PreparedKindSourceSymbolIndex,
		OperatorFamily:    FamilyFileInspection,
		NormalizedCommand: "workspace top-level source symbol/import index",
		InputFingerprints: map[string]string{
			"repo_root":          hashString(ws.RepoRoot),
			"file_content_epoch": ws.FileContentEpoch,
		},
		OutputFingerprints: map[string]string{
			"symbol_signal":     signalHash,
			"symbol_count_hash": hashString(strconv.Itoa(symbolSignals)),
			"source_file_count": hashString(strconv.Itoa(len(fileSignals))),
		},
		InvalidationEpoch: ws.FileContentEpoch,
		EvidenceQuality:   ws.EvidenceQuality,
		ReplayEligible:    false,
		Privacy:           "source lines and symbol names are hashed; no raw source content is persisted",
		PreparedAt:        time.Now(),
		Notes:             []string{"supports future deterministic source-map proof work; not replay eligible"},
	}
	ledger.UpsertPrepared(entry)
	return 1
}

func sourceSymbolSignals(rel string, content []byte, limit int) []string {
	if limit <= 0 {
		return nil
	}
	ext := strings.ToLower(filepath.Ext(rel))
	lines := strings.Split(string(content), "\n")
	signals := make([]string, 0, minInt(limit, 32))
	for _, raw := range lines {
		if len(signals) >= limit {
			break
		}
		line := strings.TrimSpace(raw)
		if line == "" || len(line) > 512 {
			continue
		}
		kind := sourceSymbolKind(ext, line)
		if kind == "" {
			continue
		}
		signals = append(signals, kind+"\x00"+hashString(line))
	}
	return signals
}

func sourceSymbolKind(ext, line string) string {
	switch ext {
	case ".go":
		if strings.HasPrefix(line, "func ") || strings.HasPrefix(line, "type ") || strings.HasPrefix(line, "const ") || strings.HasPrefix(line, "var ") || strings.HasPrefix(line, "import ") || line == "import (" {
			return "go"
		}
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs":
		if strings.HasPrefix(line, "import ") || strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "function ") || strings.HasPrefix(line, "class ") || strings.HasPrefix(line, "const ") || strings.HasPrefix(line, "let ") {
			return "js-ts"
		}
	case ".py":
		if strings.HasPrefix(line, "def ") || strings.HasPrefix(line, "async def ") || strings.HasPrefix(line, "class ") || strings.HasPrefix(line, "import ") || strings.HasPrefix(line, "from ") {
			return "python"
		}
	case ".rs":
		if strings.HasPrefix(line, "fn ") || strings.HasPrefix(line, "pub fn ") || strings.HasPrefix(line, "struct ") || strings.HasPrefix(line, "pub struct ") || strings.HasPrefix(line, "enum ") || strings.HasPrefix(line, "use ") {
			return "rust"
		}
	case ".java", ".kt", ".kts", ".cs", ".php", ".rb", ".swift":
		if strings.Contains(line, " class ") || strings.HasPrefix(line, "class ") || strings.HasPrefix(line, "interface ") || strings.HasPrefix(line, "import ") || strings.HasPrefix(line, "package ") || strings.HasPrefix(line, "def ") || strings.HasPrefix(line, "func ") {
			return "object-lang"
		}
	case ".c", ".h", ".cc", ".cpp", ".hpp":
		if strings.HasPrefix(line, "#include ") || strings.HasPrefix(line, "typedef ") || strings.HasPrefix(line, "struct ") || strings.Contains(line, ") {") {
			return "c-family"
		}
	}
	return ""
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func workspaceFileCount(root string) int {
	var count int
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() && (name == ".git" || name == ".squire") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	return count
}
