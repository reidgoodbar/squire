package kernel

import "time"

type EvidenceQuality string

const (
	EvidenceStrong     EvidenceQuality = "strong"
	EvidencePartial    EvidenceQuality = "partial"
	EvidenceInferred   EvidenceQuality = "inferred"
	EvidenceMissing    EvidenceQuality = "missing"
	EvidenceConflicted EvidenceQuality = "conflicted"
)

type OperatorFamily string

const (
	FamilyLocalRepoMetadata OperatorFamily = "local_repo_metadata"
	FamilyRepoState         OperatorFamily = "repo_state"
	FamilySearchList        OperatorFamily = "search_list"
	FamilyFileInspection    OperatorFamily = "file_inspection"
	FamilyValidation        OperatorFamily = "validation_build_test"
	FamilyEditOrMutation    OperatorFamily = "edit_or_mutation"
	FamilyPackageSetup      OperatorFamily = "package_setup"
	FamilyShellUnknown      OperatorFamily = "shell_unknown"
)

type Mode string

const (
	ModeNative Mode = "native"
	ModeShadow Mode = "shadow"
	ModeReplay Mode = "replay"
	ModeNever  Mode = "never"
)

type Operation struct {
	OperationID       string          `json:"operation_id"`
	SessionID         string          `json:"session_id"`
	OperatorFamily    OperatorFamily  `json:"operator_family"`
	NormalizedCommand string          `json:"normalized_command"`
	Argv              []string        `json:"-"`
	CWD               string          `json:"cwd"`
	RepoRoot          string          `json:"repo_root"`
	Mode              Mode            `json:"mode"`
	EvidenceQuality   EvidenceQuality `json:"evidence_quality"`
}

type WorldState struct {
	RepoRoot              string            `json:"repo_root"`
	Head                  string            `json:"head,omitempty"`
	Branch                string            `json:"branch,omitempty"`
	GitDir                string            `json:"git_dir,omitempty"`
	GitDirAbs             string            `json:"git_dir_abs,omitempty"`
	RemoteURL             string            `json:"remote_url,omitempty"`
	RemoteURLFingerprint  string            `json:"remote_url_fingerprint,omitempty"`
	ConfigFingerprint     string            `json:"config_fingerprint,omitempty"`
	IndexFingerprint      string            `json:"index_fingerprint,omitempty"`
	IgnoreRuleFingerprint string            `json:"ignore_rule_fingerprint,omitempty"`
	DirtyState            string            `json:"dirty_state"`
	UntrackedSummary      string            `json:"untracked_summary,omitempty"`
	ToolIdentity          map[string]string `json:"tool_identity,omitempty"`
	HeadEpoch             string            `json:"head_epoch,omitempty"`
	ConfigEpoch           string            `json:"config_epoch,omitempty"`
	IndexEpoch            string            `json:"index_epoch,omitempty"`
	FileTreeEpoch         string            `json:"file_tree_epoch,omitempty"`
	FileContentEpoch      string            `json:"file_content_epoch,omitempty"`
	WorkspaceEpoch        string            `json:"workspace_epoch,omitempty"`
	EvidenceQuality       EvidenceQuality   `json:"evidence_quality"`
	OracleAvailable       bool              `json:"oracle_available"`
	OracleDiagnostics     []string          `json:"oracle_diagnostics,omitempty"`
	CollectedAtUnixNano   int64             `json:"collected_at_unix_nano"`
}

type Observation struct {
	OperationID  string    `json:"operation_id"`
	StdoutHash   string    `json:"stdout_hash"`
	StderrHash   string    `json:"stderr_hash"`
	StdoutSize   int       `json:"stdout_size"`
	StderrSize   int       `json:"stderr_size"`
	ExitCode     int       `json:"exit_code"`
	NativeWallMS int64     `json:"native_wall_ms"`
	Timestamp    time.Time `json:"timestamp"`
	OutputRef    string    `json:"output_ref,omitempty"`
}

type LedgerEntry struct {
	OperationKey        string            `json:"operation_key"`
	OperatorFamily      OperatorFamily    `json:"operator_family"`
	InputFingerprints   map[string]string `json:"input_fingerprints"`
	OutputFingerprints  map[string]string `json:"output_fingerprints"`
	InvalidationEpoch   string            `json:"invalidation_epoch"`
	ShadowMatchCount    int               `json:"shadow_match_count"`
	ShadowMismatchCount int               `json:"shadow_mismatch_count"`
	ReplacementCount    int               `json:"replacement_count"`
	FallbackCount       int               `json:"fallback_count"`
	NetROIHistoryMS     []int64           `json:"net_roi_history_ms,omitempty"`
	LastDecision        Mode              `json:"last_decision"`
	LastValidatedAt     time.Time         `json:"last_validated_at"`
	Observation         Observation       `json:"observation"`
	MismatchExamples    []string          `json:"mismatch_examples,omitempty"`
}

type ProofRecord struct {
	OperationKeyMatched        bool   `json:"operation_key_matched"`
	InputFingerprintsMatched   bool   `json:"input_fingerprints_matched"`
	InvalidationEpochUnchanged bool   `json:"invalidation_epoch_unchanged"`
	OperatorAllowlisted        bool   `json:"operator_allowlisted"`
	OutputAvailable            bool   `json:"output_available"`
	OutputExact                bool   `json:"output_exact"`
	PolicyAllowedReplay        bool   `json:"policy_allowed_replay"`
	NativeFallbackAvailable    bool   `json:"native_fallback_available"`
	OperationKey               string `json:"operation_key,omitempty"`
	Reason                     string `json:"reason,omitempty"`
}

type NativeResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Wall     time.Duration
	Err      error
}

type RunResult struct {
	Stdout      []byte
	Stderr      []byte
	ExitCode    int
	Mode        Mode
	Family      OperatorFamily
	Observation Observation
	Proof       *ProofRecord
	Diagnostics []string
	NativeWall  time.Duration
}
