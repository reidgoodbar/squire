package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
)

type RootHelpPayload struct {
	Name             string                   `json:"name"`
	Description      string                   `json:"description"`
	AgentDescription string                   `json:"agent_description"`
	FeaturedExamples []string                 `json:"featured_examples"`
	Commands         []Command                `json:"commands"`
	AllCommands      []Command                `json:"all_commands"`
	ByCategory       map[string][]Command     `json:"by_category"`
	ByPath           map[string]Command       `json:"by_path"`
	MCPTools         []map[string]interface{} `json:"mcp_tools"`
}

func RootHelpJSON() RootHelpPayload {
	c := MustLoad()
	byCategory := map[string][]Command{}
	byPath := map[string]Command{}
	for _, cmd := range c.Commands {
		byCategory[cmd.Category] = append(byCategory[cmd.Category], cmd)
	}
	for _, cmd := range AllCommands() {
		byPath[cmd.Path] = cmd
	}
	return RootHelpPayload{
		Name:             c.Name,
		Description:      c.Description,
		AgentDescription: c.AgentDescription,
		FeaturedExamples: append([]string(nil), c.FeaturedExamples...),
		Commands:         TopLevelCommands(),
		AllCommands:      AllCommands(),
		ByCategory:       byCategory,
		ByPath:           byPath,
		MCPTools:         MCPToolMetadata(),
	}
}

func RootHelpText() string {
	c := MustLoad()
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", c.Description)
	categories := orderedCategories(c.Commands)
	for _, category := range categories {
		fmt.Fprintf(&b, "%s:\n", strings.Title(category))
		for _, cmd := range commandsInCategory(c.Commands, category) {
			fmt.Fprintf(&b, "  squire %-11s %s\n", cmd.Name, cmd.Summary)
		}
		fmt.Fprintln(&b)
	}
	if len(c.FeaturedExamples) > 0 {
		fmt.Fprintln(&b, "Examples:")
		for _, example := range c.FeaturedExamples {
			fmt.Fprintf(&b, "  %s\n", example)
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintln(&b, "Use \"squire <command> --help\" for command-specific usage.")
	fmt.Fprintln(&b, "Use \"squire --help --json\" for a machine-readable command catalog.")
	return b.String()
}

func CommandHelpText(path string) (string, bool) {
	cmd, ok := Find(path)
	if !ok {
		return "", false
	}
	var b strings.Builder
	if cmd.Usage != "" {
		fmt.Fprintf(&b, "Usage: %s\n\n", cmd.Usage)
	} else {
		fmt.Fprintf(&b, "Usage: squire %s\n\n", strings.ReplaceAll(cmd.Path, ".", " "))
	}
	fmt.Fprintf(&b, "%s\n\n", cmd.Description)
	if len(cmd.Subcommands) > 0 {
		fmt.Fprintln(&b, "Subcommands:")
		for _, sub := range cmd.Subcommands {
			fmt.Fprintf(&b, "  squire %-18s %s\n", strings.ReplaceAll(sub.Path, ".", " "), sub.Summary)
		}
		fmt.Fprintln(&b)
	}
	if len(cmd.WhenToUse) > 0 {
		fmt.Fprintln(&b, "Use when:")
		for _, item := range cmd.WhenToUse {
			fmt.Fprintf(&b, "  - %s\n", item)
		}
		fmt.Fprintln(&b)
	}
	if len(cmd.WhenNotToUse) > 0 {
		fmt.Fprintln(&b, "Avoid when:")
		for _, item := range cmd.WhenNotToUse {
			fmt.Fprintf(&b, "  - %s\n", item)
		}
		fmt.Fprintln(&b)
	}
	status := publicStatusText(cmd)
	if status != "" || len(cmd.PolicyNotes) > 0 {
		fmt.Fprintln(&b, "Public service:")
		if status != "" {
			fmt.Fprintf(&b, "  - %s\n", status)
		}
		for _, note := range cmd.PolicyNotes {
			fmt.Fprintf(&b, "  - %s\n", note)
		}
		fmt.Fprintln(&b)
	}
	if len(cmd.Arguments) > 0 || len(cmd.HelpFlags) > 0 {
		fmt.Fprintln(&b, "Arguments and flags:")
		for _, arg := range cmd.Arguments {
			required := ""
			if arg.Required {
				required = " (required)"
			}
			fmt.Fprintf(&b, "  %s%s  %s\n", arg.Name, required, arg.Description)
		}
		for _, flag := range cmd.HelpFlags {
			fmt.Fprintf(&b, "  --%s", flag.Name)
			if flag.Type != "" && flag.Type != "boolean" {
				fmt.Fprintf(&b, " <%s>", flag.Type)
			}
			var extras []string
			if flag.Required {
				extras = append(extras, "required")
			}
			if flag.Default != "" {
				extras = append(extras, "default: "+flag.Default)
			}
			if len(flag.Enum) > 0 {
				extras = append(extras, "one of: "+strings.Join(flag.Enum, ", "))
			}
			if flag.Repeatable {
				extras = append(extras, "repeatable")
			}
			if len(extras) > 0 {
				fmt.Fprintf(&b, " (%s)", strings.Join(extras, "; "))
			}
			fmt.Fprintf(&b, "  %s\n", flag.Description)
		}
		fmt.Fprintln(&b)
	}
	if len(cmd.Examples) > 0 {
		fmt.Fprintln(&b, "Examples:")
		for _, example := range cmd.Examples {
			fmt.Fprintf(&b, "  %s\n", example)
		}
	}
	return b.String(), true
}

func publicStatusText(cmd Command) string {
	switch cmd.PublicStatus {
	case "enabled":
		return "enabled"
	case "disabled":
		if cmd.DisabledReason != "" {
			return "disabled: " + cmd.DisabledReason
		}
		return "disabled"
	case "trusted_only":
		if cmd.DisabledReason != "" {
			return "trusted-only: " + cmd.DisabledReason
		}
		return "trusted-only"
	default:
		return ""
	}
}

func orderedCategories(commands []Command) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, cmd := range commands {
		if _, ok := seen[cmd.Category]; ok {
			continue
		}
		seen[cmd.Category] = struct{}{}
		out = append(out, cmd.Category)
	}
	return out
}

func commandsInCategory(commands []Command, category string) []Command {
	out := make([]Command, 0)
	for _, cmd := range commands {
		if cmd.Category == category {
			out = append(out, cmd)
		}
	}
	return out
}

func MCPToolMetadata() []map[string]interface{} {
	commands := MCPCommands()
	out := make([]map[string]interface{}, 0, len(commands))
	for _, cmd := range commands {
		out = append(out, map[string]interface{}{
			"name":        cmd.MCPToolName,
			"title":       cmd.Title,
			"description": cmd.Description,
			"inputSchema": cmd.MCPInputSchema,
		})
	}
	return out
}

func RenderMarkdownReference() (string, error) {
	c := MustLoad()
	var b strings.Builder
	fmt.Fprintln(&b, "<!-- Generated from internal/catalog/commands.json. Do not edit by hand. -->")
	fmt.Fprintf(&b, "# %s Command Reference\n\n", displayName(c.Name))
	fmt.Fprintf(&b, "%s\n\n", c.AgentDescription)
	for _, category := range orderedCategories(c.Commands) {
		fmt.Fprintf(&b, "## %s\n\n", strings.Title(category))
		for _, cmd := range commandsInCategory(c.Commands, category) {
			renderMarkdownCommand(&b, cmd)
		}
	}
	return b.String(), nil
}

func renderMarkdownCommand(b *strings.Builder, cmd Command) {
	fmt.Fprintf(b, "### `squire %s`\n\n", strings.ReplaceAll(cmd.Path, ".", " "))
	fmt.Fprintf(b, "%s\n\n", cmd.Description)
	if cmd.Usage != "" {
		fmt.Fprintf(b, "- Usage: `%s`\n", cmd.Usage)
	}
	if status := publicStatusText(cmd); status != "" {
		fmt.Fprintf(b, "- Public status: `%s`\n", status)
	}
	if cmd.SupportsJSON {
		fmt.Fprintln(b, "- Supports `--json` output")
	}
	if cmd.OfflineOnly {
		fmt.Fprintln(b, "- Offline only")
	}
	if cmd.RequiresTrusted {
		fmt.Fprintln(b, "- Trusted access required on the public service")
	}
	if cmd.MCPExposed {
		fmt.Fprintf(b, "- MCP tool: `%s`\n", cmd.MCPToolName)
	}
	for _, note := range cmd.PolicyNotes {
		fmt.Fprintf(b, "- %s\n", note)
	}
	if len(cmd.WhenToUse) > 0 {
		fmt.Fprintln(b, "- Use when:")
		for _, item := range cmd.WhenToUse {
			fmt.Fprintf(b, "  - %s\n", item)
		}
	}
	if len(cmd.WhenNotToUse) > 0 {
		fmt.Fprintln(b, "- Avoid when:")
		for _, item := range cmd.WhenNotToUse {
			fmt.Fprintf(b, "  - %s\n", item)
		}
	}
	if len(cmd.HelpFlags) > 0 {
		fmt.Fprintln(b)
		fmt.Fprintln(b, "Flags:")
		fmt.Fprintln(b)
		for _, flag := range cmd.HelpFlags {
			fmt.Fprintf(b, "- `--%s`", flag.Name)
			if flag.Type != "" && flag.Type != "boolean" {
				fmt.Fprintf(b, " `<%s>`", flag.Type)
			}
			fmt.Fprintf(b, ": %s\n", flag.Description)
		}
	}
	if len(cmd.Examples) > 0 {
		fmt.Fprintln(b)
		fmt.Fprintln(b, "Examples:")
		fmt.Fprintln(b)
		fmt.Fprintln(b, "```bash")
		for _, example := range cmd.Examples {
			fmt.Fprintln(b, example)
		}
		fmt.Fprintln(b, "```")
	}
	fmt.Fprintln(b)
	for _, sub := range cmd.Subcommands {
		renderMarkdownCommand(b, sub)
	}
}

type websitePageData struct {
	Name        string
	Description string
	Categories  []websiteCategory
}

type websiteCategory struct {
	Title    string
	Commands []Command
}

func RenderWebsiteCommandsPage() (string, error) {
	c := MustLoad()
	data := websitePageData{
		Name:        displayName(c.Name),
		Description: c.AgentDescription,
		Categories:  make([]websiteCategory, 0),
	}
	for _, category := range orderedCategories(c.Commands) {
		data.Categories = append(data.Categories, websiteCategory{
			Title:    strings.Title(category),
			Commands: commandsInCategory(c.Commands, category),
		})
	}
	const page = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>{{.Name}} Commands</title>
    <meta name="description" content="{{.Description}}" />
    <link rel="stylesheet" href="./styles.css" />
  </head>
  <body>
    <main id="top">
      <section class="hero">
        <p><a href="./index.html">Back to home</a></p>
        <h1>{{.Name}} command reference</h1>
        <p class="lede">{{.Description}}</p>
      </section>
      {{range .Categories}}
      <section class="panel">
          <h2>{{.Title}}</h2>
        {{range .Commands}}
        <article class="panel">
          <h3><code>squire {{replace .Path "." " "}}</code></h3>
          <p><strong>{{.Summary}}</strong></p>
          <p>{{.Description}}</p>
          {{if .Usage}}<p><strong>Usage:</strong> <code>{{.Usage}}</code></p>{{end}}
          {{if .WhenToUse}}
          <p><strong>Use when:</strong></p>
          <ul class="module-list">
            {{range .WhenToUse}}<li>{{.}}</li>{{end}}
          </ul>
          {{end}}
          {{if or .OfflineOnly .RequiresTrusted (gt (len .PolicyNotes) 0) (ne .PublicStatus "enabled")}}
          <p><strong>Public policy:</strong></p>
          <ul class="module-list">
            {{if ne .PublicStatus "enabled"}}<li>{{publicStatus .}}</li>{{end}}
            {{if .OfflineOnly}}<li>Offline only.</li>{{end}}
            {{if .RequiresTrusted}}<li>Trusted access required on the public service.</li>{{end}}
            {{range .PolicyNotes}}<li>{{.}}</li>{{end}}
          </ul>
          {{end}}
          {{if .Examples}}
          <pre><code>{{join .Examples "\n"}}</code></pre>
          {{end}}
          {{if .Subcommands}}
          <p><strong>Subcommands:</strong></p>
          <ul class="module-list">
            {{range .Subcommands}}<li><code>squire {{replace .Path "." " "}}</code>: {{.Summary}}</li>{{end}}
          </ul>
          {{end}}
        </article>
        {{end}}
      </section>
      {{end}}
    </main>
  </body>
</html>`
	tmpl, err := template.New("commands").Funcs(template.FuncMap{
		"join":         strings.Join,
		"replace":      strings.ReplaceAll,
		"publicStatus": func(cmd Command) string { return publicStatusText(cmd) },
	}).Parse(page)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
}

func RootHelpJSONBytes() ([]byte, error) {
	return json.MarshalIndent(RootHelpJSON(), "", "  ")
}

func displayName(name string) string {
	if name == "" {
		return ""
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
