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
	case "verify":
		return runVerify(ctx, args[1:], stdin, stdout, stderr)
	case "scale":
		return runScale(ctx, args[1:], stdin, stdout, stderr)
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

func runScale(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scale", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "", "Scale mode: data or media")
	scriptPath := fs.String("script", "", "Path to the Python script to execute")
	inputPath := fs.String("input", "", "Path to an input file for multipart upload")
	useStdin := fs.Bool("stdin", false, "Read small input payload from stdin and send JSON")
	timeout := fs.Int("timeout", 120, "Scale timeout in seconds")
	jsonOut := fs.Bool("json", false, "Print raw JSON response")
	apiBaseURL := fs.String("api-base-url", "", "Override API base URL for this request")
	fs.Usage = func() { printScaleHelp(fs.Output()) }
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
		err = client.DoJSON(ctx, http.MethodPost, "/v1/scale", protocol.ScaleJSONRequest{
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
		req, err := newScaleMultipartRequest(ctx, client.BaseURL+"/v1/scale", cfg.SessionToken, *mode, *scriptPath, *inputPath, *timeout)
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
	fmt.Fprintf(stdout, "user: %s\ntrust: %s\nverify rpm: %d\nscale rph: %d\nfeatures: %s\n",
		resp.UserID, resp.TrustTier, resp.Quotas.VerifyRequestsPerMinute, resp.Quotas.ScaleRequestsPerHour, strings.Join(resp.FeatureFlags, ","))
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
  squire verify
  squire scale
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

func printVerifyHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire verify --lang <bash|python|node> [--code <snippet> | --file <path>] [--targets <csv>] [--timeout <seconds>] [--json]

Examples:
  echo "print('hi')" | squire verify --lang python --targets alpine-3.20,ubuntu-24.04,debian-12 --json
  squire verify --lang bash --file script.sh --targets alpine-3.20,ubuntu-24.04,debian-12
`)
}

func printScaleHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: squire scale --mode <data|media> --script <path> [--input <path> | --stdin] [--timeout <seconds>] [--json]

Examples:
  squire scale --mode data --script transform.py --input big.csv --json
  cat small.csv | squire scale --mode data --script transform.py --stdin --json
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
