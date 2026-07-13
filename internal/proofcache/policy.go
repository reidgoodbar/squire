package proofcache

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
	if IsProofGatedReplayCandidate(op.Argv) {
		if ledger == nil {
			return PolicyDecision{Mode: ModeNative, Reason: "validity ledger unavailable"}
		}
		if !proofGatedCandidateUsable(op, ws) {
			return PolicyDecision{Mode: ModeNative, Reason: "proof-gated candidate lacks stable local proof inputs"}
		}
		key := operationKey(op, ws)
		fps := inputFingerprints(op, ws)
		epoch := invalidationEpoch(op, ws)
		entry, valid := ledger.FindValid(key, fps, epoch)
		if valid && entry.Observation.OutputRef != "" && entry.WarmObservationCount > 0 {
			return PolicyDecision{Mode: ModeReplay, Reason: "proof-gated exact observation available"}
		}
		return PolicyDecision{Mode: ModeNative, Reason: "no prepared exact observation"}
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
		"git rev-parse --show-toplevel",
		"git rev-parse --is-inside-work-tree",
	}
}

func ProofGatedReplayCandidates() []string {
	return []string{
		"git status --short",
		"git status --porcelain",
		"git ls-files",
		"git log -1 --format=%H%n%s",
		"git diff / git diff --stat / git diff -- <path>",
		"cat <bounded workspace source/config file>",
		"sed -n <bounded-range>p <bounded workspace source/config file>",
		"head -n <bounded-lines> <bounded workspace source/config file>",
		"tail -n <bounded-lines> <bounded workspace source/config file>",
		"file <bounded workspace source/config file>",
		"grep -F <literal> <bounded workspace source/config file>",
		"grep -q -F <literal> <bounded workspace source/config file>",
		"rg -F <literal> <bounded workspace source/config file>",
		"rg -n/-q -F <literal> <bounded workspace source/config file>",
		"ls / ls -p for safe workspace directories",
		"<tool> --version / <tool> version",
		"pip/pip3 --version",
		"which <common-tool>",
		"command -v <common-tool> (external PATH executable only)",
		"whoami / hostname / id / uname static environment probes",
		"printenv <non-sensitive variable>",
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
