package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

//go:embed commands.json
var commandsJSON []byte

type Catalog struct {
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	AgentDescription string    `json:"agent_description"`
	FeaturedExamples []string  `json:"featured_examples"`
	Commands         []Command `json:"commands"`
}

type Command struct {
	Name            string                 `json:"name"`
	Path            string                 `json:"path"`
	Category        string                 `json:"category"`
	Title           string                 `json:"title"`
	Summary         string                 `json:"summary"`
	Description     string                 `json:"description"`
	Usage           string                 `json:"usage,omitempty"`
	WhenToUse       []string               `json:"when_to_use,omitempty"`
	WhenNotToUse    []string               `json:"when_not_to_use,omitempty"`
	PublicStatus    string                 `json:"public_status"`
	PolicyNotes     []string               `json:"policy_notes,omitempty"`
	SupportsJSON    bool                   `json:"supports_json"`
	MCPExposed      bool                   `json:"mcp_exposed"`
	MCPToolName     string                 `json:"mcp_tool_name,omitempty"`
	MCPInputSchema  map[string]interface{} `json:"mcp_input_schema,omitempty"`
	Examples        []string               `json:"examples,omitempty"`
	HelpFlags       []Flag                 `json:"help_flags,omitempty"`
	AgentHint       string                 `json:"agent_hint,omitempty"`
	Stability       string                 `json:"stability,omitempty"`
	Audience        string                 `json:"audience,omitempty"`
	PublicExamples  []string               `json:"public_examples,omitempty"`
	DisabledReason  string                 `json:"disabled_reason,omitempty"`
	RequiresTrusted bool                   `json:"requires_trusted,omitempty"`
	OfflineOnly     bool                   `json:"offline_only,omitempty"`
	Aliases         []string               `json:"aliases,omitempty"`
	Arguments       []Argument             `json:"arguments,omitempty"`
	Subcommands     []Command              `json:"subcommands,omitempty"`
}

type Argument struct {
	Name        string `json:"name"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description"`
}

type Flag struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Required    bool     `json:"required,omitempty"`
	Description string   `json:"description"`
	Default     string   `json:"default,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Repeatable  bool     `json:"repeatable,omitempty"`
}

var (
	once      sync.Once
	loaded    Catalog
	loadError error
)

func MustLoad() Catalog {
	once.Do(func() {
		loadError = json.Unmarshal(commandsJSON, &loaded)
		if loadError != nil {
			return
		}
		loadError = validateCatalog(loaded)
	})
	if loadError != nil {
		panic(loadError)
	}
	return loaded
}

func validateCatalog(c Catalog) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("catalog name is required")
	}
	if len(c.Commands) == 0 {
		return fmt.Errorf("catalog commands are required")
	}
	seenPaths := map[string]struct{}{}
	for _, cmd := range c.Commands {
		if err := validateCommand(cmd, seenPaths, false); err != nil {
			return err
		}
	}
	return nil
}

func validateCommand(cmd Command, seenPaths map[string]struct{}, nested bool) error {
	if strings.TrimSpace(cmd.Name) == "" {
		return fmt.Errorf("command name is required")
	}
	if strings.TrimSpace(cmd.Path) == "" {
		return fmt.Errorf("command %s path is required", cmd.Name)
	}
	if _, ok := seenPaths[cmd.Path]; ok {
		return fmt.Errorf("duplicate command path %s", cmd.Path)
	}
	seenPaths[cmd.Path] = struct{}{}
	if strings.TrimSpace(cmd.Category) == "" {
		return fmt.Errorf("command %s category is required", cmd.Path)
	}
	if strings.TrimSpace(cmd.Title) == "" {
		return fmt.Errorf("command %s title is required", cmd.Path)
	}
	if strings.TrimSpace(cmd.Summary) == "" {
		return fmt.Errorf("command %s summary is required", cmd.Path)
	}
	if strings.TrimSpace(cmd.Description) == "" {
		return fmt.Errorf("command %s description is required", cmd.Path)
	}
	switch cmd.PublicStatus {
	case "enabled", "disabled", "trusted_only":
	default:
		return fmt.Errorf("command %s has invalid public_status %q", cmd.Path, cmd.PublicStatus)
	}
	if cmd.MCPExposed {
		if strings.TrimSpace(cmd.MCPToolName) == "" {
			return fmt.Errorf("command %s is MCP exposed but missing mcp_tool_name", cmd.Path)
		}
		if cmd.MCPInputSchema == nil {
			return fmt.Errorf("command %s is MCP exposed but missing mcp_input_schema", cmd.Path)
		}
	}
	for _, sub := range cmd.Subcommands {
		if err := validateCommand(sub, seenPaths, true); err != nil {
			return err
		}
	}
	_ = nested
	return nil
}

func TopLevelCommands() []Command {
	c := MustLoad()
	out := make([]Command, len(c.Commands))
	copy(out, c.Commands)
	return out
}

func AllCommands() []Command {
	var out []Command
	for _, cmd := range MustLoad().Commands {
		flattenCommand(&out, cmd)
	}
	return out
}

func flattenCommand(out *[]Command, cmd Command) {
	*out = append(*out, cmd)
	for _, sub := range cmd.Subcommands {
		flattenCommand(out, sub)
	}
}

func Find(path string) (Command, bool) {
	path = normalizePath(path)
	for _, cmd := range AllCommands() {
		if cmd.Path == path {
			return cmd, true
		}
	}
	return Command{}, false
}

func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.ReplaceAll(path, " ", ".")
	return strings.Trim(path, ".")
}

func FindTopLevel(name string) (Command, bool) {
	name = strings.TrimSpace(name)
	for _, cmd := range MustLoad().Commands {
		if cmd.Name == name {
			return cmd, true
		}
	}
	return Command{}, false
}

func MCPCommands() []Command {
	out := make([]Command, 0)
	for _, cmd := range AllCommands() {
		if cmd.MCPExposed {
			out = append(out, cmd)
		}
	}
	return out
}

func TopLevelNames() []string {
	commands := MustLoad().Commands
	out := make([]string, 0, len(commands))
	for _, cmd := range commands {
		out = append(out, cmd.Name)
	}
	return out
}

func FindByMCPToolName(name string) (Command, bool) {
	name = strings.TrimSpace(name)
	for _, cmd := range MCPCommands() {
		if cmd.MCPToolName == name {
			return cmd, true
		}
	}
	return Command{}, false
}

func MCPToolNames() []string {
	commands := MCPCommands()
	out := make([]string, 0, len(commands))
	for _, cmd := range commands {
		out = append(out, cmd.MCPToolName)
	}
	return out
}
