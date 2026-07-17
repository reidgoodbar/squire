package green

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"
)

func RunScheduler(ctx context.Context, repoRoot, storeRoot string, options SchedulerOptions) (SchedulerReport, error) {
	canonical, err := canonicalRepoRoot(repoRoot)
	if err != nil {
		return SchedulerReport{RepoRoot: repoRoot}, err
	}
	repoRoot = canonical
	report := SchedulerReport{RepoRoot: repoRoot}
	store := newStateStore(storeRoot)
	if err := store.replaceRunning(repoRoot, os.Getpid()); err != nil {
		report.Errors = append(report.Errors, err.Error())
	}
	config, configErr := LoadConfig(repoRoot)
	if configErr != nil && !errors.Is(configErr, ErrNotConfigured) {
		report.Errors = appendBounded(report.Errors, configErr.Error(), 32)
	}
	monitor, monitorErr := startRepositoryMonitor(repoRoot, config.Checks)
	if monitorErr != nil {
		report.Errors = appendBounded(report.Errors, monitorErr.Error(), 32)
	}
	defer func() {
		if monitor != nil {
			monitor.Close()
		}
	}()
	poll := schedulerPoll(options.PollInterval, config.PollInterval)
	quiescence := schedulerDuration(options.Quiescence, config.Quiescence)
	debounce := time.NewTimer(quiescence)
	defer debounce.Stop()
	reconcile := time.NewTicker(poll)
	defer reconcile.Stop()
	trustPoll := time.NewTicker(time.Second)
	defer trustPoll.Stop()
	trusted := false
	if configErr == nil {
		trusted, _ = ConfigTrusted(repoRoot, storeRoot, config.Digest)
	}
	for {
		var changes <-chan struct{}
		var monitorErrors <-chan error
		var trustChanges <-chan time.Time
		if monitor != nil {
			changes = monitor.changes
			monitorErrors = monitor.errors
		}
		if configErr == nil && !trusted {
			trustChanges = trustPoll.C
		}
		select {
		case <-ctx.Done():
			return report, nil
		case err := <-monitorErrors:
			report.Errors = appendBounded(report.Errors, "repository monitor: "+err.Error(), 32)
			resetSchedulerTimer(debounce, quiescence)
		case <-changes:
			resetSchedulerTimer(debounce, quiescence)
		case <-reconcile.C:
			resetSchedulerTimer(debounce, quiescence)
		case <-trustChanges:
			if configErr == nil {
				currentTrust, trustErr := ConfigTrusted(repoRoot, storeRoot, config.Digest)
				if trustErr != nil {
					report.Errors = appendBounded(report.Errors, trustErr.Error(), 32)
				}
				if currentTrust != trusted {
					trusted = currentTrust
					resetSchedulerTimer(debounce, quiescence)
				}
			}
		case <-debounce.C:
			latest, err := LoadConfig(repoRoot)
			latestValid := err == nil
			if err != nil && !errors.Is(err, ErrNotConfigured) {
				report.Errors = appendBounded(report.Errors, err.Error(), 32)
			}
			configChanged := latestValid != (configErr == nil) || (latestValid && latest.Digest != config.Digest)
			if configChanged {
				monitor.Close()
				monitor, monitorErr = startRepositoryMonitor(repoRoot, latest.Checks)
				if monitorErr != nil {
					report.Errors = appendBounded(report.Errors, monitorErr.Error(), 32)
					monitor = nil
				}
			}
			config = latest
			configErr = err
			trusted = false
			if latestValid {
				trusted, err = ConfigTrusted(repoRoot, storeRoot, config.Digest)
				if err != nil {
					report.Errors = appendBounded(report.Errors, err.Error(), 32)
					trusted = false
				}
			}
			newPoll := schedulerPoll(options.PollInterval, config.PollInterval)
			newQuiescence := schedulerDuration(options.Quiescence, config.Quiescence)
			if newPoll != poll {
				poll = newPoll
				reconcile.Reset(poll)
			}
			quiescence = newQuiescence
			if !latestValid || !trusted {
				continue
			}
			concurrency := options.Concurrency
			if concurrency <= 0 {
				concurrency = config.Concurrency
			}
			proofs, proofErrors := proofSet(ctx, repoRoot, config, "")
			report.Cycles++
			for _, proofErr := range proofErrors {
				report.Errors = appendBounded(report.Errors, proofErr.Error(), 32)
			}
			if len(proofErrors) == 0 {
				started, completed, discarded := runPendingWithProofs(ctx, repoRoot, config, proofs, store, concurrency)
				report.Started += started
				report.Completed += completed
				report.Discarded += discarded
			}
		}
	}
}

func RunPending(ctx context.Context, repoRoot, storeRoot string) (SchedulerReport, error) {
	canonical, err := canonicalRepoRoot(repoRoot)
	if err != nil {
		return SchedulerReport{RepoRoot: repoRoot}, err
	}
	repoRoot = canonical
	report := SchedulerReport{RepoRoot: repoRoot, Cycles: 1}
	config, err := LoadConfig(repoRoot)
	if err != nil {
		return report, err
	}
	trusted, err := ConfigTrusted(repoRoot, storeRoot, config.Digest)
	if err != nil {
		return report, err
	}
	if !trusted {
		return report, ErrConfigUntrusted
	}
	store := newStateStore(storeRoot)
	workspace := observeWorkspace(ctx, repoRoot)
	proofs, proofErrors := proofSet(ctx, repoRoot, config, workspace.ID)
	if len(proofErrors) > 0 {
		return report, proofErrors[0]
	}
	report.Started, report.Completed, report.Discarded = runPendingWithProofs(ctx, repoRoot, config, proofs, store, config.Concurrency)
	return report, nil
}

func runPendingWithProofs(ctx context.Context, repoRoot string, config Config, proofs map[string]CheckProof, store *stateStore, concurrency int) (int, int, int) {
	state, err := store.load(repoRoot)
	if err != nil {
		state = emptyState(repoRoot)
	}
	checks := make([]Check, 0, len(config.Checks))
	for _, check := range config.Checks {
		proof, ok := proofs[check.Name]
		if !ok {
			continue
		}
		record, exists := state.Checks[check.Name]
		if exists && record.ScopeProof == proof.Digest {
			switch record.State {
			case "passed", "failed", "timed_out", "running":
				continue
			}
		}
		checks = append(checks, check)
	}
	if len(checks) == 0 {
		return 0, 0, 0
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 8 {
		concurrency = 8
	}
	semaphore := make(chan struct{}, concurrency)
	results := make(chan CheckRecord, len(checks))
	var group sync.WaitGroup
	for _, check := range checks {
		check := check
		group.Add(1)
		go func() {
			defer group.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			results <- runCheck(ctx, repoRoot, config, check, store)
		}()
	}
	group.Wait()
	close(results)
	completed := 0
	discarded := 0
	for result := range results {
		completed++
		if result.State == "discarded" {
			discarded++
		}
	}
	return len(checks), completed, discarded
}

func proofSet(ctx context.Context, repoRoot string, config Config, workspaceID string) (map[string]CheckProof, []error) {
	proofs := make(map[string]CheckProof, len(config.Checks))
	var proofErrors []error
	for _, check := range config.Checks {
		proof, err := computeCheckProofAtWorkspace(ctx, repoRoot, config, check, workspaceID)
		if err != nil {
			proofErrors = append(proofErrors, err)
			continue
		}
		proofs[check.Name] = proof
	}
	return proofs, proofErrors
}

func schedulerPoll(override, configured time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	if configured > 0 {
		return configured
	}
	return defaultPollInterval
}

func schedulerDuration(override, configured time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	if configured > 0 {
		return configured
	}
	return defaultQuiescence
}

func resetSchedulerTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func appendBounded(values []string, value string, maximum int) []string {
	if len(values) > 0 && values[len(values)-1] == value {
		return values
	}
	values = append(values, value)
	if len(values) > maximum {
		values = append([]string(nil), values[len(values)-maximum:]...)
	}
	return values
}
