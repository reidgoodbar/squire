package catalog

import "testing"

func TestCatalogLoadsAndContainsExpectedCommands(t *testing.T) {
	c := MustLoad()
	if c.Name != "squire" {
		t.Fatalf("unexpected catalog name: %q", c.Name)
	}
	for _, path := range []string{
		"login",
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
