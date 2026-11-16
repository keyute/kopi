package pigruntime

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

const execOutput = "exec-output-91c2"

func callTool(name, id string) ai.FauxResponseStep {
	return ai.FauxAssistantMessage(
		ai.FauxContentBlocks{ai.FauxToolCall(name, map[string]any{}, &ai.FauxToolCallOptions{ID: id})},
		ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)},
	)
}

func execTool(ran *atomic.Int32) tool.Tool {
	return tool.Tool{
		Name:        "Exec",
		Label:       "Exec",
		Description: "Test-only tool that needs execute mode.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		Capability:  policy.Exec,
		Run: func(context.Context, json.RawMessage) (string, error) {
			ran.Add(1)
			return execOutput, nil
		},
	}
}

func allowAll(context.Context, policy.Request) (policy.Answer, error) { return policy.Allow, nil }

func lastToolResult(t *testing.T, msgs []ai.Message) string {
	t.Helper()
	texts := toolResultTexts(msgs)
	require.NotEmpty(t, texts)
	return texts[len(texts)-1]
}

func TestHookBlocksToolUnknownToGate(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "ok")})

	var ran atomic.Bool
	probe := probeTool(func(context.Context, json.RawMessage) (string, error) {
		ran.Store(true)
		return toolOutput, nil
	})
	authority := policy.NewAuthority()
	sess, err := r.open(t.Context(), OpenOptions{Model: fauxModel, Tools: []tool.Tool{probe}, Authority: authority}, policy.NewGate(authority, nil, nil, nil))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	_, err = sess.Send(t.Context(), Prompt{Text: "probe"}, nil)
	require.NoError(t, err)
	assert.Contains(t, lastToolResult(t, <-requests), policy.ErrUnknownTool.Error())
	assert.False(t, ran.Load())
}

func TestExecToolRunsOnlyInExecuteMode(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{
		callTool("Exec", "call-plan"), captureRequest(requests, "denied"),
		callTool("Exec", "call-exec"), captureRequest(requests, "ran"),
	})

	var ran atomic.Int32
	authority := policy.NewAuthority()
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, Tools: []tool.Tool{execTool(&ran)}, Authority: authority, Approve: allowAll})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	_, err = sess.Send(t.Context(), Prompt{Text: "run it"}, nil)
	require.NoError(t, err)
	denied := lastToolResult(t, <-requests)
	assert.Contains(t, denied, policy.ErrDenied.Error())
	assert.Contains(t, denied, "plan mode")
	assert.Zero(t, ran.Load())

	authority.EnterExecute()
	_, err = sess.Send(t.Context(), Prompt{Text: "run it now"}, nil)
	require.NoError(t, err)
	assert.Equal(t, execOutput, lastToolResult(t, <-requests))
	assert.Equal(t, int32(1), ran.Load())
}

func TestExecuteRefusesCallRevokedAfterBefore(t *testing.T) {
	var ran atomic.Int32
	authority := policy.NewAuthority()
	authority.EnterExecute()
	gate := policy.NewGate(authority, map[string]policy.ToolSpec{"Exec": {Capability: policy.Exec}}, nil, allowAll)
	require.NoError(t, gate.Before(t.Context(), "call-1", "Exec", json.RawMessage(`{}`)))
	authority.EnterPlan()

	res, err := agentTool{tool: execTool(&ran), gate: gate}.Execute(t.Context(), "call-1", json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, toolResultContentText(res.Content), policy.ErrRevoked.Error())
	assert.Zero(t, ran.Load())
}

func TestHookPassesArgsToGate(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	call := ai.FauxAssistantMessage(
		ai.FauxContentBlocks{ai.FauxToolCall("Exec", map[string]any{"path": "a.txt"}, &ai.FauxToolCallOptions{ID: "call-1"})},
		ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)},
	)
	faux.SetResponses([]ai.FauxResponseStep{call, captureRequest(requests, "ran")})

	var ran atomic.Int32
	asked := make(chan policy.Request, 1)
	authority := policy.NewAuthority()
	authority.EnterExecute()
	sess, err := r.Open(t.Context(), OpenOptions{
		Model: fauxModel, Tools: []tool.Tool{execTool(&ran)}, Authority: authority,
		Approve: func(_ context.Context, req policy.Request) (policy.Answer, error) {
			asked <- req
			return policy.Allow, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	_, err = sess.Send(t.Context(), Prompt{Text: "run it"}, nil)
	require.NoError(t, err)
	req := <-asked
	assert.Equal(t, "call-1", req.CallID)
	assert.Equal(t, "Exec", req.Tool)
	assert.JSONEq(t, `{"path":"a.txt"}`, string(req.Args))
	assert.Equal(t, execOutput, lastToolResult(t, <-requests))
}

func TestRefusalTextReachesModel(t *testing.T) {
	cases := []struct {
		name    string
		opts    func(t *testing.T) OpenOptions
		reasons []string
	}{
		{
			name: "rejected",
			opts: func(*testing.T) OpenOptions {
				return OpenOptions{Approve: func(context.Context, policy.Request) (policy.Answer, error) { return policy.Reject, nil }}
			},
			reasons: []string{policy.ErrRejected.Error(), "stop and wait for their instructions"},
		},
		{
			name: "denied by rule",
			opts: func(t *testing.T) OpenOptions {
				rules, err := policy.NewRules(t.TempDir(), t.TempDir(), []string{"Exec"}, policy.Source{Name: "config", Deny: []string{"Exec"}})
				require.NoError(t, err)
				return OpenOptions{Rules: rules, Approve: allowAll}
			},
			reasons: []string{policy.ErrDenied.Error(), "Exec (config)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := newSandbox(t)
			r, faux := newTestRuntime(t, sb)
			requests := make(chan []ai.Message, 1)
			faux.SetResponses([]ai.FauxResponseStep{callTool("Exec", "call-1"), captureRequest(requests, "refused")})

			var ran atomic.Int32
			o := tc.opts(t)
			o.Model, o.Tools, o.Authority = fauxModel, []tool.Tool{execTool(&ran)}, policy.NewAuthority()
			o.Authority.EnterExecute()
			sess, err := r.Open(t.Context(), o)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

			_, err = sess.Send(t.Context(), Prompt{Text: "run it"}, nil)
			require.NoError(t, err)
			result := lastToolResult(t, <-requests)
			for _, reason := range tc.reasons {
				assert.Contains(t, result, reason)
			}
			assert.Zero(t, ran.Load())
		})
	}
}

func TestCancellingSendUnblocksPendingApproval(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{callTool("Exec", "call-1"), reply("should not be requested")})

	var ran atomic.Int32
	waiting, returned := make(chan struct{}), make(chan struct{})
	authority := policy.NewAuthority()
	authority.EnterExecute()
	sess, err := r.Open(t.Context(), OpenOptions{
		Model: fauxModel, Tools: []tool.Tool{execTool(&ran)}, Authority: authority,
		Approve: func(ctx context.Context, _ policy.Request) (policy.Answer, error) {
			defer close(returned)
			close(waiting)
			<-ctx.Done()
			return policy.Reject, ctx.Err()
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	turnCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := sess.Send(turnCtx, Prompt{Text: "run it"}, nil)
		done <- err
	}()
	<-waiting
	cancel()
	<-returned
	require.NoError(t, <-done)
	assert.Zero(t, ran.Load())
	assert.Equal(t, 1, faux.CallCount())
}

// gatedAbortContext preserves cancellation but holds its AfterFunc forwarding.
// Value must not expose the forwarding context's private cancellation state.
type gatedAbortContext struct {
	context.Context
	forward context.Context
	started chan struct{}
	release chan struct{}
}

func (c *gatedAbortContext) Done() <-chan struct{} { return c.forward.Done() }
func (c *gatedAbortContext) Err() error            { return c.forward.Err() }
func (c *gatedAbortContext) AfterFunc(f func()) func() bool {
	return context.AfterFunc(c.forward, func() {
		close(c.started)
		<-c.release
		f()
	})
}

func TestInterruptTurnRejectsBeforeCancellationForwarding(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{
		ai.FauxAssistantMessage(ai.FauxContentBlocks{
			ai.FauxToolCall("Exec", map[string]any{}, &ai.FauxToolCallOptions{ID: "reject"}),
			ai.FauxToolCall("Probe", map[string]any{}, &ai.FauxToolCallOptions{ID: "allowed"}),
		}, ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)}),
		reply("unreached"),
	})
	var ran atomic.Int32
	hooks := make(chan context.Context, 1)
	reject := make(chan struct{})
	authority := policy.NewAuthority()
	authority.EnterExecute()
	rules, err := policy.NewRules(sb.ws, sb.kopiHome, []string{"Exec", "Probe"}, policy.Source{Name: "test", Allow: []string{"Probe"}})
	require.NoError(t, err)
	sess, err := r.Open(t.Context(), OpenOptions{
		Model: fauxModel, Authority: authority, Rules: rules,
		Tools: []tool.Tool{execTool(&ran), probeTool(func(context.Context, json.RawMessage) (string, error) {
			ran.Add(1)
			return toolOutput, nil
		})},
		Approve: func(ctx context.Context, _ policy.Request) (policy.Answer, error) {
			hooks <- ctx
			<-reject
			return policy.Reject, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	runEnded := make(chan struct{}, 1)
	t.Cleanup(sess.inner.Subscribe(func(ev agent.AgentEvent) {
		if _, ok := ev.(agent.AgentEndEvent); ok {
			runEnded <- struct{}{}
		}
	}))
	forward, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &gatedAbortContext{Context: context.WithoutCancel(t.Context()), forward: forward, started: make(chan struct{}), release: make(chan struct{})}
	type result struct {
		res SendResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := sess.Send(ctx, Prompt{Text: "run"}, nil)
		done <- result{res, err}
	}()
	hook := <-hooks
	cancel()
	<-ctx.started
	require.True(t, InterruptTurn(hook))
	assert.ErrorIs(t, hook.Err(), context.Canceled)
	close(reject)
	<-runEnded
	// Send must still join the held forwarding after the run finishes.
	close(ctx.release)
	first := <-done
	require.NoError(t, first.err)
	assert.Equal(t, event.Interrupted, first.res.Outcome.Status)
	assert.Zero(t, ran.Load())
	assert.Equal(t, 1, faux.CallCount())

	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	started, release := make(chan struct{}), make(chan struct{})
	// The next tool approval is rule-allowed; hold its tool execution instead.
	t.Cleanup(sess.inner.Subscribe(func(ev agent.AgentEvent) {
		if _, ok := ev.(agent.AgentStartEvent); ok {
			close(started)
			<-release
		}
	}))
	go func() {
		res, err := sess.Send(t.Context(), Prompt{Text: "next"}, nil)
		done <- result{res, err}
	}()
	<-started
	assert.False(t, InterruptTurn(hook), "retired hook must not abort the new Send")
	assert.False(t, InterruptTurn(t.Context()))
	close(release)
	next := <-done
	require.NoError(t, next.err)
	assert.Equal(t, event.Completed, next.res.Outcome.Status)
	assert.Equal(t, int32(1), ran.Load())
	assert.Contains(t, toolResultTexts(<-requests)[0], policy.ErrRejected.Error())
}
