package green

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
)

func Inspect(ctx context.Context, repoRoot, storeRoot string) Report {
	report := Report{
		State:      "unconfigured",
		RepoRoot:   repoRoot,
		ConfigPath: ConfigRelativePath,
	}
	canonical, canonicalErr := canonicalRepoRoot(repoRoot)
	if canonicalErr != nil {
		report.State = "error"
		report.Diagnostics = append(report.Diagnostics, canonicalErr.Error())
		return report
	}
	repoRoot = canonical
	report.RepoRoot = canonical
	config, err := LoadConfig(repoRoot)
	if err != nil {
		if !errors.Is(err, ErrNotConfigured) {
			report.State = "error"
			report.Diagnostics = append(report.Diagnostics, err.Error())
		}
		return report
	}
	report.Configured = true
	report.ConfigPath = config.Path
	trusted, trustErr := ConfigTrusted(repoRoot, storeRoot, config.Digest)
	if trustErr != nil {
		report.State = "error"
		report.Diagnostics = append(report.Diagnostics, trustErr.Error())
		return report
	}
	report.Trusted = trusted
	if !trusted {
		report.State = "untrusted"
		for _, check := range config.Checks {
			if check.Required {
				report.RequiredChecks++
			}
			report.Checks = append(report.Checks, CheckStatus{
				Name:     check.Name,
				Required: check.Required,
				State:    "untrusted",
				Reason:   "review and trust the exact checks config before native execution",
			})
		}
		return report
	}
	workspace := observeWorkspace(ctx, repoRoot)
	report.ObservedWorkspaceID = workspace.ID
	report.WorkspaceState = workspace.State
	report.UntrackedFiles = workspace.Untracked
	state, stateErr := newStateStore(storeRoot).load(repoRoot)
	if stateErr != nil {
		report.Diagnostics = append(report.Diagnostics, stateErr.Error())
		state = emptyState(repoRoot)
	}

	allRequiredPassed := true
	anyRunning := false
	for _, check := range config.Checks {
		if check.Required {
			report.RequiredChecks++
		}
		status := CheckStatus{Name: check.Name, Required: check.Required, State: "pending"}
		files, inputDigest, matchedBytes, inputErr := collectInputProof(ctx, repoRoot, check)
		if inputErr != nil {
			status.State = "error"
			status.Reason = "current input proof is unavailable: " + inputErr.Error()
			report.Diagnostics = append(report.Diagnostics, check.Name+": "+inputErr.Error())
			if check.Required {
				allRequiredPassed = false
			}
			report.Checks = append(report.Checks, status)
			continue
		}
		status.MatchedFiles = len(files)
		status.MatchedBytes = matchedBytes
		record, exists := state.Checks[check.Name]
		if !exists {
			status.Reason = "check has not run for this configuration"
		} else {
			status.Duration = record.Duration
			status.ExitCode = record.ExitCode
			status.CompletedAt = record.CompletedAt
			status.ObservedWorkspaceID = record.ObservedWorkspaceID
			executableCurrent := executableProofCurrent(ctx, record)
			if state.ConfigDigest != config.Digest || record.InputProof != inputDigest || !executableCurrent {
				status.State = "stale"
				switch {
				case state.ConfigDigest != config.Digest:
					status.Reason = "check configuration changed"
				case record.InputProof != inputDigest:
					status.Reason = "declared inputs changed"
				default:
					status.Reason = "validation executable changed"
				}
			} else {
				switch record.State {
				case "passed", "failed", "timed_out":
					status.State = record.State
					status.Current = true
					report.CurrentChecks++
				case "running":
					status.State = "running"
					status.Reason = "native validation is in progress"
					anyRunning = true
				case "discarded":
					status.State = "stale"
					status.Reason = record.Error
				default:
					status.State = "error"
					status.Reason = record.Error
				}
			}
		}
		if check.Required && !(status.Current && status.State == "passed") {
			allRequiredPassed = false
		}
		report.Checks = append(report.Checks, status)
	}
	sort.SliceStable(report.Checks, func(i, j int) bool {
		return report.Checks[i].Name < report.Checks[j].Name
	})
	report.Green = allRequiredPassed && report.RequiredChecks > 0 && len(report.Diagnostics) == 0
	switch {
	case report.Green:
		report.State = "green"
	case anyRunning:
		report.State = "running"
	case len(report.Diagnostics) > 0:
		report.State = "error"
	default:
		report.State = "not_green"
	}
	return report
}

func executableProofCurrent(ctx context.Context, record CheckRecord) bool {
	if record.ExecutablePath == "" || record.ExecutableProof == "" {
		return false
	}
	if err := ctx.Err(); err != nil {
		return false
	}
	info, err := os.Stat(record.ExecutablePath)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	identity, err := readRegularIdentity(ctx, record.ExecutablePath)
	if err != nil {
		return false
	}
	digest := digestStrings([]string{"squire-green-executable-v1", record.ExecutablePath, identity.mode, identity.changeToken, identity.digest})
	return digest == record.ExecutableProof
}

func StateLabel(state string) string {
	switch state {
	case "passed":
		return "PASS"
	case "failed":
		return "FAIL"
	case "timed_out":
		return "TIMEOUT"
	case "running":
		return "RUNNING"
	case "stale":
		return "STALE"
	case "pending":
		return "PENDING"
	case "error":
		return "ERROR"
	case "untrusted":
		return "UNTRUSTED"
	default:
		return fmt.Sprintf("%s", state)
	}
}
