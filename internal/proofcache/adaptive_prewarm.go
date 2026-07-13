package proofcache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const adaptiveSedPrewarmMaxCandidates = 4

func HasAdaptivePrewarmCandidates(argv []string) bool {
	argv = normalizeArgvForPolicy(argv)
	return len(adaptiveSedPrewarmCandidates(argv)) > 0 || adaptiveManifestLikely(argv)
}

func (k *Engine) PrewarmAdjacent(ctx context.Context, cwd, sessionID string, argv []string) (int, error) {
	if k.Store == nil {
		return 0, nil
	}
	if k.Oracle == nil {
		k.Oracle = NewRepoOracle()
	}
	inv := NormalizeInvocation(cwd, argv)
	candidates := adaptivePrewarmCandidates(inv.PolicyCWD, inv.PolicyArgv)
	if len(candidates) == 0 {
		return 0, nil
	}
	if err := k.Store.Init(); err != nil {
		return 0, err
	}
	ledger, err := k.Store.Load()
	if err != nil {
		return 0, err
	}
	ws := k.Oracle.ShadowSnapshot(ctx, inv.PolicyCWD)
	var phases PhaseTimings
	var count int
	for _, candidate := range candidates {
		if k.prewarmProofGatedCandidate(ctx, inv.PolicyCWD, sessionID, candidate, ws, ledger, &phases, "adaptive adjacent sed window; native fallback still available") {
			count++
		}
	}
	if count == 0 {
		return 0, nil
	}
	if err := k.Store.Save(ledger); err != nil {
		return 0, err
	}
	k.hydratePreparedReplayCache(ledger, k.Store.Signal(), &phases)
	k.mu.Lock()
	k.ledger = ledger
	k.ledgerLoaded = true
	k.ledgerAvailable = true
	k.ledgerDiag = nil
	k.ledgerSignal = k.Store.Signal()
	k.mu.Unlock()
	return count, nil
}

func (k *Engine) prewarmProofGatedCandidate(ctx context.Context, cwd, sessionID string, argv []string, ws WorldState, ledger *ValidityLedger, phases *PhaseTimings, note string) bool {
	if !IsProofGatedReplayCandidate(argv) || proofGatedWarmBlocked(cwd, argv) {
		return false
	}
	beforeFPS, beforeEpoch, beforeOK := preparedHotProof(cwd, argv)
	if !beforeOK {
		return false
	}
	native := runProofGatedNative(ctx, cwd, argv)
	if native.ExitCode != 0 || proofGatedWarmBlocked(cwd, argv) {
		return false
	}
	afterFPS, afterEpoch, afterOK := preparedHotProof(cwd, argv)
	if !afterOK || beforeEpoch != afterEpoch || !mapsEqual(beforeFPS, afterFPS) {
		return false
	}
	op := Operation{
		OperationID:       hashString(sessionID + ":prewarm:" + normalizeArgv(argv) + ":" + time.Now().String()),
		SessionID:         sessionID,
		OperatorFamily:    Classify(argv),
		NormalizedCommand: normalizedFastPath(argv),
		Argv:              argv,
		CWD:               cwd,
		RepoRoot:          ws.RepoRoot,
		Mode:              ModeNative,
		EvidenceQuality:   ws.EvidenceQuality,
	}
	if !proofGatedCandidateUsable(op, ws) {
		return false
	}
	key := operationKey(op, ws)
	fps := inputFingerprints(op, ws)
	epoch := invalidationEpoch(op, ws)
	warmFilePrepared := false
	if isReplayableFileInspection(argv) {
		warmFilePrepared = k.prepareWarmFileFromCommand(cwd, argv, ws, ledger, phases, "adaptive file bytes prepared for arbitrary bounded sed/cat replay; native fallback still available")
	}
	if err := k.storeReplayableObservation(argv, sessionID, cwd, native, ws, ledger, phases, ModeNative); err != nil {
		return warmFilePrepared
	}
	k.mu.Lock()
	ledger.RecordWarmObservation(key, op.OperatorFamily, fps, epoch)
	entry, ok := ledger.FindValid(key, fps, epoch)
	k.mu.Unlock()
	if !ok || entry.Observation.OutputRef == "" {
		return false
	}
	ledger.UpsertPrepared(PreparedEntry{
		PreparedID:           hashString("prepared:adaptive:" + key + ":" + epoch),
		Kind:                 PreparedKindProofGatedOutput,
		OperatorFamily:       op.OperatorFamily,
		NormalizedCommand:    op.NormalizedCommand,
		InputFingerprints:    fps,
		HotFingerprints:      afterFPS,
		OutputFingerprints:   entry.OutputFingerprints,
		InvalidationEpoch:    epoch,
		HotInvalidationEpoch: afterEpoch,
		EvidenceQuality:      ws.EvidenceQuality,
		ReplayEligible:       true,
		OutputRef:            entry.Observation.OutputRef,
		Privacy:              proofGatedOutputPrivacy(argv),
		PreparedAt:           time.Now(),
		Notes:                []string{note},
	})
	return true
}

func adaptivePrewarmCandidates(cwd string, argv []string) [][]string {
	var candidates [][]string
	candidates = append(candidates, adaptiveSedPrewarmCandidates(argv)...)
	candidates = append(candidates, adaptiveManifestPrewarmCandidates(cwd, argv)...)
	return dedupeAdaptiveCandidates(candidates)
}

func adaptiveSedPrewarmCandidates(argv []string) [][]string {
	argv = normalizeArgvForPolicy(argv)
	if !isBoundedSedPrint(argv) {
		return nil
	}
	start, end, ok := parseSedPrintRange(argv[2])
	if !ok {
		return nil
	}
	path := argv[3]
	width := end - start + 1
	if width < 80 {
		width = 80
	}
	if width > 300 {
		width = 300
	}
	type window struct {
		start int
		end   int
	}
	overlapStart := end - 40
	if overlapStart < 1 {
		overlapStart = 1
	}
	windows := []window{
		{start: end + 1, end: end + width},
		{start: overlapStart, end: overlapStart + width},
		{start: end + 1, end: end + 300},
		{start: overlapStart, end: overlapStart + 300},
	}
	if start > 1 {
		prevEnd := start - 1
		prevStart := prevEnd - width + 1
		if prevStart < 1 {
			prevStart = 1
		}
		windows = append(windows, window{start: prevStart, end: prevEnd})
	}
	seen := map[string]bool{}
	var out [][]string
	for _, w := range windows {
		if len(out) >= adaptiveSedPrewarmMaxCandidates || w.start < 1 || w.end < w.start || w.end > 10000 || w.end-w.start > 500 {
			continue
		}
		expr := fmt.Sprintf("%d,%dp", w.start, w.end)
		candidate := []string{"sed", "-n", expr, path}
		key := normalizeArgv(candidate)
		if seen[key] || !isBoundedSedPrint(candidate) {
			continue
		}
		seen[key] = true
		out = append(out, candidate)
	}
	return out
}

func adaptiveManifestLikely(argv []string) bool {
	path := replayableInspectionArgPath(argv)
	return path != "" && isWellKnownManifestName(filepath.Base(filepath.Clean(path)))
}

func adaptiveManifestPrewarmCandidates(cwd string, argv []string) [][]string {
	argv = normalizeArgvForPolicy(argv)
	path := replayableInspectionArgPath(argv)
	if path == "" {
		return nil
	}
	base := filepath.Base(filepath.Clean(path))
	if !isWellKnownManifestName(base) {
		return nil
	}
	addExisting := func(out *[][]string, rels ...string) {
		for _, rel := range rels {
			if adaptiveRelativeFileExists(cwd, rel) {
				*out = append(*out, []string{"cat", rel})
			}
		}
	}
	var out [][]string
	switch base {
	case "package.json", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "tsconfig.json":
		addExisting(&out, "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "tsconfig.json")
		out = append(out,
			[]string{"node", "--version"},
			[]string{"npm", "--version"},
			[]string{"command", "-v", "node"},
			[]string{"command", "-v", "npm"},
		)
	case "go.mod", "go.sum", "go.work", "go.work.sum":
		addExisting(&out, "go.sum", "go.work", "go.work.sum")
		out = append(out,
			[]string{"go", "version"},
			[]string{"command", "-v", "go"},
		)
	case "pyproject.toml", "poetry.lock", "requirements.txt", "requirements-dev.txt", "setup.cfg", "tox.ini":
		addExisting(&out, "poetry.lock", "requirements.txt", "requirements-dev.txt", "setup.cfg", "tox.ini")
		out = append(out,
			[]string{"python3", "--version"},
			[]string{"pip3", "--version"},
			[]string{"command", "-v", "python3"},
			[]string{"command", "-v", "pip3"},
		)
	case "Cargo.toml", "Cargo.lock", "rust-toolchain", "rust-toolchain.toml":
		addExisting(&out, "Cargo.lock", "rust-toolchain", "rust-toolchain.toml")
		out = append(out,
			[]string{"cargo", "--version"},
			[]string{"rustc", "--version"},
			[]string{"command", "-v", "cargo"},
			[]string{"command", "-v", "rustc"},
		)
	case "Makefile", "makefile":
		out = append(out, []string{"command", "-v", "make"})
	}
	return out
}

func adaptiveRelativeFileExists(cwd, rel string) bool {
	clean := filepath.Clean(rel)
	if !safeRelativeInspectionPath(clean) || !isReplayableInspectionName(filepath.Base(clean)) {
		return false
	}
	info, err := os.Stat(filepath.Join(cwd, clean))
	return err == nil && info.Mode().IsRegular() && info.Size() <= maxReplayableInspectionFileBytes
}

func dedupeAdaptiveCandidates(candidates [][]string) [][]string {
	seen := map[string]bool{}
	var out [][]string
	for _, candidate := range candidates {
		key := normalizeArgv(candidate)
		if seen[key] || !isHotPreparedReplayCandidate(candidate) {
			continue
		}
		seen[key] = true
		out = append(out, candidate)
	}
	return out
}

func parseSedPrintRange(expr string) (int, int, bool) {
	if !strings.HasSuffix(expr, "p") {
		return 0, 0, false
	}
	body := strings.TrimSuffix(expr, "p")
	parts := strings.Split(body, ",")
	if len(parts) > 2 {
		return 0, 0, false
	}
	start, ok := parsePositiveSmallLine(parts[0])
	if !ok {
		return 0, 0, false
	}
	end := start
	if len(parts) == 2 {
		var endOK bool
		end, endOK = parsePositiveSmallLine(parts[1])
		if !endOK {
			return 0, 0, false
		}
	}
	if start > end {
		return 0, 0, false
	}
	return start, end, true
}
