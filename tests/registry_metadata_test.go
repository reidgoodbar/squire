package tests

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type registryServerFile struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Packages    []struct {
		RegistryType string `json:"registryType"`
		Identifier   string `json:"identifier"`
		Transport    struct {
			Type string `json:"type"`
		} `json:"transport"`
		EnvironmentVariables []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			IsRequired  bool   `json:"isRequired"`
			Format      string `json:"format"`
			IsSecret    bool   `json:"isSecret"`
		} `json:"environmentVariables"`
	} `json:"packages"`
}

func TestRegistryServerMetadataMatchesCurrentVersion(t *testing.T) {
	versionBytes, err := os.ReadFile("../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(versionBytes))
	data, err := os.ReadFile("../registry/server.json")
	if err != nil {
		t.Fatal(err)
	}
	var server registryServerFile
	if err := json.Unmarshal(data, &server); err != nil {
		t.Fatal(err)
	}
	if server.Name != "io.github.reidgoodbar/squire" {
		t.Fatalf("unexpected registry name: %q", server.Name)
	}
	if server.Version != version {
		t.Fatalf("registry/server.json version %q does not match VERSION %q", server.Version, version)
	}
	if len(server.Packages) != 1 {
		t.Fatalf("expected 1 package, got %d", len(server.Packages))
	}
	pkg := server.Packages[0]
	if pkg.RegistryType != "oci" {
		t.Fatalf("unexpected registryType: %q", pkg.RegistryType)
	}
	wantIdentifier := "ghcr.io/reidgoodbar/squire-mcp:" + version
	if pkg.Identifier != wantIdentifier {
		t.Fatalf("unexpected identifier: %q != %q", pkg.Identifier, wantIdentifier)
	}
	if pkg.Transport.Type != "stdio" {
		t.Fatalf("unexpected transport type: %q", pkg.Transport.Type)
	}
	env := map[string]struct {
		required bool
		secret   bool
		format   string
	}{}
	for _, variable := range pkg.EnvironmentVariables {
		env[variable.Name] = struct {
			required bool
			secret   bool
			format   string
		}{
			required: variable.IsRequired,
			secret:   variable.IsSecret,
			format:   variable.Format,
		}
	}
	token, ok := env["SQUIRE_TOKEN"]
	if !ok {
		t.Fatal("missing SQUIRE_TOKEN environment variable")
	}
	if token.required || !token.secret || token.format != "string" {
		t.Fatalf("unexpected SQUIRE_TOKEN metadata: %+v", token)
	}
	baseURL, ok := env["SQUIRE_API_BASE_URL"]
	if !ok {
		t.Fatal("missing SQUIRE_API_BASE_URL environment variable")
	}
	if baseURL.required || baseURL.secret || baseURL.format != "string" {
		t.Fatalf("unexpected SQUIRE_API_BASE_URL metadata: %+v", baseURL)
	}
}

func TestRegistryWrapperFilesStayAligned(t *testing.T) {
	dockerfile, err := os.ReadFile("../registry/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), `io.modelcontextprotocol.server.name="io.github.reidgoodbar/squire"`) {
		t.Fatal("registry Dockerfile is missing the required MCP ownership label")
	}
	entrypoint, err := os.ReadFile("../registry/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	entry := string(entrypoint)
	if !strings.Contains(entry, "squire mcp serve") {
		t.Fatal("registry entrypoint should launch squire mcp serve")
	}
}

func TestREADMEReferencesRegistryServer(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)
	if !strings.Contains(text, "io.github.reidgoodbar/squire") {
		t.Fatal("README should mention the MCP Registry server name")
	}
	if !strings.Contains(text, "anonymous access") {
		t.Fatal("README should mention anonymous MCP access")
	}
}
