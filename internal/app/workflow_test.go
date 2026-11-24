package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/subagent"
	"github.com/keyute/kopi/internal/tool"
)

// newWorkflowHarness is an agent harness whose workflow agents run on the
// child provider, so the root's scripted responses stay the root's.
func newWorkflowHarness(t *testing.T, sb pigruntimetest.Sandbox, cfg Config, defs ...subagent.Definition) *agentHarness {
	t.Helper()
	cfg.WorkflowModel = childProvider + "/faux-1"
	return newAgentHarness(t, sb, cfg, defs...)
}

func (a *agentHarness) startWorkflow(t *testing.T, description, script string) string {
	t.Helper()
	id, err := a.c.startWorkflow(description, script)
	require.NoError(t, err)
	return id
}

// echoChild answers with the text of the last prompt it received.
func echoChild() ai.FauxResponseStep {
	return ai.FauxFactoryStep(func(tc ai.TranscriptContext, _ ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
		var text string
		for _, m := range tc.Messages() {
			var u ai.UserMessage
			switch m := m.(type) {
			case ai.UserMessage:
				u = m
			case *ai.UserMessage:
				u = *m
			default:
				continue
			}
			switch c := u.Content.(type) {
			case ai.UserText:
				text = string(c)
			case ai.UserContentBlocks:
				var parts []string
				for _, b := range c {
					if tb, ok := b.(ai.TextContent); ok {
						parts = append(parts, tb.Text)
					}
				}
				text = strings.Join(parts, "\n")
			}
		}
		return ai.FauxAssistantMessage(ai.FauxContentString(text), ai.FauxAssistantMessageOptions{}), nil
	})
}

// blockingHarness opens a harness whose agents have a tool that blocks until its
// call is cancelled; started receives each call.
func blockingHarness(t *testing.T, cfg Config, defs ...subagent.Definition) (*agentHarness, <-chan struct{}) {
	t.Helper()
	started := make(chan struct{}, 4)
	cfg.AgentTools = []tool.Tool{blockingTool(started)}
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), cfg, defs...)
	return a, started
}

func TestWorkflowRunsTheDefaultAgentAndReturnsItsReply(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	a := newWorkflowHarness(t, sb, Config{AgentTools: []tool.Tool{readTool(t, sb)}})
	requests := make(chan childRequest, 1)
	a.child.SetResponses([]ai.FauxResponseStep{captureChild(requests, nil, "found it")})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `return await agent("look around")`)
	req := receiveChild(t, requests)
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.Equal(t, []string{"Read"}, req.tools)
	assert.Contains(t, req.system, "You are a workflow agent: a script gave you your task, and your final message is returned to that script as the result.")
	assert.NotContains(t, req.system, "subagent")
	assert.Contains(t, req.raw, "look around")
	assert.Equal(t, `"found it"`, a.info(t, id).Result)
	assert.Contains(t, a.info(t, id).Output, "#1 agent: started\n#1 agent: completed · ")
}

func TestWorkflowAgentWebSearchIsCallableOnlyForASearchingSmallModel(t *testing.T) {
	for _, tc := range []struct {
		name, small string
		want        []string
	}{
		{"searching", "kopi-search/search-1", []string{"WebSearch"}},
		{"not searching", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newWorkflowHarness(t, searchSandbox(t), Config{SmallModel: tc.small})
			requests := make(chan childRequest, 1)
			a.child.SetResponses([]ai.FauxResponseStep{captureChild(requests, nil, "found it")})
			a.rootAcks(1)

			id := a.startWorkflow(t, "audit", `return await agent("look around")`)
			got := receiveChild(t, requests).tools
			a.untilStatus(t, id, tool.TaskCompleted)

			assert.Equal(t, tc.want, got)
		})
	}
}

func TestWorkflowNamedTypeGetsItsPromptToolsEffortAndModel(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	def := agentDef("explorer")
	def.Effort, def.Prompt, def.Tools = "high", "AGENT_BODY", []string{"Read"}
	a := newAgentHarness(t, sb, Config{AgentTools: []tool.Tool{readTool(t, sb), execTool(new(atomic.Int32))}}, def)
	requests := make(chan childRequest, 1)
	a.child.SetResponses([]ai.FauxResponseStep{captureChild(requests, nil, "done")})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `return await agent("go", {agentType: "explorer", label: "auth"})`)
	req := receiveChild(t, requests)
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.Equal(t, []string{"Read"}, req.tools)
	assert.Equal(t, ai.ThinkingLevelHigh, req.thinking)
	assert.Contains(t, req.system, "AGENT_BODY")
	assert.Contains(t, req.system, "You are a workflow agent")
	assert.Zero(t, a.child.CallCount()-1, "the definition's model served the one agent")
	assert.Contains(t, a.info(t, id).Output, "#1 explorer › auth: started")
}

func TestWorkflowUnknownTypeRejects(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{}, agentDef("explorer"))
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `try { await agent("go", {agentType: "nope"}) } catch (e) { return e.message }`)
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.Contains(t, a.info(t, id).Result, "unknown agent type: nope")
	assert.Zero(t, a.child.CallCount())
}

func TestWorkflowModelOrder(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	def := agentDef("explorer")
	def.Model = "kopi-def/faux-1"
	register := func(a *agentHarness) *ai.FauxProviderHandle {
		p := ai.NewFauxProvider(ai.FauxConfig{ProviderID: "kopi-def", Models: []ai.FauxModelDefinition{{ID: "faux-1"}}})
		require.NoError(t, a.c.rt.RegisterProvider(p.Provider()))
		return p
	}

	t.Run("definition", func(t *testing.T) {
		a := newWorkflowHarness(t, sb, Config{}, def)
		defProvider := register(a)
		defProvider.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("def")})
		a.rootAcks(1)
		id := a.startWorkflow(t, "audit", `return await agent("go", {agentType: "explorer"})`)
		a.untilStatus(t, id, tool.TaskCompleted)
		assert.Equal(t, `"def"`, a.info(t, id).Result)
		assert.Zero(t, a.child.CallCount())
	})
	t.Run("workflows.model", func(t *testing.T) {
		a := newWorkflowHarness(t, sb, Config{}, def)
		register(a)
		a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("key")})
		a.rootAcks(1)
		id := a.startWorkflow(t, "audit", `return await agent("go")`)
		a.untilStatus(t, id, tool.TaskCompleted)
		assert.Equal(t, `"key"`, a.info(t, id).Result)
	})
	t.Run("session", func(t *testing.T) {
		a := newAgentHarness(t, sb, Config{}, def)
		requests := make(chan pigruntimetest.Request, 2)
		a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "session"), pigruntimetest.Text("ack")})
		id := a.startWorkflow(t, "audit", `return await agent("go")`)
		a.untilStatus(t, id, tool.TaskCompleted)
		assert.Equal(t, `"session"`, a.info(t, id).Result)
		assert.Zero(t, a.child.CallCount())
	})
}

func TestWorkflowAgentStartedAfterModelCommandRunsOnTheNewModel(t *testing.T) {
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{})
	other, otherRef := pigruntimetest.RegisterFaux(t, a.c.rt, "kopi-other")
	started, gate := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	a.faux.SetResponses([]ai.FauxResponseStep{ai.FauxFactoryStep(func(ai.TranscriptContext, ai.StreamOptions, *ai.FauxProviderState, *ai.Model) (ai.AssistantMessage, error) {
		close(started)
		<-gate
		return ai.FauxAssistantMessage(ai.FauxContentString("first"), ai.FauxAssistantMessageOptions{}), nil
	})})
	requests := make(chan pigruntimetest.Request, 1)
	other.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "second"), pigruntimetest.Text("ack")})
	changed := subscribeTo[event.ModelChanged](a.c)

	id := a.startWorkflow(t, "audit", `await agent("first"); return await agent("second")`)
	wait(t, started)
	_, err := a.c.Submit(t.Context(), Main, "/model "+otherRef)
	require.NoError(t, err)
	receiveEvent(t, changed)
	close(gate)

	assert.Equal(t, []string{"second"}, receive(t, requests).UserTexts)
	a.untilStatus(t, id, tool.TaskCompleted)
	assert.Equal(t, 1, a.faux.CallCount(), "the first agent ran on the old model")
}

func TestWorkflowAgentsAreInTheTotalAsEachEnds(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	def := agentDef("explorer")
	a := newAgentHarness(t, sb, Config{WorkflowModel: sb.PricedModel(t)}, def)
	gate := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	a.child.SetResponses([]ai.FauxResponseStep{gated(gate, "second")})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `await agent("first"); return await agent("second", {agentType: "explorer"})`)

	a.until(t, func() bool { return a.c.Status().TotalCost == pigruntimetest.PricedModelCost })
	assert.Equal(t, tool.TaskRunning, a.info(t, id).Status)
	close(gate)
	a.untilStatus(t, id, tool.TaskCompleted)
	assert.Equal(t, pigruntimetest.PricedModelCost, a.c.Status().TotalCost)
}

func TestWorkflowTotalSumsItsAgents(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	a := newAgentHarness(t, sb, Config{WorkflowModel: sb.PricedModel(t)})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `return await parallel([() => agent("a"), () => agent("b")])`)
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.Equal(t, 2*pigruntimetest.PricedModelCost, a.c.Status().TotalCost)
}

func TestWorkflowAgentModelRowSurvivesItsEnd(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	priced := sb.PricedModel(t)
	a := newAgentHarness(t, sb, Config{WorkflowModel: priced})
	a.rootAcks(1)
	ends := turnEnds(a.c)

	id := a.startWorkflow(t, "audit", `return await agent("a")`)
	a.untilStatus(t, id, tool.TaskCompleted)
	receiveEnd(t, ends)

	got := a.c.Usage()
	assert.Contains(t, got.Models, ModelUsage{Model: priced, Tokens: 1_000_000, Cost: pigruntimetest.PricedModelCost}, "the agent's row survives its end")
	assert.Equal(t, a.c.Status().TotalCost, pigruntime.Usage{Models: got.Models}.Cost())
}

func TestWorkflowSchemaReturnsTheRecordedObject(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	requests := make(chan childRequest, 1)
	a.child.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "StructuredOutput", ID: "s1", Args: map[string]any{"claims": []any{"a", "b"}}}),
		captureChild(requests, nil, "done"),
	})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `
const S = {type: "object", properties: {claims: {type: "array", items: {type: "string"}}}, required: ["claims"]}
return await agent("go", {schema: S})`)
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.JSONEq(t, `{"claims":["a","b"]}`, a.info(t, id).Result)
	assert.Contains(t, receiveChild(t, requests).tools, "StructuredOutput")
}

func TestWorkflowSchemaReturnsTheLastCallInOneResponse(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	a.child.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(
			pigruntimetest.Call{Name: "StructuredOutput", ID: "s1", Args: map[string]any{"claims": []any{"first"}}},
			pigruntimetest.Call{Name: "StructuredOutput", ID: "s2", Args: map[string]any{"claims": []any{"last"}}},
		),
		pigruntimetest.Text("done"),
	})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `
const S = {type: "object", properties: {claims: {type: "array", items: {type: "string"}}}, required: ["claims"]}
return await agent("go", {schema: S})`)
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.JSONEq(t, `{"claims":["last"]}`, a.info(t, id).Result)
}

func TestWorkflowSchemaWithoutACallRejects(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("no call")})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `try { return await agent("go", {schema: {type: "object"}}) } catch (e) { return e.message }`)
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.Contains(t, a.info(t, id).Result, "StructuredOutput")
	assert.Contains(t, a.info(t, id).Output, "failed: "+ErrNoStructuredOutput.Error())
}

func TestWorkflowNonObjectSchemaRejectsBeforeASessionOpens(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `
const r = []
for (const schema of [{type: "string"}, "object", {}]) {
  try { await agent("go", {schema}) } catch (e) { r.push(e.message) }
}
return r`)
	a.untilStatus(t, id, tool.TaskCompleted)

	want, err := json.Marshal([]string{ErrBadSchema.Error(), ErrBadSchema.Error(), ErrBadSchema.Error()})
	require.NoError(t, err)
	assert.JSONEq(t, string(want), a.info(t, id).Result)
	assert.Zero(t, a.child.CallCount())
	assert.NotContains(t, a.info(t, id).Output, "started")
}

func TestWorkflowDeniedStructuredOutputRejectsASchemaAgentBeforeASessionOpens(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	rules, err := policy.NewRules(sb.Workspace, sb.Home, map[string]policy.ToolSpec{"StructuredOutput": {}}, policy.Source{Name: "test", Deny: []string{"StructuredOutput"}})
	require.NoError(t, err)
	a := newWorkflowHarness(t, sb, Config{Rules: rules})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("plain")})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `
let m
try { await agent("go", {schema: {type: "object"}}) } catch (e) { m = e.message }
return [m, await agent("again")]`)
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.JSONEq(t, `["`+ErrStructuredOutputDenied.Error()+`", "plain"]`, a.info(t, id).Result)
	assert.Equal(t, 1, a.child.CallCount(), "only the agent without a schema opened a session")
}

func TestWorkflowParallelKeepsOrderWithNullForAFailure(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	a.child.SetResponses([]ai.FauxResponseStep{echoChild(), echoChild()})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `return await parallel([() => agent("a"), () => agent("b", {agentType: "nope"}), () => agent("c")])`)
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.JSONEq(t, `["a", null, "c"]`, a.info(t, id).Result)
}

func TestWorkflowPipelinePassesPreviousItemAndIndex(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	a.child.SetResponses([]ai.FauxResponseStep{echoChild(), echoChild(), echoChild(), echoChild()})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `return await pipeline(["x", "y"],
  (v, item, i) => agent("s1:" + v + ":" + item + ":" + i),
  (v, item, i) => agent("s2:" + v + ":" + item + ":" + i))`)
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.JSONEq(t, `["s2:s1:x:x:0:x:0", "s2:s1:y:y:1:y:1"]`, a.info(t, id).Result)
}

func TestWorkflowApprovalCarriesTheAgentNameAndRunIDAndABlankNoStopsOnlyThatAgent(t *testing.T) {
	ran := new(atomic.Int32)
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{AgentTools: []tool.Tool{probeTool(ran)}})
	approvals := subscribeTo[event.ApprovalRequested](a.c)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("p1", 1)), pigruntimetest.Text("second ok")})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `
let first
try { await agent("go", {label: "auth"}) } catch (e) { first = e.message }
return [first, await agent("again")]`)
	req := receiveEvent(t, approvals)

	assert.Equal(t, "workflow #1 auth", req.Agent)
	assert.Equal(t, id, req.TaskID)
	assert.True(t, a.c.Answer(req.Token, policy.Decision{Answer: policy.Reject}))
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.JSONEq(t, `["`+ErrAgentStopped.Error()+`", "second ok"]`, a.info(t, id).Result)
	assert.Zero(t, ran.Load())
	assert.Contains(t, a.info(t, id).Output, "#1 agent › auth: stopped")
	assert.Contains(t, a.info(t, id).Output, "#2 agent: completed")
}

func TestWorkflowStopEndsTheRunAndItsAgentsAndLeavesNothingRunning(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stop   func(*testing.T, *agentHarness, string)
		notice string
	}{
		{"TaskStop", func(t *testing.T, a *agentHarness, id string) { _, _, err := a.tasks.Stop(id); require.NoError(t, err) }, ""},
		{"StopTask", func(t *testing.T, a *agentHarness, id string) { require.NoError(t, a.c.StopTask(id)) }, "Workflow %s stopped by the user: audit"},
		{"switch", func(t *testing.T, a *agentHarness, _ string) { require.NoError(t, a.c.NewSession(t.Context())) }, ""},
		{"Close", func(t *testing.T, a *agentHarness, _ string) { require.NoError(t, a.c.Close(context.Background())) }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, started := blockingHarness(t, Config{Wake: true})
			ignore := goleak.IgnoreCurrent()
			a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "c1"))})
			id := a.startWorkflow(t, "audit", `return await agent("go")`)
			wait(t, started)
			approvalsBefore := len(ofType[event.ApprovalRequested](a.rec.all()))

			tc.stop(t, a, id)

			assert.Equal(t, approvalsBefore, len(ofType[event.ApprovalRequested](a.rec.all())))
			assert.Zero(t, a.c.Status().Workflows)
			if tc.name == "TaskStop" || tc.name == "StopTask" {
				assert.Equal(t, tool.TaskStopped, a.info(t, id).Status)
			} else {
				assert.Empty(t, a.c.Tasks(), "the switch forgot the task")
			}
			notices := a.queuedNotices()
			if tc.notice == "" {
				assert.Empty(t, notices)
			} else {
				require.Len(t, notices, 1)
				assert.True(t, strings.HasPrefix(notices[0].Text, fmt.Sprintf(tc.notice, id)), notices[0].Text)
			}
			assert.True(t, a.idle(), "a stop does not wake the model")
			assert.Zero(t, a.faux.CallCount())

			require.NoError(t, a.c.Close(context.Background()))
			goleak.VerifyNone(t, ignore)
		})
	}
}

func TestWorkflowStopMakesItsPendingApprovalStale(t *testing.T) {
	ran := new(atomic.Int32)
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{AgentTools: []tool.Tool{probeTool(ran)}})
	approvals, ended := subscribeTo[event.ApprovalRequested](a.c), subscribeTo[event.ApprovalEnded](a.c)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("p1", 1)), pigruntimetest.Text("unreached")})
	id := a.startWorkflow(t, "audit", `return await agent("go")`)
	req := receiveEvent(t, approvals)

	_, _, err := a.tasks.Stop(id)
	require.NoError(t, err)

	assert.Equal(t, req.Token, receiveEvent(t, ended).Token)
	assert.False(t, a.c.Answer(req.Token, policy.Decision{Answer: policy.Allow}))
	assert.Zero(t, ran.Load())
}

func TestWorkflowAgentsWaitForASlotOfTheirOwnRun(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{MaxAgents: 2, MaxWorkflows: 2})
	requests, gate := make(chan childRequest, 4), make(chan struct{})
	a.child.SetResponses([]ai.FauxResponseStep{
		captureChild(requests, gate, "r"), captureChild(requests, gate, "r"), captureChild(requests, gate, "r"), captureChild(requests, gate, "r"),
	})
	a.rootAcks(2)
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})

	prompt := func(req childRequest) string {
		for _, p := range []string{"a1", "a2", "a3", "b1"} {
			if strings.Contains(req.raw, fmt.Sprintf(`"text":%q`, p)) {
				return p
			}
		}
		return ""
	}

	first := a.startWorkflow(t, "first", `return await parallel([() => agent("a1"), () => agent("a2"), () => agent("a3")])`)
	running := []string{prompt(receiveChild(t, requests)), prompt(receiveChild(t, requests))}
	second := a.startWorkflow(t, "second", `return await agent("b1")`)

	assert.Equal(t, "b1", prompt(receiveChild(t, requests)), "the second run's agent starts while the first run's third waits")
	assert.Empty(t, requests)

	close(gate)
	a.untilStatus(t, first, tool.TaskCompleted)
	a.untilStatus(t, second, tool.TaskCompleted)
	assert.JSONEq(t, `["r", "r", "r"]`, a.info(t, first).Result)
	running = append(running, prompt(receiveChild(t, requests)))
	slices.Sort(running)
	assert.Equal(t, []string{"a1", "a2", "a3"}, running)
}

func TestWorkflowLimitRefusesAnotherRunUntilOneEnds(t *testing.T) {
	a, started := blockingHarness(t, Config{MaxWorkflows: 1})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "c1"))})
	a.rootAcks(1)
	first := a.startWorkflow(t, "first", `return await agent("go")`)
	wait(t, started)

	_, err := a.c.startWorkflow("second", `return 1`)

	require.ErrorIs(t, err, ErrTooManyWorkflows)
	assert.Contains(t, err.Error(), "1 workflows are already running")
	_, _, err = a.tasks.Stop(first)
	require.NoError(t, err)
	second := a.startWorkflow(t, "second", `return 1`)
	a.untilStatus(t, second, tool.TaskCompleted)
	assert.Equal(t, "1", a.info(t, second).Result)
}

func TestWorkflowLimitIsFreedBySessionSwitch(t *testing.T) {
	a, started := blockingHarness(t, Config{MaxWorkflows: 1})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "c1"))})
	a.rootAcks(1)
	a.startWorkflow(t, "first", `return await agent("go")`)
	wait(t, started)

	require.NoError(t, a.c.NewSession(t.Context()))
	second := a.startWorkflow(t, "second", `return 1`)

	a.untilStatus(t, second, tool.TaskCompleted)
}

func TestWorkflowPhaseCoercesItsArgumentToAString(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("r")})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `phase(1); return await agent("go")`)
	a.untilStatus(t, id, tool.TaskCompleted)

	agents := a.workflowAgents(id)
	require.Len(t, agents, 1)
	assert.Equal(t, tool.TaskCompleted, agents[0].State)
	assert.Equal(t, "1", agents[0].Phase)
}

func TestWorkflowPerCallPhaseOptionCoercesToAString(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("r")})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `return await agent("go", {phase: 2})`)
	a.untilStatus(t, id, tool.TaskCompleted)

	agents := a.workflowAgents(id)
	require.Len(t, agents, 1)
	assert.Equal(t, tool.TaskCompleted, agents[0].State)
	assert.Equal(t, "2", agents[0].Phase)
}

func TestWorkflowPhaseNullClearsThePhase(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("r")})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `phase("A"); phase(null); return await agent("go")`)
	a.untilStatus(t, id, tool.TaskCompleted)

	agents := a.workflowAgents(id)
	require.Len(t, agents, 1)
	assert.Equal(t, tool.TaskCompleted, agents[0].State)
	assert.Empty(t, agents[0].Phase)
}

func TestWorkflowLabelAndPhaseKeepOnlyTheirFirstLine(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("r")})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `return await agent("go", {label: "first\nsecond", phase: "A\nB"})`)
	a.untilStatus(t, id, tool.TaskCompleted)

	agents := a.workflowAgents(id)
	require.Len(t, agents, 1)
	assert.Equal(t, "first", agents[0].Description)
	assert.Equal(t, "A", agents[0].Phase)
}

func TestWorkflowAgentWhoseTurnFailsIsRecordedFailedAndItsSessionClosed(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	evs := recordEvents(a.c)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ProviderError("boom")})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `return await agent("go").catch(() => "caught")`)
	a.untilStatus(t, id, tool.TaskCompleted)

	agents := a.workflowAgents(id)
	require.Len(t, agents, 1)
	assert.Equal(t, tool.TaskFailed, agents[0].State)
	assert.NotEmpty(t, agents[0].ID)
	assert.Contains(t, agents[0].Reason, "boom")
	assert.False(t, agents[0].Ended.IsZero())
	var closed []string
	for len(evs) > 0 {
		if e, ok := (<-evs).(event.SessionClosed); ok {
			closed = append(closed, e.ID)
		}
	}
	assert.Equal(t, []string{agents[0].ID}, closed)
}

func TestWorkflowAgentsPendingApprovalEndsOnceBeforeSessionChangedOnASwitch(t *testing.T) {
	ran := new(atomic.Int32)
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{AgentTools: []tool.Tool{probeTool(ran)}})
	approvals := subscribeTo[event.ApprovalRequested](a.c)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("p1", 1)), pigruntimetest.Text("unreached")})
	a.rootAcks(1)
	oldID := a.c.Status().SessionID
	a.startWorkflow(t, "audit", `return await agent("go")`)
	req := receiveEvent(t, approvals)

	require.NoError(t, a.c.NewSession(t.Context()))

	evs := a.rec.all()
	changedAt := slices.IndexFunc(evs, func(ev event.Event) bool {
		changed, ok := ev.(event.SessionChanged)
		return ok && changed.SessionID != oldID
	})
	require.GreaterOrEqual(t, changedAt, 0)
	ended := event.Event(event.ApprovalEnded{Token: req.Token})
	assert.Len(t, slices.DeleteFunc(slices.Clone(evs), func(ev event.Event) bool { return ev != ended }), 1)
	assert.Less(t, slices.Index(evs, ended), changedAt)
	assert.False(t, a.c.Answer(req.Token, policy.Decision{Answer: policy.Allow}))
	assert.Zero(t, ran.Load())
}

func TestWorkflowEndAfterASwitchReportsNothingToTheNewSession(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	a.c.mu.Lock()
	old := a.c.main.sess
	a.c.mu.Unlock()
	require.NoError(t, a.c.NewSession(t.Context()))
	a.c.mu.Lock()
	current := a.c.main.sess
	a.c.mu.Unlock()
	require.NotSame(t, old, current)

	// StopAll forgets every task of the old session, so a run completing
	// naturally between the install and the StopAll is reproduced with a task
	// registered after the switch.
	end := func(parent *pigruntime.Session) {
		r := &workflowRun{c: a.c, parent: parent, cancel: func(error) {}}
		r.task = a.tasks.StartAgent(tool.WorkflowAgent, "audit", func() {})
		r.end(tool.TaskCompleted, "", "1")
	}
	requests := make(chan pigruntimetest.Request, 1)
	a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ack")})

	end(old)
	assert.Empty(t, a.queuedNotices())
	assert.True(t, a.idle())
	assert.Zero(t, a.faux.CallCount())

	end(current)
	assert.Contains(t, receive(t, requests).Raw, "Workflow t")
}

func TestWorkflowAgentOfAPlanPinnedDefinitionIsRefusedWritesWhileOthersFollowTheMode(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	var ran atomic.Int32
	rules, err := policy.NewRules(sb.Workspace, sb.Home, map[string]policy.ToolSpec{"Exec": {}}, policy.Source{Name: "test", Allow: []string{"Exec"}})
	require.NoError(t, err)
	planner, worker := agentDef("planner"), agentDef("worker")
	planner.Plan = true
	a := newWorkflowHarness(t, sb, Config{AgentTools: []tool.Tool{execTool(&ran)}, Rules: rules}, planner, worker)
	a.c.EnterExecute()
	requests := make(chan pigruntimetest.Request, 1)
	a.rootAcks(3)
	run := func(agentType, toolCall string) []string {
		a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Exec", toolCall)), pigruntimetest.Capture(t, requests, "done")})
		id := a.startWorkflow(t, agentType, fmt.Sprintf(`return await agent("go", {agentType: %q})`, agentType))
		a.untilStatus(t, id, tool.TaskCompleted)
		return receive(t, requests).ToolResults
	}

	results := run("planner", "c1")
	require.Len(t, results, 1)
	assert.Contains(t, results[0], "needs execute mode")
	assert.Zero(t, ran.Load())

	assert.Equal(t, []string{execOutput}, run("worker", "c2"))
	assert.Equal(t, int32(1), ran.Load())

	a.c.EnterPlan()
	results = run("worker", "c3")
	require.Len(t, results, 1)
	assert.Contains(t, results[0], "needs execute mode")
	assert.Equal(t, int32(1), ran.Load())
}

func TestWorkflowCallBeyondTheLimitFailsTheRunEvenWhenCaught(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	requests := make(chan pigruntimetest.Request, 1)
	a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ack")})

	id := a.startWorkflow(t, "audit", fmt.Sprintf(`
for (let i = 0; i <= %d; i++) {
  try { await agent("x", {agentType: "nope"}) } catch (e) {}
}
return "done"`, tool.WorkflowCalls))
	a.untilStatus(t, id, tool.TaskFailed)

	info := a.info(t, id)
	assert.Equal(t, ErrTooManyWorkflowCalls.Error(), info.Reason)
	assert.Contains(t, info.Reason, fmt.Sprint(tool.WorkflowCalls))
	assert.Equal(t, []string{fmt.Sprintf("Workflow %s failed (%s): audit", id, info.Reason)}, receive(t, requests).UserTexts)
}

func TestWorkflowEndWakesMainOnceWithTheValueAndNoNoticePerAgent(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("one"), pigruntimetest.Text("two")})
	requests := make(chan pigruntimetest.Request, 2)
	a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ack")})
	ends := turnEnds(a.c)

	id := a.startWorkflow(t, "audit", `return (await parallel([() => agent("a"), () => agent("b")])).sort()`)
	receiveEnd(t, ends)

	assert.Equal(t, []string{fmt.Sprintf("Workflow %s completed: audit\n[\"one\",\"two\"]", id)}, receive(t, requests).UserTexts)
	assert.Equal(t, 1, a.faux.CallCount())
	assert.Empty(t, ofType[event.TaskEnded](a.rec.all())[1:], "no task end but the run's")
}

func TestWorkflowThrowFailsTheRunWithTheError(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	requests := make(chan pigruntimetest.Request, 1)
	a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ack")})

	id := a.startWorkflow(t, "audit", "\n\nthrow new Error(\"boom\")")
	a.untilStatus(t, id, tool.TaskFailed)

	text := receive(t, requests).UserTexts[0]
	assert.True(t, strings.HasPrefix(text, fmt.Sprintf("Workflow %s failed (", id)), text)
	assert.Contains(t, text, "boom")
	assert.True(t, strings.HasSuffix(text, "): audit"), text)
}

func TestRunningWorkflowIsNotASubagentAndRefusesLogin(t *testing.T) {
	a, started := blockingHarness(t, Config{}, agentDef("explorer"))
	pigruntimetest.RegisterLoginProvider(t, a.c.rt, loginProvider)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "c1"))})
	a.c.queue.Lock()
	a.c.live = maxAgents
	a.c.queue.Unlock()

	id := a.startWorkflow(t, "audit", `return await agent("go")`)
	wait(t, started)

	assert.Equal(t, "t1", id, "one task, none for the agent")
	assert.Equal(t, 1, a.c.Status().Workflows)
	assert.Zero(t, a.c.Status().Agents)
	assert.Len(t, a.c.Agents(), 1, "only the main session")
	a.c.queue.Lock()
	assert.Empty(t, a.c.children)
	a.c.queue.Unlock()
	assert.Equal(t, maxAgents, a.live(), "the workflow's agent is not counted")
	_, err := a.c.Submit(t.Context(), Main, "/login "+loginProvider)
	assert.ErrorIs(t, err, ErrWorkStillRunning)

	_, _, err = a.tasks.Stop(id)
	require.NoError(t, err)
	a.untilStatus(t, id, tool.TaskStopped)
	assert.Zero(t, a.c.Status().Workflows)
	a.c.queue.Lock()
	a.c.live = 0
	a.c.queue.Unlock()
	_, err = a.c.Submit(t.Context(), Main, "/login "+loginProvider)
	assert.NotErrorIs(t, err, ErrWorkStillRunning, "a login is no longer refused once the workflow has ended")
}

func TestTwentyRunningSubagentsDoNotDelayAWorkflowAgent(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{}, agentDef("explorer"))
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("ok")})
	a.rootAcks(1)
	a.c.queue.Lock()
	a.c.live = maxAgents
	a.c.queue.Unlock()

	id := a.startWorkflow(t, "audit", `return await agent("go")`)
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.Equal(t, `"ok"`, a.info(t, id).Result)
	_, err := a.c.launch(t.Context(), "explorer", "more", "go")
	assert.ErrorIs(t, err, ErrTooManyAgents)
}

func TestWorkflowToolIsOfferedToTheMainSessionOnlyAndNotDescribedAsDeferredMCP(t *testing.T) {
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{}, agentDef("explorer"))

	assert.Contains(t, toolNames(a.c.tools), "Workflow")
	assert.NotContains(t, a.c.prompt[0].Text, "deferred")
	assert.Len(t, a.c.workflowSpecs, 2, "the default agent and the definition")
	assert.Equal(t, "", a.c.workflowSpecs[0].def.Name)
}

func TestNoChildSpecHasANeverChildToolWhateverItsDefinitionNames(t *testing.T) {
	excluded := []string{"Agent", "SendMessage", "TaskStop", "Workflow"}
	agentTools := []tool.Tool{testTool("Look", policy.Read, nil)}
	for _, name := range excluded {
		agentTools = append(agentTools, testTool(name, policy.Read, nil))
	}
	all, named := agentDef("all"), agentDef("named")
	named.Tools = append([]string{"Look"}, excluded...)
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{AgentTools: agentTools}, all, named)

	require.Len(t, a.c.workflowSpecs, 3)
	for _, spec := range slices.Concat(a.c.agents, a.c.workflowSpecs) {
		names := toolNames(spec.tools)
		assert.Contains(t, names, "Look")
		for _, name := range excluded {
			assert.NotContains(t, names, name)
		}
	}
}

func TestKeywordMakesWorkflowCallableForThatPrompt(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"ultracode check this", true},
		{"Ultracode.", true},
		{"ultracoder", false},
		{"myultracode", false},
		{"see @ultracode", false},
		{"nothing", false},
	} {
		t.Run(tc.text, func(t *testing.T) {
			a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{})
			requests := make(chan pigruntimetest.Request, 1)
			a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ok")})

			a.turn(t, tc.text)

			assert.Equal(t, tc.want, slices.Contains(receive(t, requests).ToolNames, "Workflow"))
		})
	}
}

func TestKeywordInsideASkillCommandDoesNotMakeWorkflowCallable(t *testing.T) {
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), skillConfig("skill", "ultracode-review"))
	requests := make(chan pigruntimetest.Request, 1)
	a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ok")})

	a.turn(t, "/skill:ultracode-review now")

	assert.NotContains(t, receive(t, requests).ToolNames, "Workflow")
}

func TestKeywordStaysOnForTheSession(t *testing.T) {
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{})
	requests := make(chan pigruntimetest.Request, 2)
	a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "one"), pigruntimetest.Capture(t, requests, "two")})

	a.turn(t, "ultracode")
	a.turn(t, "plain")

	receive(t, requests)
	assert.Contains(t, receive(t, requests).ToolNames, "Workflow")
}

func TestKeywordSetsPromptToolsOnlyForTheMainSessionWhenOffered(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	deny, err := policy.NewRules(sb.Workspace, sb.Home, map[string]policy.ToolSpec{"Workflow": {}}, policy.Source{Name: "test", Deny: []string{"Workflow"}})
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		cfg     Config
		session string // "" is the main session, "child" the subagent
		want    []string
	}{
		{name: "main", want: []string{"Workflow"}},
		{name: "subagent", session: "child"},
		{name: "unattended", cfg: Config{Unattended: true}},
		{name: "denied", cfg: Config{Rules: deny}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAgentHarness(t, sb, tc.cfg, agentDef("explorer"))
			rootGate, childGate := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() { close(childGate) })
			t.Cleanup(func() { close(rootGate) })
			a.faux.SetResponses([]ai.FauxResponseStep{gated(rootGate, "root")})
			a.child.SetResponses([]ai.FauxResponseStep{gated(childGate, "child")})
			id := a.launch(t, "explorer", "find it", "go")
			_, err := a.c.Submit(t.Context(), Main, "first")
			require.NoError(t, err)
			session, target := Main, a.c.main
			if tc.session != "" {
				session, target = id, a.childOf(id)
			}

			_, err = a.c.Submit(t.Context(), session, "ultracode please")
			require.NoError(t, err)

			a.c.queue.Lock()
			defer a.c.queue.Unlock()
			require.Len(t, target.queued, 1)
			assert.Equal(t, tc.want, target.queued[0].prompt.Tools)
		})
	}
}

// workflowAgents is the agents of workflow run id.
func (a *agentHarness) workflowAgents(id string) []WorkflowAgent {
	_, agents, _ := a.c.Workflow(id)
	return agents
}

func TestWorkflowSnapshotFollowsACallFromQueuedToEnded(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{MaxAgents: 1})
	requests, gate := make(chan childRequest, 2), make(chan struct{})
	a.child.SetResponses([]ai.FauxResponseStep{captureChild(requests, gate, "r"), captureChild(requests, gate, "r")})
	a.rootAcks(1)
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})

	id := a.startWorkflow(t, "audit", `phase("Survey")
const first = agent("a1", {label: "first"})
const second = agent("b1", {label: "second", phase: "Review"})
const third = first.then(() => agent("c1", {agentType: "nope", label: "third"})).catch(e => e.message)
return await Promise.all([first, second, third])`)
	receiveChild(t, requests)
	a.until(t, func() bool {
		agents := a.workflowAgents(id)
		return len(agents) == 2 && (agents[0].State == WorkflowQueued) != (agents[1].State == WorkflowQueued)
	})

	status, agents, ok := a.c.Workflow(id)
	require.True(t, ok)
	require.Len(t, agents, 2)
	running, queued := agents[0], agents[1]
	if running.State == WorkflowQueued {
		running, queued = queued, running
	}
	assert.Equal(t, tool.TaskRunning, status.State)
	assert.Equal(t, "audit", status.Description)
	assert.False(t, status.Started.IsZero())
	assert.True(t, status.Ended.IsZero())
	assert.Equal(t, WorkflowStatus{
		ID: id, Description: "audit", State: tool.TaskRunning, Started: status.Started,
		Running: 1, Queued: 1, Phase: running.Phase,
	}, status)
	assert.Equal(t, tool.TaskRunning, running.State)
	assert.Equal(t, childProvider+"/faux-1", running.Model)
	assert.NotEmpty(t, running.ID)
	assert.False(t, running.Started.IsZero())
	assert.Empty(t, queued.ID)
	assert.True(t, queued.Started.IsZero())
	assert.Equal(t, 1, agents[0].N)
	assert.Equal(t, "first", agents[0].Description)
	assert.Equal(t, "Survey", agents[0].Phase)
	assert.Equal(t, 2, agents[1].N)
	assert.Equal(t, "second", agents[1].Description)
	assert.Equal(t, "Review", agents[1].Phase, "the option overrides the heading")
	assert.Equal(t, []WorkflowStatus{status}, a.c.Workflows())

	close(gate)
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.Empty(t, a.c.Workflows(), "only running runs")
	status, agents, ok = a.c.Workflow(id)
	require.True(t, ok)
	assert.Equal(t, tool.TaskCompleted, status.State)
	assert.False(t, status.Ended.IsZero())
	assert.Equal(t, 2, status.Done)
	assert.Equal(t, 1, status.Failed)
	assert.Zero(t, status.Running+status.Queued)
	assert.Empty(t, status.Phase)
	require.Len(t, agents, 3)
	for i, want := range []tool.TaskStatus{tool.TaskCompleted, tool.TaskCompleted, tool.TaskFailed} {
		assert.Equal(t, i+1, agents[i].N)
		assert.Equal(t, want, agents[i].State)
		assert.False(t, agents[i].Ended.IsZero())
	}
	assert.Equal(t, "nope", agents[2].Agent)
	assert.Equal(t, "Survey", agents[2].Phase)
	assert.Equal(t, running.ID, agents[running.N-1].ID)
	assert.Equal(t, childProvider+"/faux-1", agents[running.N-1].Model, "the record keeps what it was when the agent closed")
	assert.Empty(t, agents[0].Reason)
	assert.Contains(t, agents[2].Reason, "unknown agent type: nope")
	assert.Empty(t, agents[2].ID)

	require.NoError(t, a.c.NewSession(t.Context()))
	_, _, ok = a.c.Workflow(id)
	assert.False(t, ok, "a session switch forgets the run")
}

func TestWorkflowPhaseHoldsUntilTheNextPhaseCall(t *testing.T) {
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
	requests, gate := make(chan childRequest, 3), make(chan struct{})
	a.child.SetResponses([]ai.FauxResponseStep{captureChild(requests, gate, "r"), captureChild(requests, gate, "r"), captureChild(requests, gate, "r")})
	a.rootAcks(1)
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})

	id := a.startWorkflow(t, "audit", `phase("A"); const x = agent("one"); phase("B"); const y = agent("two", {phase: "C"}); const z = agent("three"); return await Promise.all([x, y, z])`)
	for range 3 {
		receiveChild(t, requests)
	}

	status, agents, _ := a.c.Workflow(id)
	require.Len(t, agents, 3)
	var phases []string
	for _, agent := range agents {
		phases = append(phases, agent.Phase)
	}
	assert.Equal(t, []string{"A", "C", "B"}, phases)
	assert.Equal(t, "B", status.Phase, "the phase of the latest running call")
	close(gate)
	a.untilStatus(t, id, tool.TaskCompleted)
}

func TestStopWorkflowAgentStopsOnlyThatQueuedOrRunningCall(t *testing.T) {
	a, started := blockingHarness(t, Config{MaxAgents: 1})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "c1"))})
	a.rootAcks(1)
	ends := subscribeTo[event.TaskEnded](a.c)

	id := a.startWorkflow(t, "audit", `const ts = [agent("a"), agent("b")]; return await Promise.all(ts.map(t => t.catch(e => e.message)))`)
	wait(t, started)
	a.until(t, func() bool {
		agents := a.workflowAgents(id)
		return len(agents) == 2 && (agents[0].State == WorkflowQueued) != (agents[1].State == WorkflowQueued)
	})
	byState := func(state tool.TaskStatus) WorkflowAgent {
		for _, agent := range a.workflowAgents(id) {
			if agent.State == state {
				return agent
			}
		}
		return WorkflowAgent{}
	}
	runningCall, queuedCall := byState(tool.TaskRunning), byState(WorkflowQueued)
	require.NotZero(t, runningCall.N)
	require.NotZero(t, queuedCall.N)

	a.c.StopWorkflowAgent(id, queuedCall.N)
	a.c.StopWorkflowAgent(id, 99)
	a.c.StopWorkflowAgent("t99", 1)
	a.until(t, func() bool { return byState(tool.TaskStopped).N == queuedCall.N })

	assert.Equal(t, tool.TaskRunning, byState(tool.TaskRunning).State, "the running call is untouched")
	assert.Equal(t, tool.TaskRunning, a.info(t, id).Status)
	assert.True(t, byState(tool.TaskStopped).Started.IsZero(), "it never started")

	a.c.StopWorkflowAgent(id, runningCall.N)
	a.untilStatus(t, id, tool.TaskCompleted)

	status, agents, _ := a.c.Workflow(id)
	assert.Equal(t, 2, status.Failed, "stopped calls count as failed")
	for _, agent := range agents {
		assert.Equal(t, tool.TaskStopped, agent.State)
		assert.Empty(t, agent.Reason)
	}
	a.c.StopWorkflowAgent(id, runningCall.N)
	assert.JSONEq(t, `["`+ErrAgentStopped.Error()+`", "`+ErrAgentStopped.Error()+`"]`, a.info(t, id).Result)
	ended := receiveEvent(t, ends)
	assert.Equal(t, 2, ended.Agents)
	assert.Equal(t, 2, ended.Failed)
}

func TestWorkflowTaskEndedCarriesCountsReasonAndResult(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
		a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("fine")})
		a.rootAcks(1)
		ends := subscribeTo[event.TaskEnded](a.c)

		id := a.startWorkflow(t, "audit", `const ok = await agent("go"); await agent("x", {agentType: "nope"}).catch(() => 0); return ok`)
		a.untilStatus(t, id, tool.TaskCompleted)

		ended := receiveEvent(t, ends)
		assert.Equal(t, 2, ended.Agents)
		assert.Equal(t, 1, ended.Failed)
		assert.Equal(t, `"fine"`, ended.Result)
		assert.Empty(t, ended.Reason)
	})
	t.Run("failed", func(t *testing.T) {
		a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{})
		a.rootAcks(1)
		ends := subscribeTo[event.TaskEnded](a.c)

		id := a.startWorkflow(t, "audit", `throw new Error("boom")`)
		a.untilStatus(t, id, tool.TaskFailed)

		ended := receiveEvent(t, ends)
		assert.Zero(t, ended.Agents)
		assert.Contains(t, ended.Reason, "boom")
		assert.Empty(t, ended.Result)
	})
	t.Run("StopTask", func(t *testing.T) {
		a, started := blockingHarness(t, Config{})
		a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "c1"))})
		id := a.startWorkflow(t, "audit", `return await agent("go")`)
		wait(t, started)

		require.NoError(t, a.c.StopTask(id))

		notices := a.queuedNotices()
		require.Len(t, notices, 1)
		assert.Equal(t, 1, notices[0].Shown.(event.TaskEnded).Agents)
		assert.Equal(t, 1, notices[0].Shown.(event.TaskEnded).Failed, "the call that has not yet noticed the stop counts as stopped")
		a.untilStatus(t, id, tool.TaskStopped)
		a.until(t, func() bool { return a.workflowAgents(id)[0].State != tool.TaskRunning })
		agents := a.workflowAgents(id)
		require.Len(t, agents, 1)
		assert.Equal(t, tool.TaskStopped, agents[0].State)
		assert.Empty(t, agents[0].Reason)
	})
}

func TestWorkflowAgentEventsArriveWrappedThenSessionClosed(t *testing.T) {
	look := testTool("Look", policy.Read, func(context.Context, json.RawMessage) (string, error) { return "looked", nil })
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{AgentTools: []tool.Tool{look}})
	evs := recordEvents(a.c)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Look", "c1")), pigruntimetest.Text("found it")})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `return await agent("look around")`)
	a.untilStatus(t, id, tool.TaskCompleted)
	sessionID := a.workflowAgents(id)[0].ID
	require.NotEmpty(t, sessionID)

	var got []event.Event
	for len(evs) > 0 {
		got = append(got, <-evs)
	}
	own := sessionOf(got, sessionID)
	require.NotEmpty(t, own)
	assert.Equal(t, event.TurnStart{Prompt: "look around"}, own[0])
	assert.Contains(t, own, event.Event(event.TextDelta{Text: "found it"}))
	assert.True(t, slices.ContainsFunc(own, func(ev event.Event) bool { _, ok := ev.(event.ToolStart); return ok }), "the tool call arrived wrapped")
	end, ok := own[len(own)-1].(event.TurnEnd)
	require.True(t, ok)
	assert.Equal(t, event.Completed, end.Outcome.Status)
	var closed []int
	for i, ev := range got {
		switch e := ev.(type) {
		case event.SessionClosed:
			assert.Equal(t, sessionID, e.ID)
			closed = append(closed, i)
		case event.ToolStart, event.ToolEnd, event.UserMessage:
			t.Errorf("an agent's %T arrived bare", e)
		}
	}
	require.Len(t, closed, 1)
	for i, ev := range got {
		if s, ok := ev.(event.Session); ok && s.ID == sessionID {
			assert.Less(t, i, closed[0], "nothing of the session follows its close")
		}
	}
}

func TestWorkflowHistoryReturnsAClosedAgentsTranscriptAsDisplayEvents(t *testing.T) {
	look := testTool("Look", policy.Read, func(context.Context, json.RawMessage) (string, error) { return "looked", nil })
	look.Display.Title = func(json.RawMessage) string { return "Looked around" }
	a := newWorkflowHarness(t, pigruntimetest.NewSandbox(t), Config{AgentTools: []tool.Tool{look}})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Look", "c1")), pigruntimetest.Text("done")})
	a.rootAcks(1)

	id := a.startWorkflow(t, "audit", `try { await agent("x", {agentType: "nope"}) } catch (e) {} return await agent("go")`)
	a.untilStatus(t, id, tool.TaskCompleted)

	got, err := a.c.WorkflowHistory(id, 2)

	require.NoError(t, err)
	var titles []string
	for _, ev := range got {
		if start, ok := ev.(event.ToolStart); ok {
			titles = append(titles, start.Title)
		}
	}
	assert.Equal(t, []string{"Looked around"}, titles)
	assert.Contains(t, got, event.Event(event.TextDelta{Text: "done"}))
	_, err = a.c.WorkflowHistory(id, 1)
	assert.ErrorIs(t, err, pigruntime.ErrNotSaved, "an agent that never opened has none")
	_, err = a.c.WorkflowHistory(id, 99)
	assert.ErrorIs(t, err, tool.ErrUnknownTask)
	_, err = a.c.WorkflowHistory("t99", 1)
	assert.ErrorIs(t, err, tool.ErrUnknownTask)
}

func TestWorkflowHistoryRefusesARunningAgent(t *testing.T) {
	a, started := blockingHarness(t, Config{})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "c1"))})
	id := a.startWorkflow(t, "audit", `return await agent("go")`)
	wait(t, started)

	_, err := a.c.WorkflowHistory(id, 1)

	assert.ErrorIs(t, err, ErrAgentRunning)
	_, _, err = a.tasks.Stop(id)
	require.NoError(t, err)
}

func TestWorkflowHistoryRefusesAQueuedAgent(t *testing.T) {
	a, started := blockingHarness(t, Config{MaxAgents: 1})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "c1"))})
	id := a.startWorkflow(t, "audit", `return await Promise.all([agent("one"), agent("two")])`)
	wait(t, started)
	a.until(t, func() bool {
		agents := a.workflowAgents(id)
		return len(agents) == 2 && (agents[0].State == WorkflowQueued) != (agents[1].State == WorkflowQueued)
	})
	queued := a.workflowAgents(id)[0]
	if queued.State != WorkflowQueued {
		queued = a.workflowAgents(id)[1]
	}

	_, err := a.c.WorkflowHistory(id, queued.N)

	assert.ErrorIs(t, err, ErrAgentRunning)
	_, _, err = a.tasks.Stop(id)
	require.NoError(t, err)
}

func TestControlsRefuseAWorkflowAgentsSession(t *testing.T) {
	a, started := blockingHarness(t, Config{})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "c1"))})
	id := a.startWorkflow(t, "audit", `return await agent("go")`)
	wait(t, started)
	sessionID := a.workflowAgents(id)[0].ID
	require.NotEmpty(t, sessionID)

	_, err := a.c.Submit(t.Context(), sessionID, "hello")
	assert.ErrorIs(t, err, tool.ErrUnknownTask)
	assert.ErrorIs(t, a.c.SendNow(t.Context(), sessionID, "hello"), tool.ErrUnknownTask)
	assert.Nil(t, a.c.TakeQueued(sessionID))
	assert.False(t, a.c.Interrupt(sessionID))
	assert.Len(t, a.c.Agents(), 1, "only the main session")

	_, _, err = a.tasks.Stop(id)
	require.NoError(t, err)
}
