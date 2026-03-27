package protocol

type AuditRequest struct {
	Kind           string       `json:"kind"`
	Language       string       `json:"language,omitempty"`
	Tool           string       `json:"tool,omitempty"`
	Config         string       `json:"config,omitempty"`
	Targets        []string     `json:"targets,omitempty"`
	Files          []SourceFile `json:"files"`
	TimeoutSeconds int          `json:"timeout_seconds,omitempty"`
}

type AuditSummary struct {
	Passed   int `json:"passed"`
	Failed   int `json:"failed"`
	Findings int `json:"findings"`
}

type AuditFinding struct {
	RuleID   string `json:"rule_id,omitempty"`
	Severity string `json:"severity,omitempty"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

type AuditResult struct {
	Target         string         `json:"target"`
	Status         string         `json:"status"`
	ExitCode       int            `json:"exit_code"`
	RuntimeMS      int64          `json:"runtime_ms"`
	Findings       []AuditFinding `json:"findings,omitempty"`
	FindingsCount  int            `json:"findings_count"`
	Stdout         string         `json:"stdout"`
	Stderr         string         `json:"stderr"`
	CombinedOutput string         `json:"combined_output"`
	Truncated      bool           `json:"truncated"`
}

type AuditResponse struct {
	RequestID     string         `json:"request_id"`
	Kind          string         `json:"kind"`
	Language      string         `json:"language,omitempty"`
	Tool          string         `json:"tool,omitempty"`
	Summary       AuditSummary   `json:"summary"`
	Results       []AuditResult  `json:"results"`
	FailureGroups []FailureGroup `json:"failure_groups"`
	AgentSummary  string         `json:"agent_summary"`
}

type BuildRequest struct {
	Language       string       `json:"language"`
	Targets        []string     `json:"targets"`
	Files          []SourceFile `json:"files"`
	TimeoutSeconds int          `json:"timeout_seconds,omitempty"`
}

type BuildSummary struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type BuildResult struct {
	Target         string       `json:"target"`
	Status         string       `json:"status"`
	ExitCode       int          `json:"exit_code"`
	RuntimeMS      int64        `json:"runtime_ms"`
	Artifacts      []Artifact   `json:"artifacts,omitempty"`
	Preview        ScalePreview `json:"preview"`
	Stdout         string       `json:"stdout"`
	Stderr         string       `json:"stderr"`
	CombinedOutput string       `json:"combined_output"`
	Truncated      bool         `json:"truncated"`
}

type BuildResponse struct {
	RequestID     string         `json:"request_id"`
	Language      string         `json:"language"`
	Summary       BuildSummary   `json:"summary"`
	Results       []BuildResult  `json:"results"`
	FailureGroups []FailureGroup `json:"failure_groups"`
	AgentSummary  string         `json:"agent_summary"`
}

type BenchRequest struct {
	Language       string       `json:"language"`
	Targets        []string     `json:"targets"`
	Files          []SourceFile `json:"files"`
	Iterations     int          `json:"iterations,omitempty"`
	TimeoutSeconds int          `json:"timeout_seconds,omitempty"`
}

type BenchSummary struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type BenchResult struct {
	Target         string  `json:"target"`
	Status         string  `json:"status"`
	ExitCode       int     `json:"exit_code"`
	RuntimeMS      int64   `json:"runtime_ms"`
	Iterations     int     `json:"iterations"`
	SamplesMS      []int64 `json:"samples_ms,omitempty"`
	MinRuntimeMS   int64   `json:"min_runtime_ms,omitempty"`
	MaxRuntimeMS   int64   `json:"max_runtime_ms,omitempty"`
	AvgRuntimeMS   int64   `json:"avg_runtime_ms,omitempty"`
	Stdout         string  `json:"stdout"`
	Stderr         string  `json:"stderr"`
	CombinedOutput string  `json:"combined_output"`
	Truncated      bool    `json:"truncated"`
}

type BenchResponse struct {
	RequestID     string         `json:"request_id"`
	Language      string         `json:"language"`
	Summary       BenchSummary   `json:"summary"`
	Results       []BenchResult  `json:"results"`
	FailureGroups []FailureGroup `json:"failure_groups"`
	AgentSummary  string         `json:"agent_summary"`
}

type BrowserRequest struct {
	Browser        string       `json:"browser"`
	Files          []SourceFile `json:"files,omitempty"`
	ScriptPath     string       `json:"script_path,omitempty"`
	URL            string       `json:"url,omitempty"`
	ScreenshotName string       `json:"screenshot_name,omitempty"`
	AllowNetwork   bool         `json:"allow_network,omitempty"`
	TimeoutSeconds int          `json:"timeout_seconds,omitempty"`
}

type BrowserConsoleEntry struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type BrowserResponse struct {
	RequestID      string                `json:"request_id"`
	Browser        string                `json:"browser"`
	Status         string                `json:"status"`
	RuntimeMS      int64                 `json:"runtime_ms"`
	FinalURL       string                `json:"final_url,omitempty"`
	Title          string                `json:"title,omitempty"`
	TextContent    string                `json:"text_content,omitempty"`
	Console        []BrowserConsoleEntry `json:"console,omitempty"`
	Artifacts      []Artifact            `json:"artifacts,omitempty"`
	Preview        ScalePreview          `json:"preview"`
	Stdout         string                `json:"stdout"`
	Stderr         string                `json:"stderr"`
	CombinedOutput string                `json:"combined_output"`
	Truncated      bool                  `json:"truncated"`
	AgentSummary   string                `json:"agent_summary"`
}
