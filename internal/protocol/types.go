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
	ScaleRequestsPerHour    int `json:"scale_requests_per_hour"`
	VerifyConcurrency       int `json:"verify_concurrency"`
	ScaleConcurrency        int `json:"scale_concurrency"`
}

func QuotasForTrustTier(tier string) Quotas {
	switch tier {
	case TrustAdmin:
		return Quotas{VerifyRequestsPerMinute: 240, ScaleRequestsPerHour: 120, VerifyConcurrency: 12, ScaleConcurrency: 8}
	case TrustScaleAllowed:
		return Quotas{VerifyRequestsPerMinute: 90, ScaleRequestsPerHour: 48, VerifyConcurrency: 8, ScaleConcurrency: 3}
	case TrustTrusted:
		return Quotas{VerifyRequestsPerMinute: 60, ScaleRequestsPerHour: 24, VerifyConcurrency: 6, ScaleConcurrency: 2}
	case TrustGitHubBasic:
		return Quotas{VerifyRequestsPerMinute: 30, ScaleRequestsPerHour: 12, VerifyConcurrency: 4, ScaleConcurrency: 1}
	default:
		return Quotas{}
	}
}

func FeaturesForTrustTier(tier string) []string {
	switch tier {
	case TrustAdmin:
		return []string{"verify", "scale", "admin"}
	case TrustScaleAllowed:
		return []string{"verify", "scale"}
	case TrustTrusted, TrustGitHubBasic:
		return []string{"verify", "scale"}
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
