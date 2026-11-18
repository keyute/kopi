package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/classifier"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/subagent"
	"github.com/keyute/kopi/internal/tool"
)

// childProvider serves the subagents, so the root's and their scripted
// responses never interleave.
const childProvider = "kopi-child"

type agentHarness struct {
	*harness
	child *ai.FauxProviderHandle
	tasks *tool.Tasks
	// changed holds one pending TasksChanged, however many arrived.
	changed chan struct{}
}

func agentDef(name string) subagent.Definition {
	return subagent.Definition{Name: name, Description: name + " agent", Model: childProvider + "/faux-1", Instructions: true}
}

// newAgentHarness builds a controller with the definitions defs over the real
// task registry and opens its session.
func newAgentHarness(t *testing.T, sb pigruntimetest.Sandbox, cfg Config, defs ...subagent.Definition) *agentHarness {
	t.Helper()
	cfg.Agents = defs
	tasks := tool.NewTasks()
	cfg.Tasks = tasks
	h := newHarness(t, sb, cfg)
	child := ai.NewFauxProvider(ai.FauxConfig{ProviderID: childProvider, Models: []ai.FauxModelDefinition{{ID: "faux-1", Reasoning: true}}})
	require.NoError(t, h.c.rt.RegisterProvider(child.Provider()))
	a := &agentHarness{harness: h, child: child, tasks: tasks, changed: make(chan struct{}, 1)}
	h.c.Subscribe(func(ev event.Event) {
		if _, ok := ev.(event.TasksChanged); ok {
			select {
			case a.changed <- struct{}{}:
			default:
			}
		}
	})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	return a
}

func (a *agentHarness) launch(t *testing.T, agent, description, prompt string) string {
	t.Helper()
	id, err := a.c.launch(t.Context(), agent, description, prompt)
	require.NoError(t, err)
	return id
}

func (a *agentHarness) info(t *testing.T, id string) tool.TaskInfo {
	t.Helper()
	info, err := a.tasks.Output(id)
	require.NoError(t, err)
	return info
}

// until waits for cond, which changes only together with a TasksChanged.
func (a *agentHarness) until(t *testing.T, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-a.changed:
		case <-time.After(waitLimit):
			t.Fatal("timed out waiting for the tasks to change")
		}
	}
}

func (a *agentHarness) untilStatus(t *testing.T, id string, status tool.TaskStatus) {
	t.Helper()
	a.until(t, func() bool { return a.info(t, id).Status == status })
}

func (a *agentHarness) live() int {
	a.c.queue.Lock()
	defer a.c.queue.Unlock()
	return a.c.live
}

func (a *agentHarness) childOf(id string) *child {
	a.c.queue.Lock()
	defer a.c.queue.Unlock()
	return a.c.children[id]
}

type childRequest struct {
	system   string
	tools    []string
	thinking ai.ThinkingLevel
	raw      string
}

// captureChild answers text once gate, if not nil, is closed, and sends the
// request it received to requests.
func captureChild(requests chan<- childRequest, gate <-chan struct{}, text string) ai.FauxResponseStep {
	return ai.FauxFactoryStep(func(tc ai.TranscriptContext, o ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
		msgs := tc.Messages()
		raw, _ := json.Marshal(msgs)
		req := childRequest{system: ai.GetCurrentSystemPrompt(msgs), thinking: o.Thinking, raw: string(raw)}
		for _, ts := range ai.GetCurrentTools(msgs) {
			req.tools = append(req.tools, ts.Name)
		}
		requests <- req
		if gate != nil {
			<-gate
		}
		return ai.FauxAssistantMessage(ai.FauxContentString(text), ai.FauxAssistantMessageOptions{}), nil
	})
}

func receiveChild(t *testing.T, requests <-chan childRequest) childRequest {
	t.Helper()
	select {
	case req := <-requests:
		return req
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for a subagent request")
		return childRequest{}
	}
}

func TestNoDefinitionsMeansNoAgentTools(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	read := readTool(t, sb)
	h := newHarness(t, sb, Config{Tools: []tool.Tool{read}})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "done")})

	h.turn(t, "hi")

	req := receive(t, requests)
	assert.Equal(t, []string{"Read", "AskUserQuestion"}, req.ToolNames)
	assert.Equal(t, joinPrompt(buildPrompt(sb.Workspace, []tool.Tool{read, tool.NewAskUserQuestion(nil)}, nil, nil, false, nil)), req.SystemPrompt)
}

func TestDefinitionsAddAgentAndSendMessageUnlessRuleRemovesThem(t *testing.T) {
	specs := map[string]policy.ToolSpec{"Agent": {}, "SendMessage": {}}
	for _, tc := range []struct {
		name string
		deny string
		want []string
	}{
		{"no rule", "", []string{"AskUserQuestion", "Agent", "SendMessage"}},
		{"agent", "Agent", []string{"AskUserQuestion"}},
		{"send message", "SendMessage", []string{"AskUserQuestion", "Agent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := pigruntimetest.NewSandbox(t)
			var cfg Config
			if tc.deny != "" {
				rules, err := policy.NewRules(sb.Workspace, sb.Home, specs, policy.Source{Name: "test", Deny: []string{tc.deny}})
				require.NoError(t, err)
				cfg.Rules = rules
			}
			a := newAgentHarness(t, sb, cfg, agentDef("explorer"))
			assert.Equal(t, tc.want, toolNames(a.c.tools))
		})
	}
}

func TestAgentToolsAreResolvedPerDefinition(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	read := readTool(t, sb)
	agentTools := []tool.Tool{
		read, execTool(new(atomic.Int32)), testTool("Gone", policy.Read, nil), testTool("Agent", policy.Read, nil),
		testTool("SendMessage", policy.Read, nil), testTool("TaskOutput", policy.Read, nil), testTool("TaskStop", policy.Read, nil),
		tool.NewToolSearch(nil, nil, nil),
	}
	specs := map[string]policy.ToolSpec{"Read": {}, "Exec": {}, "Gone": {}, "ToolSearch": {}}
	plain, named, all := agentDef("plain"), agentDef("named"), agentDef("all")
	plain.Tools = []string{"Read"}
	named.Tools = []string{"Read", "Gone", "Agent", "TaskStop", "mcp__s__a", "mcp__missing__x"}
	statuses := servers(
		pigruntime.ServerStatus{Name: "s", Instructions: "use s", Tools: []tool.Tool{serverTool("mcp__s__a"), serverTool("mcp__s__b")}},
		pigruntime.ServerStatus{Name: "t", Tools: []tool.Tool{serverTool("mcp__t__c")}},
	)

	t.Run("rules removing a tool", func(t *testing.T) {
		rules, err := policy.NewRules(sb.Workspace, sb.Home, specs, policy.Source{Name: "test", Deny: []string{"Gone"}})
		require.NoError(t, err)
		a := newAgentHarness(t, sb, Config{Tools: []tool.Tool{read}, AgentTools: agentTools, Rules: rules, Servers: statuses}, all, named, plain)

		require.Len(t, a.c.agents, 3)
		assert.Equal(t, []string{"Read", "Exec", "ToolSearch", "mcp__s__a", "mcp__s__b", "mcp__t__c"}, toolNames(a.c.agents[0].tools))
		assert.Equal(t, []string{"Read", "ToolSearch", "mcp__s__a"}, toolNames(a.c.agents[1].tools))
		assert.Equal(t, []string{"Read"}, toolNames(a.c.agents[2].tools))

		assert.Equal(t, []string{"preamble", "mcp-servers"}, partNames(a.c.agents[0].prompt))
		assert.Contains(t, a.c.agents[0].prompt[1].Text, "MCP server t tools: mcp__t__c.")
		assert.Equal(t, []string{"preamble", "mcp-servers"}, partNames(a.c.agents[1].prompt))
		assert.Equal(t, "MCP server s tools: mcp__s__a.\nInstructions from MCP server s:\nuse s", a.c.agents[1].prompt[1].Text)
		assert.Equal(t, []string{"preamble"}, partNames(a.c.agents[2].prompt))
	})

	t.Run("rule removing ToolSearch", func(t *testing.T) {
		rules, err := policy.NewRules(sb.Workspace, sb.Home, specs, policy.Source{Name: "test", Deny: []string{"ToolSearch"}})
		require.NoError(t, err)
		a := newAgentHarness(t, sb, Config{Tools: []tool.Tool{read}, AgentTools: agentTools, Rules: rules, Servers: statuses}, all)

		assert.Equal(t, []string{"Read", "Exec", "Gone"}, toolNames(a.c.agents[0].tools), "the deferred tools cannot be loaded, so the child has none")
		assert.Equal(t, []string{"preamble"}, partNames(a.c.agents[0].prompt))
	})
}

func TestLaunchReturnsAtOnceWhileChildRunsWithItsDefinition(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	def := agentDef("explorer")
	def.Effort, def.Prompt = "high", "AGENT_BODY"
	a := newAgentHarness(t, sb, Config{AgentTools: []tool.Tool{readTool(t, sb)}}, def)
	requests, gate := make(chan childRequest, 1), make(chan struct{})
	a.child.SetResponses([]ai.FauxResponseStep{captureChild(requests, gate, "final")})
	a.rootAcks(1)

	id := a.launch(t, "explorer", "find it", "go find it")

	assert.Equal(t, "t1", id)
	req := receiveChild(t, requests)
	assert.Equal(t, ai.ThinkingLevelHigh, req.thinking)
	assert.Equal(t, []string{"Read"}, req.tools)
	assert.Equal(t, joinPrompt(a.c.agents[0].prompt), req.system)
	assert.Contains(t, req.system, "You are the subagent explorer")
	assert.Contains(t, req.system, "AGENT_BODY")
	assert.Zero(t, a.faux.CallCount(), "the definition's model serves the child, not the root's")
	assert.Equal(t, tool.TaskRunning, a.info(t, id).Status)
	assert.Equal(t, 1, a.c.Status().Agents)

	close(gate)
	a.untilStatus(t, id, tool.TaskCompleted)
	assert.Equal(t, "final", a.info(t, id).Result)
	a.until(t, func() bool { return a.live() == 0 })
}

func TestChildWithoutModelUsesTheRootsModel(t *testing.T) {
	def := agentDef("explorer")
	def.Model = ""
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{}, def)
	requests := make(chan pigruntimetest.Request, 2)
	a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "final"), pigruntimetest.Capture(t, requests, "ack")})

	a.launch(t, "explorer", "find it", "go")

	assert.Equal(t, []string{"go"}, receive(t, requests).UserTexts)
	assert.Contains(t, receive(t, requests).UserTexts[0], "completed: find it")
}

func TestSubagentsAreOfferedWebSearchForASearchingSmallModel(t *testing.T) {
	plain, named := agentDef("plain"), agentDef("named")
	named.Tools = []string{"Read"}
	a := newAgentHarness(t, searchSandbox(t), Config{SmallModel: "kopi-search/search-1"}, plain, named)

	assert.Equal(t, []string{"WebSearch"}, toolNames(a.c.agents[0].tools))
	assert.Empty(t, a.c.agents[1].tools)
}

func TestChildWithoutModelRunsUnderTheResolvedDefaultModel(t *testing.T) {
	def := agentDef("explorer")
	def.Model = ""
	h := newUnsetHarness(t, Config{Agents: []subagent.Definition{def}, Tasks: tool.NewTasks()}, true)
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	requests := make(chan pigruntimetest.Request, 2)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "final"), pigruntimetest.Capture(t, requests, "ack")})

	_, err := h.c.launch(t.Context(), "explorer", "find it", "go")
	require.NoError(t, err)

	assert.Equal(t, []string{"go"}, receive(t, requests).UserTexts)
	assert.Contains(t, receive(t, requests).UserTexts[0], "completed: find it")
}

func TestCompletionNoticeCarriesFullFinalMessageAndWakesRoot(t *testing.T) {
	for _, wake := range []bool{false, true} {
		t.Run(fmt.Sprintf("wake=%t", wake), func(t *testing.T) {
			a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{Wake: wake}, agentDef("explorer"))
			final := strings.Repeat("line\n", 30) + "last"
			a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text(final)})
			requests := make(chan pigruntimetest.Request, 1)
			a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ack")})
			ends := turnEnds(a.c)

			id := a.launch(t, "explorer", "find it", "go")
			receiveEnd(t, ends)

			assert.Equal(t, []string{fmt.Sprintf("Agent %s (explorer) completed: find it\n%s", id, final)}, receive(t, requests).UserTexts)
			assert.Equal(t, 1, a.faux.CallCount())
		})
	}
}

func TestFailedChildNoticeNamesTheReason(t *testing.T) {
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{}, agentDef("explorer"))
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ProviderError("boom")})
	requests := make(chan pigruntimetest.Request, 1)
	a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ack")})
	ends := turnEnds(a.c)

	id := a.launch(t, "explorer", "find it", "go")
	receiveEnd(t, ends)

	assert.Equal(t, tool.TaskFailed, a.info(t, id).Status)
	assert.Equal(t, []string{fmt.Sprintf("Agent %s (explorer) failed (boom): find it", id)}, receive(t, requests).UserTexts)
}

func TestLaunchAndRerunRefuseOverTheCap(t *testing.T) {
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{}, agentDef("explorer"))
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("done")})
	a.rootAcks(1)
	id := a.launch(t, "explorer", "find it", "go")
	a.untilStatus(t, id, tool.TaskCompleted)
	a.until(t, func() bool { return a.live() == 0 })
	a.c.queue.Lock()
	a.c.live = maxAgents
	a.c.queue.Unlock()

	_, err := a.c.launch(t.Context(), "explorer", "more", "go")
	assert.ErrorIs(t, err, ErrTooManyAgents)
	_, err = a.c.message(t.Context(), id, "again")
	assert.ErrorIs(t, err, ErrTooManyAgents)
	assert.Equal(t, maxAgents, a.live(), "a refusal holds no slot")
	assert.Len(t, a.tasks.List(), 1)
}

func TestMessageToRunningChildArrivesAtItsNextTurnBoundary(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{AgentTools: []tool.Tool{releaseTool(started, release)}}, agentDef("explorer"))
	requests := make(chan pigruntimetest.Request, 1)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "c1")), pigruntimetest.Capture(t, requests, "done")})
	a.rootAcks(1)
	id := a.launch(t, "explorer", "find it", "go")
	wait(t, started)

	resumed, err := a.c.message(t.Context(), id, "also check b")
	require.NoError(t, err)
	assert.False(t, resumed)
	close(release)
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.Equal(t, []string{"go", "also check b"}, receive(t, requests).UserTexts)
	assert.Contains(t, a.info(t, id).Output, "› also check b\n")
}

func TestFailedTurnReportsAMessageQueuedMeanwhileAsUndelivered(t *testing.T) {
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{}, agentDef("explorer"))
	started, gate := make(chan struct{}), make(chan struct{})
	childRequests := make(chan pigruntimetest.Request, 1)
	a.child.SetResponses([]ai.FauxResponseStep{
		ai.FauxFactoryStep(func(ai.TranscriptContext, ai.StreamOptions, *ai.FauxProviderState, *ai.Model) (ai.AssistantMessage, error) {
			close(started)
			<-gate
			return ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonError), ErrorMessage: "boom"}), nil
		}),
		pigruntimetest.Capture(t, childRequests, "unexpected"),
	})
	rootRequests := make(chan pigruntimetest.Request, 2)
	a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, rootRequests, "ack"), pigruntimetest.Capture(t, rootRequests, "ack")})
	ends := turnEnds(a.c)
	id := a.launch(t, "explorer", "find it", "go")
	wait(t, started)

	resumed, err := a.c.message(t.Context(), id, "also check b")
	require.NoError(t, err)
	assert.False(t, resumed)
	close(gate)
	a.untilStatus(t, id, tool.TaskFailed)
	a.until(t, func() bool { return a.live() == 0 })
	receiveEnd(t, ends)

	assert.Equal(t, "boom", a.info(t, id).Reason)
	assert.Equal(t, []string{fmt.Sprintf("Agent %s (explorer) failed (boom): find it\nUndelivered messages:\nalso check b", id)}, receive(t, rootRequests).UserTexts)
	assert.Empty(t, childRequests, "a failed turn is not continued with the queued message")
	assert.Empty(t, rootRequests, "one end notice")
}

func TestMessageToFinishedChildContinuesItsContextUnderTheSameID(t *testing.T) {
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{}, agentDef("explorer"))
	childRequests := make(chan pigruntimetest.Request, 1)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("first"), pigruntimetest.Capture(t, childRequests, "second")})
	rootRequests := make(chan pigruntimetest.Request, 2)
	a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, rootRequests, "ack"), pigruntimetest.Capture(t, rootRequests, "ack")})
	id := a.launch(t, "explorer", "find it", "go")
	a.untilStatus(t, id, tool.TaskCompleted)
	first := receive(t, rootRequests)

	resumed, err := a.c.message(t.Context(), id, "again")
	require.NoError(t, err)
	assert.True(t, resumed)
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.Equal(t, []string{"go", "again"}, receive(t, childRequests).UserTexts, "the context is kept")
	assert.Equal(t, []string{fmt.Sprintf("Agent %s (explorer) completed: find it\nfirst", id)}, first.UserTexts)
	second := receive(t, rootRequests)
	assert.Contains(t, second.UserTexts, fmt.Sprintf("Agent %s (explorer) completed: find it\nsecond", id))
	assert.Len(t, a.tasks.List(), 1)
	assert.Equal(t, "second", a.info(t, id).Result)
	assert.Contains(t, a.info(t, id).Output, "› go\nfirst\n› again\nsecond")
}

func TestMessageToOtherTasksIsUnknown(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	a := newAgentHarness(t, sb, Config{}, agentDef("explorer"))
	shell, err := a.tasks.Start(t.Context(), tool.NewShell(sb.Workspace, nil), "true")
	require.NoError(t, err)

	for _, id := range []string{"t99", shell.ID} {
		_, err := a.c.message(t.Context(), id, "hello")
		assert.ErrorIs(t, err, tool.ErrUnknownTask, id)
	}
}

func TestRegistryStopQueuesNoNoticeButStopTaskQueuesANonWakingOne(t *testing.T) {
	started := make(chan struct{}, 2)
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{AgentTools: []tool.Tool{blockingTool(started)}}, agentDef("explorer"))
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "c1")), pigruntimetest.ToolCalls(call("Block", "c2"))})

	byModel := a.launch(t, "explorer", "first", "go")
	wait(t, started)
	_, err := a.tasks.Stop(byModel)
	require.NoError(t, err)
	a.until(t, func() bool { return a.live() == 0 })
	assert.Empty(t, a.queuedNotices(), "the model's own tool result covers it")

	byHuman := a.launch(t, "explorer", "second", "go")
	wait(t, started)
	require.NoError(t, a.c.StopTask(byHuman))
	a.until(t, func() bool { return a.live() == 0 })

	queued := a.queuedNotices()
	require.Len(t, queued, 1)
	assert.Equal(t, fmt.Sprintf("Agent %s (explorer) stopped by the user: second\n%s", byHuman, outputTail(a.info(t, byHuman).Output)), queued[0].Text)
	assert.True(t, a.idle())
	assert.Zero(t, a.faux.CallCount(), "a stop wakes nobody")
}

func TestRootInterruptLeavesChildrenRunning(t *testing.T) {
	rootStarted, childStarted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{Tools: []tool.Tool{blockTool(rootStarted)}, AgentTools: []tool.Tool{releaseTool(childStarted, release)}}, agentDef("explorer"))
	a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "r1"))})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "c1")), pigruntimetest.Text("done")})
	w, err := a.c.Submit(t.Context(), "go")
	require.NoError(t, err)
	wait(t, rootStarted)
	id := a.launch(t, "explorer", "find it", "go")
	wait(t, childStarted)

	require.True(t, a.c.Interrupt())
	wait(t, w.Done())

	assert.Equal(t, tool.TaskRunning, a.info(t, id).Status)
	close(release)
	a.untilStatus(t, id, tool.TaskCompleted)
	a.until(t, func() bool { return a.live() == 0 })
	assert.Len(t, a.queuedNotices(), 1, "after the interrupt the notice waits for the next prompt")
}

func TestSwitchAndCloseStopChildrenAndCloseTheirSessions(t *testing.T) {
	for _, transition := range []string{"new session", "close"} {
		t.Run(transition, func(t *testing.T) {
			started := make(chan struct{}, 1)
			a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{AgentTools: []tool.Tool{blockingTool(started)}}, agentDef("explorer"))
			a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "c1"))})
			id := a.launch(t, "explorer", "find it", "go")
			wait(t, started)
			ch := a.childOf(id)
			require.NotNil(t, ch)

			if transition == "close" {
				require.NoError(t, a.c.Close(t.Context()))
			} else {
				require.NoError(t, a.c.NewSession(t.Context()))
			}

			assert.Nil(t, a.childOf(id))
			assert.Zero(t, a.live())
			assert.Empty(t, a.tasks.List())
			assert.Empty(t, a.queuedNotices())
			_, err := ch.sess.Send(t.Context(), pigruntime.Prompt{Text: "still there?"}, nil, nil)
			assert.Error(t, err, "the child's session is closed")
		})
	}
}

// switchProbe begins a session switch when the context is first used, which
// is while the session is being opened for the child.
type switchProbe struct {
	context.Context
	once   sync.Once
	onUsed func()
}

func (p *switchProbe) use() { p.once.Do(p.onUsed) }

func (p *switchProbe) Done() <-chan struct{} { p.use(); return p.Context.Done() }

func (p *switchProbe) Err() error { p.use(); return p.Context.Err() }

func (p *switchProbe) Value(key any) any { p.use(); return p.Context.Value(key) }

func TestLaunchRacingASwitchLeavesNothingRunning(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	tasks := tool.NewTasks()
	h := newHarness(t, sb, Config{Agents: []subagent.Definition{agentDef("explorer")}, Tasks: tasks})
	child := ai.NewFauxProvider(ai.FauxConfig{ProviderID: childProvider, Model: "faux-1"})
	require.NoError(t, h.c.rt.RegisterProvider(child.Provider()))
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	ctx := &switchProbe{Context: t.Context(), onUsed: func() {
		h.c.mu.Lock()
		h.c.switching = &sessionSwitch{}
		h.c.mu.Unlock()
	}}

	_, err := h.c.launch(ctx, "explorer", "find it", "go")

	require.ErrorIs(t, err, ErrBusy)
	h.c.mu.Lock()
	h.c.switching = nil
	h.c.mu.Unlock()
	h.c.queue.Lock()
	assert.Empty(t, h.c.children)
	assert.Zero(t, h.c.live)
	h.c.queue.Unlock()
	assert.Empty(t, tasks.List(), "a child that did not start leaves no task behind")
}

func TestPlanPinnedChildIsRefusedWritesWhileOthersFollowTheMode(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	var ran atomic.Int32
	rules, err := policy.NewRules(sb.Workspace, sb.Home, map[string]policy.ToolSpec{"Exec": {}}, policy.Source{Name: "test", Allow: []string{"Exec"}})
	require.NoError(t, err)
	planner, worker := agentDef("planner"), agentDef("worker")
	planner.Plan = true
	a := newAgentHarness(t, sb, Config{AgentTools: []tool.Tool{execTool(&ran)}, Rules: rules}, planner, worker)
	a.c.EnterExecute()
	requests := make(chan pigruntimetest.Request, 1)
	a.rootAcks(2)

	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Exec", "c1")), pigruntimetest.Capture(t, requests, "done")})
	id := a.launch(t, "planner", "plan it", "go")
	a.untilStatus(t, id, tool.TaskCompleted)
	assert.Zero(t, ran.Load())
	require.Len(t, receive(t, requests).ToolResults, 1)

	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Exec", "c2")), pigruntimetest.Capture(t, requests, "done")})
	id = a.launch(t, "worker", "do it", "go")
	a.untilStatus(t, id, tool.TaskCompleted)
	assert.Equal(t, int32(1), ran.Load())
	assert.Equal(t, []string{execOutput}, receive(t, requests).ToolResults)
}

func TestRulesApplyToAChildsCalls(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	require.NoError(t, os.WriteFile(filepath.Join(sb.Workspace, "secret.txt"), []byte("alpha\n"), 0o600))
	ws, err := tool.NewWorkspace(sb.Workspace, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ws.Close()) })
	edit := tool.NewEdit(ws)
	rules, err := policy.NewRules(sb.Workspace, sb.Home, tool.Specs([]tool.Tool{edit}), policy.Source{Name: "test", Deny: []string{"Edit(secret.txt)"}})
	require.NoError(t, err)
	a := newAgentHarness(t, sb, Config{AgentTools: []tool.Tool{edit}, Rules: rules}, agentDef("worker"))
	a.c.EnterExecute()
	requests := make(chan pigruntimetest.Request, 1)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("c1", "secret.txt")), pigruntimetest.Capture(t, requests, "done")})
	a.rootAcks(1)

	id := a.launch(t, "worker", "edit it", "go")
	a.untilStatus(t, id, tool.TaskCompleted)

	results := receive(t, requests).ToolResults
	require.Len(t, results, 1)
	assert.Contains(t, results[0], "denied by rule")
	assert.Equal(t, "alpha\n", fileText(t, sb, "secret.txt"))
}

func TestChildClassifierRecordHoldsTheChildsTask(t *testing.T) {
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{Classifier: reviewing("", make(chan string, 1))}, agentDef("explorer"))
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("done")})
	a.rootAcks(1)

	id := a.launch(t, "explorer", "find it", "find the config loader")
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.Equal(t, classifier.Conversation{Task: "find the config loader"}, a.childOf(id).rec.Conversation(""))
}

func TestTaskOutputHasPromptToolAndTextLines(t *testing.T) {
	look := testTool("Look", policy.Read, func(context.Context, json.RawMessage) (string, error) { return "seen", nil })
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{AgentTools: []tool.Tool{look}}, agentDef("explorer"))
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Look", "c1")), pigruntimetest.Text("the answer")})
	a.rootAcks(1)

	id := a.launch(t, "explorer", "find it", "do it")
	a.untilStatus(t, id, tool.TaskCompleted)

	assert.Equal(t, "› do it\n• Look\nthe answer", a.info(t, id).Output)
}

// rootAcks scripts n answers for the root turns its children's notices wake.
func (a *agentHarness) rootAcks(n int) {
	steps := make([]ai.FauxResponseStep, n)
	for i := range steps {
		steps[i] = pigruntimetest.Text("ack")
	}
	a.faux.SetResponses(steps)
}

// blockingTool signals started on each call and blocks until its ctx is cancelled.
func blockingTool(started chan<- struct{}) tool.Tool {
	return testTool("Block", policy.Read, func(ctx context.Context, _ json.RawMessage) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	})
}

func TestPendingAcrossLaunchEndAndWake(t *testing.T) {
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{}, agentDef("explorer"))
	childGate, rootGate := make(chan struct{}), make(chan struct{})
	a.child.SetResponses([]ai.FauxResponseStep{gated(childGate, "done")})
	a.faux.SetResponses([]ai.FauxResponseStep{gated(rootGate, "woken")})
	ends := turnEnds(a.c)

	q := a.c.FollowWork()
	defer q.Close()
	w, live, _ := q.Next()
	assert.Nil(t, w)
	assert.False(t, live)

	a.launch(t, "explorer", "find it", "go")
	w, live, changed := q.Next()
	assert.Nil(t, w)
	assert.True(t, live)

	close(childGate)
	for live {
		select {
		case <-changed:
		case <-time.After(waitLimit):
			t.Fatal("timed out waiting for the child to end")
		}
		w, live, changed = q.Next()
	}
	require.NotNil(t, w, "no running child is never seen without the turn its notice woke")

	close(rootGate)
	wait(t, w.Done())
	receiveEnd(t, ends)
	w, live, _ = q.Next()
	assert.Nil(t, w)
	assert.False(t, live)
}

func TestParallelAgentCallsInOneMessageLaunchAndNotifyEach(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	a := newAgentHarness(t, sb, Config{Tools: []tool.Tool{readTool(t, sb)}}, agentDef("explorer"))
	agentCall := func(id, description string) pigruntimetest.Call {
		return pigruntimetest.Call{Name: "Agent", ID: id, Args: map[string]any{"description": description, "prompt": "go", "subagent_type": "explorer"}}
	}
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("the result"), pigruntimetest.Text("the result")})
	rootRequests := make(chan pigruntimetest.Request, 4)
	a.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(agentCall("a1", "one"), agentCall("a2", "two"), pigruntimetest.Call{Name: "Read", ID: "r1", Args: map[string]any{"file_path": "missing.txt"}}),
		pigruntimetest.Capture(t, rootRequests, "launched"),
		pigruntimetest.Capture(t, rootRequests, "ack"),
		pigruntimetest.Capture(t, rootRequests, "ack"),
	})

	w, err := a.c.Submit(t.Context(), "delegate")
	require.NoError(t, err)
	wait(t, w.Done())
	a.untilStatus(t, "t1", tool.TaskCompleted)
	a.untilStatus(t, "t2", tool.TaskCompleted)
	a.until(t, func() bool { return a.live() == 0 })

	assert.Len(t, a.tasks.List(), 2)
	var seen strings.Builder
	for !strings.Contains(seen.String(), "completed: one\nthe result") || !strings.Contains(seen.String(), "completed: two\nthe result") {
		seen.WriteString(strings.Join(receive(t, rootRequests).UserTexts, "\n"))
	}
	assert.Contains(t, seen.String(), "completed: one\nthe result")
	assert.Contains(t, seen.String(), "completed: two\nthe result")
}

func TestLoginIsRefusedWhileAChildRunsAndSubagentsCannotStartDuringOne(t *testing.T) {
	started := make(chan struct{}, 1)
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{AgentTools: []tool.Tool{blockingTool(started)}}, agentDef("explorer"))
	pigruntimetest.RegisterLoginProvider(t, a.c.rt, loginProvider)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "c1")), pigruntimetest.Text("done")})
	id := a.launch(t, "explorer", "find it", "go")
	wait(t, started)

	_, err := a.c.Submit(t.Context(), "/login "+loginProvider)
	require.ErrorIs(t, err, ErrWorkStillRunning)

	_, err = a.tasks.Stop(id)
	require.NoError(t, err)
	a.until(t, func() bool { return a.live() == 0 })
	_, err = a.c.Submit(t.Context(), "/login "+loginProvider)
	require.NoError(t, err, "a login works again once no child runs")

	_, err = a.c.launch(t.Context(), "explorer", "more", "go")
	assert.ErrorIs(t, err, ErrLoggingIn)
	_, err = a.c.message(t.Context(), id, "again")
	assert.ErrorIs(t, err, ErrLoggingIn)
	assert.Zero(t, a.live())
}

func TestEndNoticeIsNotQueuedIntoTheNextSession(t *testing.T) {
	started := make(chan struct{}, 1)
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{AgentTools: []tool.Tool{blockingTool(started)}}, agentDef("explorer"))
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "c1"))})
	id := a.launch(t, "explorer", "find it", "go")
	wait(t, started)
	ch := a.childOf(id)
	require.NotNil(t, ch)
	require.NoError(t, a.c.NewSession(t.Context()))

	// What runChild does when End reported before the switch forgot the task.
	for _, status := range []tool.TaskStatus{tool.TaskCompleted, tool.TaskFailed, tool.TaskStopped} {
		ch.userStopped = true
		a.c.queue.Lock()
		a.c.queueEndNoticeLocked(ch, tool.TaskInfo{ID: id, Agent: "explorer", Command: "find it", Status: status, Result: "late"}, nil)
		a.c.queue.Unlock()
	}

	assert.Empty(t, a.queuedNotices())
	assert.True(t, a.idle(), "nothing woke the new session")
}

// doneProbe reports when Done is first read.
type doneProbe struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (p *doneProbe) Done() <-chan struct{} {
	p.once.Do(func() { close(p.entered) })
	return p.Context.Done()
}

func TestMessageToACancelledRunIsNotLost(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{AgentTools: []tool.Tool{releaseTool(started, release)}}, agentDef("explorer"))
	requests := make(chan pigruntimetest.Request, 1)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "c1")), pigruntimetest.Capture(t, requests, "again done")})
	a.rootAcks(2)
	id := a.launch(t, "explorer", "find it", "go")
	wait(t, started)
	ch := a.childOf(id)
	a.c.queue.Lock()
	ch.cancel()
	a.c.queue.Unlock()

	// The run is stopping, so the message waits for it rather than joining the inbox.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	resumed, err := a.c.message(cancelled, id, "lost?")
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, resumed)
	a.c.queue.Lock()
	assert.Empty(t, ch.inbox)
	a.c.queue.Unlock()

	type result struct {
		resumed bool
		err     error
	}
	done := make(chan result, 1)
	waiting := &doneProbe{Context: t.Context(), entered: make(chan struct{})}
	go func() {
		r, err := a.c.message(waiting, id, "more work")
		done <- result{r, err}
	}()
	// Done is read as the message starts waiting for the run to end; the run
	// cannot end before release is closed.
	wait(t, waiting.entered)
	a.c.queue.Lock()
	assert.True(t, ch.running && ch.runCtx.Err() != nil)
	a.c.queue.Unlock()
	close(release)
	var res result
	select {
	case res = <-done:
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for the message")
	}
	require.NoError(t, res.err)
	assert.True(t, res.resumed, "the child runs again with the message")
	a.untilStatus(t, id, tool.TaskCompleted)
	assert.Contains(t, receive(t, requests).UserTexts, "more work")
}

func TestUnpinnedChildFollowsTheHumansModeMidRun(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	var ran atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	rules, err := policy.NewRules(sb.Workspace, sb.Home, map[string]policy.ToolSpec{"Exec": {}}, policy.Source{Name: "test", Allow: []string{"Exec"}})
	require.NoError(t, err)
	a := newAgentHarness(t, sb, Config{AgentTools: []tool.Tool{releaseTool(started, release), execTool(&ran)}, Rules: rules}, agentDef("worker"))
	a.c.EnterExecute()
	requests := make(chan pigruntimetest.Request, 2)
	a.rootAcks(2)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "h1")), pigruntimetest.ToolCalls(call("Exec", "e1")), pigruntimetest.Capture(t, requests, "done")})
	id := a.launch(t, "worker", "work", "go")
	wait(t, started)

	a.c.EnterPlan()
	close(release)
	a.untilStatus(t, id, tool.TaskCompleted)
	assert.Zero(t, ran.Load())
	assert.Contains(t, receive(t, requests).ToolResults[1], "needs execute mode")

	a.c.EnterExecute()
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Exec", "e2")), pigruntimetest.Capture(t, requests, "done")})
	id = a.launch(t, "worker", "work again", "go")
	a.untilStatus(t, id, tool.TaskCompleted)
	assert.Equal(t, int32(1), ran.Load())
}

func TestRootEditsGrantCoversAnUnpinnedChildButNotAPlanPinnedOne(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(sb.Workspace, name), []byte("alpha\n"), 0o600))
	}
	ws, err := tool.NewWorkspace(sb.Workspace, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ws.Close()) })
	edits := []tool.Tool{tool.NewEdit(ws), tool.NewWrite(ws)}
	planner, worker := agentDef("planner"), agentDef("worker")
	planner.Plan = true
	a := newAgentHarness(t, sb, Config{Tools: edits, AgentTools: edits}, planner, worker)
	a.c.EnterExecute()
	approvals := subscribeTo[event.ApprovalRequested](a.c)
	a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("r1", "a.txt")), pigruntimetest.Text("granted"), pigruntimetest.Text("ack"), pigruntimetest.Text("ack")})
	w, err := a.c.Submit(t.Context(), "edit a")
	require.NoError(t, err)
	req := receiveEvent(t, approvals)
	require.True(t, a.c.Answer(req.Token, policy.Decision{Answer: policy.AllowEdits}))
	wait(t, w.Done())

	requests := make(chan pigruntimetest.Request, 1)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("p1", "b.txt")), pigruntimetest.Capture(t, requests, "done")})
	id := a.launch(t, "planner", "plan", "go")
	a.untilStatus(t, id, tool.TaskCompleted)
	results := receive(t, requests).ToolResults
	require.Len(t, results, 1)
	assert.Contains(t, results[0], "needs execute mode")
	assert.Equal(t, "alpha\n", fileText(t, sb, "b.txt"))

	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("w1", "c.txt")), pigruntimetest.Text("done")})
	id = a.launch(t, "worker", "work", "go")
	a.untilStatus(t, id, tool.TaskCompleted)
	assert.Equal(t, "beta\n", fileText(t, sb, "c.txt"))
	assert.Len(t, ofType[event.ApprovalRequested](a.rec.all()), 1, "no second ask for the unpinned child")
}

func TestDelayedChildStopDoesNotCancelReplacement(t *testing.T) {
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{}, agentDef("explorer"))
	firstGate, secondGate := make(chan struct{}), make(chan struct{})
	requests := make(chan childRequest, 2)
	a.child.SetResponses([]ai.FauxResponseStep{captureChild(requests, firstGate, "first"), captureChild(requests, secondGate, "second")})
	a.rootAcks(2)
	id := a.launch(t, "explorer", "find it", "go")
	receiveChild(t, requests)
	ch := a.childOf(id)
	selectedStop := ch.cancel
	close(firstGate)
	a.until(t, func() bool { return a.live() == 0 })
	resumed, err := a.c.message(t.Context(), id, "again")
	require.NoError(t, err)
	require.True(t, resumed)
	receiveChild(t, requests)
	selectedStop()
	a.c.queue.Lock()
	assert.NoError(t, ch.runCtx.Err(), "a delayed stop belongs to the old run")
	a.c.queue.Unlock()
	close(secondGate)
	a.untilStatus(t, id, tool.TaskCompleted)
	assert.Equal(t, "second", a.info(t, id).Result)
}
