package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"squire.run/internal/kernel"
)

var adapterWriteBufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

const (
	adapterMaintainerMemoTTL = 5 * time.Second
	adapterHotMissMemoTTL    = time.Second
)

type adapterOptions struct {
	Stdio            bool
	EnsureMaintainer bool
}

type adapterServer struct {
	defaultCWD       string
	defaultSessionID string
	ensureMaintainer bool
	kernels          map[string]*kernel.Kernel
	states           map[string]adapterCWDState
	plans            map[string]adapterCommandPlan
	lastPlanCWD      string
	lastPlanArgv     []string
	lastPlan         adapterCommandPlan
	hotMisses        map[string]time.Time
	maintainers      map[string]adapterMaintainerMemo
}

type adapterCWDState struct {
	storeRoot string
	kernel    *kernel.Kernel
}

type adapterCommandPlan struct {
	key           string
	inv           kernel.CommandInvocation
	family        kernel.OperatorFamily
	replayAllowed bool
}

type adapterMaintainerMemo struct {
	status    kernel.BackgroundMaintainerStatus
	checkedAt time.Time
}

type adapterRequest struct {
	ID               string            `json:"id,omitempty"`
	CWD              string            `json:"cwd,omitempty"`
	Argv             []string          `json:"argv"`
	Script           string            `json:"script,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	ClearEnv         bool              `json:"clear_env,omitempty"`
	SessionID        string            `json:"session_id,omitempty"`
	EnsureMaintainer *bool             `json:"ensure_maintainer,omitempty"`
	ReplayOnly       bool              `json:"replay_only,omitempty"`
	Debug            bool              `json:"debug,omitempty"`
}

type adapterResponse struct {
	ID                string                `json:"id,omitempty"`
	OK                bool                  `json:"ok"`
	ReplayHit         bool                  `json:"replay_hit,omitempty"`
	ReplayMiss        bool                  `json:"replay_miss,omitempty"`
	MissReason        string                `json:"miss_reason,omitempty"`
	StdoutB64         string                `json:"stdout_b64,omitempty"`
	StderrB64         string                `json:"stderr_b64,omitempty"`
	ExitCode          int                   `json:"exit_code"`
	Mode              kernel.Mode           `json:"mode,omitempty"`
	Family            kernel.OperatorFamily `json:"family,omitempty"`
	Proof             string                `json:"proof,omitempty"`
	NativeWallMS      int64                 `json:"native_wall_ms,omitempty"`
	Phases            *kernel.PhaseTimings  `json:"phases,omitempty"`
	Diagnostics       []string              `json:"diagnostics,omitempty"`
	MaintainerRunning bool                  `json:"maintainer_running,omitempty"`
	MaintainerStarted bool                  `json:"maintainer_started,omitempty"`
	MaintainerAlready bool                  `json:"maintainer_already_running,omitempty"`
	Error             string                `json:"error,omitempty"`
}

func runKernelAdapter(ctx context.Context, defaultCWD string, args []string) error {
	opts, err := parseAdapterOptions(args)
	if err != nil {
		return err
	}
	if !opts.Stdio {
		return fmt.Errorf("squire kernel adapter requires --stdio")
	}
	sessionID := os.Getenv("SQUIRE_KERNEL_SESSION_ID")
	if sessionID == "" {
		sessionID = "adapter"
	}
	server := &adapterServer{
		defaultCWD:       defaultCWD,
		defaultSessionID: sessionID,
		ensureMaintainer: opts.EnsureMaintainer,
		kernels:          make(map[string]*kernel.Kernel),
		states:           make(map[string]adapterCWDState),
		plans:            make(map[string]adapterCommandPlan),
		hotMisses:        make(map[string]time.Time),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	if opts.EnsureMaintainer {
		server.primeDefaultRepo(ctx)
	}
	return server.serve(ctx, os.Stdin, os.Stdout)
}

func parseAdapterOptions(args []string) (adapterOptions, error) {
	opts := adapterOptions{EnsureMaintainer: true}
	for _, arg := range args {
		switch arg {
		case "--stdio":
			opts.Stdio = true
		case "--ensure-maintainer":
			// Kept as a compatibility no-op; the product adapter owns the
			// maintainer lifecycle by default.
			opts.EnsureMaintainer = true
		case "--no-maintainer":
			opts.EnsureMaintainer = false
		default:
			return opts, fmt.Errorf("unknown kernel adapter option %q", arg)
		}
	}
	return opts, nil
}

func (s *adapterServer) serve(ctx context.Context, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req adapterRequest
		if err := json.Unmarshal(line, &req); err != nil {
			if err := writeAdapterResponse(w, adapterResponse{OK: false, Error: "invalid request JSON: " + err.Error()}); err != nil {
				return err
			}
			continue
		}
		resp := s.handleRequest(ctx, req)
		if err := writeAdapterResponse(w, resp); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func writeAdapterResponse(w io.Writer, resp adapterResponse) error {
	buf := adapterWriteBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer adapterWriteBufferPool.Put(buf)
	if resp.Phases != nil {
		if err := json.NewEncoder(buf).Encode(resp); err != nil {
			return err
		}
		_, err := w.Write(buf.Bytes())
		return err
	}
	writeAdapterResponseFast(buf, resp)
	_, err := w.Write(buf.Bytes())
	return err
}

func writeAdapterResponseFast(buf *bytes.Buffer, resp adapterResponse) {
	buf.WriteByte('{')
	first := true
	writeJSONFieldString(buf, &first, "id", resp.ID)
	writeJSONFieldBoolAlways(buf, &first, "ok", resp.OK)
	writeJSONFieldBool(buf, &first, "replay_hit", resp.ReplayHit)
	writeJSONFieldBool(buf, &first, "replay_miss", resp.ReplayMiss)
	writeJSONFieldString(buf, &first, "miss_reason", resp.MissReason)
	writeJSONFieldString(buf, &first, "stdout_b64", resp.StdoutB64)
	writeJSONFieldString(buf, &first, "stderr_b64", resp.StderrB64)
	writeJSONFieldIntAlways(buf, &first, "exit_code", resp.ExitCode)
	writeJSONFieldString(buf, &first, "mode", string(resp.Mode))
	writeJSONFieldString(buf, &first, "family", string(resp.Family))
	writeJSONFieldString(buf, &first, "proof", resp.Proof)
	writeJSONFieldInt64(buf, &first, "native_wall_ms", resp.NativeWallMS)
	writeJSONFieldStringSlice(buf, &first, "diagnostics", resp.Diagnostics)
	writeJSONFieldBool(buf, &first, "maintainer_running", resp.MaintainerRunning)
	writeJSONFieldBool(buf, &first, "maintainer_started", resp.MaintainerStarted)
	writeJSONFieldBool(buf, &first, "maintainer_already_running", resp.MaintainerAlready)
	writeJSONFieldString(buf, &first, "error", resp.Error)
	buf.WriteByte('}')
	buf.WriteByte('\n')
}

func writeJSONFieldPrefix(buf *bytes.Buffer, first *bool, name string) {
	if !*first {
		buf.WriteByte(',')
	}
	*first = false
	writeJSONString(buf, name)
	buf.WriteByte(':')
}

func writeJSONFieldString(buf *bytes.Buffer, first *bool, name, value string) {
	if value == "" {
		return
	}
	writeJSONFieldPrefix(buf, first, name)
	writeJSONString(buf, value)
}

func writeJSONFieldStringSlice(buf *bytes.Buffer, first *bool, name string, values []string) {
	if len(values) == 0 {
		return
	}
	writeJSONFieldPrefix(buf, first, name)
	buf.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeJSONString(buf, value)
	}
	buf.WriteByte(']')
}

func writeJSONFieldBool(buf *bytes.Buffer, first *bool, name string, value bool) {
	if !value {
		return
	}
	writeJSONFieldBoolAlways(buf, first, name, value)
}

func writeJSONFieldBoolAlways(buf *bytes.Buffer, first *bool, name string, value bool) {
	writeJSONFieldPrefix(buf, first, name)
	if value {
		buf.WriteString("true")
	} else {
		buf.WriteString("false")
	}
}

func writeJSONFieldIntAlways(buf *bytes.Buffer, first *bool, name string, value int) {
	writeJSONFieldPrefix(buf, first, name)
	buf.WriteString(strconv.Itoa(value))
}

func writeJSONFieldInt64(buf *bytes.Buffer, first *bool, name string, value int64) {
	if value == 0 {
		return
	}
	writeJSONFieldPrefix(buf, first, name)
	buf.WriteString(strconv.FormatInt(value, 10))
}

func writeJSONString(buf *bytes.Buffer, value string) {
	quoted := buf.AvailableBuffer()
	quoted = strconv.AppendQuote(quoted, value)
	buf.Write(quoted)
}

func writeAdapterResponseSlow(w io.Writer, resp adapterResponse) error {
	buf := adapterWriteBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer adapterWriteBufferPool.Put(buf)
	if err := json.NewEncoder(buf).Encode(resp); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func (s *adapterServer) handleRequest(ctx context.Context, req adapterRequest) adapterResponse {
	if len(req.Argv) == 0 && req.Script == "" {
		return adapterResponse{ID: req.ID, OK: false, Error: "missing argv or script"}
	}
	return withAdapterEnv(req.Env, req.ClearEnv, func() adapterResponse {
		cwd := req.CWD
		if cwd == "" {
			cwd = s.defaultCWD
		}
		sessionID := req.SessionID
		if sessionID == "" {
			sessionID = s.defaultSessionID
		}
		resp := adapterResponse{ID: req.ID, OK: true}
		if req.Script != "" {
			if !req.ReplayOnly {
				return adapterResponse{ID: req.ID, OK: false, Error: "script requests require replay_only"}
			}
			state := s.stateFor(cwd)
			storeRoot := state.storeRoot
			ensure := s.ensureMaintainer
			if req.EnsureMaintainer != nil {
				ensure = *req.EnsureMaintainer
			}
			if ensure {
				status, diagnostics := s.ensureBackgroundMaintainer(ctx, cwd, storeRoot)
				resp.MaintainerRunning = status.Running
				resp.MaintainerStarted = status.Started
				resp.MaintainerAlready = status.AlreadyRunning
				resp.Diagnostics = append(resp.Diagnostics, diagnostics...)
			}
			serveStart := time.Now()
			if res, ok := state.kernel.ReplayComposedShell(ctx, sessionID, cwd, req.Script); ok {
				kernel.RecordHotClientResult(storeRoot, res, time.Since(serveStart))
				s.populateRunResponse(&resp, res, req.Debug)
				resp.ReplayHit = true
				return resp
			}
			resp.Family = kernel.FamilyShellUnknown
			resp.ReplayMiss = true
			resp.MissReason = "composed shell replay miss"
			return resp
		}
		plan := s.planFor(cwd, req.Argv)
		if !plan.replayAllowed {
			if req.ReplayOnly {
				resp.Family = plan.family
				resp.ReplayMiss = true
				resp.MissReason = "operator is not replay-allowed"
				return resp
			}
			res := kernel.RunNativeDirectInvocation(ctx, plan.inv, plan.family)
			s.populateRunResponse(&resp, res, req.Debug)
			return resp
		}
		state := s.stateFor(plan.inv.PolicyCWD)
		storeRoot := state.storeRoot
		missKey := adapterHotMissKey(storeRoot, plan.key)
		if s.hotMissValid(missKey) {
			if req.ReplayOnly {
				resp.Family = plan.family
				resp.ReplayMiss = true
				resp.MissReason = "recent hot snapshot miss"
				return resp
			}
			res := kernel.RunNativeDirectInvocation(ctx, plan.inv, plan.family)
			s.populateRunResponse(&resp, res, req.Debug)
			return resp
		}
		ensure := s.ensureMaintainer
		if req.EnsureMaintainer != nil {
			ensure = *req.EnsureMaintainer
		}
		if ensure {
			status, diagnostics := s.ensureBackgroundMaintainer(ctx, cwd, storeRoot)
			resp.MaintainerRunning = status.Running
			resp.MaintainerStarted = status.Started
			resp.MaintainerAlready = status.AlreadyRunning
			resp.Diagnostics = append(resp.Diagnostics, diagnostics...)
		}

		serveStart := time.Now()
		if res, ok := state.kernel.FastReplayInvocation(ctx, sessionID, plan.inv); ok {
			delete(s.hotMisses, missKey)
			kernel.RecordHotClientResult(storeRoot, *res, time.Since(serveStart))
			s.populateRunResponse(&resp, *res, req.Debug)
			resp.ReplayHit = true
			return resp
		}
		if s.hotMisses == nil {
			s.hotMisses = make(map[string]time.Time)
		}
		s.hotMisses[missKey] = time.Now().Add(adapterHotMissMemoTTL)
		if req.ReplayOnly {
			resp.Family = plan.family
			resp.ReplayMiss = true
			resp.MissReason = "hot snapshot miss"
			return resp
		}
		res := kernel.RunNativeDirectInvocation(ctx, plan.inv, plan.family)
		s.populateRunResponse(&resp, res, req.Debug)
		return resp
	})
}

func (s *adapterServer) primeDefaultRepo(ctx context.Context) {
	inv := kernel.NormalizeInvocation(s.defaultCWD, []string{"git", "rev-parse", "HEAD"})
	state := s.stateFor(inv.PolicyCWD)
	_, _ = s.ensureBackgroundMaintainer(ctx, inv.PolicyCWD, state.storeRoot)
	_ = state.kernel.PreloadHotSnapshot()
}

func (s *adapterServer) ensureBackgroundMaintainer(ctx context.Context, cwd, storeRoot string) (kernel.BackgroundMaintainerStatus, []string) {
	if storeRoot != "" {
		if memo, ok := s.maintainers[storeRoot]; ok && memo.status.Running && time.Since(memo.checkedAt) < adapterMaintainerMemoTTL {
			status := memo.status
			status.Started = false
			status.AlreadyRunning = true
			status.CheckedAt = time.Now()
			return status, nil
		}
		status, err := kernel.LoadBackgroundMaintainerStatus(ctx, cwd, storeRoot)
		if err == nil && status.Running {
			s.maintainers[storeRoot] = adapterMaintainerMemo{status: status, checkedAt: time.Now()}
			status.AlreadyRunning = true
			return status, nil
		}
	}
	status, err := kernel.StartBackgroundMaintainer(ctx, cwd, storeRoot, kernel.DefaultBackgroundMaintainerOptions())
	if err != nil {
		return status, []string{"background maintainer unavailable: " + err.Error()}
	}
	if storeRoot != "" && status.Running {
		s.maintainers[storeRoot] = adapterMaintainerMemo{status: status, checkedAt: time.Now()}
	}
	return status, status.Diagnostics
}

func (s *adapterServer) kernelFor(storeRoot string) *kernel.Kernel {
	if k := s.kernels[storeRoot]; k != nil {
		return k
	}
	k := kernel.New(storeRoot)
	s.kernels[storeRoot] = k
	return k
}

func (s *adapterServer) stateFor(cwd string) adapterCWDState {
	if s.states == nil {
		s.states = make(map[string]adapterCWDState)
	}
	key := adapterStateKey(cwd)
	if state, ok := s.states[key]; ok {
		return state
	}
	storeRoot := storeRootFor(cwd)
	state := adapterCWDState{
		storeRoot: storeRoot,
		kernel:    s.kernelFor(storeRoot),
	}
	s.states[key] = state
	return state
}

func (s *adapterServer) planFor(cwd string, argv []string) adapterCommandPlan {
	if s.lastPlan.key != "" && s.lastPlanCWD == cwd && argvEqual(s.lastPlanArgv, argv) {
		return s.lastPlan
	}
	if s.plans == nil {
		s.plans = make(map[string]adapterCommandPlan)
	}
	key := adapterPlanKey(cwd, argv)
	if plan, ok := s.plans[key]; ok {
		s.lastPlanCWD = cwd
		s.lastPlanArgv = append(s.lastPlanArgv[:0], argv...)
		s.lastPlan = plan
		return plan
	}
	inv := kernel.NormalizeInvocation(cwd, argv)
	family := kernel.Classify(inv.PolicyArgv)
	plan := adapterCommandPlan{
		key:           key,
		inv:           inv,
		family:        family,
		replayAllowed: kernel.IsReplayAllowed(inv.PolicyArgv),
	}
	s.plans[key] = plan
	s.lastPlanCWD = cwd
	s.lastPlanArgv = append(s.lastPlanArgv[:0], argv...)
	s.lastPlan = plan
	return plan
}

func argvEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (s *adapterServer) hotMissValid(key string) bool {
	if s.hotMisses == nil || key == "" {
		return false
	}
	expires, ok := s.hotMisses[key]
	if !ok {
		return false
	}
	if time.Now().After(expires) {
		delete(s.hotMisses, key)
		return false
	}
	return true
}

func adapterStateKey(cwd string) string {
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	return cwd + "\x00" + os.Getenv("SQUIRE_KERNEL_STORE_ROOT")
}

func adapterPlanKey(cwd string, argv []string) string {
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	return cwd + "\x00" + strings.Join(argv, "\x00")
}

func adapterHotMissKey(storeRoot, planKey string) string {
	if storeRoot == "" || planKey == "" {
		return ""
	}
	return storeRoot + "\x00" + planKey
}

func (s *adapterServer) populateRunResponse(resp *adapterResponse, res kernel.RunResult, includePhases bool) {
	resp.StdoutB64 = base64.StdEncoding.EncodeToString(res.Stdout)
	resp.StderrB64 = base64.StdEncoding.EncodeToString(res.Stderr)
	resp.ExitCode = res.ExitCode
	resp.Mode = res.Mode
	resp.Family = res.Family
	resp.NativeWallMS = res.Observation.NativeWallMS
	if resp.NativeWallMS == 0 && res.NativeWall > 0 {
		resp.NativeWallMS = res.NativeWall.Milliseconds()
	}
	if includePhases {
		phases := res.Phases
		resp.Phases = &phases
	}
	resp.Diagnostics = append(resp.Diagnostics, res.Diagnostics...)
	if res.Proof != nil {
		resp.Proof = res.Proof.OperationKey
	}
}

func withAdapterEnv(env map[string]string, clear bool, fn func() adapterResponse) adapterResponse {
	if len(env) == 0 && !clear {
		return fn()
	}
	if clear {
		saved := os.Environ()
		os.Clearenv()
		for k, v := range env {
			_ = os.Setenv(k, v)
		}
		defer func() {
			os.Clearenv()
			for _, kv := range saved {
				k, v, ok := strings.Cut(kv, "=")
				if ok {
					_ = os.Setenv(k, v)
				}
			}
		}()
		return fn()
	}
	type savedEnv struct {
		value string
		set   bool
	}
	saved := make(map[string]savedEnv, len(env))
	for k, v := range env {
		old, ok := os.LookupEnv(k)
		saved[k] = savedEnv{value: old, set: ok}
		_ = os.Setenv(k, v)
	}
	defer func() {
		for k, old := range saved {
			if old.set {
				_ = os.Setenv(k, old.value)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	}()
	return fn()
}
