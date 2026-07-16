package proofcache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type MaintainerOptions struct {
	PollInterval time.Duration `json:"poll_interval"`
	MaxCycles    int           `json:"max_cycles"`
	MaxRuntime   time.Duration `json:"max_runtime"`
}

type MaintainerReport struct {
	Claim                   string    `json:"claim"`
	Mode                    string    `json:"mode"`
	RepoRoot                string    `json:"repo_root"`
	HotCacheSocket          string    `json:"hot_cache_socket,omitempty"`
	OracleAvailable         bool      `json:"oracle_available"`
	PollCycles              int       `json:"poll_cycles"`
	WarmCycles              int       `json:"warm_cycles"`
	InvalidationsObserved   int       `json:"invalidations_observed"`
	FastPathPrepared        int       `json:"fast_path_prepared"`
	ProofGatedPrewarmed     int       `json:"proof_gated_prewarmed"`
	PreparedEntriesObserved int       `json:"prepared_entries_observed"`
	PrepareRequestsObserved int       `json:"prepare_requests_observed"`
	PrepareRequestsPrepared int       `json:"prepare_requests_prepared"`
	PrepareRequestsRejected int       `json:"prepare_requests_rejected"`
	LastSignal              string    `json:"last_signal,omitempty"`
	LastWorkspaceEpoch      string    `json:"last_workspace_epoch,omitempty"`
	LastMaintainedAt        time.Time `json:"last_maintained_at,omitempty"`
	AgentVisibleSuggestions bool      `json:"agent_visible_suggestions"`
	NativeFallbackAvailable bool      `json:"native_fallback_available"`
	Diagnostics             []string  `json:"diagnostics,omitempty"`
}

type MaintainerResult struct {
	Report MaintainerReport
	Err    error
}

func DefaultMaintainerOptions() MaintainerOptions {
	return MaintainerOptions{
		PollInterval: 2 * time.Second,
	}
}

func Maintain(ctx context.Context, cwd, storeRoot string, opts MaintainerOptions) (MaintainerReport, error) {
	return New(storeRoot).RunMaintainer(ctx, cwd, opts)
}

func (k *Engine) StartMaintainer(ctx context.Context, cwd string, opts MaintainerOptions) <-chan MaintainerResult {
	done := make(chan MaintainerResult, 1)
	go func() {
		report, err := k.RunMaintainer(ctx, cwd, opts)
		done <- MaintainerResult{Report: report, Err: err}
	}()
	return done
}

func (k *Engine) RunMaintainer(ctx context.Context, cwd string, opts MaintainerOptions) (MaintainerReport, error) {
	k.mu.Lock()
	k.asyncForegroundObserve = true
	k.mu.Unlock()
	opts = normalizeMaintainerOptions(opts)
	if opts.MaxRuntime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.MaxRuntime)
		defer cancel()
	}
	report := MaintainerReport{
		Claim:                   scopedProofClaim,
		Mode:                    "resident_bounded",
		AgentVisibleSuggestions: false,
		NativeFallbackAvailable: true,
	}
	server, err := startHotCacheServer(ctx, k, k.Store.Root)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, "hot cache ipc unavailable: "+err.Error())
	} else {
		report.HotCacheSocket = server.socketPath
		defer server.Close()
	}
	recordRequests := func(requests prepareRequestCycle, requestErr error) {
		report.PrepareRequestsObserved += requests.Observed
		report.PrepareRequestsPrepared += requests.Prepared
		report.PrepareRequestsRejected += requests.Rejected
		if requestErr != nil {
			report.Diagnostics = append(report.Diagnostics, requestErr.Error())
		}
		if requests.Prepared > 0 {
			report.LastMaintainedAt = time.Now()
		}
	}
	for {
		cycleStart := time.Now()
		ws := k.Oracle.ShadowSnapshot(ctx, cwd)
		report.PollCycles++
		report.RepoRoot = ws.RepoRoot
		report.OracleAvailable = ws.OracleAvailable
		report.LastWorkspaceEpoch = ws.WorkspaceEpoch
		signal := maintainerSignal(ws)
		shouldWarm := report.LastSignal == "" || signal != report.LastSignal
		if shouldWarm {
			if report.LastSignal != "" {
				report.InvalidationsObserved++
			}
			warm, err := k.Warm(ctx, cwd)
			if err != nil {
				report.Diagnostics = append(report.Diagnostics, err.Error())
				return report, err
			}
			report.WarmCycles++
			report.FastPathPrepared += warm.FastPathPrepared
			report.ProofGatedPrewarmed += warm.ProofGatedPrewarmed
			report.PreparedEntriesObserved += len(warm.Prepared)
			report.LastMaintainedAt = time.Now()
			report.RepoRoot = warm.RepoRoot
			report.OracleAvailable = warm.OracleAvailable
		}
		recordRequests(k.consumePrepareRequests(ctx, 32))
		report.LastSignal = signal
		if opts.MaxCycles > 0 && report.PollCycles >= opts.MaxCycles {
			return report, nil
		}
		wait := opts.PollInterval - time.Since(cycleStart)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		demandInterval := 100 * time.Millisecond
		if wait > 0 && wait < demandInterval {
			demandInterval = wait
		}
		demand := time.NewTicker(demandInterval)
	waitLoop:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				demand.Stop()
				return report, nil
			case <-timer.C:
				demand.Stop()
				break waitLoop
			case <-demand.C:
				recordRequests(k.consumePrepareRequests(ctx, 32))
			}
		}
	}
}

func normalizeMaintainerOptions(opts MaintainerOptions) MaintainerOptions {
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultMaintainerOptions().PollInterval
	}
	return opts
}

func maintainerSignal(ws WorldState) string {
	return hashString(ws.RepoRoot + "|" +
		ws.HeadEpoch + "|" +
		ws.ConfigEpoch + "|" +
		ws.IndexEpoch + "|" +
		ws.FileTreeEpoch + "|" +
		ws.FileContentEpoch + "|" +
		ws.WorkspaceEpoch + "|" +
		proofGatedMaintainerSignal(ws) + "|" +
		hashString(osPathEnvSignal()) + "|" +
		deterministicVersionEnvHash())
}

func osPathEnvSignal() string {
	return os.Getenv("PATH")
}

func proofGatedMaintainerSignal(ws WorldState) string {
	var parts []string
	root := ws.RepoRoot
	if root != "" {
		if tree, content, complete := exactWorkspaceEpochs(root, 10000, true); complete {
			parts = append(parts, "workspace-exact:"+tree+"|"+content)
		}
		for _, rel := range replayableInspectionPrewarmFiles(root, 160) {
			path := filepath.Join(root, filepath.FromSlash(rel))
			if fp, ok := hashFile(path); ok {
				if info, err := os.Stat(path); err == nil && !info.IsDir() {
					parts = append(parts, "inspection:"+hashString(rel)+"|"+fp+"|"+hashString(info.Mode().String())+"|"+hashString(fmt.Sprintf("%d", info.Size())))
				}
			}
		}
	}
	cwd := root
	if cwd == "" {
		cwd = "."
	}
	for _, tool := range []string{"git", "rg", "go", "node", "npm", "python3", "make"} {
		if signal, ok := executableSignal(cwd, tool); ok {
			parts = append(parts, "tool:"+hashString(tool)+"|"+signal.PathHash+"|"+signal.FileHash)
		}
	}
	sort.Strings(parts)
	return hashString(strings.Join(parts, "\n"))
}
