package kernel

import "context"

type PolicyDecision struct {
	Mode   Mode
	Reason string
}

type PolicyEngine struct{}

func (p PolicyEngine) Decide(ctx context.Context, op Operation, ws WorldState, ledger *ValidityLedger) PolicyDecision {
	switch op.OperatorFamily {
	case FamilyValidation, FamilyEditOrMutation, FamilyPackageSetup:
		return PolicyDecision{Mode: ModeNever, Reason: "operator family is never replayed"}
	}
	if !ws.OracleAvailable && (op.OperatorFamily == FamilyLocalRepoMetadata || op.OperatorFamily == FamilyRepoState) {
		return PolicyDecision{Mode: ModeNative, Reason: "repo oracle unavailable"}
	}
	if IsShadowCandidate(op.Argv) {
		if ledger != nil && ledger.HasShadowMismatch(operationKey(op, ws)) {
			return PolicyDecision{Mode: ModeNative, Reason: "shadow mismatch history"}
		}
		return PolicyDecision{Mode: ModeShadow, Reason: "shadow candidate"}
	}
	if IsFastPathAllowed(op.Argv) {
		if ledger == nil {
			return PolicyDecision{Mode: ModeNative, Reason: "validity ledger unavailable"}
		}
		key := operationKey(op, ws)
		fps := inputFingerprints(op, ws)
		epoch := invalidationEpoch(op, ws)
		if entry, ok := ledger.FindValid(key, fps, epoch); ok && entry.Observation.OutputRef != "" {
			return PolicyDecision{Mode: ModeReplay, Reason: "valid exact observation available"}
		}
		return PolicyDecision{Mode: ModeNative, Reason: "no valid exact observation"}
	}
	return PolicyDecision{Mode: ModeNative, Reason: "not replay allowlisted"}
}

func EnabledFastPaths() []string {
	return []string{
		"git rev-parse HEAD",
		"git rev-parse --git-dir",
		"git rev-parse --abbrev-ref HEAD",
	}
}

func ShadowCandidates() []string {
	return []string{
		"git status --short",
		"git status --porcelain",
		"git ls-files",
		"rg --files",
	}
}

func NeverReplayPolicy() []string {
	return []string{
		"validation/build/test commands",
		"edits",
		"git add",
		"git commit",
		"git checkout",
		"git reset",
		"git merge",
		"git rebase",
		"package installs",
		"package fetches",
		"terraform apply",
		"kubectl apply",
		"shell-ambiguous commands",
	}
}
