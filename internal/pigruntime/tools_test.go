package pigruntime

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

const deferredOutput = "deferred-output-5e1b"

func readTool(name string, run func(ctx context.Context) (string, error)) tool.Tool {
	return tool.Tool{
		Name:        name,
		Label:       name,
		Description: "Test-only read tool.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		Capability:  policy.Read,
		Run: func(ctx context.Context, _ json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (tool.Result, error) {
			if _, err := admit(policy.Target{}); err != nil {
				return tool.Result{}, err
			}
			out, err := run(ctx)
			return tool.Result{Text: out}, err
		},
	}
}

func loaderTool() tool.Tool {
	return readTool("Load", func(ctx context.Context) (string, error) {
		return "loaded", LoadTools(ctx, []string{"Deferred"})
	})
}

func deferredTool(ran *atomic.Bool) tool.Tool {
	t := readTool("Deferred", func(context.Context) (string, error) {
		ran.Store(true)
		return deferredOutput, nil
	})
	t.Deferred = true
	return t
}

// captureAndCall sends the request it received to requests and answers with
// one call to name.
func captureAndCall(requests chan<- []ai.Message, name, id string) ai.FauxResponseStep {
	return ai.FauxFactoryStep(func(tc ai.TranscriptContext, _ ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
		requests <- tc.Messages()
		return ai.FauxAssistantMessage(
			ai.FauxContentBlocks{ai.FauxToolCall(name, map[string]any{}, &ai.FauxToolCallOptions{ID: id})},
			ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)},
		), nil
	})
}

func declared(msgs []ai.Message) []string { return toolSchemaNames(ai.GetCurrentTools(msgs)) }

func TestDeferredToolIsDeclaredOnlyAfterLoad(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 3)
	faux.SetResponses([]ai.FauxResponseStep{
		captureAndCall(requests, "Load", "call-1"),
		captureAndCall(requests, "Deferred", "call-2"),
		captureRequest(requests, "done"),
	})

	var ran atomic.Bool
	sess := openTestSession(t, r, loaderTool(), deferredTool(&ran))

	res, err := sess.Send(t.Context(), Prompt{Text: "load it"}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "done", res.Reply)
	assert.Equal(t, []string{"Load"}, declared(<-requests))
	assert.ElementsMatch(t, []string{"Load", "Deferred"}, declared(<-requests))
	assert.Equal(t, deferredOutput, lastToolResult(t, <-requests))
	assert.True(t, ran.Load())
}

func TestCallToDeferredToolBeforeLoadIsNotFound(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callTool("Deferred", "call-1"), captureRequest(requests, "ok")})

	var ran atomic.Bool
	deferred := deferredTool(&ran)
	deferred.Ask = true
	asked := make(chan policy.Request, 1)
	sess, err := r.Open(t.Context(), OpenOptions{
		Model: fauxModel, Tools: []tool.Tool{loaderTool(), deferred}, Authority: policy.NewAuthority(),
		Approve: func(_ context.Context, req policy.Request) (policy.Decision, error) {
			asked <- req
			return policy.Decision{Answer: policy.Allow}, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	_, err = sess.Send(t.Context(), Prompt{Text: "call it"}, nil, nil)
	require.NoError(t, err)
	assert.Contains(t, lastToolResult(t, <-requests), "not found")
	assert.False(t, ran.Load())
	assert.Empty(t, asked)
}

func TestLoadToolsConcurrentlyPreservesActiveTools(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 2)
	faux.SetResponses([]ai.FauxResponseStep{
		captureAndCall(requests, "Load", "call-1"),
		captureRequest(requests, "done"),
	})

	loadErrors := make(chan error, 2)
	loader := readTool("Load", func(ctx context.Context) (string, error) {
		ready := make(chan struct{}, 2)
		start := make(chan struct{})
		var loaders sync.WaitGroup
		for _, name := range []string{"DeferredA", "DeferredB"} {
			loaders.Add(1)
			go func() {
				defer loaders.Done()
				ready <- struct{}{}
				<-start
				loadErrors <- LoadTools(ctx, []string{name})
			}()
		}
		<-ready
		<-ready
		close(start)
		loaders.Wait()
		return "loaded", nil
	})
	tools := []tool.Tool{loader}
	for _, name := range []string{"Original", "DeferredA", "DeferredB"} {
		candidate := readTool(name, func(context.Context) (string, error) { return "ok", nil })
		candidate.Deferred = name != "Original"
		tools = append(tools, candidate)
	}
	sess := openTestSession(t, r, tools...)

	res, err := sess.Send(t.Context(), Prompt{Text: "load both"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "done", res.Reply)
	assert.NoError(t, <-loadErrors)
	assert.NoError(t, <-loadErrors)
	assert.ElementsMatch(t, []string{"Load", "Original"}, declared(<-requests))
	assert.ElementsMatch(t, []string{"Load", "Original", "DeferredA", "DeferredB"}, declared(<-requests))
}

func TestLoadToolsOutsideTurnFails(t *testing.T) {
	assert.ErrorIs(t, LoadTools(context.Background(), []string{"Deferred"}), ErrNoTurn)
}

func TestRankTools(t *testing.T) {
	tools := []tool.Tool{
		{Name: "Read", Description: "Read a file from the workspace."},
		{Name: "Grep", Description: "Search file contents with a regular expression."},
		{Name: "WebFetch", Description: "Fetch a URL over HTTP."},
	}
	assert.Equal(t, "Grep", RankTools("Grep", tools, 3)[0])
	assert.Equal(t, []string{"Grep"}, RankTools("regular", tools, 3))
	assert.Len(t, RankTools("file", tools, 1), 1)
	assert.Empty(t, RankTools("zebra", tools, 3))
}

func TestResumedSessionStartsWithDeferredToolsUnloaded(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callTool("Load", "call-1"), captureRequest(requests, "loaded")})

	var ran atomic.Bool
	tools := []tool.Tool{loaderTool(), deferredTool(&ran)}
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: kopiPrompt, Tools: tools, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	_, err = sess.Send(t.Context(), Prompt{Text: "load it"}, nil, nil)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"Load", "Deferred"}, declared(<-requests))
	require.NoError(t, sess.Close(t.Context()))

	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "again")})
	_, err = resume(t, r, sess.ID(), tools...).Send(t.Context(), Prompt{Text: "again"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"Load"}, declared(<-requests))
}

func TestPromptToolsDeclareTheToolInTheSameRequest(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 2)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "with"), captureRequest(requests, "without")})

	var ran atomic.Bool
	sess := openTestSession(t, r, deferredTool(&ran))

	_, err := sess.Send(t.Context(), Prompt{Text: "a", Tools: []string{"Deferred", "Missing"}}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"Deferred"}, declared(<-requests))

	// The tool stays active in the session, so a fresh session is the one without it.
	sess = openTestSession(t, r, deferredTool(&ran))
	_, err = sess.Send(t.Context(), Prompt{Text: "b"}, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, declared(<-requests))
}

func TestHandedOverPromptToolsDeclareTheToolInTheNextRequest(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})

	var ran atomic.Bool
	sess := openTestSession(t, r, probeTool(okProbe), deferredTool(&ran))
	more, _ := moreOnce(Prompt{Text: "x", Tools: []string{"Deferred"}})

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
	require.NoError(t, err)
	require.Equal(t, "done", res.Reply)
	assert.ElementsMatch(t, []string{"Probe", "Deferred"}, declared(<-requests))
}

func TestUnknownToolsDoNotReportToolsChanged(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)

	var ran atomic.Bool
	sess := openTestSession(t, r, deferredTool(&ran))
	changed := 0
	t.Cleanup(sess.Subscribe(func(ev event.Event) {
		if _, ok := ev.(event.ToolsChanged); ok {
			changed++
		}
	}))

	sess.loadTools([]string{"Missing"})
	assert.False(t, sess.Callable("Missing"))
	assert.False(t, sess.Callable("Deferred"))
	assert.Zero(t, changed)

	sess.loadTools([]string{"Deferred", "Missing"})
	assert.True(t, sess.Callable("Deferred"))
	assert.False(t, sess.Callable("Missing"))
	assert.Equal(t, 1, changed)

	sess.loadTools([]string{"Deferred", "Missing"})
	sess.loadTools([]string{"Missing"})
	assert.False(t, sess.Callable("Missing"))
	assert.Equal(t, 1, changed, "repeat loads add nothing")
}

func TestCallableAndToolsChangedFollowLoadedTools(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 2)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "a"), captureRequest(requests, "b")})

	var ran atomic.Bool
	sess := openTestSession(t, r, deferredTool(&ran))
	changed := 0
	t.Cleanup(sess.Subscribe(func(ev event.Event) {
		if _, ok := ev.(event.ToolsChanged); ok {
			changed++
		}
	}))
	require.False(t, sess.Callable("Deferred"))

	_, err := sess.Send(t.Context(), Prompt{Text: "a", Tools: []string{"Deferred"}}, nil, nil)
	require.NoError(t, err)
	assert.True(t, sess.Callable("Deferred"))
	assert.Equal(t, 1, changed)

	_, err = sess.Send(t.Context(), Prompt{Text: "b", Tools: []string{"Deferred"}}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, changed, "a repeat load adds nothing")
}

func TestSetCallableTurnsAToolOnAndOffAndReportsToolsChanged(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "done")})

	var ran atomic.Bool
	sess := openTestSession(t, r, deferredTool(&ran), readTool("Plain", func(context.Context) (string, error) { return "", nil }))
	changed := 0
	t.Cleanup(sess.Subscribe(func(ev event.Event) {
		if _, ok := ev.(event.ToolsChanged); ok {
			changed++
		}
	}))
	require.False(t, sess.Callable("Deferred"))

	sess.SetCallable("Deferred", true)
	assert.True(t, sess.Callable("Deferred"))
	assert.Equal(t, 1, changed)

	sess.SetCallable("Deferred", true)
	assert.Equal(t, 1, changed, "turning on a callable tool reports nothing")

	sess.SetCallable("Deferred", false)
	assert.False(t, sess.Callable("Deferred"))
	assert.True(t, sess.Callable("Plain"))
	assert.Equal(t, 2, changed)

	sess.SetCallable("Missing", true)
	sess.SetCallable("Missing", false)
	assert.False(t, sess.Callable("Missing"))
	assert.Equal(t, 2, changed, "a name the session lacks changes nothing")

	sess.SetCallable("Deferred", true)
	assert.Equal(t, 3, changed)
	_, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"Plain", "Deferred"}, declared(<-requests))
}

const joiningName = "mcp__late__echo"

// joiningTool is a tool of the server "late", which a session gets after it
// opens.
func joiningTool(ran *atomic.Bool) tool.Tool {
	t := readTool(joiningName, func(context.Context) (string, error) {
		ran.Store(true)
		return deferredOutput, nil
	})
	t.Deferred = true
	return t
}

func joiningLoader() tool.Tool {
	return readTool("Load", func(ctx context.Context) (string, error) {
		return "loaded", LoadTools(ctx, []string{joiningName})
	})
}

func TestAddedToolIsCallableOnceLoadedAndRunsThroughTheGate(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 4)
	asked := make(chan policy.Request, 1)
	sess, err := r.Open(t.Context(), OpenOptions{
		Model: fauxModel, SystemPrompt: kopiPrompt, Tools: []tool.Tool{joiningLoader()}, Joining: []string{"late"}, Authority: policy.NewAuthority(),
		Approve: func(_ context.Context, req policy.Request) (policy.Decision, error) {
			asked <- req
			return policy.Decision{Answer: policy.Allow}, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	changed := 0
	t.Cleanup(sess.Subscribe(func(ev event.Event) {
		if _, ok := ev.(event.ToolsChanged); ok {
			changed++
		}
	}))

	faux.SetResponses([]ai.FauxResponseStep{callTool(joiningName, "call-1"), captureRequest(requests, "ok")})
	_, err = sess.Send(t.Context(), Prompt{Text: "call it"}, nil, nil)
	require.NoError(t, err)
	assert.Contains(t, lastToolResult(t, <-requests), "not found", "the session does not have the tool yet")

	var ran atomic.Bool
	joining := joiningTool(&ran)
	joining.Ask = true
	require.NoError(t, sess.AddTools([]tool.Tool{joining}))
	assert.Equal(t, 1, changed)
	assert.False(t, sess.Callable(joiningName))

	faux.SetResponses([]ai.FauxResponseStep{
		captureAndCall(requests, "Load", "call-2"),
		captureAndCall(requests, joiningName, "call-3"),
		captureRequest(requests, "done"),
	})
	_, err = sess.Send(t.Context(), Prompt{Text: "load it and call it"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"Load"}, declared(<-requests), "an added tool is not declared until it is loaded")
	assert.ElementsMatch(t, []string{"Load", joiningName}, declared(<-requests))
	assert.Equal(t, deferredOutput, lastToolResult(t, <-requests))
	assert.True(t, ran.Load())
	assert.Equal(t, joiningName, (<-asked).Tool)
}

func TestAddedExecToolIsRefusedInPlanMode(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 4)
	sess, err := r.Open(t.Context(), OpenOptions{
		Model: fauxModel, SystemPrompt: kopiPrompt, Tools: []tool.Tool{joiningLoader()}, Joining: []string{"late"}, Authority: policy.NewAuthority(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	var ran atomic.Bool
	joining := joiningTool(&ran)
	joining.Capability = policy.Exec
	require.NoError(t, sess.AddTools([]tool.Tool{joining}))

	faux.SetResponses([]ai.FauxResponseStep{
		captureAndCall(requests, "Load", "call-1"),
		captureAndCall(requests, joiningName, "call-2"),
		captureRequest(requests, "done"),
	})
	_, err = sess.Send(t.Context(), Prompt{Text: "load it and call it"}, nil, nil)
	require.NoError(t, err)
	<-requests
	assert.ElementsMatch(t, []string{"Load", joiningName}, declared(<-requests))
	assert.Contains(t, lastToolResult(t, <-requests), "needs execute mode")
	assert.False(t, ran.Load())
}

func TestAddToolsFailsOnAChildSession(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	root := openTestSession(t, r)
	child, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, Parent: root, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, child.Close(context.Background())) })

	var ran atomic.Bool
	assert.ErrorIs(t, child.AddTools([]tool.Tool{joiningTool(&ran)}), ErrNoJoining)
}

func TestChildSessionLoadsAndCallsItsDeferredTool(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 3)
	root := openTestSession(t, r)
	var ran atomic.Bool
	child, err := r.Open(t.Context(), OpenOptions{
		Model: fauxModel, Parent: root, SystemPrompt: kopiPrompt, Tools: []tool.Tool{loaderTool(), deferredTool(&ran)}, Authority: policy.NewAuthority(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, child.Close(context.Background())) })

	faux.SetResponses([]ai.FauxResponseStep{
		captureAndCall(requests, "Load", "call-1"),
		captureAndCall(requests, "Deferred", "call-2"),
		captureRequest(requests, "done"),
	})
	_, err = child.Send(t.Context(), Prompt{Text: "load it and call it"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"Load"}, declared(<-requests))
	assert.ElementsMatch(t, []string{"Load", "Deferred"}, declared(<-requests))
	assert.Equal(t, deferredOutput, lastToolResult(t, <-requests))
	assert.True(t, ran.Load())
}

func TestAddToolsLeavesTheOtherToolsAsTheyWere(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	var ran atomic.Bool
	tools := []tool.Tool{readTool("Plain", func(context.Context) (string, error) { return "", nil }), deferredTool(&ran)}
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: kopiPrompt, Tools: tools, Joining: []string{"late"}, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	require.NoError(t, sess.AddTools([]tool.Tool{joiningTool(&ran)}))
	assert.True(t, sess.Callable("Plain"))
	assert.False(t, sess.Callable("Deferred"), "a tool that was not callable stays so")
	assert.False(t, sess.Callable(joiningName))

	sess.SetCallable("Deferred", true)
	sess.SetCallable("Deferred", false)
	other := joiningTool(&ran)
	other.Name = "mcp__late__other"
	require.NoError(t, sess.AddTools([]tool.Tool{other}))
	assert.False(t, sess.Callable("Deferred"), "a tool that was withdrawn stays so")
	assert.True(t, sess.Callable("Plain"))
}

func TestToolAddedToAResumedSessionIsNotCallableUntilLoadedAgain(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 3)
	var ran atomic.Bool
	open := func(r *Runtime, resume string) *Session {
		sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, ResumeID: resume, SystemPrompt: kopiPrompt, Tools: []tool.Tool{joiningLoader()}, Joining: []string{"late"}, Authority: policy.NewAuthority()})
		require.NoError(t, err)
		return sess
	}
	sess := open(r, "")
	require.NoError(t, sess.AddTools([]tool.Tool{joiningTool(&ran)}))
	faux.SetResponses([]ai.FauxResponseStep{callTool("Load", "call-1"), captureRequest(requests, "loaded")})
	_, err := sess.Send(t.Context(), Prompt{Text: "load it"}, nil, nil)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"Load", joiningName}, declared(<-requests))
	require.NoError(t, sess.Close(t.Context()))

	// A new process has a runtime whose extension holds no tool yet. PiG keeps
	// the tools a transcript last declared pending until they are registered
	// (PiG v0.8.0 coding/session_tools.go restoreToolsFromTranscript).
	r, faux = newTestRuntime(t, sb)
	resumed := open(r, sess.ID())
	t.Cleanup(func() { assert.NoError(t, resumed.Close(context.Background())) })
	require.NoError(t, resumed.AddTools([]tool.Tool{joiningTool(&ran)}))
	assert.False(t, resumed.Callable(joiningName))
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "again")})
	_, err = resumed.Send(t.Context(), Prompt{Text: "again"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"Load"}, declared(<-requests))
}
