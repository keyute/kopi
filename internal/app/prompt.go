package app

import (
	"fmt"
	"slices"
	"strings"

	"github.com/keyute/kopi/internal/tool"
)

func systemPrompt(workspace string, tools []tool.Tool) string {
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
	if slices.Contains(names, "Write") || slices.Contains(names, "Edit") {
		lines = append(lines, "Write and Edit work only in execute mode and may need the user's approval.")
	}
	lines = append(lines, "A tool call that is refused or that the user rejects returns an error saying so; never retry a rejected call.")
	return strings.Join(lines, "\n")
}
