package app

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/keyute/kopi/internal/instruction"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/subagent"
	"github.com/keyute/kopi/internal/tool"
)

// maxServerInstructions is how many bytes of each MCP server's instructions
// the system prompt carries.
const maxServerInstructions = 2048

// mcpServer is what the system prompt says about a started MCP server: tools
// are the full names of its tools that the rules left in.
type mcpServer struct {
	name, instructions string
	tools              []string
}

// buildPrompt lists only tools that are not deferred as available; the
// read-only and execute-mode sentence still counts the deferred ones, which
// the model can load and call. The parts run from what changes least to
// most: Kopi's own text, the instruction groups, then what the MCP servers
// report.
//
// def is nil for the root session and for the default workflow agent; a
// subagent's or a workflow agent's preamble says who it is, and a definition's
// own part follows the preamble.
func buildPrompt(workspace string, tools []tool.Tool, groups []instruction.Group, servers []mcpServer, reviewed bool, def *subagent.Definition, workflow bool) []pigruntime.PromptPart {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if !t.Deferred {
			names = append(names, t.Name)
		}
	}
	var available string
	switch {
	case len(names) > 0:
		available = "Tools available: " + strings.Join(names, ", ") + "."
	case len(tools) == 0:
		available = "No tools are available."
	}
	lines := []string{
		fmt.Sprintf("You are Kopi, a coding agent working in %s.", workspace),
	}
	switch {
	case workflow:
		lines = append(lines, "You are a workflow agent: a script gave you your task, and your final message is returned to that script as the result.")
	case def != nil:
		lines = append(lines, fmt.Sprintf("You are the subagent %s: another agent gave you your task, and your final message is returned to that agent as the result.", def.Name))
	}
	modes := "The session has a plan mode and an execute mode; only the user can switch between them."
	works := "Read-only tools work in both modes, the others only in execute mode"
	if def != nil && def.Plan {
		modes = "You stay in plan mode, whatever mode the user is in."
		works = "Read-only tools work, the others do not"
	}
	if available != "" {
		lines = append(lines, available)
	}
	lines = append(lines, modes)
	if slices.ContainsFunc(tools, func(t tool.Tool) bool { return t.Capability != policy.Read }) {
		line := works + ", and any tool call may need the user's approval."
		if i := slices.IndexFunc(tools, func(t tool.Tool) bool { return t.PlanSandboxed }); i >= 0 {
			line = works + ", and any tool call may need the user's approval; in plan mode " + tools[i].Name + " runs too, but cannot write the workspace."
			if def != nil && def.Plan {
				line = "Read-only tools work, and " + tools[i].Name + " runs but cannot write the workspace; the others do not, and any tool call may need the user's approval."
			}
		}
		lines = append(lines, line)
		if reviewed {
			lines = append(lines, "Tool calls that would need the user's approval and that no permission rule decides are first reviewed by a classifier model, which can allow them, deny them with a reason, or leave them to the user.")
		}
	}
	lines = append(lines, "A tool call that is refused or that the user rejects returns an error saying so, with the user's feedback when they gave any.")
	if slices.ContainsFunc(tools, func(t tool.Tool) bool { return t.Name == "ToolSearch" }) {
		lines = append(lines, "Tools of MCP servers are deferred: they are listed below by name and cannot be called until ToolSearch loads them. "+
			"Call ToolSearch with keywords for what you need, or with exact tool names; the tools it loads can be called in your next response, not in the one that calls ToolSearch.")
	}
	parts := []pigruntime.PromptPart{{Name: "preamble", Text: strings.Join(lines, "\n")}}
	if def != nil && strings.TrimSpace(def.Prompt) != "" {
		parts = append(parts, pigruntime.PromptPart{Name: "agent " + def.Name, Text: def.Prompt})
	}
	for _, g := range groups {
		texts := make([]string, 0, len(g.Files))
		for _, f := range g.Files {
			texts = append(texts, "Instructions from "+f.Path+":\n"+f.Text)
		}
		scope := "project"
		if g.Global {
			scope = "global"
		}
		parts = append(parts, pigruntime.PromptPart{Name: "instructions " + scope + " " + g.Dir, Text: strings.Join(texts, "\n\n")})
	}
	var serverLines []string
	servers = slices.SortedFunc(slices.Values(servers), func(a, b mcpServer) int { return strings.Compare(a.name, b.name) })
	for _, s := range servers {
		if len(s.tools) > 0 {
			serverLines = append(serverLines, "MCP server "+s.name+" tools: "+strings.Join(s.tools, ", ")+".")
		}
		if text := strings.TrimSpace(s.instructions); text != "" {
			serverLines = append(serverLines, "Instructions from MCP server "+s.name+":", truncate(text, maxServerInstructions))
		}
	}
	if len(serverLines) > 0 {
		parts = append(parts, pigruntime.PromptPart{Name: "mcp-servers", Text: strings.Join(serverLines, "\n")})
	}
	return parts
}

// truncate cuts s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
