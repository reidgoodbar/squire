package tests

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestTestJSONRequestViaCLI(t *testing.T) {
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

	projectDir := t.TempDir()
	testPath := filepath.Join(projectDir, "test_app.py")
	if err := os.WriteFile(testPath, []byte("def test_ok():\n    assert 1 == 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if r.URL.Path != "/v1/test" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req protocol.TestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Language != "python" || req.Command != "pytest -q" || len(req.Targets) != 2 || len(req.Files) != 1 || req.Files[0].Path != "test_app.py" {
			t.Fatalf("unexpected test request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(protocol.TestResponse{
			RequestID: "req_test",
			Language:  "python",
			Summary:   protocol.TestSummary{Passed: 2},
			Results: []protocol.TestResult{
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
	code := cliapp.Run([]string{"test", "--lang", "python", "--file", testPath, "--cmd", "pytest -q", "--targets", "py310,py311", "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_test"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestLintJSONRequestViaCLI(t *testing.T) {
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

	projectDir := t.TempDir()
	mainPath := filepath.Join(projectDir, "src", "main.rs")
	if err := os.MkdirAll(filepath.Dir(mainPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mainPath, []byte("fn main() { println!(\"hi\"); }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(projectDir, "Cargo.toml")
	if err := os.WriteFile(manifestPath, []byte("[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if r.URL.Path != "/v1/lint" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req protocol.LintRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Language != "rust" || req.Tool != "clippy" || len(req.Targets) != 2 || len(req.Files) != 2 {
			t.Fatalf("unexpected lint request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(protocol.LintResponse{
			RequestID: "req_lint",
			Language:  "rust",
			Tool:      "clippy",
			Summary:   protocol.LintSummary{Passed: 2},
			Results: []protocol.LintResult{
				{Target: "stable", Status: "pass", RuntimeMS: 10},
				{Target: "nightly", Status: "pass", RuntimeMS: 11},
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
	code := cliapp.Run([]string{"lint", "--lang", "rust", "--tool", "clippy", "--file", manifestPath, "--file", mainPath, "--targets", "stable,nightly", "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_lint"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestAuditJSONRequestViaCLI(t *testing.T) {
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

	projectDir := t.TempDir()
	srcDir := filepath.Join(projectDir, "src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(srcDir, "app.py")
	if err := os.WriteFile(appPath, []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if r.URL.Path != "/v1/audit" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req protocol.AuditRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Kind != "secrets" || len(req.Files) != 1 || req.Files[0].Path != "src/app.py" {
			t.Fatalf("unexpected audit request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(protocol.AuditResponse{
			RequestID: "req_audit",
			Kind:      "secrets",
			Summary:   protocol.AuditSummary{Passed: 1},
			Results:   []protocol.AuditResult{{Target: "default", Status: "pass", RuntimeMS: 9}},
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
	code := cliapp.Run([]string{"audit", "--secrets", "--path", srcDir, "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_audit"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestBuildJSONRequestViaCLI(t *testing.T) {
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

	projectDir := t.TempDir()
	pkgDir := filepath.Join(projectDir, "demo")
	if err := os.MkdirAll(pkgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(projectDir, "pyproject.toml")
	if err := os.WriteFile(manifestPath, []byte("[build-system]\nrequires=[\"setuptools\",\"wheel\"]\nbuild-backend=\"setuptools.build_meta\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	initPath := filepath.Join(pkgDir, "__init__.py")
	if err := os.WriteFile(initPath, []byte("__version__='0.1.0'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if r.URL.Path != "/v1/build" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req protocol.BuildRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Language != "python" || len(req.Targets) != 2 || len(req.Files) != 2 {
			t.Fatalf("unexpected build request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(protocol.BuildResponse{
			RequestID: "req_build",
			Language:  "python",
			Summary:   protocol.BuildSummary{Passed: 2},
			Results: []protocol.BuildResult{
				{Target: "manylinux", Status: "pass", RuntimeMS: 10},
				{Target: "musllinux", Status: "pass", RuntimeMS: 11},
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
	code := cliapp.Run([]string{"build", "--lang", "python", "--file", manifestPath, "--path", pkgDir, "--targets", "manylinux,musllinux", "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_build"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestBenchJSONRequestViaCLI(t *testing.T) {
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

	projectDir := t.TempDir()
	benchPath := filepath.Join(projectDir, "bench.py")
	if err := os.WriteFile(benchPath, []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if r.URL.Path != "/v1/bench" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req protocol.BenchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Language != "python" || req.Iterations != 3 || len(req.Targets) != 2 || len(req.Files) != 1 {
			t.Fatalf("unexpected bench request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(protocol.BenchResponse{
			RequestID: "req_bench",
			Language:  "python",
			Summary:   protocol.BenchSummary{Passed: 2},
			Results: []protocol.BenchResult{
				{Target: "py310", Status: "pass", RuntimeMS: 10, Iterations: 3, AvgRuntimeMS: 4},
				{Target: "py311", Status: "pass", RuntimeMS: 11, Iterations: 3, AvgRuntimeMS: 5},
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
	code := cliapp.Run([]string{"bench", "--lang", "python", "--file", benchPath, "--targets", "py310,py311", "--iterations", "3", "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_bench"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestBrowserJSONRequestViaCLI(t *testing.T) {
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

	projectDir := t.TempDir()
	scriptPath := filepath.Join(projectDir, "browser.js")
	if err := os.WriteFile(scriptPath, []byte("await page.setContent('<h1>hi</h1>');\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if r.URL.Path != "/v1/browser" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req protocol.BrowserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Browser != "chromium" || req.ScriptPath != "browser.js" || req.ScreenshotName != "page.png" || len(req.Files) != 1 {
			t.Fatalf("unexpected browser request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(protocol.BrowserResponse{
			RequestID: "req_browser",
			Browser:   "chromium",
			Status:    "ok",
			RuntimeMS: 12,
			Title:     "Demo",
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
	code := cliapp.Run([]string{"browser", "--script", scriptPath, "--screenshot", "page.png", "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_browser"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestBrowserRemoteURLRejectedInCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCLIConfig(t, home, protocol.CLIConfig{
		APIBaseURL:   "http://placeholder",
		SessionToken: "sqh_test",
		UserID:       "user-test",
		TrustTier:    protocol.TrustTrusted,
		TokenType:    protocol.TokenTypeHeadless,
		CreatedAt:    time.Now().UTC(),
	})

	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"browser", "--url", "https://example.com"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected usage exit, got %d", code)
	}
	if !strings.Contains(stderr.String(), "browser remote URLs are disabled") {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestBrowserPathStagesBinaryAssetsViaCLI(t *testing.T) {
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

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "index.html"), []byte("<img src=\"./logo.png\" alt=\"logo\" />\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logoBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xff}
	if err := os.WriteFile(filepath.Join(projectDir, "logo.png"), logoBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req protocol.BrowserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if len(req.Files) != 2 {
			t.Fatalf("expected 2 staged files, got %d", len(req.Files))
		}
		var htmlFile, logoFile *protocol.SourceFile
		for i := range req.Files {
			switch req.Files[i].Path {
			case filepath.ToSlash(filepath.Join(filepath.Base(projectDir), "index.html")):
				htmlFile = &req.Files[i]
			case filepath.ToSlash(filepath.Join(filepath.Base(projectDir), "logo.png")):
				logoFile = &req.Files[i]
			}
		}
		if htmlFile == nil || logoFile == nil {
			t.Fatalf("unexpected staged files: %+v", req.Files)
		}
		if htmlFile.Encoding != "" {
			t.Fatalf("expected html file to stay text, got encoding=%q", htmlFile.Encoding)
		}
		if logoFile.Encoding != "base64" {
			t.Fatalf("expected binary logo to use base64 encoding, got %+v", *logoFile)
		}
		decoded, err := base64.StdEncoding.DecodeString(logoFile.Content)
		if err != nil {
			t.Fatalf("decode binary asset: %v", err)
		}
		if !bytes.Equal(decoded, logoBytes) {
			t.Fatalf("unexpected binary payload: %v", decoded)
		}
		_ = json.NewEncoder(w).Encode(protocol.BrowserResponse{
			RequestID: "req_browser_assets",
			Browser:   "chromium",
			Status:    "ok",
			RuntimeMS: 12,
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
	code := cliapp.Run([]string{"browser", "--path", projectDir, "--screenshot", "page.png", "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_browser_assets"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestCompileJSONRequestViaCLI(t *testing.T) {
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

	projectDir := t.TempDir()
	mainPath := filepath.Join(projectDir, "main.go")
	if err := os.WriteFile(mainPath, []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if r.URL.Path != "/v1/compile" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req protocol.CompileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Language != "go" || len(req.Targets) != 2 || len(req.Files) != 1 || req.Files[0].Path != "main.go" {
			t.Fatalf("unexpected compile request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(protocol.CompileResponse{
			RequestID: "req_compile",
			Language:  "go",
			Summary:   protocol.CompileSummary{Passed: 2},
			Results: []protocol.CompileResult{
				{Target: "linux/amd64", Status: "pass", RuntimeMS: 10},
				{Target: "linux/arm64", Status: "pass", RuntimeMS: 11},
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
	code := cliapp.Run([]string{"compile", "--lang", "go", "--file", mainPath, "--targets", "linux/amd64,linux/arm64", "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_compile"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestTestJSONRequestPreservesNestedRelativePaths(t *testing.T) {
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

	projectRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectRoot, "test"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "package.json"), []byte(`{"name":"demo","scripts":{"test":"node --test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "test", "app.test.mjs"), []byte("import test from 'node:test';\ntest('ok', () => {});\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req protocol.TestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		gotPaths := make([]string, 0, len(req.Files))
		for _, file := range req.Files {
			gotPaths = append(gotPaths, file.Path)
		}
		for _, want := range []string{"package.json", "test/app.test.mjs"} {
			if !containsString(gotPaths, want) {
				t.Fatalf("expected staged path %q in %+v", want, gotPaths)
			}
		}
		_ = json.NewEncoder(w).Encode(protocol.TestResponse{
			RequestID: "req_test_paths",
			Language:  "node",
			Summary:   protocol.TestSummary{Passed: 1},
			Results:   []protocol.TestResult{{Target: "node20", Status: "pass", RuntimeMS: 12}},
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

	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(projectRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(originalWD)
	})

	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"test", "--lang", "node", "--file", "package.json", "--file", "test/app.test.mjs", "--cmd", "node --test", "--targets", "node20", "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_test_paths"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestCompileJSONRequestPreservesNestedRelativePaths(t *testing.T) {
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

	projectRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectRoot, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "Cargo.toml"), []byte("[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "src", "main.rs"), []byte("fn main() { println!(\"ok\"); }\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req protocol.CompileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		gotPaths := make([]string, 0, len(req.Files))
		for _, file := range req.Files {
			gotPaths = append(gotPaths, file.Path)
		}
		for _, want := range []string{"Cargo.toml", "src/main.rs"} {
			if !containsString(gotPaths, want) {
				t.Fatalf("expected staged path %q in %+v", want, gotPaths)
			}
		}
		_ = json.NewEncoder(w).Encode(protocol.CompileResponse{
			RequestID: "req_compile_paths",
			Language:  "rust",
			Summary:   protocol.CompileSummary{Passed: 1},
			Results:   []protocol.CompileResult{{Target: "linux/amd64-musl", Status: "pass", RuntimeMS: 15}},
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

	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(projectRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(originalWD)
	})

	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"compile", "--lang", "rust", "--file", "Cargo.toml", "--file", "src/main.rs", "--targets", "linux/amd64-musl", "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_compile_paths"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestCompileJSONRequestPreservesNestedAbsolutePathsOutsideCWD(t *testing.T) {
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

	projectRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectRoot, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(projectRoot, "Cargo.toml")
	mainPath := filepath.Join(projectRoot, "src", "main.rs")
	if err := os.WriteFile(manifestPath, []byte("[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mainPath, []byte("fn main() { println!(\"ok\"); }\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req protocol.CompileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		gotPaths := make([]string, 0, len(req.Files))
		for _, file := range req.Files {
			gotPaths = append(gotPaths, file.Path)
		}
		for _, want := range []string{"Cargo.toml", "src/main.rs"} {
			if !containsString(gotPaths, want) {
				t.Fatalf("expected staged path %q in %+v", want, gotPaths)
			}
		}
		_ = json.NewEncoder(w).Encode(protocol.CompileResponse{
			RequestID: "req_compile_abs_paths",
			Language:  "rust",
			Summary:   protocol.CompileSummary{Passed: 1},
			Results:   []protocol.CompileResult{{Target: "linux/amd64-musl", Status: "pass", RuntimeMS: 15}},
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
	code := cliapp.Run([]string{"compile", "--lang", "rust", "--file", manifestPath, "--file", mainPath, "--targets", "linux/amd64-musl", "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_compile_abs_paths"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestAuditStaticConfigUsesStagedRequestPath(t *testing.T) {
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

	projectRoot := t.TempDir()
	srcDir := filepath.Join(projectRoot, "src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(projectRoot, "semgrep.yml")
	if err := os.WriteFile(configPath, []byte("rules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "app.py"), []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req protocol.AuditRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Config != "semgrep.yml" {
			t.Fatalf("expected staged config path, got %q", req.Config)
		}
		gotPaths := make([]string, 0, len(req.Files))
		for _, file := range req.Files {
			gotPaths = append(gotPaths, file.Path)
		}
		if !containsString(gotPaths, "semgrep.yml") {
			t.Fatalf("expected staged config file in %+v", gotPaths)
		}
		_ = json.NewEncoder(w).Encode(protocol.AuditResponse{
			RequestID: "req_audit_static_config",
			Kind:      "static",
			Tool:      "semgrep",
			Summary:   protocol.AuditSummary{Passed: 1},
			Results:   []protocol.AuditResult{{Target: "default", Status: "pass", RuntimeMS: 12}},
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
	code := cliapp.Run([]string{"audit", "--static", "--tool", "semgrep", "--config", configPath, "--path", srcDir, "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_audit_static_config"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestSolveJSONRequestViaCLI(t *testing.T) {
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

	modelPath := filepath.Join(t.TempDir(), "constraints.smt2")
	if err := os.WriteFile(modelPath, []byte("(check-sat)\n(get-model)\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if r.URL.Path != "/v1/solve" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req protocol.SolveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Solver != "z3" || !strings.Contains(req.Input, "(check-sat)") {
			t.Fatalf("unexpected solve request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(protocol.SolveResponse{
			RequestID: "req_solve",
			Solver:    "z3",
			Status:    "sat",
			RuntimeMS: 9,
			Model:     map[string]string{"x": "11"},
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
	code := cliapp.Run([]string{"solve", "--solver", "z3", "--file", modelPath, "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_solve"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestQuantumSimulateJSONRequestViaCLI(t *testing.T) {
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

	projectDir := t.TempDir()
	entryPath := filepath.Join(projectDir, "shor.py")
	helperPath := filepath.Join(projectDir, "helpers.py")
	if err := os.WriteFile(entryPath, []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helperPath, []byte("VALUE = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if r.URL.Path != "/v1/quantum/simulate" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req protocol.QuantumSimulateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.EntryFile != "shor.py" || req.Shots != 2048 || req.Backend != "aer_simulator" || len(req.Files) != 2 {
			t.Fatalf("unexpected quantum request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(protocol.QuantumSimulateResponse{
			RequestID: "req_quantum",
			Status:    "ok",
			RuntimeMS: 42,
			Backend:   "aer_simulator",
			Shots:     2048,
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
	code := cliapp.Run([]string{"quantum", "simulate", "--file", entryPath, "--file", helperPath, "--shots", "2048", "--backend", "aer_simulator", "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_quantum"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestUpdateInstallsReleaseArchive(t *testing.T) {
	installDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(installDir, "squire")
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v9.9.9/") {
			t.Fatalf("unexpected update path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/gzip")
		if err := writeReleaseArchive(w, []byte("new-binary")); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()
	t.Setenv("SQUIRE_UPDATE_BASE_URL", server.URL)

	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"update", "--version", "v9.9.9", "--install-dir", installDir, "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}

	var resp struct {
		Status           string `json:"status"`
		CurrentVersion   string `json:"current_version"`
		InstalledVersion string `json:"installed_version"`
		InstalledPath    string `json:"installed_path"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" {
		t.Fatalf("unexpected update status: %+v", resp)
	}
	if resp.InstalledVersion != "v9.9.9" {
		t.Fatalf("unexpected installed version: %+v", resp)
	}
	if resp.InstalledPath != targetPath {
		t.Fatalf("unexpected installed path: %+v", resp)
	}
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new-binary" {
		t.Fatalf("unexpected installed binary contents: %q", string(data))
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("expected installed binary to be executable, got mode %v", info.Mode())
	}
}

func TestRootHelpListsNewCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"--help"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("unexpected exit code: %d", code)
	}
	text := stdout.String()
	for _, needle := range []string{"squire update", "squire mcp", "squire deps", "squire sql", "squire test", "squire lint", "squire audit", "squire build", "squire bench", "squire browser", "squire compile", "squire solve", "squire quantum", "squire data", "squire media", "squire --help --json"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("expected root help to contain %q, got %s", needle, text)
		}
	}
}

func TestRootHelpJSONListsCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"--help", "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("unexpected exit code: %d, stderr=%s", code, stderr.String())
	}
	var payload struct {
		Name     string `json:"name"`
		Commands []struct {
			Name    string   `json:"name"`
			Aliases []string `json:"aliases"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("parse help json: %v", err)
	}
	if payload.Name != "squire" {
		t.Fatalf("unexpected payload name: %q", payload.Name)
	}
	needles := map[string]bool{
		"mcp":     false,
		"verify":  false,
		"deps":    false,
		"compile": false,
		"quantum": false,
		"data":    false,
		"media":   false,
		"scale":   false,
	}
	for _, command := range payload.Commands {
		if _, ok := needles[command.Name]; ok {
			needles[command.Name] = true
		}
		if command.Name == "scale" && len(command.Aliases) == 0 {
			t.Fatal("expected scale aliases in help json")
		}
	}
	for name, found := range needles {
		if !found {
			t.Fatalf("missing command %q in help json", name)
		}
	}
}

func TestMCPToolsListIncludesCoreCommands(t *testing.T) {
	requests := []map[string]interface{}{
		{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]interface{}{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]interface{}{},
				"clientInfo": map[string]interface{}{
					"name":    "test-client",
					"version": "1.0.0",
				},
			},
		},
		{
			"jsonrpc": "2.0",
			"method":  "notifications/initialized",
			"params":  map[string]interface{}{},
		},
		{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "tools/list",
			"params":  map[string]interface{}{},
		},
	}

	var stdin bytes.Buffer
	for _, request := range requests {
		data, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		stdin.Write(data)
		stdin.WriteByte('\n')
	}

	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"mcp", "serve"}, &stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
	}

	var responses []map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	for {
		var response map[string]interface{}
		if err := decoder.Decode(&response); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode mcp response: %v\nraw=%s", err, stdout.String())
		}
		responses = append(responses, response)
	}
	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d: %s", len(responses), stdout.String())
	}

	initResult, ok := responses[0]["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing initialize result: %+v", responses[0])
	}
	if initResult["protocolVersion"] != "2025-11-25" {
		t.Fatalf("unexpected protocol version: %+v", initResult)
	}

	listResult, ok := responses[1]["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing tools/list result: %+v", responses[1])
	}
	rawTools, ok := listResult["tools"].([]interface{})
	if !ok {
		t.Fatalf("missing tools array: %+v", listResult)
	}
	seen := map[string]bool{}
	for _, rawTool := range rawTools {
		tool, ok := rawTool.(map[string]interface{})
		if !ok {
			t.Fatalf("unexpected tool payload: %#v", rawTool)
		}
		name, _ := tool["name"].(string)
		seen[name] = true
	}
	for _, name := range []string{"help", "whoami", "verify", "deps", "sql", "test", "lint", "audit", "build", "bench", "browser", "compile", "solve", "quantum_simulate", "data", "media"} {
		if !seen[name] {
			t.Fatalf("missing MCP tool %q in %v", name, seen)
		}
	}
}

func TestMCPQuantumSimulateToolCallUsesExistingCLIFlow(t *testing.T) {
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

	projectDir := t.TempDir()
	entryPath := filepath.Join(projectDir, "shor.py")
	if err := os.WriteFile(entryPath, []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if r.URL.Path != "/v1/quantum/simulate" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req protocol.QuantumSimulateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.EntryFile != "shor.py" || req.Shots != 1024 {
			t.Fatalf("unexpected quantum request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(protocol.QuantumSimulateResponse{
			RequestID: "req_quantum_mcp",
			Status:    "ok",
			RuntimeMS: 55,
			Backend:   "aer_simulator",
			Shots:     1024,
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

	requests := []map[string]interface{}{
		{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]interface{}{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]interface{}{},
				"clientInfo": map[string]interface{}{
					"name":    "test-client",
					"version": "1.0.0",
				},
			},
		},
		{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "tools/call",
			"params": map[string]interface{}{
				"name": "quantum_simulate",
				"arguments": map[string]interface{}{
					"files": []string{entryPath},
				},
			},
		},
	}

	var stdin bytes.Buffer
	for _, request := range requests {
		data, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		stdin.Write(data)
		stdin.WriteByte('\n')
	}

	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"mcp", "serve"}, &stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
	}

	var responses []map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	for {
		var response map[string]interface{}
		if err := decoder.Decode(&response); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode mcp response: %v\nraw=%s", err, stdout.String())
		}
		responses = append(responses, response)
	}
	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d: %s", len(responses), stdout.String())
	}

	callResult, ok := responses[1]["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing tools/call result: %+v", responses[1])
	}
	structured, ok := callResult["structuredContent"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing structuredContent: %+v", callResult)
	}
	if structured["request_id"] != "req_quantum_mcp" {
		t.Fatalf("unexpected structured response: %+v", structured)
	}
}

func TestMCPVerifyToolCallUsesExistingCLIFlow(t *testing.T) {
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if r.URL.Path != "/v1/verify" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req protocol.VerifyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Language != "python" || req.Code != "print('hi')" || len(req.Targets) != 2 {
			t.Fatalf("unexpected verify request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(protocol.VerifyResponse{
			RequestID: "req_verify_mcp",
			Summary:   protocol.VerifySummary{Passed: 2},
			Results: []protocol.VerifyResult{
				{Target: "alpine-3.20", Status: "pass", RuntimeMS: 12},
				{Target: "ubuntu-24.04", Status: "pass", RuntimeMS: 14},
			},
			AgentSummary: "verify passed on both targets.",
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

	requests := []map[string]interface{}{
		{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]interface{}{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]interface{}{},
				"clientInfo": map[string]interface{}{
					"name":    "test-client",
					"version": "1.0.0",
				},
			},
		},
		{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "tools/call",
			"params": map[string]interface{}{
				"name": "verify",
				"arguments": map[string]interface{}{
					"language": "python",
					"code":     "print('hi')",
					"targets":  []string{"alpine-3.20", "ubuntu-24.04"},
					"timeout":  25,
				},
			},
		},
	}

	var stdin bytes.Buffer
	for _, request := range requests {
		data, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		stdin.Write(data)
		stdin.WriteByte('\n')
	}

	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"mcp", "serve"}, &stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
	}

	var responses []map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	for {
		var response map[string]interface{}
		if err := decoder.Decode(&response); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode mcp response: %v\nraw=%s", err, stdout.String())
		}
		responses = append(responses, response)
	}
	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d: %s", len(responses), stdout.String())
	}

	callResult, ok := responses[1]["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing tools/call result: %+v", responses[1])
	}
	if isError, _ := callResult["isError"].(bool); isError {
		t.Fatalf("expected successful tool result: %+v", callResult)
	}
	structured, ok := callResult["structuredContent"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing structuredContent: %+v", callResult)
	}
	if structured["request_id"] != "req_verify_mcp" {
		t.Fatalf("unexpected structured response: %+v", structured)
	}
	content, ok := callResult["content"].([]interface{})
	if !ok || len(content) == 0 {
		t.Fatalf("missing content blocks: %+v", callResult)
	}
}

func TestMCPMediaToolCallCanDownloadArtifacts(t *testing.T) {
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

	projectDir := t.TempDir()
	scriptPath := filepath.Join(projectDir, "clip.py")
	if err := os.WriteFile(scriptPath, []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(projectDir, "image.png")
	if err := os.WriteFile(inputPath, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	downloadDir := filepath.Join(projectDir, "artifacts")
	artifactBody := []byte("artifact-body")

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/media":
			_ = json.NewEncoder(w).Encode(protocol.ScaleResponse{
				RequestID: "req_media_mcp",
				Status:    "ok",
				Mode:      "media",
				Artifacts: []protocol.Artifact{{
					Name:        "square.png",
					ContentType: "image/png",
					SizeBytes:   int64(len(artifactBody)),
					DownloadURL: server.URL + "/v1/jobs/req_media_mcp/artifacts/square.png",
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/req_media_mcp/artifacts/square.png":
			if got := r.Header.Get("Authorization"); got != "Bearer "+token {
				t.Fatalf("unexpected download auth header: %q", got)
			}
			_, _ = w.Write(artifactBody)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
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

	requests := []map[string]interface{}{
		{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]interface{}{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]interface{}{},
				"clientInfo": map[string]interface{}{
					"name":    "test-client",
					"version": "1.0.0",
				},
			},
		},
		{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "tools/call",
			"params": map[string]interface{}{
				"name": "media",
				"arguments": map[string]interface{}{
					"script":                 scriptPath,
					"input":                  inputPath,
					"download_artifacts_dir": downloadDir,
				},
			},
		},
	}

	var stdin bytes.Buffer
	for _, request := range requests {
		data, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		stdin.Write(data)
		stdin.WriteByte('\n')
	}

	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"mcp", "serve"}, &stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(downloadDir, "square.png"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, artifactBody) {
		t.Fatalf("unexpected artifact contents: %q", data)
	}

	var responses []map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	for {
		var response map[string]interface{}
		if err := decoder.Decode(&response); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode mcp response: %v\nraw=%s", err, stdout.String())
		}
		responses = append(responses, response)
	}
	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d: %s", len(responses), stdout.String())
	}
	callResult, ok := responses[1]["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing tools/call result: %+v", responses[1])
	}
	structured, ok := callResult["structuredContent"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing structuredContent: %+v", callResult)
	}
	if structured["request_id"] != "req_media_mcp" {
		t.Fatalf("unexpected structured response: %+v", structured)
	}
}

func TestMediaCLIArtifactDownloadUsesAuthenticatedFetch(t *testing.T) {
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

	scriptPath := filepath.Join(t.TempDir(), "clip.py")
	if err := os.WriteFile(scriptPath, []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(inputPath, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	downloadDir := filepath.Join(t.TempDir(), "downloads")
	artifactBody := []byte("artifact-bytes")
	downloadSeen := false

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/media":
			if got := r.Header.Get("Authorization"); got != "Bearer "+token {
				t.Fatalf("unexpected auth header: %q", got)
			}
			_ = json.NewEncoder(w).Encode(protocol.ScaleResponse{
				RequestID: "req_media",
				Status:    "ok",
				Mode:      "media",
				Artifacts: []protocol.Artifact{
					{
						Name:        "square.png",
						ContentType: "image/png",
						SizeBytes:   int64(len(artifactBody)),
						DownloadURL: server.URL + "/v1/jobs/req_media/artifacts/square.png",
					},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/req_media/artifacts/square.png":
			if got := r.Header.Get("Authorization"); got != "Bearer "+token {
				t.Fatalf("unexpected download auth header: %q", got)
			}
			if got := r.Header.Get("User-Agent"); !strings.HasPrefix(got, "squire-cli/") {
				t.Fatalf("unexpected user agent: %q", got)
			}
			downloadSeen = true
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(artifactBody)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
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
	code := cliapp.Run([]string{"media", "--script", scriptPath, "--input", inputPath, "--download-artifacts", downloadDir, "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	if !downloadSeen {
		t.Fatal("expected authenticated artifact download")
	}
	data, err := os.ReadFile(filepath.Join(downloadDir, "square.png"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, artifactBody) {
		t.Fatalf("unexpected artifact contents: %q", data)
	}
	if !strings.Contains(stdout.String(), `"request_id":"req_media"`) {
		t.Fatalf("unexpected cli stdout: %s", stdout.String())
	}
}

func TestBuildCLIArtifactDownloadUsesTargetSubdirectories(t *testing.T) {
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

	manifestPath := filepath.Join(t.TempDir(), "package.json")
	if err := os.WriteFile(manifestPath, []byte(`{"name":"demo","version":"1.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	downloadDir := filepath.Join(t.TempDir(), "downloads")

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/build":
			_ = json.NewEncoder(w).Encode(protocol.BuildResponse{
				RequestID: "req_build",
				Language:  "node",
				Summary:   protocol.BuildSummary{Passed: 2},
				Results: []protocol.BuildResult{
					{
						Target:    "linux/amd64",
						Status:    "pass",
						RuntimeMS: 10,
						Artifacts: []protocol.Artifact{{
							Name:        "package.tgz",
							ContentType: "application/gzip",
							SizeBytes:   3,
							DownloadURL: server.URL + "/v1/jobs/req_build/artifacts/linux_amd64.tgz",
						}},
					},
					{
						Target:    "linux/arm64",
						Status:    "pass",
						RuntimeMS: 11,
						Artifacts: []protocol.Artifact{{
							Name:        "package.tgz",
							ContentType: "application/gzip",
							SizeBytes:   3,
							DownloadURL: server.URL + "/v1/jobs/req_build/artifacts/linux_arm64.tgz",
						}},
					},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/req_build/artifacts/linux_amd64.tgz":
			_, _ = w.Write([]byte("amd"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/req_build/artifacts/linux_arm64.tgz":
			_, _ = w.Write([]byte("arm"))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
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
	code := cliapp.Run([]string{"build", "--lang", "node", "--file", manifestPath, "--download-artifacts", downloadDir}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cli exited %d stderr=%s", code, stderr.String())
	}
	amd, err := os.ReadFile(filepath.Join(downloadDir, "linux_amd64", "package.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	if string(amd) != "amd" {
		t.Fatalf("unexpected amd64 artifact: %q", amd)
	}
	arm, err := os.ReadFile(filepath.Join(downloadDir, "linux_arm64", "package.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	if string(arm) != "arm" {
		t.Fatalf("unexpected arm64 artifact: %q", arm)
	}
	if !strings.Contains(stdout.String(), "downloaded artifacts: 2") {
		t.Fatalf("expected download summary in stdout, got %s", stdout.String())
	}
}

func TestMediaHelpMentionsFFmpegAndArtifactDownloads(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"help", "media"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
	}
	text := stdout.String()
	for _, needle := range []string{"ffmpeg", "SQUIRE_INPUT_PATH", "SQUIRE_OUTPUT_DIR", "--download-artifacts"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("expected media help to contain %q, got %s", needle, text)
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

func writeReleaseArchive(w io.Writer, binary []byte) error {
	if _, err := exec.LookPath("tar"); err != nil {
		return fmt.Errorf("tar not available: %w", err)
	}
	stageDir, err := os.MkdirTemp("", "squire-release-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stageDir)
	if err := os.WriteFile(filepath.Join(stageDir, "squire"), binary, 0o755); err != nil {
		return err
	}
	cmd := exec.Command("tar", "-C", stageDir, "-czf", "-", "squire")
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("tar failed: %s", message)
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
