package green

import "time"

const (
	ConfigRelativePath = ".squire/checks.toml"
	stateVersion       = 1
	configVersion      = 1
)

type Check struct {
	Name     string
	Command  []string
	Inputs   []string
	Exclude  []string
	CWD      string
	Required bool
	Timeout  time.Duration
	Env      map[string]string
}

type Config struct {
	Path         string
	Digest       string
	Version      int
	Quiescence   time.Duration
	PollInterval time.Duration
	Concurrency  int
	Checks       []Check
}

type CheckProof struct {
	Digest              string
	InputDigest         string
	EnvironmentDigest   string
	ExecutablePath      string
	ExecutableDigest    string
	MatchedFiles        int
	MatchedBytes        int64
	ObservedWorkspaceID string
	Environment         []string
	CWD                 string
}

type CheckRecord struct {
	Name                string        `json:"name"`
	State               string        `json:"state"`
	ScopeProof          string        `json:"scope_proof,omitempty"`
	InputProof          string        `json:"input_proof,omitempty"`
	EnvironmentProof    string        `json:"environment_proof,omitempty"`
	ExecutablePath      string        `json:"executable_path,omitempty"`
	ExecutableProof     string        `json:"executable_proof,omitempty"`
	ObservedWorkspaceID string        `json:"observed_workspace_epoch,omitempty"`
	MatchedFiles        int           `json:"matched_files,omitempty"`
	MatchedBytes        int64         `json:"matched_bytes,omitempty"`
	StartedAt           time.Time     `json:"started_at,omitempty"`
	CompletedAt         time.Time     `json:"completed_at,omitempty"`
	Duration            time.Duration `json:"duration_ns,omitempty"`
	ExitCode            int           `json:"exit_code"`
	StdoutDigest        string        `json:"stdout_digest,omitempty"`
	StderrDigest        string        `json:"stderr_digest,omitempty"`
	StdoutBytes         int64         `json:"stdout_bytes,omitempty"`
	StderrBytes         int64         `json:"stderr_bytes,omitempty"`
	PID                 int           `json:"pid,omitempty"`
	Error               string        `json:"error,omitempty"`
	Attempt             int64         `json:"attempt"`
}

type State struct {
	Version      int                    `json:"version"`
	RepoRoot     string                 `json:"repo_root"`
	ConfigDigest string                 `json:"config_digest,omitempty"`
	DaemonPID    int                    `json:"daemon_pid,omitempty"`
	UpdatedAt    time.Time              `json:"updated_at"`
	Checks       map[string]CheckRecord `json:"checks"`
}

type CheckStatus struct {
	Name                string        `json:"name"`
	Required            bool          `json:"required"`
	State               string        `json:"state"`
	Current             bool          `json:"current"`
	Duration            time.Duration `json:"duration_ns,omitempty"`
	ExitCode            int           `json:"exit_code,omitempty"`
	MatchedFiles        int           `json:"matched_files,omitempty"`
	MatchedBytes        int64         `json:"matched_bytes,omitempty"`
	ObservedWorkspaceID string        `json:"observed_workspace_epoch,omitempty"`
	CompletedAt         time.Time     `json:"completed_at,omitempty"`
	Reason              string        `json:"reason,omitempty"`
}

type Report struct {
	Configured          bool          `json:"configured"`
	Trusted             bool          `json:"trusted"`
	Green               bool          `json:"green"`
	State               string        `json:"state"`
	RepoRoot            string        `json:"repo_root,omitempty"`
	ConfigPath          string        `json:"config_path,omitempty"`
	ObservedWorkspaceID string        `json:"observed_workspace_epoch,omitempty"`
	WorkspaceState      string        `json:"workspace_state,omitempty"`
	UntrackedFiles      int           `json:"untracked_files,omitempty"`
	RequiredChecks      int           `json:"required_checks"`
	CurrentChecks       int           `json:"current_checks"`
	Checks              []CheckStatus `json:"checks,omitempty"`
	Diagnostics         []string      `json:"diagnostics,omitempty"`
}

type TrustRecord struct {
	Version      int       `json:"version"`
	RepoRoot     string    `json:"repo_root"`
	ConfigDigest string    `json:"config_digest"`
	TrustedAt    time.Time `json:"trusted_at"`
}

type SchedulerOptions struct {
	PollInterval time.Duration
	Quiescence   time.Duration
	Concurrency  int
}

type SchedulerReport struct {
	RepoRoot  string
	Cycles    int
	Started   int
	Completed int
	Discarded int
	Errors    []string
}
