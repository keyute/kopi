package app

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/instruction"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

// joinPrompt is the system prompt the provider receives for parts.
func joinPrompt(parts []pigruntime.PromptPart) string {
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n\n")
}

func partNames(parts []pigruntime.PromptPart) []string {
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		names = append(names, p.Name)
	}
	return names
}

func TestPromptMentionsExecuteModeOnlyWhenNonReadToolAvailable(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	ws, err := tool.NewWorkspace(sb.Workspace, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ws.Close()) })
	read, write, edit := tool.NewRead(ws), tool.NewWrite(ws), tool.NewEdit(ws)
	const sentence = "Read-only tools work in both modes, the others only in execute mode, and any tool call may need the user's approval."

	assert.Contains(t, joinPrompt(systemPrompt(sb.Workspace, []tool.Tool{read, edit}, nil, nil, false)), sentence)
	assert.Contains(t, joinPrompt(systemPrompt(sb.Workspace, []tool.Tool{read, {Name: "Probe", Capability: policy.Exec}}, nil, nil, false)), sentence)
	assert.Contains(t, joinPrompt(systemPrompt(sb.Workspace, []tool.Tool{read, {Name: "Probe", Capability: policy.Exec, PlanSandboxed: true}}, nil, nil, false)),
		"Read-only tools work in both modes, the others only in execute mode, and any tool call may need the user's approval; in plan mode Probe runs too, but cannot write the workspace.")
	rules, err := policy.NewRules(sb.Workspace, sb.Home, map[string]policy.ToolSpec{"Read": {}, "Write": {}, "Edit": {}}, policy.Source{Name: "test", Deny: []string{"Write", "Edit"}})
	require.NoError(t, err)
	h := newHarness(t, sb, Config{Tools: []tool.Tool{read, write, edit}, Rules: rules})
	assert.NotContains(t, joinPrompt(h.c.prompt), sentence)
	assert.Contains(t, joinPrompt(h.c.prompt), "Tools available: Read.\n")
}

func TestPromptAppendsServerInstructionsInNameOrder(t *testing.T) {
	long := strings.Repeat("a", maxServerInstructions-1) + "é" + "tail"
	parts := systemPrompt("/w", nil, nil, []mcpServer{
		{name: "zeta", instructions: "Use zeta last."},
		{name: "alpha", instructions: " Use alpha first.\n"},
		{name: "empty", instructions: "  "},
		{name: "long", instructions: long},
	}, false)

	require.Equal(t, []string{"preamble", "mcp-servers"}, partNames(parts))
	assert.Equal(t, systemPrompt("/w", nil, nil, nil, false), parts[:1])
	assert.Equal(t, "Instructions from MCP server alpha:\nUse alpha first.\n"+
		"Instructions from MCP server long:\n"+strings.Repeat("a", maxServerInstructions-1)+"\n"+
		"Instructions from MCP server zeta:\nUse zeta last.", parts[1].Text, "a cut never splits a character")
	assert.NotContains(t, joinPrompt(parts), "server empty")
}

func TestPromptListsDeferredServerToolsByServer(t *testing.T) {
	read := tool.Tool{Name: "Read", Capability: policy.Read}
	search := tool.Tool{Name: "ToolSearch", Capability: policy.Read}
	zt := tool.Tool{Name: "mcp__zeta__t", Capability: policy.Read, Deferred: true}
	a1 := tool.Tool{Name: "mcp__alpha__one", Capability: policy.Exec, Deferred: true}
	a2 := tool.Tool{Name: "mcp__alpha__two", Capability: policy.Read, Deferred: true}
	parts := systemPrompt("/w", []tool.Tool{read, search, zt, a1, a2}, nil, []mcpServer{
		{name: "zeta", tools: []string{zt.Name}},
		{name: "empty", instructions: "Nothing left."},
		{name: "alpha", instructions: "Use alpha.", tools: []string{a1.Name, a2.Name}},
	}, false)

	require.Equal(t, []string{"preamble", "mcp-servers"}, partNames(parts))
	assert.Contains(t, parts[0].Text, "\nTools available: Read, ToolSearch.\n")
	assert.Contains(t, parts[0].Text, "Read-only tools work in both modes, the others only in execute mode", "a deferred tool still needs execute mode")
	_, after, ok := strings.Cut(parts[0].Text, "\nTools of MCP servers are deferred")
	require.True(t, ok)
	assert.Equal(t, ": they are listed below by name and cannot be called until ToolSearch loads them. "+
		"Call ToolSearch with keywords for what you need, or with exact tool names; the tools it loads can be called in your next response, not in the one that calls ToolSearch.", after)
	assert.Equal(t, "MCP server alpha tools: mcp__alpha__one, mcp__alpha__two.\n"+
		"Instructions from MCP server alpha:\nUse alpha.\n"+
		"Instructions from MCP server empty:\nNothing left.\n"+
		"MCP server zeta tools: mcp__zeta__t.", parts[1].Text)

	plain := systemPrompt("/w", []tool.Tool{read}, nil, []mcpServer{{name: "empty"}}, false)
	assert.Equal(t, []string{"preamble"}, partNames(plain), "no server lines, no mcp-servers part")
	assert.NotContains(t, plain[0].Text, "deferred")
	assert.NotContains(t, plain[0].Text, "MCP server")
	assert.NotContains(t, plain[0].Text, "ToolSearch")
}

func TestPromptInstructionNamesAreScopedAndStable(t *testing.T) {
	global := instruction.Group{Dir: "/w", Global: true, Files: []instruction.File{{Path: "/w/AGENTS.md", Text: "Global."}}}
	project := instruction.Group{Dir: "/w", Files: []instruction.File{{Path: "/AGENTS.md", Text: "Project."}}}
	parts := systemPrompt("/w", nil, []instruction.Group{global, project}, nil, false)

	assert.Equal(t, []string{"preamble", "instructions global /w", "instructions project /w"}, partNames(parts))
	withoutGlobal := systemPrompt("/w", nil, []instruction.Group{project}, nil, false)
	assert.Equal(t, parts[2], withoutGlobal[1], "project identity does not depend on the global group's presence")
}

func TestPromptPlacesInstructionGroupsBetweenPreambleAndServers(t *testing.T) {
	global := instruction.Group{Dir: "/home/.kopi", Global: true, Files: []instruction.File{{Path: "/home/.kopi/AGENTS.md", Text: "Global rule."}}}
	project := instruction.Group{Dir: "/w", Files: []instruction.File{
		{Path: "/w/AGENTS.md", Text: "Outer rule."},
		{Path: "/w/sub/CLAUDE.md", Text: "Inner rule.\nSecond line."},
	}}
	parts := systemPrompt("/w", nil, []instruction.Group{global, project}, []mcpServer{{name: "s", instructions: "Use s."}}, false)

	assert.Equal(t, []string{"preamble", "instructions global /home/.kopi", "instructions project /w", "mcp-servers"}, partNames(parts))
	assert.Equal(t, systemPrompt("/w", nil, nil, nil, false), parts[:1])
	assert.Equal(t, "Instructions from /home/.kopi/AGENTS.md:\nGlobal rule.", parts[1].Text)
	assert.Equal(t, "Instructions from /w/AGENTS.md:\nOuter rule.\n\nInstructions from /w/sub/CLAUDE.md:\nInner rule.\nSecond line.", parts[2].Text)
	assert.Equal(t, "Instructions from MCP server s:\nUse s.", parts[3].Text)

	assert.Equal(t, []string{"preamble"}, partNames(systemPrompt("/w", nil, nil, nil, false)), "no groups, no instruction parts")
}

func TestPromptMentionsClassifierOnlyWhenOnAndApprovalCanBeNeeded(t *testing.T) {
	const sentence = "Tool calls that would need the user's approval are first reviewed by a classifier model, which can allow them, deny them with a reason, or leave them to the user."
	exec := []tool.Tool{{Name: "Probe", Capability: policy.Exec}}
	read := []tool.Tool{{Name: "Probe", Capability: policy.Read}}

	on := joinPrompt(systemPrompt("/w", exec, nil, nil, true))
	assert.Contains(t, on, "any tool call may need the user's approval.\n"+sentence+"\n")
	assert.Equal(t, strings.Replace(on, sentence+"\n", "", 1), joinPrompt(systemPrompt("/w", exec, nil, nil, false)))
	assert.NotContains(t, joinPrompt(systemPrompt("/w", read, nil, nil, true)), sentence)
}
