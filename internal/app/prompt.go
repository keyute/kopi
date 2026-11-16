package app

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

// maxServerInstructions is how many bytes of each MCP server's instructions
// the system prompt carries.
const maxServerInstructions = 2048

func systemPrompt(workspace string, tools []tool.Tool, serverInstructions map[string]string) string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
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
		lines = append(lines, "Read-only tools work in both modes, the others only in execute mode, and any tool call may need the user's approval.")
	}
	lines = append(lines, "A tool call that is refused or that the user rejects returns an error saying so; when the rejection carries the user's feedback, act on it; never retry a rejected call unchanged.")
	for _, name := range slices.Sorted(maps.Keys(serverInstructions)) {
		text := strings.TrimSpace(serverInstructions[name])
		if text == "" {
			continue
		}
		lines = append(lines, "Instructions from MCP server "+name+":", truncate(text, maxServerInstructions))
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
