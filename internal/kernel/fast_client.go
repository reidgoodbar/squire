package kernel

import (
	"context"
	"path/filepath"
	"time"
)

func FastStoreRoot(cwd string) (string, bool) {
	_, storeRoot, ok := FastWorkspace(cwd)
	return storeRoot, ok
}

// FastWorkspace resolves the canonical repository identity without spawning
// Git. It is the shared workspace boundary used by agent runtime adapters.
func FastWorkspace(cwd string) (repoRoot, storeRoot string, ok bool) {
	repoRoot, gitDirAbs, ok := discoverGitDir(cwd)
	if !ok || gitDirAbs == "" {
		return "", "", false
	}
	return repoRoot, hotStoreRoot(gitDirAbs), true
}

func hotStoreRoot(gitDirAbs string) string {
	return filepath.Join(gitDirAbs, "squire", "kernel")
}

func FastHotClientReplay(ctx context.Context, sessionID, cwd, storeRoot string, argv []string) (*RunResult, bool) {
	_ = sessionID
	select {
	case <-ctx.Done():
		return nil, false
	default:
	}
	if storeRoot == "" || len(argv) == 0 {
		return nil, false
	}
	inv := NormalizeInvocation(cwd, argv)
	family := Classify(inv.PolicyArgv)
	if !isHotPreparedReplayCandidate(inv.PolicyArgv) {
		return nil, false
	}
	var phases PhaseTimings
	start := time.Now()
	resp, ok := readHotSnapshotResponse(hotCacheSnapshotPath(storeRoot), inv)
	phases.OutputMaterializeMS += elapsedMS(start)
	if !ok {
		return nil, false
	}
	return &RunResult{
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
		ExitCode: resp.ExitCode,
		Mode:     ModeReplay,
		Family:   family,
		Observation: Observation{
			StdoutHash:   resp.StdoutHash,
			StderrHash:   resp.StderrHash,
			StdoutSize:   len(resp.Stdout),
			StderrSize:   len(resp.Stderr),
			ExitCode:     resp.ExitCode,
			NativeWallMS: resp.NativeWallMS,
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
			OperationKey:               "cli-mmap-hot-snapshot",
		},
		Phases: phases,
	}, true
}

func (k *Kernel) FastReplay(ctx context.Context, sessionID, cwd string, argv []string) (*RunResult, bool) {
	inv := NormalizeInvocation(cwd, argv)
	return k.FastReplayInvocation(ctx, sessionID, inv)
}

func (k *Kernel) FastReplayInvocation(ctx context.Context, sessionID string, inv CommandInvocation) (*RunResult, bool) {
	_ = sessionID
	select {
	case <-ctx.Done():
		return nil, false
	default:
	}
	if k == nil || len(inv.PolicyArgv) == 0 {
		return nil, false
	}
	family := Classify(inv.PolicyArgv)
	var phases PhaseTimings
	if replay, ok := k.tryHotSnapshotReplay(inv, family, &phases); ok {
		return &replay, true
	}
	return nil, false
}
