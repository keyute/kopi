package app

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/keyute/kopi/internal/policy"
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

// systemPrompt lists only tools that are not deferred as available; the
// read-only and execute-mode sentence still counts the deferred ones, which
// the model can load and call.
func systemPrompt(workspace string, tools []tool.Tool, servers []mcpServer) string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if !t.Deferred {
			names = append(names, t.Name)
		}
	}
	available := "No tools are available."
	if len(names) > 0 {
		available = "Tools available: " + strings.Join(names, ", ") + "."
	}
	lines := []string{
		fmt.Sprintf("You are Kopi, a coding agent working in %s.", workspace),
		available,
		"The session has a plan mode and an execute mode; only the user can switch between them.",
	}
	if slices.ContainsFunc(tools, func(t tool.Tool) bool { return t.Capability != policy.Read }) {
		line := "Read-only tools work in both modes, the others only in execute mode, and any tool call may need the user's approval."
		if i := slices.IndexFunc(tools, func(t tool.Tool) bool { return t.PlanSandboxed }); i >= 0 {
			line = "Read-only tools work in both modes, the others only in execute mode, and any tool call may need the user's approval; in plan mode " + tools[i].Name + " runs too, but cannot write the workspace."
		}
		lines = append(lines, line)
	}
	lines = append(lines, "A tool call that is refused or that the user rejects returns an error saying so; when the rejection carries the user's feedback, act on it; never retry a rejected call unchanged.")
	if slices.ContainsFunc(tools, func(t tool.Tool) bool { return t.Deferred }) {
		lines = append(lines, "Tools of MCP servers are deferred: they are listed below by name and cannot be called until ToolSearch loads them. "+
			"Call ToolSearch with keywords for what you need, or with exact tool names; the tools it loads can be called in your next response, not in the one that calls ToolSearch.")
	}
	servers = slices.SortedFunc(slices.Values(servers), func(a, b mcpServer) int { return strings.Compare(a.name, b.name) })
	for _, s := range servers {
		if len(s.tools) > 0 {
			lines = append(lines, "MCP server "+s.name+" tools: "+strings.Join(s.tools, ", ")+".")
		}
		if text := strings.TrimSpace(s.instructions); text != "" {
			lines = append(lines, "Instructions from MCP server "+s.name+":", truncate(text, maxServerInstructions))
		}
	}
	return strings.Join(lines, "\n")
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
