package app

import (
	"reflect"
	"testing"

	"squire/internal/catalog"
)

func TestRootDispatchMatchesCatalogTopLevelCommands(t *testing.T) {
	got := make([]string, 0, len(rootDispatchEntries()))
	for _, entry := range rootDispatchEntries() {
		if entry.Name == "--help" || entry.Name == "-h" {
			continue
		}
		got = append(got, entry.Name)
	}
	want := catalog.TopLevelNames()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("root dispatch mismatch\nwant: %v\ngot:  %v", want, got)
	}
}

func TestMCPToolMapMatchesCatalog(t *testing.T) {
	tools := mcpToolMap()
	for _, name := range catalog.MCPToolNames() {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("missing MCP handler for %q", name)
		}
		cmd, ok := catalog.FindByMCPToolName(name)
		if !ok {
			t.Fatalf("missing catalog command for %q", name)
		}
		if tool.Title != cmd.Title {
			t.Fatalf("tool %q title mismatch: %q != %q", name, tool.Title, cmd.Title)
		}
	}
	if len(tools) != len(catalog.MCPToolNames()) {
		t.Fatalf("unexpected MCP tool count: got %d want %d", len(tools), len(catalog.MCPToolNames()))
	}
}
