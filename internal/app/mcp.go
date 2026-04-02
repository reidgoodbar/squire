package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"squire/internal/buildinfo"
	"squire/internal/catalog"
)

const mcpProtocolVersionLatest = "2025-11-25"

var supportedMCPProtocolVersions = map[string]struct{}{
	"2024-11-05": {},
	"2025-03-26": {},
	"2025-06-18": {},
	"2025-11-25": {},
}

type mcpJSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpJSONRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id,omitempty"`
	Result  interface{}      `json:"result,omitempty"`
	Error   *mcpJSONRPCError `json:"error,omitempty"`
}

type mcpJSONRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type mcpTool struct {
	Name        string
	Title       string
	Description string
	InputSchema map[string]interface{}
	Handler     func(context.Context, map[string]interface{}) (mcpToolResult, error)
}

type mcpToolResult struct {
	Structured map[string]interface{}
	Text       string
	IsError    bool
}

type mcpInitializeParams struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	Capabilities    map[string]interface{} `json:"capabilities"`
	ClientInfo      map[string]interface{} `json:"clientInfo"`
}

type mcpToolCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type mcpServer struct {
	tools map[string]mcpTool
}

func runMCP(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printMCPHelp(stderr)
		return exitUsage
	}
	switch args[0] {
	case "serve":
		return runMCPServe(ctx, args[1:], stdin, stdout, stderr)
	case "help", "--help", "-h":
		printMCPHelp(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown mcp subcommand %q\n\n", args[0])
		printMCPHelp(stderr)
		return exitUsage
	}
}

func runMCPServe(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printCatalogCommandHelpPath(fs.Output(), "mcp.serve") }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if len(fs.Args()) > 0 {
		fmt.Fprintln(stderr, "squire mcp serve does not accept positional arguments")
		return exitUsage
	}
	server := &mcpServer{tools: mcpToolMap()}
	if err := server.serve(ctx, stdin, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}
	return exitOK
}

func (s *mcpServer) serve(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	reader := bufio.NewReader(stdin)
	for {
		raw, err := readMCPMessage(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if len(raw) == 0 {
			continue
		}

		responses, stop, err := s.handleIncoming(ctx, raw)
		if err != nil {
			return err
		}
		for _, response := range responses {
			if err := writeMCPMessage(stdout, response); err != nil {
				return err
			}
		}
		if stop {
			return nil
		}
	}
}

func (s *mcpServer) handleIncoming(ctx context.Context, raw []byte) ([]mcpJSONRPCResponse, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false, nil
	}
	if trimmed[0] == '[' {
		var messages []json.RawMessage
		if err := json.Unmarshal(trimmed, &messages); err != nil {
			return []mcpJSONRPCResponse{{
				JSONRPC: "2.0",
				Error: &mcpJSONRPCError{
					Code:    -32700,
					Message: "parse error",
				},
			}}, false, nil
		}
		responses := make([]mcpJSONRPCResponse, 0, len(messages))
		stop := false
		for _, message := range messages {
			response, messageStop := s.handleMessage(ctx, message)
			if response != nil {
				responses = append(responses, *response)
			}
			if messageStop {
				stop = true
			}
		}
		return responses, stop, nil
	}

	response, stop := s.handleMessage(ctx, trimmed)
	if response == nil {
		return nil, stop, nil
	}
	return []mcpJSONRPCResponse{*response}, stop, nil
}

func (s *mcpServer) handleMessage(ctx context.Context, raw []byte) (*mcpJSONRPCResponse, bool) {
	var request mcpJSONRPCRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return &mcpJSONRPCResponse{
			JSONRPC: "2.0",
			Error: &mcpJSONRPCError{
				Code:    -32700,
				Message: "parse error",
			},
		}, false
	}
	if request.JSONRPC != "2.0" || request.Method == "" {
		return errorResponse(request.ID, -32600, "invalid request", nil), false
	}

	notification := len(bytes.TrimSpace(request.ID)) == 0
	switch request.Method {
	case "initialize":
		if notification {
			return errorResponse(request.ID, -32600, "initialize requires an id", nil), false
		}
		var params mcpInitializeParams
		if len(request.Params) > 0 {
			if err := json.Unmarshal(request.Params, &params); err != nil {
				return errorResponse(request.ID, -32602, "invalid params", err.Error()), false
			}
		}
		return successResponse(request.ID, map[string]interface{}{
			"protocolVersion": negotiateMCPProtocolVersion(params.ProtocolVersion),
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{
					"listChanged": false,
				},
			},
			"serverInfo": map[string]interface{}{
				"name":    "squire",
				"version": buildinfo.CurrentVersion(),
			},
		}), false
	case "notifications/initialized":
		return nil, false
	case "ping":
		if notification {
			return nil, false
		}
		return successResponse(request.ID, map[string]interface{}{}), false
	case "shutdown":
		if notification {
			return nil, false
		}
		return successResponse(request.ID, map[string]interface{}{}), false
	case "exit":
		return nil, true
	case "tools/list":
		if notification {
			return nil, false
		}
		return successResponse(request.ID, map[string]interface{}{
			"tools": s.listTools(),
		}), false
	case "tools/call":
		if notification {
			return nil, false
		}
		var params mcpToolCallParams
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return errorResponse(request.ID, -32602, "invalid params", err.Error()), false
		}
		tool, ok := s.tools[params.Name]
		if !ok {
			return errorResponse(request.ID, -32601, "tool not found", params.Name), false
		}
		if params.Arguments == nil {
			params.Arguments = map[string]interface{}{}
		}
		result, err := tool.Handler(ctx, params.Arguments)
		if err != nil {
			return errorResponse(request.ID, -32602, "invalid params", err.Error()), false
		}
		contentText := strings.TrimSpace(result.Text)
		if contentText == "" && result.Structured != nil {
			contentText = prettyJSON(result.Structured)
		}
		return successResponse(request.ID, map[string]interface{}{
			"content": []map[string]string{
				{
					"type": "text",
					"text": contentText,
				},
			},
			"structuredContent": result.Structured,
			"isError":           result.IsError,
		}), false
	default:
		if notification {
			return nil, false
		}
		return errorResponse(request.ID, -32601, "method not found", request.Method), false
	}
}

func (s *mcpServer) listTools() []map[string]interface{} {
	return catalog.MCPToolMetadata()
}

func mcpToolMap() map[string]mcpTool {
	quantumCommand := mustCatalogMCPCommand("quantum_simulate")
	tools := []mcpTool{
		mcpHelpTool(),
		mcpCLIJSONTool(
			"whoami",
			func(arguments map[string]interface{}) ([]string, string, error) {
				return nil, "", nil
			},
		),
		mcpCLIJSONTool(
			"verify",
			func(arguments map[string]interface{}) ([]string, string, error) {
				language, err := requiredString(arguments, "language")
				if err != nil {
					return nil, "", err
				}
				code := optionalString(arguments, "code")
				file := optionalString(arguments, "file")
				if code == "" && file == "" {
					return nil, "", fmt.Errorf("verify requires either code or file")
				}
				if code != "" && file != "" {
					return nil, "", fmt.Errorf("verify accepts code or file, not both")
				}
				args := []string{"--lang", language}
				if code != "" {
					args = append(args, "--code", code)
				}
				if file != "" {
					args = append(args, "--file", file)
				}
				if targets := optionalStringList(arguments, "targets"); len(targets) > 0 {
					args = append(args, "--targets", strings.Join(targets, ","))
				}
				if timeout, ok, err := optionalInt(arguments, "timeout"); err != nil {
					return nil, "", err
				} else if ok {
					args = append(args, "--timeout", fmt.Sprintf("%d", timeout))
				}
				return args, "", nil
			},
		),
		mcpCLIJSONTool(
			"deps",
			func(arguments map[string]interface{}) ([]string, string, error) {
				language, err := requiredString(arguments, "language")
				if err != nil {
					return nil, "", err
				}
				file, err := requiredString(arguments, "file")
				if err != nil {
					return nil, "", err
				}
				args := []string{"--lang", language, "--file", file}
				if targets := optionalStringList(arguments, "targets"); len(targets) > 0 {
					args = append(args, "--targets", strings.Join(targets, ","))
				}
				if timeout, ok, err := optionalInt(arguments, "timeout"); err != nil {
					return nil, "", err
				} else if ok {
					args = append(args, "--timeout", fmt.Sprintf("%d", timeout))
				}
				return args, "", nil
			},
		),
		mcpCLIJSONTool(
			"sql",
			func(arguments map[string]interface{}) ([]string, string, error) {
				dialect, err := requiredString(arguments, "dialect")
				if err != nil {
					return nil, "", err
				}
				args := []string{"--dialect", dialect}
				file := optionalString(arguments, "file")
				schema := optionalString(arguments, "schema")
				query := optionalString(arguments, "query")
				queryFile := optionalString(arguments, "query_file")
				if file == "" && schema == "" && query == "" && queryFile == "" {
					return nil, "", fmt.Errorf("sql requires file, schema, query, or query_file")
				}
				if file != "" && (schema != "" || query != "" || queryFile != "") {
					return nil, "", fmt.Errorf("sql file cannot be combined with schema/query/query_file")
				}
				if file != "" {
					args = append(args, "--file", file)
				}
				if schema != "" {
					args = append(args, "--schema", schema)
				}
				if query != "" {
					args = append(args, "--query", query)
				}
				if queryFile != "" {
					args = append(args, "--query-file", queryFile)
				}
				if explain, ok, err := optionalBool(arguments, "explain"); err != nil {
					return nil, "", err
				} else if ok && explain {
					args = append(args, "--explain")
				}
				if timeout, ok, err := optionalInt(arguments, "timeout"); err != nil {
					return nil, "", err
				} else if ok {
					args = append(args, "--timeout", fmt.Sprintf("%d", timeout))
				}
				return args, "", nil
			},
		),
		mcpCLIJSONTool(
			"test",
			func(arguments map[string]interface{}) ([]string, string, error) {
				language, err := requiredString(arguments, "language")
				if err != nil {
					return nil, "", err
				}
				files, err := requiredStringArray(arguments, "files")
				if err != nil {
					return nil, "", err
				}
				args := []string{"--lang", language}
				for _, file := range files {
					args = append(args, "--file", file)
				}
				if command := optionalString(arguments, "command"); command != "" {
					args = append(args, "--cmd", command)
				}
				if targets := optionalStringList(arguments, "targets"); len(targets) > 0 {
					args = append(args, "--targets", strings.Join(targets, ","))
				}
				if timeout, ok, err := optionalInt(arguments, "timeout"); err != nil {
					return nil, "", err
				} else if ok {
					args = append(args, "--timeout", fmt.Sprintf("%d", timeout))
				}
				return args, "", nil
			},
		),
		mcpCLIJSONTool(
			"lint",
			func(arguments map[string]interface{}) ([]string, string, error) {
				language, err := requiredString(arguments, "language")
				if err != nil {
					return nil, "", err
				}
				tool, err := requiredString(arguments, "tool")
				if err != nil {
					return nil, "", err
				}
				files, err := requiredStringArray(arguments, "files")
				if err != nil {
					return nil, "", err
				}
				args := []string{"--lang", language, "--tool", tool}
				for _, file := range files {
					args = append(args, "--file", file)
				}
				if targets := optionalStringList(arguments, "targets"); len(targets) > 0 {
					args = append(args, "--targets", strings.Join(targets, ","))
				}
				if timeout, ok, err := optionalInt(arguments, "timeout"); err != nil {
					return nil, "", err
				} else if ok {
					args = append(args, "--timeout", fmt.Sprintf("%d", timeout))
				}
				return args, "", nil
			},
		),
		mcpCLIJSONTool(
			"audit",
			func(arguments map[string]interface{}) ([]string, string, error) {
				args := make([]string, 0)
				if language := optionalString(arguments, "language"); language != "" {
					args = append(args, "--lang", language)
				}
				if secrets, ok, err := optionalBool(arguments, "secrets"); err != nil {
					return nil, "", err
				} else if ok && secrets {
					args = append(args, "--secrets")
				}
				if static, ok, err := optionalBool(arguments, "static"); err != nil {
					return nil, "", err
				} else if ok && static {
					args = append(args, "--static")
				}
				if tool := optionalString(arguments, "tool"); tool != "" {
					args = append(args, "--tool", tool)
				}
				if config := optionalString(arguments, "config"); config != "" {
					args = append(args, "--config", config)
				}
				if files, err := optionalStringArray(arguments, "files"); err != nil {
					return nil, "", err
				} else {
					for _, file := range files {
						args = append(args, "--file", file)
					}
				}
				if paths, err := optionalStringArray(arguments, "paths"); err != nil {
					return nil, "", err
				} else {
					for _, path := range paths {
						args = append(args, "--path", path)
					}
				}
				if targets := optionalStringList(arguments, "targets"); len(targets) > 0 {
					args = append(args, "--targets", strings.Join(targets, ","))
				}
				if timeout, ok, err := optionalInt(arguments, "timeout"); err != nil {
					return nil, "", err
				} else if ok {
					args = append(args, "--timeout", fmt.Sprintf("%d", timeout))
				}
				return args, "", nil
			},
		),
		mcpCLIJSONTool(
			"build",
			func(arguments map[string]interface{}) ([]string, string, error) {
				language, err := requiredString(arguments, "language")
				if err != nil {
					return nil, "", err
				}
				args := []string{"--lang", language}
				if files, err := optionalStringArray(arguments, "files"); err != nil {
					return nil, "", err
				} else {
					for _, file := range files {
						args = append(args, "--file", file)
					}
				}
				if paths, err := optionalStringArray(arguments, "paths"); err != nil {
					return nil, "", err
				} else {
					for _, path := range paths {
						args = append(args, "--path", path)
					}
				}
				if targets := optionalStringList(arguments, "targets"); len(targets) > 0 {
					args = append(args, "--targets", strings.Join(targets, ","))
				}
				if timeout, ok, err := optionalInt(arguments, "timeout"); err != nil {
					return nil, "", err
				} else if ok {
					args = append(args, "--timeout", fmt.Sprintf("%d", timeout))
				}
				if dir := optionalString(arguments, "download_artifacts_dir"); dir != "" {
					args = append(args, "--download-artifacts", dir)
				}
				return args, "", nil
			},
		),
		mcpCLIJSONTool(
			"bench",
			func(arguments map[string]interface{}) ([]string, string, error) {
				language, err := requiredString(arguments, "language")
				if err != nil {
					return nil, "", err
				}
				args := []string{"--lang", language}
				if files, err := optionalStringArray(arguments, "files"); err != nil {
					return nil, "", err
				} else {
					for _, file := range files {
						args = append(args, "--file", file)
					}
				}
				if paths, err := optionalStringArray(arguments, "paths"); err != nil {
					return nil, "", err
				} else {
					for _, path := range paths {
						args = append(args, "--path", path)
					}
				}
				if targets := optionalStringList(arguments, "targets"); len(targets) > 0 {
					args = append(args, "--targets", strings.Join(targets, ","))
				}
				if iterations, ok, err := optionalInt(arguments, "iterations"); err != nil {
					return nil, "", err
				} else if ok {
					args = append(args, "--iterations", fmt.Sprintf("%d", iterations))
				}
				if timeout, ok, err := optionalInt(arguments, "timeout"); err != nil {
					return nil, "", err
				} else if ok {
					args = append(args, "--timeout", fmt.Sprintf("%d", timeout))
				}
				return args, "", nil
			},
		),
		mcpCLIJSONTool(
			"browser",
			func(arguments map[string]interface{}) ([]string, string, error) {
				args := make([]string, 0)
				if browser := optionalString(arguments, "browser"); browser != "" {
					args = append(args, "--browser", browser)
				}
				if script := optionalString(arguments, "script"); script != "" {
					args = append(args, "--script", script)
				}
				if url := optionalString(arguments, "url"); url != "" {
					args = append(args, "--url", url)
				}
				if screenshot := optionalString(arguments, "screenshot"); screenshot != "" {
					args = append(args, "--screenshot", screenshot)
				}
				if files, err := optionalStringArray(arguments, "files"); err != nil {
					return nil, "", err
				} else {
					for _, file := range files {
						args = append(args, "--file", file)
					}
				}
				if paths, err := optionalStringArray(arguments, "paths"); err != nil {
					return nil, "", err
				} else {
					for _, path := range paths {
						args = append(args, "--path", path)
					}
				}
				if timeout, ok, err := optionalInt(arguments, "timeout"); err != nil {
					return nil, "", err
				} else if ok {
					args = append(args, "--timeout", fmt.Sprintf("%d", timeout))
				}
				if dir := optionalString(arguments, "download_artifacts_dir"); dir != "" {
					args = append(args, "--download-artifacts", dir)
				}
				return args, "", nil
			},
		),
		mcpCLIJSONTool(
			"compile",
			func(arguments map[string]interface{}) ([]string, string, error) {
				language, err := requiredString(arguments, "language")
				if err != nil {
					return nil, "", err
				}
				files, err := requiredStringArray(arguments, "files")
				if err != nil {
					return nil, "", err
				}
				args := []string{"--lang", language}
				for _, file := range files {
					args = append(args, "--file", file)
				}
				if targets := optionalStringList(arguments, "targets"); len(targets) > 0 {
					args = append(args, "--targets", strings.Join(targets, ","))
				}
				if timeout, ok, err := optionalInt(arguments, "timeout"); err != nil {
					return nil, "", err
				} else if ok {
					args = append(args, "--timeout", fmt.Sprintf("%d", timeout))
				}
				return args, "", nil
			},
		),
		mcpCLIJSONTool(
			"solve",
			func(arguments map[string]interface{}) ([]string, string, error) {
				solver, err := requiredString(arguments, "solver")
				if err != nil {
					return nil, "", err
				}
				file, err := requiredString(arguments, "file")
				if err != nil {
					return nil, "", err
				}
				args := []string{"--solver", solver, "--file", file}
				if data := optionalString(arguments, "data"); data != "" {
					args = append(args, "--data", data)
				}
				if timeout, ok, err := optionalInt(arguments, "timeout"); err != nil {
					return nil, "", err
				} else if ok {
					args = append(args, "--timeout", fmt.Sprintf("%d", timeout))
				}
				return args, "", nil
			},
		),
		{
			Name:        quantumCommand.MCPToolName,
			Title:       quantumCommand.Title,
			Description: quantumCommand.Description,
			InputSchema: quantumCommand.MCPInputSchema,
			Handler: func(ctx context.Context, arguments map[string]interface{}) (mcpToolResult, error) {
				_ = ctx
				files, err := requiredStringArray(arguments, "files")
				if err != nil {
					return mcpToolResult{}, err
				}
				args := []string{"quantum", "simulate"}
				for _, file := range files {
					args = append(args, "--file", file)
				}
				if shots, ok, err := optionalInt(arguments, "shots"); err != nil {
					return mcpToolResult{}, err
				} else if ok {
					args = append(args, "--shots", fmt.Sprintf("%d", shots))
				}
				if backend := optionalString(arguments, "backend"); backend != "" {
					args = append(args, "--backend", backend)
				}
				if timeout, ok, err := optionalInt(arguments, "timeout"); err != nil {
					return mcpToolResult{}, err
				} else if ok {
					args = append(args, "--timeout", fmt.Sprintf("%d", timeout))
				}
				if dir := optionalString(arguments, "download_artifacts_dir"); dir != "" {
					args = append(args, "--download-artifacts", dir)
				}
				args = append(args, "--json")
				var stdout bytes.Buffer
				var stderr bytes.Buffer
				code := Run(args, strings.NewReader(""), &stdout, &stderr)
				return parseMCPCLIResult(stdout.String(), stderr.String(), code), nil
			},
		},
		mcpCLIJSONTool(
			"data",
			func(arguments map[string]interface{}) ([]string, string, error) {
				return buildMCPModeArgs(arguments)
			},
		),
		mcpCLIJSONTool(
			"media",
			func(arguments map[string]interface{}) ([]string, string, error) {
				return buildMCPModeArgs(arguments)
			},
		),
	}

	out := make(map[string]mcpTool, len(tools))
	for _, tool := range tools {
		out[tool.Name] = tool
	}
	return out
}

func mcpHelpTool() mcpTool {
	cmd := mustCatalogMCPCommand("help")
	return mcpTool{
		Name:        cmd.MCPToolName,
		Title:       cmd.Title,
		Description: cmd.Description,
		InputSchema: cmd.MCPInputSchema,
		Handler: func(ctx context.Context, arguments map[string]interface{}) (mcpToolResult, error) {
			_ = ctx
			command := optionalString(arguments, "command")
			if command == "" {
				payload := catalog.RootHelpJSON()
				var text bytes.Buffer
				printRootHelp(&text)
				return mcpToolResult{
					Structured: map[string]interface{}{
						"name":         payload.Name,
						"description":  payload.Description,
						"commands":     payload.Commands,
						"all_commands": payload.AllCommands,
						"by_category":  payload.ByCategory,
						"by_path":      payload.ByPath,
						"mcp_tools":    payload.MCPTools,
					},
					Text: text.String(),
				}, nil
			}

			var text bytes.Buffer
			if !printCommandHelp(command, &text, io.Discard) {
				return mcpToolResult{
					Structured: map[string]interface{}{
						"command": command,
						"error":   "unknown command",
					},
					Text:    fmt.Sprintf("unknown command %q", command),
					IsError: true,
				}, nil
			}
			return mcpToolResult{
				Structured: map[string]interface{}{
					"command": command,
					"help":    text.String(),
				},
				Text: text.String(),
			}, nil
		},
	}
}

func mcpCLIJSONTool(name string, buildArgs func(map[string]interface{}) ([]string, string, error)) mcpTool {
	cmd := mustCatalogMCPCommand(name)
	return mcpTool{
		Name:        cmd.MCPToolName,
		Title:       cmd.Title,
		Description: cmd.Description,
		InputSchema: cmd.MCPInputSchema,
		Handler: func(ctx context.Context, arguments map[string]interface{}) (mcpToolResult, error) {
			_ = ctx
			args, stdinText, err := buildArgs(arguments)
			if err != nil {
				return mcpToolResult{}, err
			}
			cliArgs := append([]string{name}, args...)
			cliArgs = append(cliArgs, "--json")
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := Run(cliArgs, strings.NewReader(stdinText), &stdout, &stderr)
			return parseMCPCLIResult(stdout.String(), stderr.String(), code), nil
		},
	}
}

func mustCatalogMCPCommand(toolName string) catalog.Command {
	cmd, ok := catalog.FindByMCPToolName(toolName)
	if !ok {
		panic("missing catalog metadata for MCP tool " + toolName)
	}
	return cmd
}

func parseMCPCLIResult(stdout, stderr string, code int) mcpToolResult {
	stdout = strings.TrimSpace(stdout)
	stderr = strings.TrimSpace(stderr)

	if stdout != "" {
		var structured map[string]interface{}
		if err := json.Unmarshal([]byte(stdout), &structured); err == nil {
			text := prettyJSON(structured)
			if stderr != "" {
				text += "\n\nstderr:\n" + stderr
			}
			return mcpToolResult{
				Structured: structured,
				Text:       text,
				IsError:    code != exitOK,
			}
		}
	}

	structured := map[string]interface{}{
		"exit_code": code,
	}
	if stdout != "" {
		structured["stdout"] = stdout
	}
	if stderr != "" {
		structured["stderr"] = stderr
	}
	text := strings.TrimSpace(strings.Join([]string{stdout, stderr}, "\n\n"))
	if text == "" {
		text = fmt.Sprintf("squire command exited with code %d", code)
	}
	return mcpToolResult{
		Structured: structured,
		Text:       text,
		IsError:    code != exitOK,
	}
}

func buildMCPModeArgs(arguments map[string]interface{}) ([]string, string, error) {
	script, err := requiredString(arguments, "script")
	if err != nil {
		return nil, "", err
	}
	args := []string{"--script", script}
	input := optionalString(arguments, "input")
	stdinText := optionalString(arguments, "stdin_text")
	if input != "" && stdinText != "" {
		return nil, "", fmt.Errorf("input and stdin_text cannot be combined")
	}
	if input != "" {
		args = append(args, "--input", input)
	}
	if stdinText != "" {
		args = append(args, "--stdin")
	}
	if timeout, ok, err := optionalInt(arguments, "timeout"); err != nil {
		return nil, "", err
	} else if ok {
		args = append(args, "--timeout", fmt.Sprintf("%d", timeout))
	}
	if dir := optionalString(arguments, "download_artifacts_dir"); dir != "" {
		args = append(args, "--download-artifacts", dir)
	}
	return args, stdinText, nil
}

func requiredString(arguments map[string]interface{}, key string) (string, error) {
	value := strings.TrimSpace(optionalString(arguments, key))
	if value == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return value, nil
}

func optionalString(arguments map[string]interface{}, key string) string {
	value, ok := arguments[key]
	if !ok || value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func requiredStringArray(arguments map[string]interface{}, key string) ([]string, error) {
	values, err := optionalStringArray(arguments, key)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%s is required", key)
	}
	return values, nil
}

func optionalStringArray(arguments map[string]interface{}, key string) ([]string, error) {
	value, ok := arguments[key]
	if !ok || value == nil {
		return nil, nil
	}
	switch v := value.(type) {
	case []string:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out, nil
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s must contain only strings", key)
			}
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out, nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		return []string{strings.TrimSpace(v)}, nil
	default:
		return nil, fmt.Errorf("%s must be a string array", key)
	}
}

func optionalStringList(arguments map[string]interface{}, key string) []string {
	values, err := optionalStringArray(arguments, key)
	if err == nil && len(values) > 0 {
		return values
	}
	single := optionalString(arguments, key)
	if single == "" {
		return nil
	}
	if strings.Contains(single, ",") {
		return splitCSV(single)
	}
	return []string{single}
}

func optionalInt(arguments map[string]interface{}, key string) (int, bool, error) {
	value, ok := arguments[key]
	if !ok || value == nil {
		return 0, false, nil
	}
	switch v := value.(type) {
	case int:
		return v, true, nil
	case int64:
		return int(v), true, nil
	case float64:
		return int(v), true, nil
	case json.Number:
		value, err := v.Int64()
		if err != nil {
			return 0, false, fmt.Errorf("%s must be an integer", key)
		}
		return int(value), true, nil
	case string:
		if strings.TrimSpace(v) == "" {
			return 0, false, nil
		}
		var out int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &out); err != nil {
			return 0, false, fmt.Errorf("%s must be an integer", key)
		}
		return out, true, nil
	default:
		return 0, false, fmt.Errorf("%s must be an integer", key)
	}
}

func optionalBool(arguments map[string]interface{}, key string) (bool, bool, error) {
	value, ok := arguments[key]
	if !ok || value == nil {
		return false, false, nil
	}
	switch v := value.(type) {
	case bool:
		return v, true, nil
	default:
		return false, false, fmt.Errorf("%s must be a boolean", key)
	}
}

func readMCPMessage(reader *bufio.Reader) ([]byte, error) {
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			continue
		}
		return trimmed, nil
	}
}

func writeMCPMessage(w io.Writer, response mcpJSONRPCResponse) error {
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", data)
	return err
}

func successResponse(id json.RawMessage, result interface{}) *mcpJSONRPCResponse {
	return &mcpJSONRPCResponse{
		JSONRPC: "2.0",
		ID:      cloneRawMessage(id),
		Result:  result,
	}
}

func errorResponse(id json.RawMessage, code int, message string, data interface{}) *mcpJSONRPCResponse {
	return &mcpJSONRPCResponse{
		JSONRPC: "2.0",
		ID:      cloneRawMessage(id),
		Error: &mcpJSONRPCError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
}

func cloneRawMessage(in json.RawMessage) json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	out := make(json.RawMessage, len(in))
	copy(out, in)
	return out
}

func negotiateMCPProtocolVersion(clientVersion string) string {
	clientVersion = strings.TrimSpace(clientVersion)
	if _, ok := supportedMCPProtocolVersions[clientVersion]; ok {
		return clientVersion
	}
	return mcpProtocolVersionLatest
}

func prettyJSON(value interface{}) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(data)
}
