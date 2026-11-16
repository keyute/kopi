package app

import (
	"fmt"
	"slices"
	"strings"

	"github.com/keyute/kopi/internal/policy"
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
	if slices.ContainsFunc(tools, func(t tool.Tool) bool { return t.Capability != policy.Read }) {
		lines = append(lines, "Tools other than Read work only in execute mode and may need the user's approval.")
	}
	lines = append(lines, "A tool call that is refused or that the user rejects returns an error saying so; when the rejection carries the user's feedback, act on it; never retry a rejected call unchanged.")
	return strings.Join(lines, "\n")
}
