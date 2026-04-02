package tests

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	cliapp "squire/app"
	"squire/internal/catalog"
)

func TestRootHelpJSONIsRichCatalog(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cliapp.Run([]string{"--help", "--json"}, bytes.NewReader(nil), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
	}
	var payload struct {
		Name        string                       `json:"name"`
		Commands    []catalog.Command            `json:"commands"`
		AllCommands []catalog.Command            `json:"all_commands"`
		ByPath      map[string]catalog.Command   `json:"by_path"`
		MCPTools    []map[string]interface{}     `json:"mcp_tools"`
		ByCategory  map[string][]catalog.Command `json:"by_category"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("parse help json: %v", err)
	}
	if payload.Name != "squire" {
		t.Fatalf("unexpected name: %q", payload.Name)
	}
	if len(payload.Commands) == 0 || len(payload.AllCommands) == 0 {
		t.Fatal("expected commands and all_commands to be populated")
	}
	if len(payload.MCPTools) == 0 {
		t.Fatal("expected mcp_tools metadata")
	}
	deps, ok := payload.ByPath["deps"]
	if !ok {
		t.Fatal("missing deps entry in by_path")
	}
	if deps.PublicStatus != "disabled" {
		t.Fatalf("expected deps public_status disabled, got %q", deps.PublicStatus)
	}
	if _, ok := payload.ByPath["quantum.simulate"]; !ok {
		t.Fatal("missing quantum.simulate in by_path")
	}
	if len(payload.ByCategory["validation"]) == 0 {
		t.Fatal("expected validation category entries")
	}
}

func TestGeneratedCatalogDocsStayInSync(t *testing.T) {
	markdown, err := catalog.RenderMarkdownReference()
	if err != nil {
		t.Fatal(err)
	}
	markdownFile, err := os.ReadFile("../docs/commands.generated.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(markdownFile) != markdown {
		t.Fatal("cli/docs/commands.generated.md is out of sync with the catalog renderer")
	}

	website, err := catalog.RenderWebsiteCommandsPage()
	if err != nil {
		t.Fatal(err)
	}
	websitePath := "../../website/public/commands.html"
	if _, err := os.Stat(websitePath); err != nil {
		if os.IsNotExist(err) {
			t.Skip("website checkout not present; skipping website sync assertion")
		}
		t.Fatal(err)
	}
	websiteFile, err := os.ReadFile(websitePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(websiteFile) != website {
		t.Fatal("website/public/commands.html is out of sync with the catalog renderer")
	}
}

func TestGeneratedDocsContainEveryCatalogCommand(t *testing.T) {
	markdown, err := catalog.RenderMarkdownReference()
	if err != nil {
		t.Fatal(err)
	}
	website, err := catalog.RenderWebsiteCommandsPage()
	if err != nil {
		t.Fatal(err)
	}
	websiteAvailable := true
	if _, err := os.Stat("../../website/public/commands.html"); err != nil {
		if os.IsNotExist(err) {
			websiteAvailable = false
		} else {
			t.Fatal(err)
		}
	}
	for _, cmd := range catalog.AllCommands() {
		humanPath := "squire " + strings.ReplaceAll(cmd.Path, ".", " ")
		if !strings.Contains(markdown, humanPath) {
			t.Fatalf("markdown reference missing %q", humanPath)
		}
		if websiteAvailable && !strings.Contains(website, humanPath) {
			t.Fatalf("website commands page missing %q", humanPath)
		}
	}
}

func TestCLIHelpCommandWorksForEveryCatalogPath(t *testing.T) {
	for _, cmd := range catalog.AllCommands() {
		args := append([]string{"help"}, strings.Fields(strings.ReplaceAll(cmd.Path, ".", " "))...)
		var stdout, stderr bytes.Buffer
		code := cliapp.Run(args, bytes.NewReader(nil), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("help failed for %q: code=%d stderr=%s", cmd.Path, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), cmd.Description) {
			t.Fatalf("help output for %q missing description", cmd.Path)
		}
	}
}
