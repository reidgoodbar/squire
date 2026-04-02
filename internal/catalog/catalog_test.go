package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogLoadsAndContainsExpectedCommands(t *testing.T) {
	c := MustLoad()
	if c.Name != "squire" {
		t.Fatalf("unexpected catalog name: %q", c.Name)
	}
	for _, path := range []string{
		"login",
		"mcp.login",
		"mcp.serve",
		"verify",
		"browser",
		"quantum.simulate",
		"scale",
	} {
		if _, ok := Find(path); !ok {
			t.Fatalf("missing command path %q", path)
		}
	}
}

func TestMCPToolCatalogIsDiscoverable(t *testing.T) {
	for _, toolName := range []string{
		"help",
		"whoami",
		"verify",
		"quantum_simulate",
		"data",
		"media",
	} {
		cmd, ok := FindByMCPToolName(toolName)
		if !ok {
			t.Fatalf("missing MCP tool %q in catalog", toolName)
		}
		if cmd.MCPInputSchema == nil {
			t.Fatalf("tool %q missing input schema", toolName)
		}
	}
}

func TestRootHelpJSONIncludesPolicyMetadata(t *testing.T) {
	payload := RootHelpJSON()
	deps, ok := payload.ByPath["deps"]
	if !ok {
		t.Fatal("deps command missing from by_path")
	}
	if deps.PublicStatus != "disabled" {
		t.Fatalf("expected deps to be disabled, got %q", deps.PublicStatus)
	}
	browser, ok := payload.ByPath["browser"]
	if !ok {
		t.Fatal("browser command missing from by_path")
	}
	if !browser.OfflineOnly {
		t.Fatal("expected browser to be marked offline-only")
	}
	quantum, ok := payload.ByPath["quantum.simulate"]
	if !ok {
		t.Fatal("quantum.simulate missing from by_path")
	}
	if !quantum.RequiresTrusted {
		t.Fatal("expected quantum.simulate to require trusted access")
	}
}

func TestEveryCatalogCommandRendersHelp(t *testing.T) {
	for _, cmd := range AllCommands() {
		text, ok := CommandHelpText(cmd.Path)
		if !ok {
			t.Fatalf("missing help text for %q", cmd.Path)
		}
		if !strings.Contains(text, cmd.Description) {
			t.Fatalf("help text for %q missing description", cmd.Path)
		}
		if cmd.Usage != "" && !strings.Contains(text, cmd.Usage) {
			t.Fatalf("help text for %q missing usage", cmd.Path)
		}
	}
}

func TestEveryMCPCommandHasValidSchemaMetadata(t *testing.T) {
	for _, cmd := range MCPCommands() {
		if strings.TrimSpace(cmd.MCPToolName) == "" {
			t.Fatalf("command %q missing MCP tool name", cmd.Path)
		}
		schema := cmd.MCPInputSchema
		if schema == nil {
			t.Fatalf("command %q missing MCP input schema", cmd.Path)
		}
		if schema["type"] != "object" {
			t.Fatalf("command %q schema type = %v, want object", cmd.Path, schema["type"])
		}
		if schema["additionalProperties"] != false {
			t.Fatalf("command %q schema must set additionalProperties=false", cmd.Path)
		}
		if _, ok := schema["properties"].(map[string]interface{}); !ok {
			t.Fatalf("command %q schema missing object properties", cmd.Path)
		}
	}
}

func TestRootHelpJSONRoundTrips(t *testing.T) {
	payload := RootHelpJSON()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded RootHelpPayload
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.AllCommands) != len(AllCommands()) {
		t.Fatalf("decoded all_commands mismatch: got %d want %d", len(decoded.AllCommands), len(AllCommands()))
	}
}
