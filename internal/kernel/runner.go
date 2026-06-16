package kernel

import (
	"context"
	"time"
)

type Kernel struct {
	Store  *LedgerStore
	Oracle *RepoOracle
	Policy PolicyEngine
}

func New(storeRoot string) *Kernel {
	return &Kernel{
		Store:  NewLedgerStore(storeRoot),
		Oracle: NewRepoOracle(),
		Policy: PolicyEngine{},
	}
}

func (k *Kernel) Run(ctx context.Context, sessionID, cwd string, argv []string) RunResult {
	if k.Oracle == nil {
		k.Oracle = NewRepoOracle()
	}
	family := Classify(argv)
	var ws WorldState
	if IsFastPathAllowed(argv) {
		ws = k.Oracle.FastSnapshot(ctx, cwd)
	} else {
		ws = k.Oracle.Snapshot(ctx, cwd)
	}
	op := Operation{
		OperationID:       hashString(time.Now().String() + normalizeArgv(argv)),
		SessionID:         sessionID,
		OperatorFamily:    family,
		NormalizedCommand: normalizedFastPath(argv),
		Argv:              argv,
		CWD:               cwd,
		RepoRoot:          ws.RepoRoot,
		Mode:              ModeNative,
		EvidenceQuality:   ws.EvidenceQuality,
	}
	var diagnostics []string
	var ledger *ValidityLedger
	ledgerAvailable := false
	if k.Store != nil {
		if err := k.Store.Init(); err != nil {
			diagnostics = append(diagnostics, "validity ledger unavailable: "+err.Error())
		} else if loaded, err := k.Store.Load(); err != nil {
			diagnostics = append(diagnostics, "validity ledger unavailable: "+err.Error())
		} else {
			ledger = loaded
			ledgerAvailable = true
		}
	} else {
		diagnostics = append(diagnostics, "validity ledger unavailable")
	}

	decision := k.Policy.Decide(ctx, op, ws, ledger)
	op.Mode = decision.Mode
	key := operationKey(op, ws)
	fps := inputFingerprints(op, ws)
	epoch := invalidationEpoch(op, ws)

	if decision.Mode == ModeReplay && ledgerAvailable {
		proof := &ProofRecord{
			OperationKeyMatched:        true,
			InputFingerprintsMatched:   true,
			InvalidationEpochUnchanged: true,
			OperatorAllowlisted:        IsFastPathAllowed(argv),
			PolicyAllowedReplay:        true,
			NativeFallbackAvailable:    true,
			OperationKey:               key,
		}
		entry, ok := ledger.FindValid(key, fps, epoch)
		if !ok {
			proof.Reason = "valid ledger entry disappeared"
			return k.nativeFallback(ctx, op, ws, key, fps, epoch, ledger, diagnostics, proof)
		}
		stdout, stderr, err := k.Store.LoadOutput(entry.Observation.OutputRef)
		if err != nil {
			proof.Reason = "output record unavailable: " + err.Error()
			return k.nativeFallback(ctx, op, ws, key, fps, epoch, ledger, diagnostics, proof)
		}
		proof.OutputAvailable = true
		proof.OutputExact = hashBytes(stdout) == entry.Observation.StdoutHash && hashBytes(stderr) == entry.Observation.StderrHash
		if !proof.OutputExact {
			proof.Reason = "output record hash mismatch"
			return k.nativeFallback(ctx, op, ws, key, fps, epoch, ledger, diagnostics, proof)
		}
		ledger.IncrementReplacement(key, int64(entry.Observation.NativeWallMS))
		_ = k.Store.Save(ledger)
		return RunResult{
			Stdout:      stdout,
			Stderr:      stderr,
			ExitCode:    entry.Observation.ExitCode,
			Mode:        ModeReplay,
			Family:      op.OperatorFamily,
			Observation: entry.Observation,
			Proof:       proof,
			Diagnostics: diagnostics,
		}
	}

	native := runNative(ctx, cwd, argv)
	result := RunResult{
		Stdout:      native.Stdout,
		Stderr:      native.Stderr,
		ExitCode:    native.ExitCode,
		Mode:        decision.Mode,
		Family:      op.OperatorFamily,
		Diagnostics: diagnostics,
		NativeWall:  native.Wall,
	}
	if decision.Mode == ModeNever {
		result.Mode = ModeNever
		return result
	}
	if decision.Mode == ModeShadow && ledgerAvailable {
		prediction := shadowPredict(ctx, cwd, argv)
		matched := prediction.ExitCode == native.ExitCode && string(prediction.Stdout) == string(native.Stdout) && string(prediction.Stderr) == string(native.Stderr)
		example := ""
		if !matched {
			example = "native=" + hashBytes(native.Stdout) + "/" + hashBytes(native.Stderr) + " predicted=" + hashBytes(prediction.Stdout) + "/" + hashBytes(prediction.Stderr)
		}
		ledger.RecordShadow(key, op.OperatorFamily, fps, epoch, matched, example)
		if err := k.Store.Save(ledger); err != nil {
			result.Diagnostics = append(result.Diagnostics, "shadow ledger save failed: "+err.Error())
		}
		return result
	}
	if IsFastPathAllowed(argv) && ledgerAvailable && native.ExitCode == 0 {
		obs := Observation{
			OperationID:  op.OperationID,
			StdoutHash:   hashBytes(native.Stdout),
			StderrHash:   hashBytes(native.Stderr),
			StdoutSize:   len(native.Stdout),
			StderrSize:   len(native.Stderr),
			ExitCode:     native.ExitCode,
			NativeWallMS: native.Wall.Milliseconds(),
			Timestamp:    time.Now(),
		}
		if ref, err := k.Store.StoreOutput(key, native.Stdout, native.Stderr); err == nil {
			obs.OutputRef = ref
		} else {
			result.Diagnostics = append(result.Diagnostics, "fast-path output not stored: "+err.Error())
		}
		entry := LedgerEntry{
			OperationKey:       key,
			OperatorFamily:     op.OperatorFamily,
			InputFingerprints:  fps,
			OutputFingerprints: map[string]string{"stdout": obs.StdoutHash, "stderr": obs.StderrHash},
			InvalidationEpoch:  epoch,
			LastDecision:       ModeNative,
			LastValidatedAt:    time.Now(),
			Observation:        obs,
		}
		ledger.UpsertObservation(entry)
		if err := k.Store.Save(ledger); err != nil {
			result.Diagnostics = append(result.Diagnostics, "validity ledger save failed: "+err.Error())
		}
		result.Observation = obs
	}
	return result
}

func (k *Kernel) nativeFallback(ctx context.Context, op Operation, ws WorldState, key string, fps map[string]string, epoch string, ledger *ValidityLedger, diagnostics []string, proof *ProofRecord) RunResult {
	native := runNative(ctx, op.CWD, op.Argv)
	if ledger != nil {
		ledger.IncrementFallback(key)
		_ = k.Store.Save(ledger)
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
	}
}

func shadowPredict(ctx context.Context, cwd string, argv []string) NativeResult {
	// Shadow v1 compares exact output while never replacing the native command.
	// The prediction path is intentionally read-only and records exactness before
	// any future replacement is considered.
	return runNative(ctx, cwd, argv)
}
