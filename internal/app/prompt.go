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

// serversPart names the prompt part that says what the MCP servers report. It
// is the last part, and the only one replaced while a session is open.
const serversPart = "mcp-servers"

// mcpServer is what the system prompt says about a started MCP server: tools
// are the full names of its tools that the rules left in.
type mcpServer struct {
	name, instructions string
	tools              []string
}

// buildPrompt builds the parts, from what changes least to most: Kopi's own
// text, the instruction groups, then what the MCP servers report. The
// preamble does not list tools; the read-only and execute-mode sentence still
// counts the deferred ones, which the model can load and call.
//
// def is nil for the root session and for the default workflow agent; a
// subagent's or a workflow agent's preamble says who it is, and a definition's
// own part follows the preamble.
//
// joining reports that MCP servers may give the session tools after it opens,
// so the preamble speaks of tools that are not read-only whatever tools has.
func buildPrompt(workspace string, tools []tool.Tool, groups []instruction.Group, servers []mcpServer, def *subagent.Definition, workflow, joining bool) []pigruntime.PromptPart {
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
	lines = append(lines, modes)
	if joining || slices.ContainsFunc(tools, func(t tool.Tool) bool { return t.Capability != policy.Read }) {
		line := works + ", and any tool call may need the user's approval."
		if i := slices.IndexFunc(tools, func(t tool.Tool) bool { return t.PlanSandboxed }); i >= 0 {
			line = works + ", and any tool call may need the user's approval; in plan mode " + tools[i].Name + " runs too, but cannot write the workspace."
			if def != nil && def.Plan {
				line = "Read-only tools work, and " + tools[i].Name + " runs but cannot write the workspace; the others do not, and any tool call may need the user's approval."
			}
		}
		lines = append(lines, line)
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
			serverLines = append(serverLines, "MCP server "+s.name+" tools, loaded by ToolSearch: "+strings.Join(s.tools, ", ")+".")
		}
		if text := strings.TrimSpace(s.instructions); text != "" {
			serverLines = append(serverLines, "Instructions from MCP server "+s.name+":", truncate(text, maxServerInstructions))
		}
	}
	if len(serverLines) > 0 {
		parts = append(parts, pigruntime.PromptPart{Name: serversPart, Text: strings.Join(serverLines, "\n")})
	}
	return parts
}

// contextOf sorts the estimated sizes of a session's prompt parts, named as
// buildPrompt names them, and of its loaded tool definitions into categories,
// and gives Messages what they leave of total. Estimates that exceed total,
// which is itself an estimate until a reply reports usage, are scaled down to it.
func contextOf(total int, parts, tools []pigruntime.Size) Context {
	var c Context
	for _, p := range parts {
		switch {
		case strings.HasPrefix(p.Name, "instructions "):
			c.Instructions += p.Tokens
		case p.Name == serversPart:
			c.MCP += p.Tokens
		default:
			c.SystemPrompt += p.Tokens
		}
	}
	for _, t := range tools {
		switch {
		case t.Name == "Skill":
			c.Skills += t.Tokens
		case t.Name == "Agent":
			c.Agents += t.Tokens
		case strings.HasPrefix(t.Name, policy.MCPPrefix):
			c.MCP += t.Tokens
		default:
			c.Tools += t.Tokens
		}
	}
	estimated := c.SystemPrompt + c.Tools + c.Instructions + c.Skills + c.Agents + c.MCP
	if estimated <= total {
		c.Messages = total - estimated
		return c
	}
	c.Messages = total
	for _, n := range []*int{&c.SystemPrompt, &c.Tools, &c.Instructions, &c.Skills, &c.Agents, &c.MCP} {
		*n = *n * total / estimated
		c.Messages -= *n
	}
	return c
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
