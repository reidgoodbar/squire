package kernel

import (
	"context"
	"sync"
	"time"
)

type Kernel struct {
	Store  *LedgerStore
	Oracle *RepoOracle
	Policy PolicyEngine

	mu              sync.Mutex
	ledger          *ValidityLedger
	ledgerLoaded    bool
	ledgerAvailable bool
	ledgerSignal    string
	ledgerDiag      []string
	saveScheduled   bool

	asyncForegroundObserve bool
	preparedLoaded         bool
	preparedSignal         string
	preparedAvailable      bool
	preparedDiag           []string
	preparedReplays        map[string][]preparedReplay
	preparedWarmFiles      map[string][]preparedWarmFile

	hotCacheMu               sync.Mutex
	hotCacheClient           *hotCacheClient
	hotCacheUnavailablePath  string
	hotCacheUnavailableUntil time.Time
	hotCacheMisses           map[string]time.Time
	hotSnapshotPath          string
	hotSnapshotSize          int64
	hotSnapshotModTime       int64
	hotSnapshotData          []byte
	hotSnapshotCleanup       func()
	hotReplayRing            hotReplayRing
}

func New(storeRoot string) *Kernel {
	return &Kernel{
		Store:  NewLedgerStore(storeRoot),
		Oracle: NewRepoOracle(),
		Policy: PolicyEngine{},
	}
}

func (k *Kernel) Run(ctx context.Context, sessionID, cwd string, argv []string) RunResult {
	var phases PhaseTimings
	if k.Oracle == nil {
		k.Oracle = NewRepoOracle()
	}
	classifyStart := time.Now()
	inv := NormalizeInvocation(cwd, argv)
	family := Classify(inv.PolicyArgv)
	phases.ClassifyMS = elapsedMS(classifyStart)

	if !requiresKernelWorld(inv.PolicyArgv) {
		nativeStart := time.Now()
		native := runNative(ctx, inv.OriginalCWD, inv.OriginalArgv)
		phases.NativeExecWaitMS += elapsedMS(nativeStart)
		mode := ModeNative
		if family == FamilyValidation || family == FamilyEditOrMutation || family == FamilyPackageSetup {
			mode = ModeNever
		}
		return RunResult{
			Stdout:     native.Stdout,
			Stderr:     native.Stderr,
			ExitCode:   native.ExitCode,
			Mode:       mode,
			Family:     family,
			NativeWall: native.Wall,
			Phases:     phases,
		}
	}
	if replay, ok := k.tryHotSnapshotReplay(inv, family, &phases); ok {
		return replay
	}
	if replay, ok := k.tryPreparedReplay(inv, family, &phases); ok {
		return replay
	}
	if replay, ok := k.tryDaemonReplay(ctx, inv, family, &phases); ok {
		return replay
	}
	if replay, ok := k.tryDiskPreparedReplay(inv, family, &phases); ok {
		return replay
	}
	if foregroundSpeculationDisabled(inv.PolicyArgv) {
		nativeStart := time.Now()
		native := runNative(ctx, inv.OriginalCWD, inv.OriginalArgv)
		phases.NativeExecWaitMS += elapsedMS(nativeStart)
		k.observeForegroundNativeAsync(sessionID, inv, native)
		return RunResult{
			Stdout:     native.Stdout,
			Stderr:     native.Stderr,
			ExitCode:   native.ExitCode,
			Mode:       ModeNative,
			Family:     family,
			NativeWall: native.Wall,
			Phases:     phases,
		}
	}

	var ws WorldState
	if IsFastPathAllowed(inv.PolicyArgv) {
		repoStart := time.Now()
		ws = k.Oracle.FastSnapshot(ctx, inv.PolicyCWD)
		phases.RepoRootLookupMS = elapsedMS(repoStart)
	} else if IsProofGatedReplayCandidate(inv.PolicyArgv) && isGitRepoState(inv.PolicyArgv) {
		worldStart := time.Now()
		ws = k.Oracle.Snapshot(ctx, inv.PolicyCWD)
		phases.WorldStateLookupMS = elapsedMS(worldStart)
	} else if IsProofGatedReplayCandidate(inv.PolicyArgv) {
		worldStart := time.Now()
		ws = k.Oracle.ShadowSnapshot(ctx, inv.PolicyCWD)
		phases.WorldStateLookupMS = elapsedMS(worldStart)
	} else {
		worldStart := time.Now()
		ws = k.Oracle.Snapshot(ctx, inv.PolicyCWD)
		phases.WorldStateLookupMS = elapsedMS(worldStart)
	}
	op := Operation{
		OperationID:       hashString(time.Now().String() + normalizeArgv(inv.PolicyArgv)),
		SessionID:         sessionID,
		OperatorFamily:    family,
		NormalizedCommand: normalizedFastPath(inv.PolicyArgv),
		Argv:              inv.PolicyArgv,
		CWD:               inv.PolicyCWD,
		RepoRoot:          ws.RepoRoot,
		Mode:              ModeNative,
		EvidenceQuality:   ws.EvidenceQuality,
	}
	var diagnostics []string
	ledger, ledgerAvailable, ledgerDiagnostics := k.loadLedger(&phases)
	diagnostics = append(diagnostics, ledgerDiagnostics...)

	epochStart := time.Now()
	decision := k.Policy.Decide(ctx, op, ws, ledger)
	op.Mode = decision.Mode
	key := operationKey(op, ws)
	fps := inputFingerprints(op, ws)
	epoch := invalidationEpoch(op, ws)
	phases.EpochCheckMS = elapsedMS(epochStart)

	if decision.Mode == ModeReplay && ledgerAvailable {
		proof := &ProofRecord{
			OperationKeyMatched:        true,
			InputFingerprintsMatched:   true,
			InvalidationEpochUnchanged: true,
			OperatorAllowlisted:        IsReplayAllowed(inv.PolicyArgv),
			PolicyAllowedReplay:        true,
			NativeFallbackAvailable:    true,
			OperationKey:               key,
		}
		lookupStart := time.Now()
		entry, ok := ledger.FindValid(key, fps, epoch)
		phases.LedgerLookupMS += elapsedMS(lookupStart)
		if !ok {
			proof.Reason = "valid ledger entry disappeared"
			return k.nativeFallback(ctx, op, key, ledger, diagnostics, proof, phases)
		}
		materializeStart := time.Now()
		stdout, stderr, err := k.materializeOutput(entry)
		phases.OutputMaterializeMS += elapsedMS(materializeStart)
		if err != nil {
			proof.Reason = "output record unavailable: " + err.Error()
			return k.nativeFallback(ctx, op, key, ledger, diagnostics, proof, phases)
		}
		proof.OutputAvailable = true
		proof.OutputExact = hashBytes(stdout) == entry.Observation.StdoutHash && hashBytes(stderr) == entry.Observation.StderrHash
		if !proof.OutputExact {
			proof.Reason = "output record hash mismatch"
			return k.nativeFallback(ctx, op, key, ledger, diagnostics, proof, phases)
		}
		eventStart := time.Now()
		k.mu.Lock()
		ledger.IncrementReplacement(key, int64(entry.Observation.NativeWallMS))
		k.mu.Unlock()
		k.scheduleLedgerSave()
		phases.EventAppendMS += elapsedMS(eventStart)
		return RunResult{
			Stdout:      stdout,
			Stderr:      stderr,
			ExitCode:    entry.Observation.ExitCode,
			Mode:        ModeReplay,
			Family:      op.OperatorFamily,
			Observation: entry.Observation,
			Proof:       proof,
			Diagnostics: diagnostics,
			Phases:      phases,
		}
	}

	nativeStart := time.Now()
	native := runNative(ctx, inv.OriginalCWD, inv.OriginalArgv)
	phases.NativeExecWaitMS += elapsedMS(nativeStart)
	result := RunResult{
		Stdout:      native.Stdout,
		Stderr:      native.Stderr,
		ExitCode:    native.ExitCode,
		Mode:        decision.Mode,
		Family:      op.OperatorFamily,
		Diagnostics: diagnostics,
		NativeWall:  native.Wall,
		Phases:      phases,
	}
	if decision.Mode == ModeNever {
		result.Mode = ModeNever
		return result
	}
	if IsFastPathAllowed(inv.PolicyArgv) && ledgerAvailable && native.ExitCode == 0 {
		if err := k.precomputeFastPathOutputs(ctx, inv.PolicyCWD, sessionID, inv.PolicyArgv, native, ws, ledger, &result.Phases); err != nil {
			result.Diagnostics = append(result.Diagnostics, "validity ledger save failed: "+err.Error())
		}
		if entry, ok := ledger.FindValid(key, fps, epoch); ok {
			result.Observation = entry.Observation
		}
	}
	return result
}

func (k *Kernel) observeForegroundNativeAsync(sessionID string, inv CommandInvocation, native NativeResult) {
	if k.Store == nil || native.ExitCode != 0 || !IsReplayAllowed(inv.PolicyArgv) {
		return
	}
	k.mu.Lock()
	enabled := k.asyncForegroundObserve
	k.mu.Unlock()
	if !enabled {
		return
	}
	if normalizeArgv(inv.OriginalArgv) != normalizeArgv(inv.PolicyArgv) || absPath(inv.OriginalCWD) != inv.PolicyCWD {
		return
	}
	observed := NativeResult{
		Stdout:   append([]byte(nil), native.Stdout...),
		Stderr:   append([]byte(nil), native.Stderr...),
		ExitCode: native.ExitCode,
		Wall:     native.Wall,
		Err:      native.Err,
	}
	argv := append([]string(nil), inv.PolicyArgv...)
	cwd := inv.PolicyCWD
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var ws WorldState
		if IsFastPathAllowed(argv) {
			ws = k.Oracle.FastSnapshot(ctx, cwd)
		} else {
			ws = k.Oracle.ShadowSnapshot(ctx, cwd)
		}
		if !ws.OracleAvailable && !IsProofGatedReplayCandidate(argv) {
			return
		}
		var phases PhaseTimings
		ledger, available, _ := k.loadLedger(&phases)
		if !available || ledger == nil {
			return
		}
		if IsFastPathAllowed(argv) {
			_ = k.precomputeFastPathOutputs(ctx, cwd, sessionID, argv, observed, ws, ledger, &phases)
			return
		}
		if !IsProofGatedReplayCandidate(argv) || !proofGatedCandidateUsable(Operation{Argv: argv, CWD: cwd, RepoRoot: ws.RepoRoot}, ws) {
			return
		}
		if err := k.storeReplayableObservation(argv, sessionID, cwd, observed, ws, ledger, &phases, ModeNative); err != nil {
			return
		}
		op := Operation{
			OperationID:       hashString("foreground-observe" + normalizeArgv(argv)),
			SessionID:         sessionID,
			OperatorFamily:    Classify(argv),
			NormalizedCommand: normalizedFastPath(argv),
			Argv:              argv,
			CWD:               cwd,
			RepoRoot:          ws.RepoRoot,
			Mode:              ModeNative,
			EvidenceQuality:   ws.EvidenceQuality,
		}
		key := operationKey(op, ws)
		fps := inputFingerprints(op, ws)
		epoch := invalidationEpoch(op, ws)
		hotFPS, hotEpoch, hotOK := preparedHotProof(cwd, argv)
		if !hotOK {
			return
		}
		k.mu.Lock()
		ledger.RecordWarmObservation(key, op.OperatorFamily, fps, epoch)
		entry, ok := ledger.FindValid(key, fps, epoch)
		k.mu.Unlock()
		if !ok || entry.Observation.OutputRef == "" {
			return
		}
		prepared := PreparedEntry{
			PreparedID:           hashString("prepared:foreground:" + key + ":" + epoch),
			Kind:                 PreparedKindProofGatedOutput,
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
			Privacy:              proofGatedOutputPrivacy(argv),
			PreparedAt:           time.Now(),
			Notes:                []string{"foreground native observation prepared asynchronously; native fallback still available"},
		}
		k.mu.Lock()
		ledger.UpsertPrepared(prepared)
		k.mu.Unlock()
		if isReplayableFileInspection(argv) {
			_ = k.prepareWarmFileFromCommand(cwd, argv, ws, ledger, &phases, "foreground observed file bytes prepared for arbitrary bounded sed/cat replay; native fallback still available")
		}
		k.hydratePreparedReplayCache(ledger, k.Store.Signal(), &phases)
		_ = k.saveLedgerSync(ledger, &phases)
	}()
}

func requiresKernelWorld(argv []string) bool {
	return IsReplayAllowed(argv)
}

func foregroundSpeculationDisabled(argv []string) bool {
	return IsReplayAllowed(argv)
}

func (k *Kernel) nativeFallback(ctx context.Context, op Operation, key string, ledger *ValidityLedger, diagnostics []string, proof *ProofRecord, phases PhaseTimings) RunResult {
	fallbackStart := time.Now()
	phases.FallbackDecisionMS += elapsedMS(fallbackStart)
	nativeStart := time.Now()
	native := runNative(ctx, op.CWD, op.Argv)
	phases.NativeExecWaitMS += elapsedMS(nativeStart)
	if ledger != nil {
		k.mu.Lock()
		ledger.IncrementFallback(key)
		k.mu.Unlock()
		k.scheduleLedgerSave()
	}
	diagnostics = append(diagnostics, "fault-open native fallback: "+proof.Reason)
	return RunResult{
		Stdout:      native.Stdout,
		Stderr:      native.Stderr,
		ExitCode:    native.ExitCode,
		Mode:        ModeNative,
		Family:      op.OperatorFamily,
		Proof:       proof,
		Diagnostics: diagnostics,
		NativeWall:  native.Wall,
		Phases:      phases,
	}
}

func (k *Kernel) loadLedger(phases *PhaseTimings) (*ValidityLedger, bool, []string) {
	lockStart := time.Now()
	k.mu.Lock()
	phases.LockWaitMS += elapsedMS(lockStart)
	defer k.mu.Unlock()
	signal := ""
	if k.Store != nil {
		signal = k.Store.Signal()
	}
	if k.ledgerLoaded && (signal == "" || signal == k.ledgerSignal) {
		return k.ledger, k.ledgerAvailable, append([]string(nil), k.ledgerDiag...)
	}
	k.ledgerLoaded = true
	if k.Store == nil {
		k.ledgerDiag = []string{"validity ledger unavailable"}
		return nil, false, append([]string(nil), k.ledgerDiag...)
	}
	lookupStart := time.Now()
	if err := k.Store.Init(); err != nil {
		k.ledgerDiag = []string{"validity ledger unavailable: " + err.Error()}
		phases.LedgerLookupMS += elapsedMS(lookupStart)
		return nil, false, append([]string(nil), k.ledgerDiag...)
	}
	loaded, err := k.Store.Load()
	phases.LedgerLookupMS += elapsedMS(lookupStart)
	if err != nil {
		k.ledgerDiag = []string{"validity ledger unavailable: " + err.Error()}
		return nil, false, append([]string(nil), k.ledgerDiag...)
	}
	k.ledger = loaded
	k.ledgerAvailable = true
	k.ledgerSignal = k.Store.Signal()
	return k.ledger, true, nil
}

func (k *Kernel) materializeOutput(entry *LedgerEntry) ([]byte, []byte, error) {
	k.mu.Lock()
	if entry.StdoutBytes != nil || entry.StderrBytes != nil {
		stdout := append([]byte(nil), entry.StdoutBytes...)
		stderr := append([]byte(nil), entry.StderrBytes...)
		k.mu.Unlock()
		return stdout, stderr, nil
	}
	k.mu.Unlock()
	stdout, stderr, err := k.Store.LoadOutput(entry.Observation.OutputRef)
	if err != nil {
		return nil, nil, err
	}
	k.mu.Lock()
	entry.StdoutBytes = append([]byte(nil), stdout...)
	entry.StderrBytes = append([]byte(nil), stderr...)
	k.mu.Unlock()
	return stdout, stderr, nil
}

func (k *Kernel) precomputeFastPathOutputs(ctx context.Context, cwd, sessionID string, observedArgv []string, observed NativeResult, ws WorldState, ledger *ValidityLedger, phases *PhaseTimings) error {
	if !ws.OracleAvailable || k.Store == nil {
		return nil
	}
	for _, argv := range enabledFastPathArgv() {
		res := observed
		if normalizeArgv(argv) != normalizeArgv(observedArgv) {
			start := time.Now()
			res = runNative(ctx, cwd, argv)
			phases.NativeExecWaitMS += elapsedMS(start)
		}
		if res.ExitCode != 0 {
			continue
		}
		if err := k.storeReplayableObservation(argv, sessionID, cwd, res, ws, ledger, phases, ModeNative); err != nil {
			return err
		}
	}
	return k.saveLedgerSync(ledger, phases)
}

func (k *Kernel) storeReplayableObservation(argv []string, sessionID, cwd string, observed NativeResult, ws WorldState, ledger *ValidityLedger, phases *PhaseTimings, decision Mode) error {
	if k.Store == nil || ledger == nil {
		return nil
	}
	if !ws.OracleAvailable && !IsProofGatedReplayCandidate(argv) {
		return nil
	}
	op := Operation{
		OperationID:       hashString(time.Now().String() + normalizeArgv(argv)),
		SessionID:         sessionID,
		OperatorFamily:    Classify(argv),
		NormalizedCommand: normalizedFastPath(argv),
		Argv:              argv,
		CWD:               cwd,
		RepoRoot:          ws.RepoRoot,
		Mode:              decision,
		EvidenceQuality:   ws.EvidenceQuality,
	}
	key := operationKey(op, ws)
	fps := inputFingerprints(op, ws)
	epoch := invalidationEpoch(op, ws)
	obs := Observation{
		OperationID:  op.OperationID,
		StdoutHash:   hashBytes(observed.Stdout),
		StderrHash:   hashBytes(observed.Stderr),
		StdoutSize:   len(observed.Stdout),
		StderrSize:   len(observed.Stderr),
		ExitCode:     observed.ExitCode,
		NativeWallMS: observed.Wall.Milliseconds(),
		Timestamp:    time.Now(),
	}
	writeStart := time.Now()
	ref, err := k.Store.StoreOutput(key, observed.Stdout, observed.Stderr)
	phases.DBOrFileWriteMS += elapsedMS(writeStart)
	if err != nil {
		return err
	}
	obs.OutputRef = ref
	k.mu.Lock()
	ledger.UpsertObservation(LedgerEntry{
		OperationKey:       key,
		OperatorFamily:     op.OperatorFamily,
		InputFingerprints:  fps,
		OutputFingerprints: map[string]string{"stdout": obs.StdoutHash, "stderr": obs.StderrHash},
		InvalidationEpoch:  epoch,
		LastDecision:       decision,
		LastValidatedAt:    time.Now(),
		Observation:        obs,
		StdoutBytes:        append([]byte(nil), observed.Stdout...),
		StderrBytes:        append([]byte(nil), observed.Stderr...),
	})
	k.mu.Unlock()
	return nil
}

func (k *Kernel) saveLedgerSync(ledger *ValidityLedger, phases *PhaseTimings) error {
	if k.Store == nil || ledger == nil {
		return nil
	}
	k.mu.Lock()
	snapshot := ledger.CloneForSave()
	k.mu.Unlock()
	start := time.Now()
	err := k.Store.Save(snapshot)
	phases.DBOrFileWriteMS += elapsedMS(start)
	if err == nil {
		k.mu.Lock()
		k.ledgerSignal = k.Store.Signal()
		k.mu.Unlock()
	}
	return err
}

func (k *Kernel) scheduleLedgerSave() {
	if k.Store == nil {
		return
	}
	k.mu.Lock()
	if k.saveScheduled {
		k.mu.Unlock()
		return
	}
	k.saveScheduled = true
	k.mu.Unlock()
	go func() {
		time.Sleep(25 * time.Millisecond)
		k.flushScheduledLedgerSave()
	}()
}

func (k *Kernel) flushLedgerNow() {
	if k.Store == nil {
		return
	}
	k.mu.Lock()
	if k.ledger == nil {
		k.saveScheduled = false
		k.mu.Unlock()
		return
	}
	ledger := k.ledger.CloneForSave()
	k.saveScheduled = false
	k.mu.Unlock()
	if err := k.Store.Save(ledger); err == nil {
		k.mu.Lock()
		k.ledgerSignal = k.Store.Signal()
		k.mu.Unlock()
	}
}

func (k *Kernel) Flush() {
	k.flushLedgerNow()
}

func (k *Kernel) flushScheduledLedgerSave() {
	if k.Store == nil {
		return
	}
	k.mu.Lock()
	if !k.saveScheduled || k.ledger == nil {
		k.mu.Unlock()
		return
	}
	ledger := k.ledger.CloneForSave()
	k.saveScheduled = false
	k.mu.Unlock()
	if err := k.Store.Save(ledger); err == nil {
		k.mu.Lock()
		k.ledgerSignal = k.Store.Signal()
		k.mu.Unlock()
	}
}

func elapsedMS(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}
