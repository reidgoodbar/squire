package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"squire/internal/config"
	"squire/internal/httpclient"
	"squire/internal/protocol"
)

const (
	exitOK          = 0
	exitUsage       = 1
	exitAuth        = 2
	exitRemoteError = 3
	exitTimeout     = 4
	exitSignal      = 130
)

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(args) == 0 {
		printRootHelp(stderr)
		return exitUsage
	}
	switch args[0] {
	case "login":
		return runLogin(ctx, args[1:], stdout, stderr)
	case "update":
		return runUpdate(ctx, args[1:], stdout, stderr)
	case "verify":
		return runVerify(ctx, args[1:], stdin, stdout, stderr)
	case "deps":
		return runDeps(ctx, args[1:], stdout, stderr)
	case "sql":
		return runSQL(ctx, args[1:], stdout, stderr)
	case "test":
		return runTest(ctx, args[1:], stdout, stderr)
	case "lint":
		return runLint(ctx, args[1:], stdout, stderr)
	case "audit":
		return runAudit(ctx, args[1:], stdout, stderr)
	case "build":
		return runBuild(ctx, args[1:], stdout, stderr)
	case "bench":
		return runBench(ctx, args[1:], stdout, stderr)
	case "browser":
		return runBrowser(ctx, args[1:], stdout, stderr)
	case "compile":
		return runCompile(ctx, args[1:], stdout, stderr)
	case "solve":
		return runSolve(ctx, args[1:], stdout, stderr)
	case "data":
		return runMode(ctx, args[1:], stdin, stdout, stderr, "data")
	case "media":
		return runMode(ctx, args[1:], stdin, stdout, stderr, "media")
	case "scale":
		return runScaleAlias(ctx, args[1:], stdin, stdout, stderr)
	case "whoami":
		return runWhoAmI(ctx, args[1:], stdout, stderr)
	case "logout":
		return runLogout(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		printRootHelp(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printRootHelp(stderr)
		return exitUsage
	}
}

func runLogin(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	token := fs.String("token", "", "Squire-issued headless token for CI or agent login")
	jsonOut := fs.Bool("json", false, "Print machine-readable JSON output")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this login")
	fs.Usage = func() { printLoginHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	cfg, _ := config.Load()
	baseURL := pickBaseURL(*apiBaseURL, cfg.APIBaseURL)
	client := httpclient.New(baseURL, "")

	if *token != "" {
		var resp protocol.LoginResponse
		if err := client.DoJSON(ctx, http.MethodPost, "/v1/auth/token/login", protocol.LoginTokenRequest{Token: *token}, &resp); err != nil {
			fmt.Fprintln(stderr, err)
			return exitAuth
		}
		if err := config.Save(protocol.CLIConfig{
			APIBaseURL:   baseURL,
			SessionToken: *token,
			UserID:       resp.UserID,
			TrustTier:    resp.TrustTier,
			TokenType:    resp.TokenType,
			CreatedAt:    time.Now().UTC(),
		}); err != nil {
			fmt.Fprintln(stderr, err)
			return exitRemoteError
		}
		if *jsonOut {
			_ = json.NewEncoder(stdout).Encode(map[string]interface{}{
				"status":        "ok",
				"user_id":       resp.UserID,
				"trust_tier":    resp.TrustTier,
				"token_type":    resp.TokenType,
				"feature_flags": resp.FeatureFlags,
			})
			return exitOK
		}
		fmt.Fprintf(stdout, "logged in as %s (%s)\n", resp.UserID, resp.TrustTier)
		return exitOK
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(stderr, "failed to start local callback server:", err)
		return exitRemoteError
	}
	defer listener.Close()

	clientNonce := fmt.Sprintf("%d", time.Now().UnixNano())
	callbackURL := "http://" + listener.Addr().String() + "/callback"
	var startResp protocol.GitHubStartResponse
	if err := client.DoJSON(ctx, http.MethodPost, "/v1/auth/github/start", protocol.GitHubStartRequest{
		CallbackURL: callbackURL,
		ClientNonce: clientNonce,
	}, &startResp); err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}

	type loginResult struct {
		TokenType    string
		SessionToken string
		UserID       string
		TrustTier    string
		Err          error
	}
	resultCh := make(chan loginResult, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("client_nonce") != clientNonce {
			http.Error(w, "invalid login nonce", http.StatusBadRequest)
			resultCh <- loginResult{Err: errors.New("invalid login nonce")}
			return
		}
		fmt.Fprint(w, "Squire login complete. You can close this window.")
		resultCh <- loginResult{
			TokenType:    query.Get("token_type"),
			SessionToken: query.Get("session_token"),
			UserID:       query.Get("user_id"),
			TrustTier:    query.Get("trust_tier"),
		}
	})
	go server.Serve(listener)
	defer server.Close()

	if err := openBrowser(startResp.AuthURL); err != nil {
		fmt.Fprintln(stderr, "Open this URL in a browser:", startResp.AuthURL)
	}

	select {
	case <-ctx.Done():
		fmt.Fprintln(stderr, "login interrupted")
		return exitSignal
	case result := <-resultCh:
		if result.Err != nil {
			fmt.Fprintln(stderr, result.Err)
			return exitAuth
		}
		if result.SessionToken == "" {
			fmt.Fprintln(stderr, "login failed: missing session token")
			return exitAuth
		}
		if err := config.Save(protocol.CLIConfig{
			APIBaseURL:   baseURL,
			SessionToken: result.SessionToken,
			UserID:       result.UserID,
			TrustTier:    result.TrustTier,
			TokenType:    result.TokenType,
			CreatedAt:    time.Now().UTC(),
		}); err != nil {
			fmt.Fprintln(stderr, err)
			return exitRemoteError
		}
		if *jsonOut {
			_ = json.NewEncoder(stdout).Encode(map[string]interface{}{
				"status":     "ok",
				"user_id":    result.UserID,
				"trust_tier": result.TrustTier,
				"token_type": result.TokenType,
			})
			return exitOK
		}
		fmt.Fprintf(stdout, "logged in as %s (%s)\n", result.UserID, result.TrustTier)
		return exitOK
	}
}

func runVerify(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lang := fs.String("lang", "", "Language to execute: bash, python, or node")
	filePath := fs.String("file", "", "Path to the script file to upload")
	inline := fs.String("code", "", "Inline code snippet to verify")
	targets := fs.String("targets", "alpine-3.20,ubuntu-24.04,debian-12", "Comma-separated target matrix")
	timeout := fs.Int("timeout", 20, "Per-target timeout in seconds")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printVerifyHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *lang == "" {
		fmt.Fprintln(stderr, "--lang is required")
		return exitUsage
	}

	code, err := resolveCode(*inline, *filePath, stdin)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.VerifyResponse
	err = client.DoJSON(ctx, http.MethodPost, "/v1/verify", protocol.VerifyRequest{
		Language:       *lang,
		Targets:        splitCSV(*targets),
		Code:           code,
		TimeoutSeconds: *timeout,
	}, &resp)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %d passed, %d failed\n", resp.RequestID, resp.Summary.Passed, resp.Summary.Failed)
		for _, result := range resp.Results {
			fmt.Fprintf(stdout, "- %s: %s (%d ms)\n", result.Target, result.Status, result.RuntimeMS)
		}
		if resp.AgentSummary != "" {
			fmt.Fprintln(stdout, resp.AgentSummary)
		}
	}
	if resp.Summary.Failed > 0 {
		return exitRemoteError
	}
	return exitOK
}

func runDeps(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("deps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lang := fs.String("lang", "", "Dependency language: python or node")
	filePath := fs.String("file", "", "Path to the dependency manifest to upload")
	targets := fs.String("targets", "", "Comma-separated dependency targets")
	timeout := fs.Int("timeout", 45, "Dependency install timeout in seconds")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printDepsHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *lang == "" || *filePath == "" {
		fmt.Fprintln(stderr, "--lang and --file are required")
		return exitUsage
	}

	manifest, err := os.ReadFile(*filePath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.DepsResponse
	if err := client.DoJSON(ctx, http.MethodPost, "/v1/deps", protocol.DepsRequest{
		Language:       *lang,
		Targets:        splitCSV(*targets),
		Manifest:       string(manifest),
		TimeoutSeconds: *timeout,
	}, &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}

	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %d passed, %d failed\n", resp.RequestID, resp.Summary.Passed, resp.Summary.Failed)
		for _, result := range resp.Results {
			fmt.Fprintf(stdout, "- %s: %s (%d ms)\n", result.Target, result.Status, result.RuntimeMS)
		}
		if resp.AgentSummary != "" {
			fmt.Fprintln(stdout, resp.AgentSummary)
		}
	}
	if resp.Summary.Failed > 0 {
		return exitRemoteError
	}
	return exitOK
}

func runSQL(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sql", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dialect := fs.String("dialect", "", "SQL dialect: sqlite or postgres-16")
	filePath := fs.String("file", "", "Path to a SQL file containing statements to apply")
	schemaPath := fs.String("schema", "", "Path to a schema file to apply before the query")
	query := fs.String("query", "", "Inline SQL query to execute after schema setup")
	queryFile := fs.String("query-file", "", "Path to a query file to execute after schema setup")
	timeout := fs.Int("timeout", 30, "SQL timeout in seconds")
	explain := fs.Bool("explain", false, "Request an execution plan when the dialect supports it")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printSQLHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dialect == "" {
		fmt.Fprintln(stderr, "--dialect is required")
		return exitUsage
	}
	if *filePath == "" && *schemaPath == "" && *query == "" && *queryFile == "" {
		fmt.Fprintln(stderr, "provide --file, --schema, --query, or --query-file")
		return exitUsage
	}
	if *filePath != "" && (*schemaPath != "" || *query != "" || *queryFile != "") {
		fmt.Fprintln(stderr, "--file cannot be combined with --schema, --query, or --query-file")
		return exitUsage
	}

	sqlText, err := readOptionalFile(*filePath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	schemaText, err := readOptionalFile(*schemaPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	queryText := *query
	if *queryFile != "" {
		if queryText != "" {
			fmt.Fprintln(stderr, "--query and --query-file are mutually exclusive")
			return exitUsage
		}
		queryText, err = readOptionalFile(*queryFile)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.SQLResponse
	if err := client.DoJSON(ctx, http.MethodPost, "/v1/sql", protocol.SQLRequest{
		Dialect:        *dialect,
		SQL:            sqlText,
		Schema:         schemaText,
		Query:          queryText,
		Explain:        *explain,
		TimeoutSeconds: *timeout,
	}, &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %s (%s, %d ms)\n", resp.RequestID, resp.Status, resp.Dialect, resp.RuntimeMS)
		if len(resp.Columns) > 0 {
			fmt.Fprintf(stdout, "columns: %s\n", strings.Join(resp.Columns, ", "))
		}
		if resp.RowCount > 0 {
			fmt.Fprintf(stdout, "rows: %d\n", resp.RowCount)
		}
		if resp.StatementsApplied > 0 {
			fmt.Fprintf(stdout, "statements applied: %d\n", resp.StatementsApplied)
		}
		if resp.AgentSummary != "" {
			fmt.Fprintln(stdout, resp.AgentSummary)
		}
	}
	switch resp.Status {
	case "ok":
		return exitOK
	case "timeout":
		return exitTimeout
	default:
		return exitRemoteError
	}
}

func runTest(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lang := fs.String("lang", "", "Test language: python, node, or bash")
	var files multiStringFlag
	fs.Var(&files, "file", "Path to a source, test, or manifest file to stage; repeat for multi-file inputs")
	cmd := fs.String("cmd", "", "Restricted test command, such as \"pytest -q\" or \"npm test\"")
	targets := fs.String("targets", "", "Comma-separated runtime targets")
	timeout := fs.Int("timeout", 60, "Test timeout in seconds")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printTestHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *lang == "" || len(files) == 0 {
		fmt.Fprintln(stderr, "--lang and at least one --file are required")
		return exitUsage
	}

	reqFiles := make([]protocol.SourceFile, 0, len(files))
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		reqFiles = append(reqFiles, protocol.SourceFile{
			Path:    requestPathForLocalFile(path),
			Content: string(data),
		})
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.TestResponse
	if err := client.DoJSON(ctx, http.MethodPost, "/v1/test", protocol.TestRequest{
		Language:       *lang,
		Targets:        splitCSV(*targets),
		Files:          reqFiles,
		Command:        *cmd,
		TimeoutSeconds: *timeout,
	}, &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %d passed, %d failed\n", resp.RequestID, resp.Summary.Passed, resp.Summary.Failed)
		for _, result := range resp.Results {
			fmt.Fprintf(stdout, "- %s: %s (%d ms)\n", result.Target, result.Status, result.RuntimeMS)
		}
		if resp.AgentSummary != "" {
			fmt.Fprintln(stdout, resp.AgentSummary)
		}
	}
	if resp.Summary.Failed > 0 {
		return exitRemoteError
	}
	return exitOK
}

func runLint(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lang := fs.String("lang", "", "Lint language: python, js, ts, or rust")
	tool := fs.String("tool", "", "Lint tool: ruff, eslint, or clippy")
	var files multiStringFlag
	fs.Var(&files, "file", "Path to a source or config file to stage; repeat for multi-file inputs")
	targets := fs.String("targets", "", "Comma-separated lint targets")
	timeout := fs.Int("timeout", 45, "Lint timeout in seconds")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printLintHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *lang == "" || *tool == "" || len(files) == 0 {
		fmt.Fprintln(stderr, "--lang, --tool, and at least one --file are required")
		return exitUsage
	}

	reqFiles := make([]protocol.SourceFile, 0, len(files))
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		reqFiles = append(reqFiles, protocol.SourceFile{
			Path:    requestPathForLocalFile(path),
			Content: string(data),
		})
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.LintResponse
	if err := client.DoJSON(ctx, http.MethodPost, "/v1/lint", protocol.LintRequest{
		Language:       *lang,
		Tool:           *tool,
		Targets:        splitCSV(*targets),
		Files:          reqFiles,
		TimeoutSeconds: *timeout,
	}, &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %d passed, %d failed\n", resp.RequestID, resp.Summary.Passed, resp.Summary.Failed)
		for _, result := range resp.Results {
			fmt.Fprintf(stdout, "- %s: %s (%d ms)\n", result.Target, result.Status, result.RuntimeMS)
		}
		if resp.AgentSummary != "" {
			fmt.Fprintln(stdout, resp.AgentSummary)
		}
	}
	if resp.Summary.Failed > 0 {
		return exitRemoteError
	}
	return exitOK
}

func runAudit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lang := fs.String("lang", "", "Audit language. Phase 2 supports python dependency audit.")
	secrets := fs.Bool("secrets", false, "Run the built-in secret scanner")
	static := fs.Bool("static", false, "Run static analysis")
	tool := fs.String("tool", "", "Audit tool. Phase 2 supports semgrep for static analysis.")
	configValue := fs.String("config", "", "Static analysis config, such as p/security or a staged config file path")
	var files multiStringFlag
	var paths multiStringFlag
	fs.Var(&files, "file", "Path to a file to stage; repeat for multiple files")
	fs.Var(&paths, "path", "Path to a directory tree to stage; repeat for multiple paths")
	targets := fs.String("targets", "", "Comma-separated audit targets")
	timeout := fs.Int("timeout", 60, "Audit timeout in seconds")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printAuditHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *secrets && *static {
		fmt.Fprintln(stderr, "choose only one of --secrets or --static")
		return exitUsage
	}

	kind := "deps"
	switch {
	case *secrets:
		kind = "secrets"
	case *static:
		kind = "static"
	}
	if kind == "deps" && *lang == "" {
		fmt.Fprintln(stderr, "--lang is required for dependency audit")
		return exitUsage
	}

	reqFiles, err := collectRequestFiles(files, paths)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if len(reqFiles) == 0 {
		fmt.Fprintln(stderr, "at least one --file or --path input is required")
		return exitUsage
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.AuditResponse
	if err := client.DoJSON(ctx, http.MethodPost, "/v1/audit", protocol.AuditRequest{
		Kind:           kind,
		Language:       *lang,
		Tool:           *tool,
		Config:         *configValue,
		Targets:        splitCSV(*targets),
		Files:          reqFiles,
		TimeoutSeconds: *timeout,
	}, &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %d passed, %d failed, %d findings\n", resp.RequestID, resp.Summary.Passed, resp.Summary.Failed, resp.Summary.Findings)
		for _, result := range resp.Results {
			fmt.Fprintf(stdout, "- %s: %s (%d ms, %d findings)\n", result.Target, result.Status, result.RuntimeMS, result.FindingsCount)
		}
		if resp.AgentSummary != "" {
			fmt.Fprintln(stdout, resp.AgentSummary)
		}
	}
	if resp.Summary.Failed > 0 {
		return exitRemoteError
	}
	return exitOK
}

func runBuild(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lang := fs.String("lang", "", "Build language: python or node")
	var files multiStringFlag
	var paths multiStringFlag
	fs.Var(&files, "file", "Path to a file to stage; repeat for multiple files")
	fs.Var(&paths, "path", "Path to a directory tree to stage; repeat for multiple paths")
	targets := fs.String("targets", "", "Comma-separated build targets")
	timeout := fs.Int("timeout", 120, "Build timeout in seconds")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printBuildHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *lang == "" {
		fmt.Fprintln(stderr, "--lang is required")
		return exitUsage
	}

	reqFiles, err := collectRequestFiles(files, paths)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if len(reqFiles) == 0 {
		fmt.Fprintln(stderr, "at least one --file or --path input is required")
		return exitUsage
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.BuildResponse
	if err := client.DoJSON(ctx, http.MethodPost, "/v1/build", protocol.BuildRequest{
		Language:       *lang,
		Targets:        splitCSV(*targets),
		Files:          reqFiles,
		TimeoutSeconds: *timeout,
	}, &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %d passed, %d failed\n", resp.RequestID, resp.Summary.Passed, resp.Summary.Failed)
		for _, result := range resp.Results {
			fmt.Fprintf(stdout, "- %s: %s (%d ms, %d artifacts)\n", result.Target, result.Status, result.RuntimeMS, len(result.Artifacts))
		}
		if resp.AgentSummary != "" {
			fmt.Fprintln(stdout, resp.AgentSummary)
		}
	}
	if resp.Summary.Failed > 0 {
		return exitRemoteError
	}
	return exitOK
}

func runBench(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lang := fs.String("lang", "", "Benchmark language: python, bash, or go")
	var files multiStringFlag
	var paths multiStringFlag
	fs.Var(&files, "file", "Path to a file to stage; repeat for multiple files")
	fs.Var(&paths, "path", "Path to a directory tree to stage; repeat for multiple paths")
	targets := fs.String("targets", "", "Comma-separated benchmark targets")
	iterations := fs.Int("iterations", 5, "Number of iterations to run per target")
	timeout := fs.Int("timeout", 60, "Benchmark timeout in seconds")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printBenchHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *lang == "" {
		fmt.Fprintln(stderr, "--lang is required")
		return exitUsage
	}

	reqFiles, err := collectRequestFiles(files, paths)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if len(reqFiles) == 0 {
		fmt.Fprintln(stderr, "at least one --file or --path input is required")
		return exitUsage
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.BenchResponse
	if err := client.DoJSON(ctx, http.MethodPost, "/v1/bench", protocol.BenchRequest{
		Language:       *lang,
		Targets:        splitCSV(*targets),
		Files:          reqFiles,
		Iterations:     *iterations,
		TimeoutSeconds: *timeout,
	}, &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %d passed, %d failed\n", resp.RequestID, resp.Summary.Passed, resp.Summary.Failed)
		for _, result := range resp.Results {
			fmt.Fprintf(stdout, "- %s: %s (%d ms avg over %d iteration(s))\n", result.Target, result.Status, result.AvgRuntimeMS, result.Iterations)
		}
		if resp.AgentSummary != "" {
			fmt.Fprintln(stdout, resp.AgentSummary)
		}
	}
	if resp.Summary.Failed > 0 {
		return exitRemoteError
	}
	return exitOK
}

func runBrowser(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("browser", flag.ContinueOnError)
	fs.SetOutput(stderr)
	browser := fs.String("browser", "chromium", "Browser runtime. Phase 3 supports chromium.")
	scriptPath := fs.String("script", "", "Path to a browser automation script to stage")
	url := fs.String("url", "", "URL to open. Remote URLs require --allow-network")
	screenshot := fs.String("screenshot", "", "Optional screenshot filename to produce as an artifact")
	allowNetwork := fs.Bool("allow-network", false, "Allow outbound browser network access for remote URLs")
	var files multiStringFlag
	var paths multiStringFlag
	fs.Var(&files, "file", "Path to a file to stage; repeat for multiple files")
	fs.Var(&paths, "path", "Path to a directory tree to stage; repeat for multiple paths")
	timeout := fs.Int("timeout", 45, "Browser timeout in seconds")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printBrowserHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if *scriptPath != "" {
		files = append(files, *scriptPath)
	}
	reqFiles, err := collectRequestFiles(files, paths)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	requestScriptPath := ""
	if *scriptPath != "" {
		requestScriptPath = requestPathForLocalFile(*scriptPath)
	}
	if *url == "" && requestScriptPath == "" && len(reqFiles) == 0 {
		fmt.Fprintln(stderr, "provide --url, --script, or staged files for browser execution")
		return exitUsage
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.BrowserResponse
	if err := client.DoJSON(ctx, http.MethodPost, "/v1/browser", protocol.BrowserRequest{
		Browser:        *browser,
		Files:          reqFiles,
		ScriptPath:     requestScriptPath,
		URL:            *url,
		ScreenshotName: *screenshot,
		AllowNetwork:   *allowNetwork,
		TimeoutSeconds: *timeout,
	}, &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %s (%s, %d ms)\n", resp.RequestID, resp.Status, resp.Browser, resp.RuntimeMS)
		if resp.Title != "" {
			fmt.Fprintf(stdout, "title: %s\n", resp.Title)
		}
		if resp.FinalURL != "" {
			fmt.Fprintf(stdout, "url: %s\n", resp.FinalURL)
		}
		if len(resp.Artifacts) > 0 {
			fmt.Fprintf(stdout, "artifacts: %d\n", len(resp.Artifacts))
		}
		if resp.AgentSummary != "" {
			fmt.Fprintln(stdout, resp.AgentSummary)
		}
	}
	switch resp.Status {
	case "ok":
		return exitOK
	case "timeout":
		return exitTimeout
	default:
		return exitRemoteError
	}
}

func runCompile(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("compile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lang := fs.String("lang", "", "Compile language: go or rust")
	var files multiStringFlag
	fs.Var(&files, "file", "Path to a source or manifest file to stage; repeat for multi-file inputs")
	targets := fs.String("targets", "", "Comma-separated compile targets")
	timeout := fs.Int("timeout", 90, "Compile timeout in seconds")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printCompileHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *lang == "" || len(files) == 0 {
		fmt.Fprintln(stderr, "--lang and at least one --file are required")
		return exitUsage
	}

	reqFiles := make([]protocol.SourceFile, 0, len(files))
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		reqFiles = append(reqFiles, protocol.SourceFile{
			Path:    requestPathForLocalFile(path),
			Content: string(data),
		})
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.CompileResponse
	if err := client.DoJSON(ctx, http.MethodPost, "/v1/compile", protocol.CompileRequest{
		Language:       *lang,
		Targets:        splitCSV(*targets),
		Files:          reqFiles,
		TimeoutSeconds: *timeout,
	}, &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}

	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %d passed, %d failed\n", resp.RequestID, resp.Summary.Passed, resp.Summary.Failed)
		for _, result := range resp.Results {
			fmt.Fprintf(stdout, "- %s: %s (%d ms)\n", result.Target, result.Status, result.RuntimeMS)
		}
		if resp.AgentSummary != "" {
			fmt.Fprintln(stdout, resp.AgentSummary)
		}
	}
	if resp.Summary.Failed > 0 {
		return exitRemoteError
	}
	return exitOK
}

func runSolve(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("solve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	solver := fs.String("solver", "", "Solver: z3 or minizinc")
	filePath := fs.String("file", "", "Path to the solver input file")
	dataPath := fs.String("data", "", "Optional MiniZinc .dzn data file")
	timeout := fs.Int("timeout", 20, "Solver timeout in seconds")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printSolveHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *solver == "" || *filePath == "" {
		fmt.Fprintln(stderr, "--solver and --file are required")
		return exitUsage
	}

	input, err := os.ReadFile(*filePath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	data := ""
	if *dataPath != "" {
		contents, err := os.ReadFile(*dataPath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		data = string(contents)
	}

	req := protocol.SolveRequest{Solver: *solver, TimeoutSeconds: *timeout}
	switch strings.ToLower(strings.TrimSpace(*solver)) {
	case "z3":
		req.Input = string(input)
	case "minizinc":
		req.Model = string(input)
		req.Data = data
	default:
		fmt.Fprintln(stderr, "unsupported solver")
		return exitUsage
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.SolveResponse
	if err := client.DoJSON(ctx, http.MethodPost, "/v1/solve", req, &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %s (%s, %d ms)\n", resp.RequestID, resp.Status, resp.Solver, resp.RuntimeMS)
		if resp.AgentSummary != "" {
			fmt.Fprintln(stdout, resp.AgentSummary)
		}
	}
	switch resp.Status {
	case "sat", "unsat", "unknown":
		return exitOK
	case "timeout":
		return exitTimeout
	default:
		return exitRemoteError
	}
}

func runMode(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, mode string) int {
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	scriptPath := fs.String("script", "", "Path to the Python script to execute")
	inputPath := fs.String("input", "", "Path to an input file for multipart upload")
	useStdin := fs.Bool("stdin", false, "Read small input payload from stdin and send JSON")
	timeout := fs.Int("timeout", 120, "Job timeout in seconds")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() {
		if mode == "media" {
			printMediaHelp(fs.Output())
			return
		}
		printDataHelp(fs.Output())
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *scriptPath == "" {
		fmt.Fprintln(stderr, "--script is required")
		return exitUsage
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.ScaleResponse

	if *useStdin {
		stdinLimit := scaleStdinMaxBytes()
		data, err := io.ReadAll(io.LimitReader(stdin, stdinLimit+1))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		if int64(len(data)) > stdinLimit {
			fmt.Fprintf(stderr, "stdin payload exceeds %d bytes; use --input for large files\n", stdinLimit)
			return exitUsage
		}
		scriptData, err := os.ReadFile(*scriptPath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		err = client.DoJSON(ctx, http.MethodPost, "/v1/"+mode, protocol.ScaleJSONRequest{
			Mode:           mode,
			ScriptName:     filepath.Base(*scriptPath),
			Script:         string(scriptData),
			StdinText:      string(data),
			TimeoutSeconds: *timeout,
		}, &resp)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitRemoteError
		}
	} else {
		req, err := newScaleMultipartRequest(ctx, client.BaseURL+"/v1/"+mode, cfg.SessionToken, mode, *scriptPath, *inputPath, *timeout)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		if err := client.Do(ctx, req, &resp); err != nil {
			fmt.Fprintln(stderr, err)
			return exitRemoteError
		}
	}

	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %s (%s, %d ms)\n", resp.RequestID, resp.Status, resp.Mode, resp.RuntimeMS)
		for _, artifact := range resp.Artifacts {
			fmt.Fprintf(stdout, "- artifact: %s (%d bytes)\n", artifact.Name, artifact.SizeBytes)
		}
		if resp.AgentSummary != "" {
			fmt.Fprintln(stdout, resp.AgentSummary)
		}
	}
	switch resp.Status {
	case "ok":
		return exitOK
	case "timeout":
		return exitTimeout
	default:
		return exitRemoteError
	}
}

func runScaleAlias(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scale", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "", "Compatibility alias for data or media")
	scriptPath := fs.String("script", "", "Path to the Python script to execute")
	inputPath := fs.String("input", "", "Path to an input file for multipart upload")
	useStdin := fs.Bool("stdin", false, "Read small input payload from stdin and send JSON")
	timeout := fs.Int("timeout", 120, "Job timeout in seconds")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printScaleAliasHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *mode == "" || *scriptPath == "" {
		fmt.Fprintln(stderr, "--mode and --script are required")
		return exitUsage
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.ScaleResponse

	if *useStdin {
		stdinLimit := scaleStdinMaxBytes()
		data, err := io.ReadAll(io.LimitReader(stdin, stdinLimit+1))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		if int64(len(data)) > stdinLimit {
			fmt.Fprintf(stderr, "stdin payload exceeds %d bytes; use --input for large files\n", stdinLimit)
			return exitUsage
		}
		scriptData, err := os.ReadFile(*scriptPath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		err = client.DoJSON(ctx, http.MethodPost, "/v1/"+*mode, protocol.ScaleJSONRequest{
			Mode:           *mode,
			ScriptName:     filepath.Base(*scriptPath),
			Script:         string(scriptData),
			StdinText:      string(data),
			TimeoutSeconds: *timeout,
		}, &resp)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitRemoteError
		}
	} else {
		req, err := newScaleMultipartRequest(ctx, client.BaseURL+"/v1/"+*mode, cfg.SessionToken, *mode, *scriptPath, *inputPath, *timeout)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		if err := client.Do(ctx, req, &resp); err != nil {
			fmt.Fprintln(stderr, err)
			return exitRemoteError
		}
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %s (%s, %d ms)\n", resp.RequestID, resp.Status, resp.Mode, resp.RuntimeMS)
	}
	switch resp.Status {
	case "ok":
		return exitOK
	case "timeout":
		return exitTimeout
	default:
		return exitRemoteError
	}
}

func runWhoAmI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printWhoAmIHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.MeResponse
	if err := client.DoJSON(ctx, http.MethodGet, "/v1/me", nil, &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
		return exitOK
	}
	fmt.Fprintf(stdout, "user: %s\ntrust: %s\nverify rpm: %d\ndata rph: %d\nmedia rph: %d\ndeps rph: %d\nsql rph: %d\ntest rph: %d\nlint rph: %d\naudit rph: %d\nbuild rph: %d\nbench rph: %d\nbrowser rph: %d\ncompile rph: %d\nsolve rph: %d\nfeatures: %s\n",
		resp.UserID,
		resp.TrustTier,
		resp.Quotas.VerifyRequestsPerMinute,
		resp.Quotas.DataRequestsPerHour,
		resp.Quotas.MediaRequestsPerHour,
		resp.Quotas.DepsRequestsPerHour,
		resp.Quotas.SQLRequestsPerHour,
		resp.Quotas.TestRequestsPerHour,
		resp.Quotas.LintRequestsPerHour,
		resp.Quotas.AuditRequestsPerHour,
		resp.Quotas.BuildRequestsPerHour,
		resp.Quotas.BenchRequestsPerHour,
		resp.Quotas.BrowserRequestsPerHour,
		resp.Quotas.CompileRequestsPerHour,
		resp.Quotas.SolveRequestsPerHour,
		strings.Join(resp.FeatureFlags, ","),
	)
	return exitOK
}

func runLogout(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	fs.Usage = func() { printLogoutHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := config.Clear(); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(map[string]string{"status": "logged_out"})
	} else {
		fmt.Fprintln(stdout, "logged out")
	}
	return exitOK
}

func newScaleMultipartRequest(ctx context.Context, endpoint, token, mode, scriptPath, inputPath string, timeout int) (*http.Request, error) {
	pipeReader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)

	go func() {
		defer pipeWriter.Close()
		defer writer.Close()
		writeField := func(name, value string) error {
			return writer.WriteField(name, value)
		}
		if err := writeField("mode", mode); err != nil {
			_ = pipeWriter.CloseWithError(err)
			return
		}
		if timeout > 0 {
			if err := writeField("timeout_seconds", fmt.Sprintf("%d", timeout)); err != nil {
				_ = pipeWriter.CloseWithError(err)
				return
			}
		}
		if err := streamMultipartFile(writer, "script", scriptPath); err != nil {
			_ = pipeWriter.CloseWithError(err)
			return
		}
		if inputPath != "" {
			if err := streamMultipartFile(writer, "input", inputPath); err != nil {
				_ = pipeWriter.CloseWithError(err)
				return
			}
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, pipeReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func streamMultipartFile(writer *multipart.Writer, fieldName, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	part, err := writer.CreateFormFile(fieldName, filepath.Base(path))
	if err != nil {
		return err
	}
	_, err = io.Copy(part, file)
	return err
}

func resolveCode(inline, filePath string, stdin io.Reader) (string, error) {
	switch {
	case inline != "":
		return inline, nil
	case filePath != "":
		data, err := os.ReadFile(filePath)
		return string(data), err
	default:
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", err
		}
		if len(data) == 0 {
			return "", errors.New("provide --code, --file, or stdin input")
		}
		return string(data), nil
	}
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func pickBaseURL(flagValue, configValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if configValue != "" {
		return configValue
	}
	return config.DefaultAPIBaseURL()
}

func scaleStdinMaxBytes() int64 {
	const defaultLimit int64 = 512 * 1024
	if value := os.Getenv("SQUIRE_SCALE_STDIN_MAX_BYTES"); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			return parsed
		}
	}
	return defaultLimit
}

func openBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}

func printRootHelp(w io.Writer) {
	fmt.Fprint(w, `Squire is a zero-trust CLI for stateless remote execution.

Commands:
  squire login
  squire update
  squire verify
  squire deps
  squire sql
  squire test
  squire lint
  squire audit
  squire build
  squire bench
  squire browser
  squire compile
  squire solve
  squire data
  squire media
  squire whoami
  squire logout

Use "squire <command> --help" for command-specific examples.
`)
}

func printLoginHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire login [--token <SQUIRE_TOKEN>] [--json]

Default behavior opens a browser for GitHub OAuth and stores session config in ~/.squire/config.json.

Examples:
  squire login
  squire login --token sqh_...
`)
}

func printUpdateHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire update [--version <tag|latest>] [--install-dir <path>] [--json]

Downloads the published CLI release for the current OS and architecture and replaces the local squire binary in place.

Examples:
  squire update
  squire update --version v0.5.1
  squire update --install-dir ~/.local/bin --json
`)
}

func printVerifyHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire verify --lang <bash|python|node> [--code <snippet> | --file <path>] [--targets <csv>] [--timeout <seconds>] [--json]

Examples:
  echo "print('hi')" | squire verify --lang python --targets alpine-3.20,ubuntu-24.04,debian-12 --json
  squire verify --lang bash --file script.sh --targets alpine-3.20,ubuntu-24.04,debian-12
`)
}

func printDepsHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire deps --lang <python|node> --file <path> [--targets <csv>] [--timeout <seconds>] [--json]

Examples:
  squire deps --lang python --file requirements.txt --targets py310,py311,py312 --json
  squire deps --lang node --file package.json --json
`)
}

func printSQLHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire sql --dialect <sqlite|postgres-16> [--file <path> | --schema <path> --query <sql> | --schema <path> --query-file <path>] [--timeout <seconds>] [--json]

Examples:
  squire sql --dialect sqlite --query "SELECT 1" --json
  squire sql --dialect sqlite --schema schema.sql --query-file query.sql --json
  squire sql --dialect postgres-16 --file migration.sql --json
`)
}

func printTestHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire test --lang <python|node|bash> --file <path> [--file <path> ...] [--cmd <command>] [--targets <csv>] [--timeout <seconds>] [--json]

Examples:
  squire test --lang python --file test_app.py --cmd "pytest -q" --targets py310,py311 --json
  squire test --lang node --file package.json --file test/app.test.mjs --cmd "npm test" --targets node20,node22 --json
  squire test --lang bash --file test.sh --targets alpine-3.20,ubuntu-24.04 --json
`)
}

func printLintHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire lint --lang <python|js|ts|rust> --tool <ruff|eslint|clippy> --file <path> [--file <path> ...] [--targets <csv>] [--timeout <seconds>] [--json]

Examples:
  squire lint --lang python --tool ruff --file app.py --json
  squire lint --lang ts --tool eslint --file src/index.ts --json
  squire lint --lang rust --tool clippy --file Cargo.toml --file src/main.rs --targets stable,nightly --json
`)
}

func printAuditHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire audit [--lang python --file <path>] [--secrets | --static --tool semgrep --config <config>] [--file <path> | --path <dir>] ... [--targets <csv>] [--timeout <seconds>] [--json]

Examples:
  squire audit --lang python --file requirements.txt --json
  squire audit --secrets --path ./src --json
  squire audit --static --tool semgrep --config p/security --path ./src --json
`)
}

func printBuildHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire build --lang <python|node> [--file <path> | --path <dir>] ... [--targets <csv>] [--timeout <seconds>] [--json]

Examples:
  squire build --lang python --file pyproject.toml --path src --targets manylinux,musllinux --json
  squire build --lang node --file package.json --path src --targets linux --json
`)
}

func printBenchHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire bench --lang <python|bash|go> [--file <path> | --path <dir>] ... [--targets <csv>] [--iterations <count>] [--timeout <seconds>] [--json]

Examples:
  squire bench --lang python --file bench.py --targets py310,py311,py312 --json
  squire bench --lang bash --file script.sh --targets alpine-3.20,ubuntu-24.04 --json
  squire bench --lang go --file main.go --targets linux/amd64 --json
`)
}

func printBrowserHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire browser [--browser chromium] [--script <path>] [--url <url>] [--file <path> | --path <dir>] ... [--screenshot <name>] [--allow-network] [--timeout <seconds>] [--json]

Examples:
  squire browser --script login-test.js --browser chromium --json
  squire browser --url https://example.com --screenshot page.png --allow-network --json
  squire browser --file index.html --screenshot page.png --json
`)
}

func printCompileHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire compile --lang <go|rust> --file <path> [--file <path> ...] [--targets <csv>] [--timeout <seconds>] [--json]

Examples:
  squire compile --lang go --file main.go --targets linux/amd64,linux/arm64 --json
  squire compile --lang rust --file Cargo.toml --file src/main.rs --targets linux/amd64-musl,linux/arm64 --json
`)
}

func printSolveHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire solve --solver <z3|minizinc> --file <path> [--data <path>] [--timeout <seconds>] [--json]

Examples:
  squire solve --solver z3 --file constraints.smt2 --json
  squire solve --solver minizinc --file model.mzn --data data.dzn --json
`)
}

func printDataHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire data --script <path> [--input <path> | --stdin] [--timeout <seconds>] [--json]

Examples:
  squire data --script transform.py --input big.csv --json
  cat small.csv | squire data --script transform.py --stdin --json
`)
}

func printMediaHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire media --script <path> [--input <path>] [--timeout <seconds>] [--json]

Examples:
  squire media --script clip.py --input video.mp4 --json
`)
}

func printScaleAliasHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire scale --mode <data|media> --script <path> [--input <path> | --stdin] [--timeout <seconds>] [--json]

Compatibility alias for "squire data" and "squire media".

Examples:
  squire scale --mode data --script transform.py --input big.csv --json
  squire scale --mode media --script clip.py --input video.mp4 --json
`)
}

func printWhoAmIHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire whoami [--json]

Shows the authenticated user, trust tier, quotas, and feature flags.
`)
}

func printLogoutHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire logout [--json]

Clears the locally stored Squire session config.
`)
}

func collectRequestFiles(files, paths []string) ([]protocol.SourceFile, error) {
	out := make([]protocol.SourceFile, 0, len(files))
	seen := map[string]struct{}{}
	addPath := func(localPath, requestPath string) error {
		data, err := os.ReadFile(localPath)
		if err != nil {
			return err
		}
		if requestPath == "" {
			requestPath = requestPathForLocalFile(localPath)
		}
		if _, ok := seen[requestPath]; ok {
			return nil
		}
		seen[requestPath] = struct{}{}
		out = append(out, protocol.SourceFile{
			Path:    requestPath,
			Content: string(data),
		})
		return nil
	}

	for _, filePath := range files {
		info, err := os.Stat(filePath)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			return nil, fmt.Errorf("--file expects a file, got directory: %s", filePath)
		}
		if err := addPath(filePath, ""); err != nil {
			return nil, err
		}
	}

	for _, root := range paths {
		rootInfo, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		if !rootInfo.IsDir() {
			if err := addPath(root, requestPathForLocalFile(root)); err != nil {
				return nil, err
			}
			continue
		}
		baseDir := filepath.Dir(root)
		err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				base := filepath.Base(path)
				if base == ".git" || base == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			relPath, err := filepath.Rel(baseDir, path)
			if err != nil {
				return err
			}
			return addPath(path, filepath.ToSlash(relPath))
		})
		if err != nil {
			return nil, err
		}
	}

	return out, nil
}

func readOptionalFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type multiStringFlag []string

func (m *multiStringFlag) String() string {
	return strings.Join(*m, ",")
}

func (m *multiStringFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("file path cannot be empty")
	}
	*m = append(*m, value)
	return nil
}

func requestPathForLocalFile(path string) string {
	path = filepath.Clean(path)
	cwd, err := os.Getwd()
	if err == nil {
		if rel, relErr := filepath.Rel(cwd, path); relErr == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.Base(path)
}
