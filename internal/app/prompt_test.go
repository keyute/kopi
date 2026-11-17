package app

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

func TestPromptMentionsExecuteModeOnlyWhenNonReadToolAvailable(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	ws, err := tool.NewWorkspace(sb.Workspace, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ws.Close()) })
	read, write, edit := tool.NewRead(ws), tool.NewWrite(ws), tool.NewEdit(ws)
	const sentence = "Read-only tools work in both modes, the others only in execute mode, and any tool call may need the user's approval."

	assert.Contains(t, systemPrompt(sb.Workspace, []tool.Tool{read, edit}, nil), sentence)
	assert.Contains(t, systemPrompt(sb.Workspace, []tool.Tool{read, {Name: "Probe", Capability: policy.Exec}}, nil), sentence)
	assert.Contains(t, systemPrompt(sb.Workspace, []tool.Tool{read, {Name: "Probe", Capability: policy.Exec, PlanSandboxed: true}}, nil),
		"Read-only tools work in both modes, the others only in execute mode, and any tool call may need the user's approval; in plan mode Probe runs too, but cannot write the workspace.")
	rules, err := policy.NewRules(sb.Workspace, sb.Home, map[string]policy.ToolSpec{"Read": {}, "Write": {}, "Edit": {}}, policy.Source{Name: "test", Deny: []string{"Write", "Edit"}})
	require.NoError(t, err)
	h := newHarness(t, sb, Config{Tools: []tool.Tool{read, write, edit}, Rules: rules})
	assert.NotContains(t, h.c.prompt, sentence)
	assert.Contains(t, h.c.prompt, "Tools available: Read.\n")
}

func TestPromptAppendsServerInstructionsInNameOrder(t *testing.T) {
	long := strings.Repeat("a", maxServerInstructions-1) + "é" + "tail"
	prompt := systemPrompt("/w", nil, []mcpServer{
		{name: "zeta", instructions: "Use zeta last."},
		{name: "alpha", instructions: " Use alpha first.\n"},
		{name: "empty", instructions: "  "},
		{name: "long", instructions: long},
	})

	before, after, ok := strings.Cut(prompt, "\nInstructions from MCP server ")
	require.True(t, ok)
	assert.Equal(t, systemPrompt("/w", nil, nil), before)
	assert.Equal(t, "alpha:\nUse alpha first.\n"+
		"Instructions from MCP server long:\n"+strings.Repeat("a", maxServerInstructions-1)+"\n"+
		"Instructions from MCP server zeta:\nUse zeta last.", after, "a cut never splits a character")
	assert.NotContains(t, prompt, "server empty")
}

func TestPromptListsDeferredServerToolsByServer(t *testing.T) {
	read := tool.Tool{Name: "Read", Capability: policy.Read}
	search := tool.Tool{Name: "ToolSearch", Capability: policy.Read}
	zt := tool.Tool{Name: "mcp__zeta__t", Capability: policy.Read, Deferred: true}
	a1 := tool.Tool{Name: "mcp__alpha__one", Capability: policy.Exec, Deferred: true}
	a2 := tool.Tool{Name: "mcp__alpha__two", Capability: policy.Read, Deferred: true}
	prompt := systemPrompt("/w", []tool.Tool{read, search, zt, a1, a2}, []mcpServer{
		{name: "zeta", tools: []string{zt.Name}},
		{name: "empty", instructions: "Nothing left."},
		{name: "alpha", instructions: "Use alpha.", tools: []string{a1.Name, a2.Name}},
	})

	assert.Contains(t, prompt, "\nTools available: Read, ToolSearch.\n")
	assert.Contains(t, prompt, "Read-only tools work in both modes, the others only in execute mode", "a deferred tool still needs execute mode")
	_, after, ok := strings.Cut(prompt, "\nTools of MCP servers are deferred")
	require.True(t, ok)
	assert.Equal(t, ": they are listed below by name and cannot be called until ToolSearch loads them. "+
		"Call ToolSearch with keywords for what you need, or with exact tool names; the tools it loads can be called in your next response, not in the one that calls ToolSearch.\n"+
		"MCP server alpha tools: mcp__alpha__one, mcp__alpha__two.\n"+
		"Instructions from MCP server alpha:\nUse alpha.\n"+
		"Instructions from MCP server empty:\nNothing left.\n"+
		"MCP server zeta tools: mcp__zeta__t.", after)

	plain := systemPrompt("/w", []tool.Tool{read}, []mcpServer{{name: "empty"}})
	assert.NotContains(t, plain, "deferred")
	assert.NotContains(t, plain, "MCP server")
	assert.NotContains(t, plain, "ToolSearch")
}
