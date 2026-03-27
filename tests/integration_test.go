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

func TestScaleMultipartStreamingViaCLI(t *testing.T) {
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
	code := cliapp.Run([]string{"scale", "--mode", "data", "--script", scriptPath, "--input", inputPath, "--json"}, bytes.NewReader(nil), &stdout, &stderr)
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

func TestScaleOversizedStdinRejectedInCLI(t *testing.T) {
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
	code := cliapp.Run([]string{"scale", "--mode", "data", "--script", scriptPath, "--stdin"}, strings.NewReader("12345"), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected usage exit for oversized stdin, got %d", code)
	}
	if !strings.Contains(stderr.String(), "stdin payload exceeds") {
		t.Fatalf("unexpected stderr: %s", stderr.String())
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
