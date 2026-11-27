package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/instruction"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/subagent"
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

	assert.Contains(t, joinPrompt(buildPrompt(sb.Workspace, []tool.Tool{read, edit}, nil, nil, nil, false, false, false)), sentence)
	assert.Contains(t, joinPrompt(buildPrompt(sb.Workspace, []tool.Tool{read, {Name: "Probe", Capability: policy.Exec}}, nil, nil, nil, false, false, false)), sentence)
	assert.Contains(t, joinPrompt(buildPrompt(sb.Workspace, []tool.Tool{read, {Name: "Probe", Capability: policy.Exec, PlanSandboxed: true}}, nil, nil, nil, false, false, false)),
		"Read-only tools work in both modes, the others only in execute mode, and any tool call may need the user's approval; in plan mode Probe runs too, but cannot write the workspace.")
	assert.Contains(t, joinPrompt(buildPrompt(sb.Workspace, []tool.Tool{read}, nil, nil, nil, false, true, false)), sentence, "servers may add tools that are not read-only")
	rules, err := policy.NewRules(sb.Workspace, sb.Home, map[string]policy.ToolSpec{"Read": {}, "Write": {}, "Edit": {}}, policy.Source{Name: "test", Deny: []string{"Write", "Edit"}})
	require.NoError(t, err)
	h := newHarness(t, sb, Config{Tools: []tool.Tool{read, write, edit}, Rules: rules})
	assert.NotContains(t, joinPrompt(h.c.prompt), sentence)
}

func TestPromptAppendsServerInstructionsInNameOrder(t *testing.T) {
	long := strings.Repeat("a", maxServerInstructions-1) + "é" + "tail"
	parts := buildPrompt("/w", nil, nil, []mcpServer{
		{name: "zeta", instructions: "Use zeta last."},
		{name: "alpha", instructions: " Use alpha first.\n"},
		{name: "empty", instructions: "  "},
		{name: "long", instructions: long},
	}, nil, false, false, false)

	require.Equal(t, []string{"preamble", "mcp-servers"}, partNames(parts))
	assert.Equal(t, buildPrompt("/w", nil, nil, nil, nil, false, false, false), parts[:1])
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
	parts := buildPrompt("/w", []tool.Tool{read, search, zt, a1, a2}, nil, []mcpServer{
		{name: "zeta", tools: []string{zt.Name}},
		{name: "empty", instructions: "Nothing left."},
		{name: "alpha", instructions: "Use alpha.", tools: []string{a1.Name, a2.Name}},
	}, nil, false, false, false)

	require.Equal(t, []string{"preamble", "mcp-servers"}, partNames(parts))
	assert.NotContains(t, parts[0].Text, " available: ")
	assert.Contains(t, parts[0].Text, "Read-only tools work in both modes, the others only in execute mode", "a deferred tool still needs execute mode")
	assert.NotContains(t, parts[0].Text, "deferred")
	assert.NotContains(t, parts[0].Text, "ToolSearch")
	assert.Equal(t, "MCP server alpha tools, loaded by ToolSearch: mcp__alpha__one, mcp__alpha__two.\n"+
		"Instructions from MCP server alpha:\nUse alpha.\n"+
		"Instructions from MCP server empty:\nNothing left.\n"+
		"MCP server zeta tools, loaded by ToolSearch: mcp__zeta__t.", parts[1].Text)

	plain := buildPrompt("/w", []tool.Tool{read}, nil, []mcpServer{{name: "empty"}}, nil, false, false, false)
	assert.Equal(t, []string{"preamble"}, partNames(plain), "no server lines, no mcp-servers part")
	assert.NotContains(t, plain[0].Text, "deferred")
	assert.NotContains(t, plain[0].Text, "MCP server")
	assert.NotContains(t, plain[0].Text, "ToolSearch")
}

func TestPromptInstructionNamesAreScopedAndStable(t *testing.T) {
	global := instruction.Group{Dir: "/w", Global: true, Files: []instruction.File{{Path: "/w/AGENTS.md", Text: "Global."}}}
	project := instruction.Group{Dir: "/w", Files: []instruction.File{{Path: "/AGENTS.md", Text: "Project."}}}
	parts := buildPrompt("/w", nil, []instruction.Group{global, project}, nil, nil, false, false, false)

	assert.Equal(t, []string{"preamble", "instructions global /w", "instructions project /w"}, partNames(parts))
	withoutGlobal := buildPrompt("/w", nil, []instruction.Group{project}, nil, nil, false, false, false)
	assert.Equal(t, parts[2], withoutGlobal[1], "project identity does not depend on the global group's presence")
}

func TestPromptPlacesInstructionGroupsBetweenPreambleAndServers(t *testing.T) {
	global := instruction.Group{Dir: "/home/.kopi", Global: true, Files: []instruction.File{{Path: "/home/.kopi/AGENTS.md", Text: "Global rule."}}}
	project := instruction.Group{Dir: "/w", Files: []instruction.File{
		{Path: "/w/AGENTS.md", Text: "Outer rule."},
		{Path: "/w/sub/CLAUDE.md", Text: "Inner rule.\nSecond line."},
	}}
	parts := buildPrompt("/w", nil, []instruction.Group{global, project}, []mcpServer{{name: "s", instructions: "Use s."}}, nil, false, false, false)

	assert.Equal(t, []string{"preamble", "instructions global /home/.kopi", "instructions project /w", "mcp-servers"}, partNames(parts))
	assert.Equal(t, buildPrompt("/w", nil, nil, nil, nil, false, false, false), parts[:1])
	assert.Equal(t, "Instructions from /home/.kopi/AGENTS.md:\nGlobal rule.", parts[1].Text)
	assert.Equal(t, "Instructions from /w/AGENTS.md:\nOuter rule.\n\nInstructions from /w/sub/CLAUDE.md:\nInner rule.\nSecond line.", parts[2].Text)
	assert.Equal(t, "Instructions from MCP server s:\nUse s.", parts[3].Text)

	assert.Equal(t, []string{"preamble"}, partNames(buildPrompt("/w", nil, nil, nil, nil, false, false, false)), "no groups, no instruction parts")
}

func TestPreambleHasNoToolListAndNoRefusalOrClassifierSentence(t *testing.T) {
	text := buildPrompt("/w", []tool.Tool{{Name: "Read", Capability: policy.Read}, {Name: "Probe", Capability: policy.Exec, PlanSandboxed: true}}, nil, nil, nil, false, false, false)[0].Text
	assert.Equal(t, "You are Kopi, a coding agent working in /w.\n"+
		"The session has a plan mode and an execute mode; only the user can switch between them.\n"+
		"Read-only tools work in both modes, the others only in execute mode, and any tool call may need the user's approval; in plan mode Probe runs too, but cannot write the workspace.", text)
}

func TestSubagentPromptStatesItsRoleThenBodyInstructionsAndItsServers(t *testing.T) {
	global := instruction.Group{Dir: "/home/.kopi", Global: true, Files: []instruction.File{{Path: "/home/.kopi/AGENTS.md", Text: "Global rule."}}}
	read := tool.Tool{Name: "Read", Capability: policy.Read}
	def := &subagent.Definition{Name: "explorer", Prompt: "Explore only."}

	parts := buildPrompt("/w", []tool.Tool{read}, []instruction.Group{global}, []mcpServer{{name: "s", tools: []string{"mcp__s__a"}}}, def, false, false, false)

	assert.Equal(t, []string{"preamble", "agent explorer", "instructions global /home/.kopi", "mcp-servers"}, partNames(parts))
	assert.Equal(t, "You are Kopi, a coding agent working in /w.\n"+
		"You are the subagent explorer: another agent gave you your task and may message you as you work. Only your final message is returned to that agent, and it is your whole result.\n"+
		"The session has a plan mode and an execute mode; only the user can switch between them.", parts[0].Text)
	assert.Equal(t, "Explore only.", parts[1].Text)
	assert.NotContains(t, partNames(parts), "tools")
	assert.NotContains(t, partNames(parts), "rules")
	assert.Equal(t, buildPrompt("/w", []tool.Tool{read}, nil, nil, nil, false, false, false)[0].Text, strings.Replace(parts[0].Text, "You are the subagent explorer: another agent gave you your task and may message you as you work. Only your final message is returned to that agent, and it is your whole result.\n", "", 1))
}

func TestSubagentPromptOmitsEmptyBodyAndInstructionsWhenOff(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	global := instruction.Group{Dir: sb.KopiHome, Global: true, Files: []instruction.File{{Path: sb.KopiHome + "/AGENTS.md", Text: "Global rule."}}}
	bodiless, bare := agentDef("bodiless"), agentDef("bare")
	bare.Prompt, bare.Instructions = "Body.", false
	h := newHarness(t, sb, Config{Instructions: []instruction.Group{global}, Agents: []subagent.Definition{bodiless, bare}})

	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))

	assert.Equal(t, []string{"preamble", "instructions global " + sb.KopiHome}, partNames(h.c.agents[0].prompt))
	assert.Equal(t, []string{"preamble", "agent bare"}, partNames(h.c.agents[1].prompt))
	assert.Equal(t, buildPrompt(sb.Workspace, h.c.tools[:len(h.c.tools)-1], []instruction.Group{global}, nil, nil, false, false, false), h.c.prompt[:2], "the root's prompt has neither agent text")
	assert.Equal(t, []string{"preamble", "instructions global " + sb.KopiHome}, partNames(h.c.prompt))
}

func TestPlanPinnedSubagentPreambleSaysItStaysInPlanMode(t *testing.T) {
	read := tool.Tool{Name: "Read", Capability: policy.Read}
	write := tool.Tool{Name: "Write", Capability: policy.Write}
	pinned := &subagent.Definition{Name: "planner", Plan: true}
	free := &subagent.Definition{Name: "worker"}

	text := buildPrompt("/w", []tool.Tool{read, write}, nil, nil, pinned, false, false, false)[0].Text
	assert.Contains(t, text, "\nYou stay in plan mode, whatever mode the user is in.\n")
	assert.Contains(t, text, "\nRead-only tools work, the others do not, and any tool call may need the user's approval.")
	assert.NotContains(t, text, "only the user can switch")
	assert.NotContains(t, text, "execute mode")

	sandboxed := tool.Tool{Name: "Probe", Capability: policy.Exec, PlanSandboxed: true}
	assert.Contains(t, buildPrompt("/w", []tool.Tool{read, sandboxed}, nil, nil, pinned, false, false, false)[0].Text,
		"\nRead-only tools work, and Probe runs but cannot write the workspace; the others do not, and any tool call may need the user's approval.")

	unpinned := buildPrompt("/w", []tool.Tool{read, write}, nil, nil, free, false, false, false)[0].Text
	assert.Contains(t, unpinned, "The session has a plan mode and an execute mode; only the user can switch between them.")
	assert.NotContains(t, unpinned, "stay in plan mode")
}

func TestWorkflowAgentPreambleReplacesTheSubagentSentence(t *testing.T) {
	read := tool.Tool{Name: "Read", Capability: policy.Read}
	write := tool.Tool{Name: "Write", Capability: policy.Write}
	sentence := "You are a workflow agent: a script gave you your task, and your final message is returned to that script as the result.\n"

	byDefault := buildPrompt("/w", []tool.Tool{read}, nil, nil, nil, true, false, false)
	assert.Contains(t, byDefault[0].Text, sentence)
	assert.NotContains(t, byDefault[0].Text, "subagent")

	typed := buildPrompt("/w", []tool.Tool{read, write}, nil, nil, &subagent.Definition{Name: "planner", Prompt: "Body.", Plan: true}, true, false, false)
	assert.Equal(t, []string{"preamble", "agent planner"}, partNames(typed))
	assert.Contains(t, typed[0].Text, sentence)
	assert.NotContains(t, typed[0].Text, "subagent")
	assert.Contains(t, typed[0].Text, "\nYou stay in plan mode, whatever mode the user is in.\n")
}

func TestOnlyTheMainSessionWithAUserShellIsToldToHandOverCommands(t *testing.T) {
	const line = "If a command will not work when you run it, give the user `! command` in a code block; its output is sent to you."
	sb := pigruntimetest.NewSandbox(t)

	with := newHarness(t, sb, Config{UserShell: tool.NewShell(sb.Workspace, nil), Agents: []subagent.Definition{agentDef("worker")}})
	require.NoError(t, with.c.Open(t.Context(), OpenRequest{}))
	assert.True(t, strings.HasSuffix(with.c.prompt[0].Text, "\n"+line))
	for _, spec := range slices.Concat(with.c.agents, with.c.workflowSpecs) {
		assert.NotContains(t, spec.prompt[0].Text, line)
	}

	without := newHarness(t, sb, Config{Agents: []subagent.Definition{agentDef("worker")}})
	require.NoError(t, without.c.Open(t.Context(), OpenRequest{}))
	assert.NotContains(t, without.c.prompt[0].Text, line)
}

func TestContextOfSortsPartsAndToolsIntoCategories(t *testing.T) {
	parts := []pigruntime.Size{
		{Name: "preamble", Tokens: 100},
		{Name: "instructions global /home", Tokens: 30},
		{Name: "instructions project /repo", Tokens: 20},
		{Name: "mcp-servers", Tokens: 5},
	}
	tools := []pigruntime.Size{
		{Name: "Read", Tokens: 40},
		{Name: "Bash", Tokens: 60},
		{Name: "Skill", Tokens: 7},
		{Name: "Agent", Tokens: 9},
		{Name: tool.MCPName("github", "search"), Tokens: 11},
	}

	got := contextOf(1000, parts, tools)

	assert.Equal(t, Context{SystemPrompt: 100, Tools: 100, Instructions: 50, Skills: 7, Agents: 9, MCP: 16, Messages: 718}, got)
}

func TestContextOfScalingPreservesTheTotal(t *testing.T) {
	got := contextOf(3, []pigruntime.Size{{Name: "preamble", Tokens: 2}}, []pigruntime.Size{{Name: "Read", Tokens: 2}})

	assert.Equal(t, Context{SystemPrompt: 1, Tools: 1, Messages: 1}, got)
	assert.Equal(t, 3, got.SystemPrompt+got.Tools+got.Instructions+got.Skills+got.Agents+got.MCP+got.Messages)
}

func TestContextOfScalesEstimatesAboveTheTotal(t *testing.T) {
	got := contextOf(100, []pigruntime.Size{{Name: "preamble", Tokens: 150}}, []pigruntime.Size{{Name: "Read", Tokens: 50}})

	assert.Equal(t, Context{SystemPrompt: 75, Tools: 25}, got)
}
