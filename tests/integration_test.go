package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cliapp "squire/app"
	"squire/internal/protocol"
)

func TestDataMultipartStreamingViaCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	token := "sqh_test"
	writeCLIConfig(t, home, protocol.CLIConfig{
		APIBaseURL:   "http://placeholder",
		SessionToken: token,
		UserID:       "user-test",
		TrustTier:    protocol.TrustScaleAllowed,
		TokenType:    protocol.TokenTypeHeadless,
		CreatedAt:    time.Now().UTC(),
	})

	scriptPath := filepath.Join(t.TempDir(), "transform.py")
	if err := os.WriteFile(scriptPath, []byte("print('x')"), 0o600); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(t.TempDir(), "input.csv")
	if err := os.WriteFile(inputPath, bytes.Repeat([]byte("1234567890\n"), 128*16), 0o600); err != nil {
		t.Fatal(err)
	}

	requestSeen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data; boundary=") {
			t.Fatalf("expected multipart content type, got %q", r.Header.Get("Content-Type"))
		}
		if r.ContentLength != -1 {
			t.Fatalf("expected streamed request with unknown content length, got %d", r.ContentLength)
		}
		requestSeen <- struct{}{}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		partCount := 0
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			partCount++
			_, _ = io.Copy(io.Discard, part)
		}
		if partCount < 3 {
			t.Fatalf("expected multipart fields and files, got %d parts", partCount)
		}
		_ = json.NewEncoder(w).Encode(protocol.ScaleResponse{RequestID: "req_test", Status: "ok", Mode: "data"})
	}))
	defer server.Close()
	writeCLIConfig(t, home, protocol.CLIConfig{
		APIBaseURL:   server.URL,
		SessionToken: token,
		UserID:       "user-test",
		TrustTier:    protocol.TrustScaleAllowed,
		TokenType:    protocol.TokenTypeHeadless,
		CreatedAt:    time.Now().UTC(),
	})

	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"data", "--script", scriptPath, "--input", inputPath, "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	select {
	case <-requestSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("expected multipart request")
	}
	if !strings.Contains(stdout.String(), `"status":"ok"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestDataOversizedStdinRejectedInCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SQUIRE_SCALE_STDIN_MAX_BYTES", "4")
	writeCLIConfig(t, home, protocol.CLIConfig{
		APIBaseURL:   "http://127.0.0.1:1",
		SessionToken: "sqh_test",
		UserID:       "user-test",
		TrustTier:    protocol.TrustScaleAllowed,
		TokenType:    protocol.TokenTypeHeadless,
		CreatedAt:    time.Now().UTC(),
	})

	scriptPath := filepath.Join(t.TempDir(), "transform.py")
	if err := os.WriteFile(scriptPath, []byte("print('x')"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"data", "--script", scriptPath, "--stdin"}, strings.NewReader("12345"), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected usage exit for oversized stdin, got %d", code)
	}
	if !strings.Contains(stderr.String(), "stdin payload exceeds") {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestDepsJSONRequestViaCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	token := "sqh_test"
	writeCLIConfig(t, home, protocol.CLIConfig{
		APIBaseURL:   "http://placeholder",
		SessionToken: token,
		UserID:       "user-test",
		TrustTier:    protocol.TrustTrusted,
		TokenType:    protocol.TokenTypeHeadless,
		CreatedAt:    time.Now().UTC(),
	})

	manifestPath := filepath.Join(t.TempDir(), "requirements.txt")
	if err := os.WriteFile(manifestPath, []byte("requests==2.32.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if r.URL.Path != "/v1/deps" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req protocol.DepsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Language != "python" || len(req.Targets) != 2 || !strings.Contains(req.Manifest, "requests==2.32.0") {
			t.Fatalf("unexpected deps request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(protocol.DepsResponse{
			RequestID: "req_deps",
			Language:  "python",
			Summary:   protocol.DepsSummary{Passed: 2},
			Results: []protocol.DepsResult{
				{Target: "py310", Status: "pass", RuntimeMS: 10},
				{Target: "py311", Status: "pass", RuntimeMS: 11},
			},
		})
	}))
	defer server.Close()
	writeCLIConfig(t, home, protocol.CLIConfig{
		APIBaseURL:   server.URL,
		SessionToken: token,
		UserID:       "user-test",
		TrustTier:    protocol.TrustTrusted,
		TokenType:    protocol.TokenTypeHeadless,
		CreatedAt:    time.Now().UTC(),
	})

	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"deps", "--lang", "python", "--file", manifestPath, "--targets", "py310,py311", "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_deps"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestSQLJSONRequestViaCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	token := "sqh_test"
	writeCLIConfig(t, home, protocol.CLIConfig{
		APIBaseURL:   "http://placeholder",
		SessionToken: token,
		UserID:       "user-test",
		TrustTier:    protocol.TrustTrusted,
		TokenType:    protocol.TokenTypeHeadless,
		CreatedAt:    time.Now().UTC(),
	})

	schemaPath := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(schemaPath, []byte("create table users(id integer);"), 0o600); err != nil {
		t.Fatal(err)
	}
	queryPath := filepath.Join(t.TempDir(), "query.sql")
	if err := os.WriteFile(queryPath, []byte("select 1 as id;"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if r.URL.Path != "/v1/sql" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req protocol.SQLRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Dialect != "sqlite" || !strings.Contains(req.Schema, "create table") || !strings.Contains(req.Query, "select 1") {
			t.Fatalf("unexpected sql request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(protocol.SQLResponse{
			RequestID:    "req_sql",
			Dialect:      "sqlite",
			Status:       "ok",
			RuntimeMS:    8,
			Columns:      []string{"id"},
			Rows:         [][]interface{}{{float64(1)}},
			RowCount:     1,
			AgentSummary: "sqlite query completed and returned 1 row(s).",
		})
	}))
	defer server.Close()
	writeCLIConfig(t, home, protocol.CLIConfig{
		APIBaseURL:   server.URL,
		SessionToken: token,
		UserID:       "user-test",
		TrustTier:    protocol.TrustTrusted,
		TokenType:    protocol.TokenTypeHeadless,
		CreatedAt:    time.Now().UTC(),
	})

	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"sql", "--dialect", "sqlite", "--schema", schemaPath, "--query-file", queryPath, "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_sql"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestRootHelpListsNewCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"help"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("unexpected exit code: %d", code)
	}
	text := stdout.String()
	for _, needle := range []string{"squire deps", "squire sql", "squire data", "squire media"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("expected root help to contain %q, got %s", needle, text)
		}
	}
}

func writeCLIConfig(t *testing.T, home string, cfg protocol.CLIConfig) {
	t.Helper()
	path := filepath.Join(home, ".squire", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
