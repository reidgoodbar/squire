package app

import (
	"context"
	"encoding/base64"
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
	"unicode/utf8"

	"squire/internal/buildinfo"
	"squire/internal/catalog"
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

type rootCommandHandler func(context.Context, []string, io.Reader, io.Writer, io.Writer) int

type rootDispatchEntry struct {
	Name    string
	Aliases []string
	Handler rootCommandHandler
}

func rootDispatchEntries() []rootDispatchEntry {
	return []rootDispatchEntry{
		{Name: "help", Aliases: []string{"--help", "-h"}, Handler: func(_ context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runRootHelp(args, stdout, stderr)
		}},
		{Name: "login", Handler: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runLogin(ctx, args, stdout, stderr)
		}},
		{Name: "update", Handler: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runUpdate(ctx, args, stdout, stderr)
		}},
		{Name: "whoami", Handler: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runWhoAmI(ctx, args, stdout, stderr)
		}},
		{Name: "logout", Handler: func(_ context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runLogout(args, stdout, stderr)
		}},
		{Name: "mcp", Handler: runMCP},
		{Name: "verify", Handler: runVerify},
		{Name: "deps", Handler: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runDeps(ctx, args, stdout, stderr)
		}},
		{Name: "sql", Handler: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runSQL(ctx, args, stdout, stderr)
		}},
		{Name: "test", Handler: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runTest(ctx, args, stdout, stderr)
		}},
		{Name: "lint", Handler: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runLint(ctx, args, stdout, stderr)
		}},
		{Name: "audit", Handler: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runAudit(ctx, args, stdout, stderr)
		}},
		{Name: "build", Handler: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runBuild(ctx, args, stdout, stderr)
		}},
		{Name: "bench", Handler: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runBench(ctx, args, stdout, stderr)
		}},
		{Name: "browser", Handler: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runBrowser(ctx, args, stdout, stderr)
		}},
		{Name: "compile", Handler: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runCompile(ctx, args, stdout, stderr)
		}},
		{Name: "solve", Handler: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runSolve(ctx, args, stdout, stderr)
		}},
		{Name: "quantum", Handler: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return runQuantum(ctx, args, stdout, stderr)
		}},
		{Name: "data", Handler: func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
			return runMode(ctx, args, stdin, stdout, stderr, "data")
		}},
		{Name: "media", Handler: func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
			return runMode(ctx, args, stdin, stdout, stderr, "media")
		}},
		{Name: "scale", Handler: runScaleAlias},
	}
}

func findRootDispatchEntry(name string) (rootDispatchEntry, bool) {
	name = strings.TrimSpace(name)
	for _, entry := range rootDispatchEntries() {
		if entry.Name == name {
			return entry, true
		}
		for _, alias := range entry.Aliases {
			if alias == name {
				return entry, true
			}
		}
	}
	return rootDispatchEntry{}, false
}

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(args) == 0 {
		printRootHelp(stderr)
		return exitUsage
	}
	entry, ok := findRootDispatchEntry(args[0])
	if !ok {
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printRootHelp(stderr)
		return exitUsage
	}
	return entry.Handler(ctx, args[1:], stdin, stdout, stderr)
}

func runRootHelp(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("help", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "Print the root command catalog as JSON")
	fs.Usage = func() { printRootHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) > 0 {
		commandPath := strings.Join(rest, " ")
		if !printCommandHelp(commandPath, stdout, stderr) {
			fmt.Fprintf(stderr, "unknown command %q\n\n", commandPath)
			printRootHelp(stderr)
			return exitUsage
		}
		return exitOK
	}
	if *jsonOut {
		payload, err := catalog.RootHelpJSONBytes()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitRemoteError
		}
		_, _ = stdout.Write(payload)
		_, _ = stdout.Write([]byte("\n"))
		return exitOK
	}
	printRootHelp(stdout)
	return exitOK
}

func printCommandHelp(command string, stdout, stderr io.Writer) bool {
	text, ok := catalog.CommandHelpText(command)
	if !ok {
		_ = stderr
		return false
	}
	_, _ = io.WriteString(stdout, text)
	return true
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
	if len(fs.Args()) > 0 {
		fmt.Fprintln(stderr, "squire login does not accept positional arguments")
		return exitUsage
	}

	result, code := loginAndSave(ctx, *token, *apiBaseURL, stderr)
	if code != exitOK {
		return code
	}
	if *jsonOut {
		payload := map[string]interface{}{
			"status":     "ok",
			"user_id":    result.Config.UserID,
			"trust_tier": result.Config.TrustTier,
			"token_type": result.Config.TokenType,
		}
		if len(result.FeatureFlags) > 0 {
			payload["feature_flags"] = result.FeatureFlags
		}
		_ = json.NewEncoder(stdout).Encode(payload)
		return exitOK
	}
	fmt.Fprintf(stdout, "logged in as %s (%s)\n", result.Config.UserID, result.Config.TrustTier)
	return exitOK
}

type loginSaveResult struct {
	Config       protocol.CLIConfig
	FeatureFlags []string
}

func loginAndSave(ctx context.Context, token, apiBaseURL string, stderr io.Writer) (loginSaveResult, int) {
	cfg, _ := config.Load()
	baseURL := pickBaseURL(apiBaseURL, cfg.APIBaseURL)
	client := httpclient.New(baseURL, "")

	if token != "" {
		var resp protocol.LoginResponse
		if err := client.DoJSON(ctx, http.MethodPost, "/v1/auth/token/login", protocol.LoginTokenRequest{Token: token}, &resp); err != nil {
			fmt.Fprintln(stderr, err)
			return loginSaveResult{}, exitAuth
		}
		saved := protocol.CLIConfig{
			APIBaseURL:   baseURL,
			SessionToken: token,
			UserID:       resp.UserID,
			TrustTier:    resp.TrustTier,
			TokenType:    resp.TokenType,
			CreatedAt:    time.Now().UTC(),
		}
		if err := config.Save(saved); err != nil {
			fmt.Fprintln(stderr, err)
			return loginSaveResult{}, exitRemoteError
		}
		return loginSaveResult{
			Config:       saved,
			FeatureFlags: append([]string(nil), resp.FeatureFlags...),
		}, exitOK
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(stderr, "failed to start local callback server:", err)
		return loginSaveResult{}, exitRemoteError
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
		return loginSaveResult{}, exitAuth
	}

	type browserLoginResult struct {
		TokenType    string
		SessionToken string
		UserID       string
		TrustTier    string
		Err          error
	}
	resultCh := make(chan browserLoginResult, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("client_nonce") != clientNonce {
			http.Error(w, "invalid login nonce", http.StatusBadRequest)
			resultCh <- browserLoginResult{Err: errors.New("invalid login nonce")}
			return
		}
		fmt.Fprint(w, "Squire login complete. You can close this window.")
		resultCh <- browserLoginResult{
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
		return loginSaveResult{}, exitSignal
	case result := <-resultCh:
		if result.Err != nil {
			fmt.Fprintln(stderr, result.Err)
			return loginSaveResult{}, exitAuth
		}
		if result.SessionToken == "" {
			fmt.Fprintln(stderr, "login failed: missing session token")
			return loginSaveResult{}, exitAuth
		}
		saved := protocol.CLIConfig{
			APIBaseURL:   baseURL,
			SessionToken: result.SessionToken,
			UserID:       result.UserID,
			TrustTier:    result.TrustTier,
			TokenType:    result.TokenType,
			CreatedAt:    time.Now().UTC(),
		}
		if err := config.Save(saved); err != nil {
			fmt.Fprintln(stderr, err)
			return loginSaveResult{}, exitRemoteError
		}
		return loginSaveResult{Config: saved}, exitOK
	}
}

func shellQuoteSingle(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
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

	reqFiles, err := collectRequestFiles(files, nil)
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

	reqFiles, err := collectRequestFiles(files, nil)
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
	lang := fs.String("lang", "", "Audit language. Dependency audit is currently disabled on the public zero-egress service.")
	secrets := fs.Bool("secrets", false, "Run the built-in secret scanner")
	static := fs.Bool("static", false, "Run static analysis")
	tool := fs.String("tool", "", "Audit tool. Phase 2 supports semgrep for static analysis.")
	configValue := fs.String("config", "", "Static analysis config as a staged local file path")
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
	if kind == "static" && strings.TrimSpace(*configValue) != "" && !containsPath(files, *configValue) {
		files = append(files, *configValue)
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
	configPath := ""
	if kind == "static" && strings.TrimSpace(*configValue) != "" {
		configPath = requestPathForLocalFileFromBase(*configValue, commonAncestorDir(files))
	}
	if err := client.DoJSON(ctx, http.MethodPost, "/v1/audit", protocol.AuditRequest{
		Kind:           kind,
		Language:       *lang,
		Tool:           *tool,
		Config:         configPath,
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
	downloadDir := fs.String("download-artifacts", "", "Download returned build artifacts into this local directory")
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
	downloadCount := 0
	if *downloadDir != "" {
		count, err := downloadArtifactPlans(ctx, client, *downloadDir, buildArtifactDownloadPlans(resp))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitRemoteError
		}
		downloadCount = count
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %d passed, %d failed\n", resp.RequestID, resp.Summary.Passed, resp.Summary.Failed)
		for _, result := range resp.Results {
			fmt.Fprintf(stdout, "- %s: %s (%d ms, %d artifacts)\n", result.Target, result.Status, result.RuntimeMS, len(result.Artifacts))
		}
		if downloadCount > 0 {
			fmt.Fprintf(stdout, "downloaded artifacts: %d -> %s\n", downloadCount, *downloadDir)
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
	url := fs.String("url", "", "URL to open. Remote URLs are disabled; stage local files instead")
	screenshot := fs.String("screenshot", "", "Optional screenshot filename to produce as an artifact")
	var files multiStringFlag
	var paths multiStringFlag
	fs.Var(&files, "file", "Path to a file to stage; repeat for multiple files")
	fs.Var(&paths, "path", "Path to a directory tree to stage; repeat for multiple paths")
	timeout := fs.Int("timeout", 45, "Browser timeout in seconds")
	downloadDir := fs.String("download-artifacts", "", "Download returned browser artifacts into this local directory")
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
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(*url)), "http://") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(*url)), "https://") {
		fmt.Fprintln(stderr, "browser remote URLs are disabled; upload local files instead")
		return exitUsage
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
		TimeoutSeconds: *timeout,
	}, &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}
	downloadCount := 0
	if *downloadDir != "" {
		count, err := downloadArtifactPlans(ctx, client, *downloadDir, topLevelArtifactDownloadPlans(resp.Artifacts))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitRemoteError
		}
		downloadCount = count
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
		if downloadCount > 0 {
			fmt.Fprintf(stdout, "downloaded artifacts: %d -> %s\n", downloadCount, *downloadDir)
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

	reqFiles, err := collectRequestFiles(files, nil)
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

func runQuantum(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printQuantumHelp(stderr)
		return exitUsage
	}
	switch args[0] {
	case "simulate":
		return runQuantumSimulate(ctx, args[1:], stdout, stderr)
	case "help", "--help", "-h":
		printQuantumHelp(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown quantum subcommand %q\n\n", args[0])
		printQuantumHelp(stderr)
		return exitUsage
	}
}

func runQuantumSimulate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("quantum simulate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var files multiStringFlag
	fs.Var(&files, "file", "Path to the entry script to stage; repeat to add helper modules or assets")
	shots := fs.Int("shots", 1024, "Shot count passed to the simulation via SQUIRE_QUANTUM_SHOTS")
	backend := fs.String("backend", "aer_simulator", "Quantum backend. v1 supports only aer_simulator")
	timeout := fs.Int("timeout", 300, "Quantum simulation timeout in seconds")
	downloadDir := fs.String("download-artifacts", "", "Download returned quantum artifacts into this local directory")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printCatalogCommandHelpPath(fs.Output(), "quantum.simulate") }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if len(files) == 0 {
		fmt.Fprintln(stderr, "at least one --file is required")
		return exitUsage
	}

	reqFiles, err := collectRequestFiles(files, nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	entryFile := reqFiles[0].Path

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitAuth
	}
	client := httpclient.New(pickBaseURL(*apiBaseURL, cfg.APIBaseURL), cfg.SessionToken)
	var resp protocol.QuantumSimulateResponse
	if err := client.DoJSON(ctx, http.MethodPost, "/v1/quantum/simulate", protocol.QuantumSimulateRequest{
		EntryFile:      entryFile,
		Files:          reqFiles,
		Shots:          *shots,
		Backend:        *backend,
		TimeoutSeconds: *timeout,
	}, &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}
	downloadCount := 0
	if *downloadDir != "" {
		count, err := downloadArtifactPlans(ctx, client, *downloadDir, topLevelArtifactDownloadPlans(resp.Artifacts))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitRemoteError
		}
		downloadCount = count
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %s (%s, %d ms)\n", resp.RequestID, resp.Status, resp.Backend, resp.RuntimeMS)
		if len(resp.Artifacts) > 0 {
			fmt.Fprintf(stdout, "artifacts: %d\n", len(resp.Artifacts))
		}
		if downloadCount > 0 {
			fmt.Fprintf(stdout, "downloaded artifacts: %d -> %s\n", downloadCount, *downloadDir)
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

func runMode(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, mode string) int {
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	scriptPath := fs.String("script", "", "Path to the Python script to execute")
	inputPath := fs.String("input", "", "Path to an input file for multipart upload")
	useStdin := fs.Bool("stdin", false, "Read small input payload from stdin and send JSON")
	timeout := fs.Int("timeout", 120, "Job timeout in seconds")
	downloadDir := fs.String("download-artifacts", "", "Download returned artifacts into this local directory")
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

	downloadCount := 0
	if *downloadDir != "" {
		count, err := downloadArtifactPlans(ctx, client, *downloadDir, topLevelArtifactDownloadPlans(resp.Artifacts))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitRemoteError
		}
		downloadCount = count
	}

	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %s (%s, %d ms)\n", resp.RequestID, resp.Status, resp.Mode, resp.RuntimeMS)
		for _, artifact := range resp.Artifacts {
			fmt.Fprintf(stdout, "- artifact: %s (%d bytes)\n", artifact.Name, artifact.SizeBytes)
		}
		if downloadCount > 0 {
			fmt.Fprintf(stdout, "downloaded artifacts: %d -> %s\n", downloadCount, *downloadDir)
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
	downloadDir := fs.String("download-artifacts", "", "Download returned artifacts into this local directory")
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
	downloadCount := 0
	if *downloadDir != "" {
		count, err := downloadArtifactPlans(ctx, client, *downloadDir, topLevelArtifactDownloadPlans(resp.Artifacts))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitRemoteError
		}
		downloadCount = count
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
	} else {
		fmt.Fprintf(stdout, "request %s: %s (%s, %d ms)\n", resp.RequestID, resp.Status, resp.Mode, resp.RuntimeMS)
		for _, artifact := range resp.Artifacts {
			fmt.Fprintf(stdout, "- artifact: %s (%d bytes)\n", artifact.Name, artifact.SizeBytes)
		}
		if downloadCount > 0 {
			fmt.Fprintf(stdout, "downloaded artifacts: %d -> %s\n", downloadCount, *downloadDir)
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

type artifactDownloadPlan struct {
	artifact     protocol.Artifact
	relativePath string
}

func topLevelArtifactDownloadPlans(artifacts []protocol.Artifact) []artifactDownloadPlan {
	plans := make([]artifactDownloadPlan, 0, len(artifacts))
	for _, artifact := range artifacts {
		plans = append(plans, artifactDownloadPlan{
			artifact:     artifact,
			relativePath: sanitizeArtifactFilename(artifact.Name),
		})
	}
	return plans
}

func buildArtifactDownloadPlans(resp protocol.BuildResponse) []artifactDownloadPlan {
	plans := make([]artifactDownloadPlan, 0)
	for _, result := range resp.Results {
		targetDir := sanitizeArtifactPathSegment(result.Target)
		for _, artifact := range result.Artifacts {
			plans = append(plans, artifactDownloadPlan{
				artifact:     artifact,
				relativePath: filepath.Join(targetDir, sanitizeArtifactFilename(artifact.Name)),
			})
		}
	}
	return plans
}

func sanitizeArtifactFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	if name == "" || name == "." || name == string(os.PathSeparator) {
		return "artifact.bin"
	}
	return name
}

func sanitizeArtifactPathSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "artifacts"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	return replacer.Replace(value)
}

func downloadArtifactPlans(ctx context.Context, client *httpclient.Client, destRoot string, plans []artifactDownloadPlan) (int, error) {
	if len(plans) == 0 {
		return 0, nil
	}
	root := filepath.Clean(destRoot)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return 0, fmt.Errorf("create artifact directory: %w", err)
	}
	rootWithSep := root + string(os.PathSeparator)
	downloaded := 0
	for _, plan := range plans {
		relative := filepath.Clean(plan.relativePath)
		if relative == "." || relative == "" {
			relative = sanitizeArtifactFilename(plan.artifact.Name)
		}
		destPath := filepath.Clean(filepath.Join(root, relative))
		if destPath != root && !strings.HasPrefix(destPath, rootWithSep) {
			return downloaded, fmt.Errorf("refusing to write artifact outside %s", root)
		}
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return downloaded, fmt.Errorf("create artifact subdirectory: %w", err)
		}
		if err := downloadArtifactFile(ctx, client, plan.artifact.DownloadURL, destPath); err != nil {
			return downloaded, err
		}
		downloaded++
	}
	return downloaded, nil
}

func downloadArtifactFile(ctx context.Context, client *httpclient.Client, rawURL, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build artifact request: %w", err)
	}
	req.Header.Set("Accept", "*/*")
	if client.Token != "" {
		req.Header.Set("Authorization", "Bearer "+client.Token)
	}
	req.Header.Set("User-Agent", "squire-cli/"+buildinfo.CurrentVersion())

	resp, err := client.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download artifact: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("download artifact %s: %s%s", filepath.Base(destPath), resp.Status, formatArtifactDownloadBody(body))
	}

	tmpPath := destPath + ".part"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open artifact file: %w", err)
	}
	if _, err := io.Copy(file, resp.Body); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write artifact: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close artifact file: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize artifact file: %w", err)
	}
	return nil
}

func formatArtifactDownloadBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	return ": " + text
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
	fmt.Fprintf(stdout, "user: %s\ntrust: %s\nverify rpm: %d\ndata rph: %d\nmedia rph: %d\ndeps rph: %d\nsql rph: %d\ntest rph: %d\nlint rph: %d\naudit rph: %d\nbuild rph: %d\nbench rph: %d\nbrowser rph: %d\ncompile rph: %d\nsolve rph: %d\nquantum rph: %d\nfeatures: %s\n",
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
		resp.Quotas.QuantumRequestsPerHour,
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
	_, _ = io.WriteString(w, catalog.RootHelpText())
}

func printLoginHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "login")
}

func printUpdateHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "update")
}

func printVerifyHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "verify")
}

func printDepsHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "deps")
}

func printSQLHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "sql")
}

func printTestHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "test")
}

func printLintHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "lint")
}

func printAuditHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "audit")
}

func printBuildHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "build")
}

func printBenchHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "bench")
}

func printBrowserHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "browser")
}

func printCompileHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "compile")
}

func printSolveHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "solve")
}

func printQuantumHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "quantum")
}

func printDataHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "data")
}

func printMediaHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "media")
}

func printScaleAliasHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "scale")
}

func printWhoAmIHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "whoami")
}

func printLogoutHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "logout")
}

func printMCPHelp(w io.Writer) {
	printCatalogCommandHelpPath(w, "mcp")
}

func printCatalogCommandHelpPath(w io.Writer, path string) {
	text, ok := catalog.CommandHelpText(path)
	if !ok {
		panic("missing catalog help for " + path)
	}
	_, _ = io.WriteString(w, text)
}

func collectRequestFiles(files, paths []string) ([]protocol.SourceFile, error) {
	out := make([]protocol.SourceFile, 0, len(files))
	seen := map[string]struct{}{}
	fileBaseDir := commonAncestorDir(files)
	addPath := func(localPath, requestPath string) error {
		file, err := sourceFileForPath(localPath, requestPath)
		if err != nil {
			return err
		}
		if _, ok := seen[file.Path]; ok {
			return nil
		}
		seen[file.Path] = struct{}{}
		out = append(out, file)
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
		if err := addPath(filePath, requestPathForLocalFileFromBase(filePath, fileBaseDir)); err != nil {
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

func sourceFileForPath(localPath, requestPath string) (protocol.SourceFile, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return protocol.SourceFile{}, err
	}
	if requestPath == "" {
		requestPath = requestPathForLocalFile(localPath)
	}
	file := protocol.SourceFile{Path: requestPath}
	if shouldInlineFileAsText(data) {
		file.Content = string(data)
		return file, nil
	}
	file.Content = base64.StdEncoding.EncodeToString(data)
	file.Encoding = "base64"
	return file, nil
}

func shouldInlineFileAsText(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	if !utf8.Valid(data) {
		return false
	}
	limit := len(data)
	if limit > 8192 {
		limit = 8192
	}
	for _, b := range data[:limit] {
		switch {
		case b == 0:
			return false
		case b < 0x09:
			return false
		case b > 0x0d && b < 0x20:
			return false
		}
	}
	return true
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
		absPath := path
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(cwd, absPath)
		}
		absPath = filepath.Clean(absPath)
		if rel, relErr := filepath.Rel(cwd, absPath); relErr == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.Base(path)
}

func requestPathForLocalFileFromBase(path, baseDir string) string {
	if strings.TrimSpace(baseDir) != "" {
		absPath, err := filepath.Abs(path)
		if err == nil {
			absPath = filepath.Clean(absPath)
			if rel, relErr := filepath.Rel(baseDir, absPath); relErr == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
				return filepath.ToSlash(rel)
			}
		}
	}
	return requestPathForLocalFile(path)
}

func commonAncestorDir(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	firstPath, err := filepath.Abs(paths[0])
	if err != nil {
		return ""
	}
	common := filepath.Dir(filepath.Clean(firstPath))
	for _, path := range paths[1:] {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return ""
		}
		absPath = filepath.Clean(absPath)
		for {
			rel, relErr := filepath.Rel(common, absPath)
			if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				break
			}
			parent := filepath.Dir(common)
			if parent == common {
				return ""
			}
			common = parent
		}
	}
	if filepath.Dir(common) == common {
		return ""
	}
	return common
}

func containsPath(paths []string, want string) bool {
	want = filepath.Clean(want)
	for _, path := range paths {
		if filepath.Clean(path) == want {
			return true
		}
	}
	return false
}
