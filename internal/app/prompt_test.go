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
	prompt := systemPrompt("/w", nil, map[string]string{"zeta": "Use zeta last.", "alpha": " Use alpha first.\n", "empty": "  ", "long": long})

	before, after, ok := strings.Cut(prompt, "\nInstructions from MCP server ")
	require.True(t, ok)
	assert.Equal(t, systemPrompt("/w", nil, nil), before)
	assert.Equal(t, "alpha:\nUse alpha first.\n"+
		"Instructions from MCP server long:\n"+strings.Repeat("a", maxServerInstructions-1)+"\n"+
		"Instructions from MCP server zeta:\nUse zeta last.", after, "a cut never splits a character")
	assert.NotContains(t, prompt, "server empty")
}
