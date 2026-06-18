package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const backgroundMaintainerStatusFile = "maintainer_status.json"

type BackgroundMaintainerOptions struct {
	Duration     time.Duration `json:"duration"`
	PollInterval time.Duration `json:"poll_interval"`
}

type BackgroundMaintainerStatus struct {
	Mode                    string    `json:"mode"`
	RepoRoot                string    `json:"repo_root,omitempty"`
	StoreRoot               string    `json:"store_root"`
	CWD                     string    `json:"cwd"`
	HotCacheSocket          string    `json:"hot_cache_socket,omitempty"`
	PID                     int       `json:"pid,omitempty"`
	Running                 bool      `json:"running"`
	Started                 bool      `json:"started,omitempty"`
	AlreadyRunning          bool      `json:"already_running,omitempty"`
	StopRequested           bool      `json:"stop_requested,omitempty"`
	StartedAt               time.Time `json:"started_at,omitempty"`
	StoppedAt               time.Time `json:"stopped_at,omitempty"`
	CheckedAt               time.Time `json:"checked_at"`
	Duration                string    `json:"duration,omitempty"`
	PollInterval            string    `json:"poll_interval,omitempty"`
	LogPath                 string    `json:"log_path,omitempty"`
	StatusPath              string    `json:"status_path"`
	AgentVisibleSuggestions bool      `json:"agent_visible_suggestions"`
	NativeFallbackAvailable bool      `json:"native_fallback_available"`
	Diagnostics             []string  `json:"diagnostics,omitempty"`
}

type backgroundCommandFactory func(cwd, storeRoot string, opts BackgroundMaintainerOptions, log *os.File) (*exec.Cmd, error)

var newBackgroundMaintainerCommand backgroundCommandFactory = defaultBackgroundMaintainerCommand

func DefaultBackgroundMaintainerOptions() BackgroundMaintainerOptions {
	return BackgroundMaintainerOptions{
		Duration:     30 * time.Minute,
		PollInterval: DefaultMaintainerOptions().PollInterval,
	}
}

func StartBackgroundMaintainer(ctx context.Context, cwd, storeRoot string, opts BackgroundMaintainerOptions) (BackgroundMaintainerStatus, error) {
	opts = normalizeBackgroundMaintainerOptions(opts)
	store := NewLedgerStore(storeRoot)
	if err := store.Init(); err != nil {
		return BackgroundMaintainerStatus{}, err
	}
	current, err := LoadBackgroundMaintainerStatus(ctx, cwd, storeRoot)
	if err == nil && current.Running {
		current.AlreadyRunning = true
		current.Started = false
		return current, nil
	}
	statusPath := backgroundStatusPath(storeRoot)
	logPath := backgroundLogPath(storeRoot)
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return BackgroundMaintainerStatus{}, err
	}
	defer log.Close()

	cmd, err := newBackgroundMaintainerCommand(cwd, storeRoot, opts, log)
	if err != nil {
		return BackgroundMaintainerStatus{}, err
	}
	if cmd.Dir == "" {
		cmd.Dir = cwd
	}
	if cmd.Stdout == nil {
		cmd.Stdout = log
	}
	if cmd.Stderr == nil {
		cmd.Stderr = log
	}
	if len(cmd.Env) == 0 {
		cmd.Env = os.Environ()
	}
	detachBackgroundCommand(cmd)
	if err := cmd.Start(); err != nil {
		return BackgroundMaintainerStatus{}, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()

	ws := NewRepoOracle().Snapshot(ctx, cwd)
	status := BackgroundMaintainerStatus{
		Mode:                    "background_process",
		RepoRoot:                ws.RepoRoot,
		StoreRoot:               storeRoot,
		CWD:                     cwd,
		HotCacheSocket:          hotCacheSocketPath(storeRoot),
		PID:                     pid,
		Running:                 processAlive(pid),
		Started:                 true,
		StartedAt:               time.Now(),
		CheckedAt:               time.Now(),
		Duration:                opts.Duration.String(),
		PollInterval:            opts.PollInterval.String(),
		LogPath:                 logPath,
		StatusPath:              statusPath,
		AgentVisibleSuggestions: false,
		NativeFallbackAvailable: true,
	}
	if !status.Running {
		status.Diagnostics = append(status.Diagnostics, "background maintainer process was started but is not alive")
	}
	if err := saveBackgroundMaintainerStatus(status); err != nil {
		return status, err
	}
	return status, nil
}

func LoadBackgroundMaintainerStatus(ctx context.Context, cwd, storeRoot string) (BackgroundMaintainerStatus, error) {
	_ = ctx
	statusPath := backgroundStatusPath(storeRoot)
	status := BackgroundMaintainerStatus{
		Mode:                    "background_process",
		StoreRoot:               storeRoot,
		CWD:                     cwd,
		CheckedAt:               time.Now(),
		StatusPath:              statusPath,
		AgentVisibleSuggestions: false,
		NativeFallbackAvailable: true,
	}
	b, err := os.ReadFile(statusPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			status.Diagnostics = []string{"background maintainer has not been started"}
			return status, nil
		}
		return status, err
	}
	if err := json.Unmarshal(b, &status); err != nil {
		return status, err
	}
	status.CheckedAt = time.Now()
	status.StatusPath = statusPath
	status.Started = false
	status.AlreadyRunning = false
	status.Running = status.PID > 0 && processAlive(status.PID)
	if status.HotCacheSocket == "" {
		status.HotCacheSocket = hotCacheSocketPath(storeRoot)
	}
	status.AgentVisibleSuggestions = false
	status.NativeFallbackAvailable = true
	if status.PID > 0 && !status.Running && status.StoppedAt.IsZero() {
		status.Diagnostics = append(status.Diagnostics, "recorded maintainer process is not running")
	}
	return status, nil
}

func StopBackgroundMaintainer(ctx context.Context, cwd, storeRoot string) (BackgroundMaintainerStatus, error) {
	status, err := LoadBackgroundMaintainerStatus(ctx, cwd, storeRoot)
	if err != nil {
		return status, err
	}
	status.StopRequested = true
	status.CheckedAt = time.Now()
	if status.PID <= 0 || !status.Running {
		status.Running = false
		status.StoppedAt = time.Now()
		status.Diagnostics = append(status.Diagnostics, "background maintainer was not running")
		_ = saveBackgroundMaintainerStatus(status)
		return status, nil
	}
	if err := terminateProcess(status.PID); err != nil {
		status.Diagnostics = append(status.Diagnostics, "stop signal failed: "+err.Error())
		_ = saveBackgroundMaintainerStatus(status)
		return status, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(status.PID) {
			status.Running = false
			status.StoppedAt = time.Now()
			_ = saveBackgroundMaintainerStatus(status)
			return status, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	status.Running = processAlive(status.PID)
	if status.Running {
		status.Diagnostics = append(status.Diagnostics, "background maintainer still running after stop signal")
	} else {
		status.StoppedAt = time.Now()
	}
	if err := saveBackgroundMaintainerStatus(status); err != nil {
		return status, err
	}
	return status, nil
}

func defaultBackgroundMaintainerCommand(cwd, storeRoot string, opts BackgroundMaintainerOptions, log *os.File) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{"kernel", "maintain", "--duration", opts.Duration.String(), "--poll-interval", opts.PollInterval.String()}
	cmd := exec.Command(exe, args...)
	cmd.Dir = cwd
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "SQUIRE_KERNEL_STORE_ROOT="+storeRoot, "GIT_OPTIONAL_LOCKS=0")
	return cmd, nil
}

func normalizeBackgroundMaintainerOptions(opts BackgroundMaintainerOptions) BackgroundMaintainerOptions {
	def := DefaultBackgroundMaintainerOptions()
	if opts.Duration <= 0 {
		opts.Duration = def.Duration
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = def.PollInterval
	}
	return opts
}

func backgroundStatusPath(storeRoot string) string {
	return filepath.Join(storeRoot, backgroundMaintainerStatusFile)
}

func backgroundLogPath(storeRoot string) string {
	return filepath.Join(storeRoot, "maintainer.log")
}

func saveBackgroundMaintainerStatus(status BackgroundMaintainerStatus) error {
	if status.StoreRoot == "" {
		return errors.New("missing store root")
	}
	if err := os.MkdirAll(status.StoreRoot, 0o700); err != nil {
		return err
	}
	if status.StatusPath == "" {
		status.StatusPath = backgroundStatusPath(status.StoreRoot)
	}
	b, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(status.StoreRoot, fmt.Sprintf("maintainer_status.%d.%d.tmp", os.Getpid(), time.Now().UnixNano()))
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, status.StatusPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
