package protocol

import "time"

const (
	TrustAnonymous    = "anonymous"
	TrustGitHubBasic  = "github_basic"
	TrustTrusted      = "trusted"
	TrustScaleAllowed = "scale_allowed"
	TrustAdmin        = "admin"
)

const (
	TokenTypeSession  = "session"
	TokenTypeHeadless = "headless"
)

const (
	ScopeUserRead   = "user:read"
	ScopeVerifyRun  = "verify:run"
	ScopeDataRun    = "data:run"
	ScopeMediaRun   = "media:run"
	ScopeDepsRun    = "deps:run"
	ScopeSQLRun     = "sql:run"
	ScopeCompileRun = "compile:run"
	ScopeSolveRun   = "solve:run"
	ScopeTestRun    = "test:run"
	ScopeLintRun    = "lint:run"
	ScopeAuditRun   = "audit:run"
	ScopeBuildRun   = "build:run"
	ScopeBenchRun   = "bench:run"
	ScopeBrowserRun = "browser:run"
	ScopeScaleData  = "scale:data"
	ScopeScaleMedia = "scale:media"
	ScopeAdmin      = "admin:*"
)

type CLIConfig struct {
	APIBaseURL   string    `json:"api_base_url"`
	SessionToken string    `json:"session_token"`
	UserID       string    `json:"user_id"`
	TrustTier    string    `json:"trust_tier"`
	TokenType    string    `json:"token_type"`
	CreatedAt    time.Time `json:"created_at"`
}

type Quotas struct {
	VerifyRequestsPerMinute int `json:"verify_requests_per_minute"`
	DataRequestsPerHour     int `json:"data_requests_per_hour"`
	MediaRequestsPerHour    int `json:"media_requests_per_hour"`
	DepsRequestsPerHour     int `json:"deps_requests_per_hour"`
	SQLRequestsPerHour      int `json:"sql_requests_per_hour"`
	CompileRequestsPerHour  int `json:"compile_requests_per_hour"`
	SolveRequestsPerHour    int `json:"solve_requests_per_hour"`
	TestRequestsPerHour     int `json:"test_requests_per_hour"`
	LintRequestsPerHour     int `json:"lint_requests_per_hour"`
	AuditRequestsPerHour    int `json:"audit_requests_per_hour"`
	BuildRequestsPerHour    int `json:"build_requests_per_hour"`
	BenchRequestsPerHour    int `json:"bench_requests_per_hour"`
	BrowserRequestsPerHour  int `json:"browser_requests_per_hour"`
	VerifyConcurrency       int `json:"verify_concurrency"`
	DataConcurrency         int `json:"data_concurrency"`
	MediaConcurrency        int `json:"media_concurrency"`
	DepsConcurrency         int `json:"deps_concurrency"`
	SQLConcurrency          int `json:"sql_concurrency"`
	CompileConcurrency      int `json:"compile_concurrency"`
	SolveConcurrency        int `json:"solve_concurrency"`
	TestConcurrency         int `json:"test_concurrency"`
	LintConcurrency         int `json:"lint_concurrency"`
	AuditConcurrency        int `json:"audit_concurrency"`
	BuildConcurrency        int `json:"build_concurrency"`
	BenchConcurrency        int `json:"bench_concurrency"`
	BrowserConcurrency      int `json:"browser_concurrency"`
}

func QuotasForTrustTier(tier string) Quotas {
	switch tier {
	case TrustAdmin:
		return Quotas{
			VerifyRequestsPerMinute: 240,
			DataRequestsPerHour:     120,
			MediaRequestsPerHour:    60,
			DepsRequestsPerHour:     120,
			SQLRequestsPerHour:      240,
			CompileRequestsPerHour:  120,
			SolveRequestsPerHour:    240,
			TestRequestsPerHour:     180,
			LintRequestsPerHour:     240,
			AuditRequestsPerHour:    180,
			BuildRequestsPerHour:    120,
			BenchRequestsPerHour:    120,
			BrowserRequestsPerHour:  60,
			VerifyConcurrency:       12,
			DataConcurrency:         8,
			MediaConcurrency:        4,
			DepsConcurrency:         6,
			SQLConcurrency:          10,
			CompileConcurrency:      6,
			SolveConcurrency:        10,
			TestConcurrency:         8,
			LintConcurrency:         10,
			AuditConcurrency:        8,
			BuildConcurrency:        6,
			BenchConcurrency:        6,
			BrowserConcurrency:      4,
		}
	case TrustScaleAllowed:
		return Quotas{
			VerifyRequestsPerMinute: 90,
			DataRequestsPerHour:     48,
			MediaRequestsPerHour:    24,
			DepsRequestsPerHour:     48,
			SQLRequestsPerHour:      120,
			CompileRequestsPerHour:  48,
			SolveRequestsPerHour:    120,
			TestRequestsPerHour:     80,
			LintRequestsPerHour:     120,
			AuditRequestsPerHour:    80,
			BuildRequestsPerHour:    40,
			BenchRequestsPerHour:    40,
			BrowserRequestsPerHour:  24,
			VerifyConcurrency:       8,
			DataConcurrency:         3,
			MediaConcurrency:        2,
			DepsConcurrency:         3,
			SQLConcurrency:          4,
			CompileConcurrency:      3,
			SolveConcurrency:        4,
			TestConcurrency:         3,
			LintConcurrency:         4,
			AuditConcurrency:        3,
			BuildConcurrency:        3,
			BenchConcurrency:        3,
			BrowserConcurrency:      2,
		}
	case TrustTrusted:
		return Quotas{
			VerifyRequestsPerMinute: 60,
			DataRequestsPerHour:     24,
			MediaRequestsPerHour:    12,
			DepsRequestsPerHour:     24,
			SQLRequestsPerHour:      60,
			CompileRequestsPerHour:  24,
			SolveRequestsPerHour:    60,
			TestRequestsPerHour:     40,
			LintRequestsPerHour:     80,
			AuditRequestsPerHour:    40,
			BuildRequestsPerHour:    20,
			BenchRequestsPerHour:    20,
			BrowserRequestsPerHour:  12,
			VerifyConcurrency:       6,
			DataConcurrency:         2,
			MediaConcurrency:        1,
			DepsConcurrency:         2,
			SQLConcurrency:          3,
			CompileConcurrency:      2,
			SolveConcurrency:        3,
			TestConcurrency:         2,
			LintConcurrency:         3,
			AuditConcurrency:        2,
			BuildConcurrency:        2,
			BenchConcurrency:        2,
			BrowserConcurrency:      1,
		}
	case TrustGitHubBasic:
		return Quotas{
			VerifyRequestsPerMinute: 30,
			DataRequestsPerHour:     12,
			MediaRequestsPerHour:    6,
			DepsRequestsPerHour:     12,
			SQLRequestsPerHour:      30,
			CompileRequestsPerHour:  12,
			SolveRequestsPerHour:    30,
			TestRequestsPerHour:     20,
			LintRequestsPerHour:     40,
			AuditRequestsPerHour:    20,
			BuildRequestsPerHour:    10,
			BenchRequestsPerHour:    10,
			BrowserRequestsPerHour:  6,
			VerifyConcurrency:       4,
			DataConcurrency:         1,
			MediaConcurrency:        1,
			DepsConcurrency:         1,
			SQLConcurrency:          2,
			CompileConcurrency:      1,
			SolveConcurrency:        2,
			TestConcurrency:         1,
			LintConcurrency:         2,
			AuditConcurrency:        1,
			BuildConcurrency:        1,
			BenchConcurrency:        1,
			BrowserConcurrency:      1,
		}
	default:
		return Quotas{}
	}
}

func FeaturesForTrustTier(tier string) []string {
	switch tier {
	case TrustAdmin:
		return []string{"verify", "data", "media", "deps", "sql", "test", "lint", "compile", "solve", "audit", "build", "bench", "browser", "scale", "admin"}
	case TrustScaleAllowed:
		return []string{"verify", "data", "media", "deps", "sql", "test", "lint", "compile", "solve", "audit", "build", "bench", "browser", "scale"}
	case TrustTrusted, TrustGitHubBasic:
		return []string{"verify", "data", "media", "deps", "sql", "test", "lint", "compile", "solve", "audit", "build", "bench", "browser", "scale"}
	default:
		return nil
	}
}

type ErrorResponse struct {
	Error     string `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

type LoginTokenRequest struct {
	Token string `json:"token"`
}

type LoginResponse struct {
	RequestID    string   `json:"request_id,omitempty"`
	UserID       string   `json:"user_id"`
	TrustTier    string   `json:"trust_tier"`
	TokenType    string   `json:"token_type"`
	FeatureFlags []string `json:"feature_flags"`
	Quotas       Quotas   `json:"quotas"`
}

type GitHubStartRequest struct {
	CallbackURL string `json:"callback_url"`
	ClientNonce string `json:"client_nonce"`
}

type GitHubStartResponse struct {
	RequestID string `json:"request_id"`
	AuthURL   string `json:"auth_url"`
	State     string `json:"state"`
}

type TokenCreateRequest struct {
	Name      string        `json:"name"`
	Scopes    []string      `json:"scopes"`
	ExpiresIn time.Duration `json:"expires_in"`
}

type TokenCreateResponse struct {
	RequestID string     `json:"request_id"`
	Token     string     `json:"token"`
	TokenID   string     `json:"token_id"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	TokenType string     `json:"token_type"`
}

type TokenRevokeRequest struct {
	TokenID string `json:"token_id"`
}

type TokenMetadata struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	TokenType  string     `json:"token_type"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

type MeResponse struct {
	RequestID    string          `json:"request_id,omitempty"`
	UserID       string          `json:"user_id"`
	Login        string          `json:"login,omitempty"`
	Email        string          `json:"email,omitempty"`
	TrustTier    string          `json:"trust_tier"`
	FeatureFlags []string        `json:"feature_flags"`
	Quotas       Quotas          `json:"quotas"`
	Tokens       []TokenMetadata `json:"tokens,omitempty"`
	Suspended    bool            `json:"suspended"`
	AbuseScore   int             `json:"abuse_score"`
}

type VerifyRequest struct {
	Language       string   `json:"language"`
	Targets        []string `json:"targets"`
	Code           string   `json:"code"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

type VerifySummary struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type VerifyResult struct {
	Target         string `json:"target"`
	Status         string `json:"status"`
	ExitCode       int    `json:"exit_code"`
	RuntimeMS      int64  `json:"runtime_ms"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	CombinedOutput string `json:"combined_output"`
	Truncated      bool   `json:"truncated"`
}

type FailureGroup struct {
	Kind        string   `json:"kind"`
	Targets     []string `json:"targets"`
	Signature   string   `json:"signature"`
	Explanation string   `json:"explanation"`
}

type VerifyResponse struct {
	RequestID     string         `json:"request_id"`
	Summary       VerifySummary  `json:"summary"`
	Results       []VerifyResult `json:"results"`
	FailureGroups []FailureGroup `json:"failure_groups"`
	AgentSummary  string         `json:"agent_summary"`
}

type DepsRequest struct {
	Language       string   `json:"language"`
	Targets        []string `json:"targets"`
	Manifest       string   `json:"manifest"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

type DepsSummary struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type DepsResult struct {
	Target         string `json:"target"`
	Status         string `json:"status"`
	ExitCode       int    `json:"exit_code"`
	RuntimeMS      int64  `json:"runtime_ms"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	CombinedOutput string `json:"combined_output"`
	Truncated      bool   `json:"truncated"`
}

type DepsResponse struct {
	RequestID     string         `json:"request_id"`
	Language      string         `json:"language"`
	Summary       DepsSummary    `json:"summary"`
	Results       []DepsResult   `json:"results"`
	FailureGroups []FailureGroup `json:"failure_groups"`
	AgentSummary  string         `json:"agent_summary"`
}

type SourceFile struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding,omitempty"`
}

type TestRequest struct {
	Language       string       `json:"language"`
	Targets        []string     `json:"targets"`
	Files          []SourceFile `json:"files"`
	Command        string       `json:"command,omitempty"`
	TimeoutSeconds int          `json:"timeout_seconds,omitempty"`
}

type TestSummary struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type TestResult struct {
	Target         string `json:"target"`
	Status         string `json:"status"`
	ExitCode       int    `json:"exit_code"`
	RuntimeMS      int64  `json:"runtime_ms"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	CombinedOutput string `json:"combined_output"`
	Truncated      bool   `json:"truncated"`
}

type TestResponse struct {
	RequestID     string         `json:"request_id"`
	Language      string         `json:"language"`
	Summary       TestSummary    `json:"summary"`
	Results       []TestResult   `json:"results"`
	FailureGroups []FailureGroup `json:"failure_groups"`
	AgentSummary  string         `json:"agent_summary"`
}

type LintRequest struct {
	Language       string       `json:"language"`
	Tool           string       `json:"tool"`
	Targets        []string     `json:"targets"`
	Files          []SourceFile `json:"files"`
	TimeoutSeconds int          `json:"timeout_seconds,omitempty"`
}

type LintSummary struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type LintResult struct {
	Target         string `json:"target"`
	Status         string `json:"status"`
	ExitCode       int    `json:"exit_code"`
	RuntimeMS      int64  `json:"runtime_ms"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	CombinedOutput string `json:"combined_output"`
	Truncated      bool   `json:"truncated"`
}

type LintResponse struct {
	RequestID     string         `json:"request_id"`
	Language      string         `json:"language"`
	Tool          string         `json:"tool"`
	Summary       LintSummary    `json:"summary"`
	Results       []LintResult   `json:"results"`
	FailureGroups []FailureGroup `json:"failure_groups"`
	AgentSummary  string         `json:"agent_summary"`
}

type CompileRequest struct {
	Language       string       `json:"language"`
	Targets        []string     `json:"targets"`
	Files          []SourceFile `json:"files"`
	TimeoutSeconds int          `json:"timeout_seconds,omitempty"`
}

type CompileSummary struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type CompileResult struct {
	Target         string `json:"target"`
	Status         string `json:"status"`
	ExitCode       int    `json:"exit_code"`
	RuntimeMS      int64  `json:"runtime_ms"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	CombinedOutput string `json:"combined_output"`
	Truncated      bool   `json:"truncated"`
}

type CompileResponse struct {
	RequestID     string          `json:"request_id"`
	Language      string          `json:"language"`
	Summary       CompileSummary  `json:"summary"`
	Results       []CompileResult `json:"results"`
	FailureGroups []FailureGroup  `json:"failure_groups"`
	AgentSummary  string          `json:"agent_summary"`
}

type ScaleJSONRequest struct {
	Mode           string `json:"mode"`
	ScriptName     string `json:"script_name"`
	Script         string `json:"script"`
	StdinText      string `json:"stdin_text,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type Artifact struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	DownloadURL string `json:"download_url"`
}

type ScalePreview struct {
	Text string                   `json:"text,omitempty"`
	Rows []map[string]interface{} `json:"rows,omitempty"`
}

type ScaleResponse struct {
	RequestID      string       `json:"request_id"`
	Status         string       `json:"status"`
	Mode           string       `json:"mode"`
	RuntimeMS      int64        `json:"runtime_ms"`
	Artifacts      []Artifact   `json:"artifacts,omitempty"`
	Preview        ScalePreview `json:"preview"`
	Stdout         string       `json:"stdout"`
	Stderr         string       `json:"stderr"`
	CombinedOutput string       `json:"combined_output"`
	Truncated      bool         `json:"truncated"`
	AgentSummary   string       `json:"agent_summary"`
}

type SQLRequest struct {
	Dialect        string `json:"dialect"`
	SQL            string `json:"sql,omitempty"`
	Schema         string `json:"schema,omitempty"`
	Query          string `json:"query,omitempty"`
	Explain        bool   `json:"explain,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type SQLResponse struct {
	RequestID         string          `json:"request_id"`
	Dialect           string          `json:"dialect"`
	Status            string          `json:"status"`
	RuntimeMS         int64           `json:"runtime_ms"`
	Columns           []string        `json:"columns,omitempty"`
	Rows              [][]interface{} `json:"rows,omitempty"`
	RowCount          int             `json:"row_count,omitempty"`
	StatementsApplied int             `json:"statements_applied,omitempty"`
	ExplainOutput     string          `json:"explain_output,omitempty"`
	Stdout            string          `json:"stdout"`
	Stderr            string          `json:"stderr"`
	CombinedOutput    string          `json:"combined_output"`
	Truncated         bool            `json:"truncated"`
	AgentSummary      string          `json:"agent_summary"`
}

type SolveRequest struct {
	Solver         string `json:"solver"`
	Input          string `json:"input,omitempty"`
	Model          string `json:"model,omitempty"`
	Data           string `json:"data,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type SolveResponse struct {
	RequestID      string            `json:"request_id"`
	Solver         string            `json:"solver"`
	Status         string            `json:"status"`
	RuntimeMS      int64             `json:"runtime_ms"`
	Model          map[string]string `json:"model,omitempty"`
	Solution       string            `json:"solution,omitempty"`
	Stdout         string            `json:"stdout"`
	Stderr         string            `json:"stderr"`
	CombinedOutput string            `json:"combined_output"`
	Truncated      bool              `json:"truncated"`
	AgentSummary   string            `json:"agent_summary"`
}

type JobResponse struct {
	RequestID string      `json:"request_id"`
	Type      string      `json:"type"`
	Status    string      `json:"status"`
	Result    interface{} `json:"result,omitempty"`
}

type AdminUserResponse struct {
	RequestID    string          `json:"request_id"`
	UserID       string          `json:"user_id"`
	Login        string          `json:"login,omitempty"`
	Email        string          `json:"email,omitempty"`
	TrustTier    string          `json:"trust_tier"`
	FeatureFlags []string        `json:"feature_flags"`
	Suspended    bool            `json:"suspended"`
	AbuseScore   int             `json:"abuse_score"`
	Tokens       []TokenMetadata `json:"tokens"`
}

type SuspendUserRequest struct {
	Reason string `json:"reason"`
}
