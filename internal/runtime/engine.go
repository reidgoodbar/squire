// Package runtime implements Squire's agent-neutral execution boundary.
// Agents submit ordinary process requests. Providers may return an exact,
// proof-backed result; every other request remains a native miss.
package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"squire.run/internal/kernel"
)

type Outcome string

const (
	OutcomeHit  Outcome = "hit"
	OutcomeMiss Outcome = "miss"
)

type Request struct {
	SessionID string
	CWD       string
	Argv      []string
	Env       map[string]string
}

type Result struct {
	Outcome      Outcome
	Provider     string
	Reason       string
	Stdout       []byte
	Stderr       []byte
	ExitCode     int
	Family       kernel.OperatorFamily
	ProofID      string
	NativeWallMS int64
}

type Explanation struct {
	Outcome       Outcome               `json:"outcome"`
	Eligible      bool                  `json:"eligible"`
	Provider      string                `json:"provider,omitempty"`
	Reason        string                `json:"reason,omitempty"`
	Family        kernel.OperatorFamily `json:"family"`
	WorkspaceRoot string                `json:"workspace_root,omitempty"`
	StoreRoot     string                `json:"store_root,omitempty"`
	ProofID       string                `json:"proof_id,omitempty"`
}

func Miss(reason string) Result {
	return Result{Outcome: OutcomeMiss, Reason: reason}
}

// Provider is a behavior-preserving implementation of an ordinary process
// request. Implementations return handled=false unless they can prove and
// materialize the complete stdout, stderr, and exit status.
type Provider interface {
	Name() string
	Try(context.Context, Request, *Workspace) (result Result, handled bool)
}

type Workspace struct {
	ID        string
	Root      string
	StoreRoot string
	Kernel    *kernel.Kernel
}

type PreparationStatus struct {
	Running     bool
	Ready       bool
	Attempts    int
	LastAttempt time.Time
	LastError   string
}

type PrepareFunc func(context.Context, *Workspace) error

const preparationRetryAfter = 5 * time.Second

type workspaceEntry struct {
	workspace *Workspace
	prepare   PreparationStatus
}

// Registry shares one prepared workspace and kernel instance across every
// agent request in a process. Repositories are keyed by their resolved Git
// directory store, so subdirectories and git -C requests converge correctly.
type Registry struct {
	mu         sync.Mutex
	workspaces map[string]*workspaceEntry
	prepare    PrepareFunc
}

func NewRegistry(prepare PrepareFunc) *Registry {
	if prepare == nil {
		prepare = startMaintainer
	}
	return &Registry{
		workspaces: make(map[string]*workspaceEntry),
		prepare:    prepare,
	}
}

func (r *Registry) Resolve(cwd string, argv []string) (*Workspace, bool) {
	inv := kernel.NormalizeInvocation(cwd, argv)
	root, storeRoot, ok := kernel.FastWorkspace(inv.PolicyCWD)
	if !ok {
		return nil, false
	}
	key := filepath.Clean(storeRoot)
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry := r.workspaces[key]; entry != nil {
		return entry.workspace, true
	}
	workspace := &Workspace{
		ID:        key,
		Root:      root,
		StoreRoot: storeRoot,
		Kernel:    kernel.New(storeRoot),
	}
	r.workspaces[key] = &workspaceEntry{workspace: workspace}
	return workspace, true
}

// EnsurePrepared requests preparation without delaying the agent's native
// fallback. Repeated cold misses for a repository collapse into one attempt.
func (r *Registry) EnsurePrepared(workspace *Workspace) bool {
	if workspace == nil {
		return false
	}
	r.mu.Lock()
	entry := r.workspaces[workspace.ID]
	if entry == nil || entry.prepare.Running {
		r.mu.Unlock()
		return false
	}
	if !entry.prepare.LastAttempt.IsZero() && time.Since(entry.prepare.LastAttempt) < preparationRetryAfter {
		r.mu.Unlock()
		return false
	}
	entry.prepare.Running = true
	entry.prepare.Ready = false
	entry.prepare.Attempts++
	entry.prepare.LastAttempt = time.Now()
	entry.prepare.LastError = ""
	r.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := r.prepare(ctx, workspace)
		r.finishPreparation(workspace.ID, err)
	}()
	return true
}

func (r *Registry) Prepare(ctx context.Context, workspace *Workspace) error {
	if workspace == nil {
		return errors.New("missing workspace")
	}
	r.mu.Lock()
	entry := r.workspaces[workspace.ID]
	if entry == nil {
		r.mu.Unlock()
		return errors.New("workspace is not registered")
	}
	if entry.prepare.Running {
		r.mu.Unlock()
		return nil
	}
	entry.prepare.Running = true
	entry.prepare.Ready = false
	entry.prepare.Attempts++
	entry.prepare.LastAttempt = time.Now()
	entry.prepare.LastError = ""
	r.mu.Unlock()

	err := r.prepare(ctx, workspace)
	r.finishPreparation(workspace.ID, err)
	return err
}

func (r *Registry) Status(workspaceID string) PreparationStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry := r.workspaces[workspaceID]; entry != nil {
		return entry.prepare
	}
	return PreparationStatus{}
}

func (r *Registry) finishPreparation(workspaceID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.workspaces[workspaceID]
	if entry == nil {
		return
	}
	entry.prepare.Running = false
	entry.prepare.Ready = err == nil
	if err != nil {
		entry.prepare.LastError = err.Error()
	}
}

func startMaintainer(ctx context.Context, workspace *Workspace) error {
	status, err := kernel.StartBackgroundMaintainer(
		ctx,
		workspace.Root,
		workspace.StoreRoot,
		kernel.DefaultBackgroundMaintainerOptions(),
	)
	if err != nil {
		return err
	}
	if !status.Running {
		return errors.New("workspace maintainer did not remain running")
	}
	return nil
}

type Engine struct {
	registry  *Registry
	providers []Provider
}

func NewEngine(registry *Registry, providers ...Provider) *Engine {
	if registry == nil {
		registry = NewRegistry(nil)
	}
	if len(providers) == 0 {
		providers = []Provider{snapshotProvider{}}
	}
	return &Engine{registry: registry, providers: append([]Provider(nil), providers...)}
}

func (e *Engine) Registry() *Registry {
	return e.registry
}

func (e *Engine) TryExecute(ctx context.Context, request Request) Result {
	return e.tryExecute(ctx, request, true)
}

// TryExecutePassive never starts preparation. It is intended for diagnostics
// and shadow verification where observing Squire must not alter runtime state.
func (e *Engine) TryExecutePassive(ctx context.Context, request Request) Result {
	return e.tryExecute(ctx, request, false)
}

func (e *Engine) Explain(ctx context.Context, request Request) Explanation {
	inv := kernel.NormalizeInvocation(request.CWD, request.Argv)
	explanation := Explanation{
		Eligible: eligible(inv),
		Family:   kernel.Classify(inv.PolicyArgv),
	}
	if workspace, ok := e.registry.Resolve(request.CWD, request.Argv); ok {
		explanation.WorkspaceRoot = workspace.Root
		explanation.StoreRoot = workspace.StoreRoot
	}
	result := e.TryExecutePassive(ctx, request)
	explanation.Outcome = result.Outcome
	explanation.Provider = result.Provider
	explanation.Reason = result.Reason
	explanation.ProofID = result.ProofID
	return explanation
}

func (e *Engine) tryExecute(ctx context.Context, request Request, prepare bool) Result {
	if request.CWD == "" || len(request.Argv) == 0 {
		return Miss("invalid execution request")
	}
	inv := kernel.NormalizeInvocation(request.CWD, request.Argv)
	if !eligible(inv) {
		result := Miss("operation is not eligible for transparent acceleration")
		result.Family = kernel.Classify(inv.PolicyArgv)
		return result
	}
	if !environmentMatchesProcess(request.Env) {
		result := Miss("execution environment differs from the prepared runtime")
		result.Family = kernel.Classify(inv.PolicyArgv)
		return result
	}
	workspace, ok := e.registry.Resolve(request.CWD, request.Argv)
	if !ok {
		result := Miss("no supported workspace")
		result.Family = kernel.Classify(inv.PolicyArgv)
		return result
	}
	for _, provider := range e.providers {
		if result, handled := provider.Try(ctx, request, workspace); handled {
			result.Outcome = OutcomeHit
			result.Provider = provider.Name()
			return result
		}
	}
	if prepare {
		e.registry.EnsurePrepared(workspace)
	}
	result := Miss("workspace result is not prepared or no longer valid")
	result.Family = kernel.Classify(inv.PolicyArgv)
	return result
}

// The Go provider evaluates proofs against this process environment. A caller
// that supplies a different child environment must fall through until a
// provider can validate directly against that environment. The native ABI
// provider performs a more selective command-aware comparison.
func environmentMatchesProcess(env map[string]string) bool {
	if env == nil {
		return true
	}
	current := make(map[string]string, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			current[key] = value
		}
	}
	if len(current) != len(env) {
		return false
	}
	for key, value := range current {
		if candidate, ok := env[key]; !ok || candidate != value {
			return false
		}
	}
	return true
}

func eligible(inv kernel.CommandInvocation) bool {
	return kernel.IsProductionRuntimeInvocationAllowed(inv.OriginalCWD, inv.OriginalArgv)
}

type snapshotProvider struct{}

func (snapshotProvider) Name() string {
	return "validated_workspace"
}

func (snapshotProvider) Try(ctx context.Context, request Request, workspace *Workspace) (Result, bool) {
	inv := kernel.NormalizeInvocation(request.CWD, request.Argv)
	var replay *kernel.RunResult
	var ok bool
	if script, scriptOK := kernel.ComposedShellArgvScript(inv.OriginalArgv); scriptOK {
		value, replayOK := workspace.Kernel.ReplayComposedShell(ctx, request.SessionID, inv.PolicyCWD, script)
		if replayOK {
			replay = &value
			ok = true
		}
	} else {
		replay, ok = workspace.Kernel.FastReplayInvocation(ctx, request.SessionID, inv)
	}
	if !ok || replay == nil || replay.Mode != kernel.ModeReplay {
		return Result{}, false
	}
	result := Result{
		Stdout:       append([]byte(nil), replay.Stdout...),
		Stderr:       append([]byte(nil), replay.Stderr...),
		ExitCode:     replay.ExitCode,
		Family:       replay.Family,
		NativeWallMS: replay.Observation.NativeWallMS,
	}
	if replay.Proof != nil {
		result.ProofID = replay.Proof.OperationKey
	}
	return result, true
}
