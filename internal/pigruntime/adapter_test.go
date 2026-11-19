package pigruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest/mcpfake"
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
		Run: func(_ context.Context, _ json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (tool.Result, error) {
			if _, err := admit(policy.Target{}); err != nil {
				return tool.Result{}, err
			}
			ran.Add(1)
			return tool.Result{Text: execOutput}, nil
		},
	}
}

func allowAll(context.Context, policy.Request) (policy.Decision, error) {
	return policy.Decision{Answer: policy.Allow}, nil
}

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
	sess, err := r.open(t.Context(), OpenOptions{Model: fauxModel, Tools: []tool.Tool{probe}, Authority: authority}, policy.NewGate(authority, nil, nil, nil, nil))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	_, err = sess.Send(t.Context(), Prompt{Text: "probe"}, nil, nil)
	require.NoError(t, err)
	assert.Contains(t, lastToolResult(t, <-requests), policy.ErrUnknownTool.Error())
	assert.False(t, ran.Load())
}

func TestToolResultCarriesImages(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "seen")})

	probe := probeTool(nil)
	probe.Run = func(_ context.Context, _ json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (tool.Result, error) {
		if _, err := admit(policy.Target{}); err != nil {
			return tool.Result{}, err
		}
		return tool.Result{Text: "img", Images: []tool.Image{{MimeType: "image/png", Data: buf.Bytes()}}}, nil
	}
	sess := openTestSession(t, r, probe)

	_, err := sess.Send(t.Context(), Prompt{Text: "look"}, nil, nil)
	require.NoError(t, err)
	content := toolResultContent(<-requests)
	require.Len(t, content, 2)
	assert.Equal(t, ai.TextContent{Text: "img"}, content[0])
	img, ok := content[1].(ai.ImageContent)
	require.True(t, ok, "%T", content[1])
	assert.Equal(t, "image/png", img.MimeType)
	assert.Equal(t, base64.StdEncoding.EncodeToString(buf.Bytes()), img.Data)
}

func TestToolResultKeepsOversizedImageForPiGToResize(t *testing.T) {
	data := widePNG(t)
	probe := probeTool(nil)
	probe.Run = func(_ context.Context, _ json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (tool.Result, error) {
		if _, err := admit(policy.Target{}); err != nil {
			return tool.Result{}, err
		}
		return tool.Result{Text: "wide image", Images: []tool.Image{{MimeType: "image/png", Data: data}}}, nil
	}
	gate := policy.NewGate(policy.NewAuthority(), tool.Specs([]tool.Tool{probe}), nil, nil, nil)
	args := json.RawMessage(`{}`)
	require.NoError(t, gate.Before(t.Context(), "call-1", probe.Name, args))

	res, err := (agentTool{tool: probe, gate: gate}).Execute(t.Context(), "call-1", args, nil)
	require.NoError(t, err)
	require.False(t, res.IsError, toolResultContentText(res.Content))
	require.Len(t, res.Content, 2)
	assert.Equal(t, ai.TextContent{Text: "wide image"}, res.Content[0])
	img, ok := res.Content[1].(ai.ImageContent)
	require.True(t, ok, "%T", res.Content[1])
	assert.Equal(t, base64.StdEncoding.EncodeToString(data), img.Data)
	assert.Equal(t, "image/png", img.MimeType)
}

func TestToolResultOmitsUnprocessableImage(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "seen")})

	probe := probeTool(nil)
	probe.Run = func(_ context.Context, _ json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (tool.Result, error) {
		if _, err := admit(policy.Target{}); err != nil {
			return tool.Result{}, err
		}
		data := []byte("\x89PNG\r\n\x1a\ngarbage-bytes")
		return tool.Result{Text: "(image/png, 20 bytes)", Images: []tool.Image{{MimeType: "image/png", Data: data}}}, nil
	}
	sess := openTestSession(t, r, probe)

	_, err := sess.Send(t.Context(), Prompt{Text: "look"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []ai.ToolResultMessageContent{ai.TextContent{Text: "(image/png, 20 bytes)\n" + tool.OmittedImageNote}}, toolResultContent(<-requests))
}

// toolResultContent returns the content of the last tool result in msgs.
func toolResultContent(msgs []ai.Message) []ai.ToolResultMessageContent {
	var content []ai.ToolResultMessageContent
	for _, m := range msgs {
		switch m := m.(type) {
		case ai.ToolResultMessage:
			content = m.Content
		case *ai.ToolResultMessage:
			content = m.Content
		}
	}
	return content
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

	_, err = sess.Send(t.Context(), Prompt{Text: "run it"}, nil, nil)
	require.NoError(t, err)
	denied := lastToolResult(t, <-requests)
	assert.Contains(t, denied, policy.ErrDenied.Error())
	assert.Contains(t, denied, "plan mode")
	assert.Zero(t, ran.Load())

	authority.EnterExecute()
	_, err = sess.Send(t.Context(), Prompt{Text: "run it now"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, execOutput, lastToolResult(t, <-requests))
	assert.Equal(t, int32(1), ran.Load())
}

func TestExecuteRefusesCallRevokedAfterBefore(t *testing.T) {
	var ran atomic.Int32
	authority := policy.NewAuthority()
	authority.EnterExecute()
	gate := policy.NewGate(authority, map[string]policy.ToolSpec{"Exec": {Capability: policy.Exec}}, nil, allowAll, nil)
	require.NoError(t, gate.Before(t.Context(), "call-1", "Exec", json.RawMessage(`{}`)))
	authority.EnterPlan()

	res, err := agentTool{tool: execTool(&ran), gate: gate}.Execute(t.Context(), "call-1", json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, toolResultContentText(res.Content), policy.ErrRevoked.Error())
	assert.Zero(t, ran.Load())
}

func TestExecutePassesTheAdmittedModeToTheTool(t *testing.T) {
	for _, want := range []policy.Mode{policy.Plan, policy.Execute} {
		authority := policy.NewAuthority()
		if want == policy.Execute {
			authority.EnterExecute()
		}
		gate := policy.NewGate(authority, map[string]policy.ToolSpec{"Exec": {Capability: policy.Exec, Sandboxed: true, PlanSandboxed: true}}, nil, nil, nil)
		require.NoError(t, gate.Before(t.Context(), "call-1", "Exec", json.RawMessage(`{}`)))
		got := make(chan policy.Mode, 1)
		exec := tool.Tool{Name: "Exec", Capability: policy.Exec, Run: func(_ context.Context, _ json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (tool.Result, error) {
			mode, err := admit(policy.Target{})
			if err != nil {
				return tool.Result{}, err
			}
			got <- mode
			return tool.Result{Text: execOutput}, nil
		}}

		res, err := agentTool{tool: exec, gate: gate}.Execute(t.Context(), "call-1", json.RawMessage(`{}`), nil)
		require.NoError(t, err)
		assert.False(t, res.IsError, toolResultContentText(res.Content))
		assert.Equal(t, want, <-got)
	}
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
		Approve: func(_ context.Context, req policy.Request) (policy.Decision, error) {
			asked <- req
			return policy.Decision{Answer: policy.Allow}, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	_, err = sess.Send(t.Context(), Prompt{Text: "run it"}, nil, nil)
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
				return OpenOptions{Approve: func(context.Context, policy.Request) (policy.Decision, error) {
					return policy.Decision{Answer: policy.Reject}, nil
				}}
			},
			reasons: []string{policy.ErrRejected.Error(), "stop and wait for their instructions"},
		},
		{
			name: "rejected with feedback",
			opts: func(*testing.T) OpenOptions {
				return OpenOptions{Approve: func(context.Context, policy.Request) (policy.Decision, error) {
					return policy.Decision{Answer: policy.Reject, Reason: "use b.txt"}, nil
				}}
			},
			reasons: []string{policy.ErrRejected.Error(), "with this feedback:\nuse b.txt"},
		},
		{
			name: "denied by rule",
			opts: func(t *testing.T) OpenOptions {
				rules, err := policy.NewRules(t.TempDir(), t.TempDir(), map[string]policy.ToolSpec{"Exec": {}}, policy.Source{Name: "config", Deny: []string{"Exec"}})
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

			_, err = sess.Send(t.Context(), Prompt{Text: "run it"}, nil, nil)
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
		Approve: func(ctx context.Context, _ policy.Request) (policy.Decision, error) {
			defer close(returned)
			close(waiting)
			<-ctx.Done()
			return policy.Decision{}, ctx.Err()
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	turnCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := sess.Send(turnCtx, Prompt{Text: "run it"}, nil, nil)
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
	rules, err := policy.NewRules(sb.ws, sb.kopiHome, map[string]policy.ToolSpec{"Exec": {}, "Probe": {}}, policy.Source{Name: "test", Allow: []string{"Probe"}})
	require.NoError(t, err)
	sess, err := r.Open(t.Context(), OpenOptions{
		Model: fauxModel, Authority: authority, Rules: rules,
		Tools: []tool.Tool{execTool(&ran), probeTool(func(context.Context, json.RawMessage) (string, error) {
			ran.Add(1)
			return toolOutput, nil
		})},
		Approve: func(ctx context.Context, _ policy.Request) (policy.Decision, error) {
			hooks <- ctx
			<-reject
			return policy.Decision{Answer: policy.Reject}, nil
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
		res, err := sess.Send(ctx, Prompt{Text: "run"}, nil, nil)
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
		res, err := sess.Send(t.Context(), Prompt{Text: "next"}, nil, nil)
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

func TestRealToolsRejectApprovalRetarget(t *testing.T) {
	for _, name := range []string{"Read", "Write", "Edit", "Glob", "Grep", "LS"} {
		for _, phase := range []string{"pending approval", "before execution"} {
			t.Run(name+"/"+phase, func(t *testing.T) {
				dir := t.TempDir()
				for _, sub := range []string{"allowed", "denied"} {
					require.NoError(t, os.Mkdir(filepath.Join(dir, sub), 0o755))
					require.NoError(t, os.WriteFile(filepath.Join(dir, sub, "file"), []byte(sub+"-secret"), 0o600))
				}
				require.NoError(t, os.WriteFile(filepath.Join(dir, "denied", "denied-filename"), []byte("denied-secret"), 0o600))
				link := filepath.Join(dir, "link")
				require.NoError(t, os.Symlink("allowed", link))
				ws, err := tool.NewWorkspace(dir, nil)
				require.NoError(t, err)
				defer ws.Close()
				tools := map[string]tool.Tool{
					"Read": tool.NewRead(ws), "Write": tool.NewWrite(ws), "Edit": tool.NewEdit(ws),
					"Glob": tool.NewGlob(ws, nil), "Grep": tool.NewGrep(ws, nil), "LS": tool.NewLS(ws, nil),
				}
				realTool := tools[name]
				args := map[string]any{"file_path": "link/file"}
				switch name {
				case "Write":
					args["content"] = "new"
				case "Edit":
					args["old_string"], args["new_string"] = "allowed", "updated"
				case "Glob":
					args = map[string]any{"path": "link", "pattern": "*"}
				case "Grep":
					args = map[string]any{"path": "link", "pattern": "secret", "output_mode": "content"}
				case "LS":
					args = map[string]any{"path": "link"}
				}
				raw, err := json.Marshal(args)
				require.NoError(t, err)
				specs := map[string]policy.ToolSpec{name: {Capability: realTool.Capability, Target: realTool.Target}}
				rules, err := policy.NewRules(dir, t.TempDir(), specs, policy.Source{Name: "test", Ask: []string{name}, Deny: []string{name + "(denied)", name + "(denied/**)"}})
				require.NoError(t, err)
				authority := policy.NewAuthority()
				authority.EnterExecute()
				waiting, answer := make(chan struct{}), make(chan struct{})
				gate := policy.NewGate(authority, specs, rules,
					func(context.Context, policy.Request) (policy.Decision, error) {
						close(waiting)
						<-answer
						return policy.Decision{Answer: policy.Allow}, nil
					}, nil)
				done := make(chan error, 1)
				go func() { done <- gate.Before(t.Context(), "call", name, raw) }()
				<-waiting
				retarget := func() {
					require.NoError(t, os.Remove(link))
					require.NoError(t, os.Symlink("denied", link))
				}
				if phase == "pending approval" {
					retarget()
				}
				close(answer)
				require.NoError(t, <-done)
				if phase == "before execution" {
					retarget()
				}
				result, err := (agentTool{tool: realTool, gate: gate}).Execute(t.Context(), "call", raw, nil)
				require.NoError(t, err)
				require.True(t, result.IsError)
				text := toolResultContentText(result.Content)
				assert.Contains(t, text, policy.ErrChangedCall.Error())
				assert.NotContains(t, text, "denied-secret")
				assert.NotContains(t, text, "denied-filename")
				for _, sub := range []string{"allowed", "denied"} {
					data, err := os.ReadFile(filepath.Join(dir, sub, "file"))
					require.NoError(t, err)
					assert.Equal(t, sub+"-secret", string(data))
				}
				assert.ErrorIs(t, gate.Before(t.Context(), "new-call", name, raw), policy.ErrDenied)
				result, err = (agentTool{tool: realTool, gate: gate}).Execute(t.Context(), "call", raw, nil)
				require.NoError(t, err)
				assert.Contains(t, toolResultContentText(result.Content), policy.ErrNoGrant.Error())
			})
		}
	}
}

func TestFailedPreparationConsumesGrant(t *testing.T) {
	dir := t.TempDir()
	ws, err := tool.NewWorkspace(dir, nil)
	require.NoError(t, err)
	defer ws.Close()
	read := tool.NewRead(ws)
	authority := policy.NewAuthority()
	gate := policy.NewGate(authority, map[string]policy.ToolSpec{"Read": {Capability: policy.Read, Target: read.Target}}, nil, nil, nil)
	raw := json.RawMessage(`{"file_path":"missing"}`)
	require.NoError(t, gate.Before(t.Context(), "call", "Read", raw))
	result, err := (agentTool{tool: read, gate: gate}).Execute(t.Context(), "call", raw, nil)
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "missing"), []byte("now exists"), 0o600))
	result, err = (agentTool{tool: read, gate: gate}).Execute(t.Context(), "call", raw, nil)
	require.NoError(t, err)
	assert.Contains(t, toolResultContentText(result.Content), policy.ErrNoGrant.Error())
	assert.NotContains(t, toolResultContentText(result.Content), "now exists")
}

func TestSessionMCPAdmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		approve   policy.Approver
		allow     []string
		wantCalls int32
	}{
		{name: "default denied"},
		{name: "approved", approve: allowAll, wantCalls: 1},
		{name: "different server rule", allow: []string{"mcp__github__*"}},
		{name: "same server rule", allow: []string{"mcp__github-other__*"}, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			fake := &mcpfake.Server{
				Tools: []map[string]any{{"name": "read", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}}},
				Reply: func(string, json.RawMessage) map[string]any {
					calls.Add(1)
					return map[string]any{"content": []any{map[string]any{"type": "text", "text": "MCP reply"}}}
				},
			}
			servers, _ := startFakes(t, map[string]*mcpfake.Server{"github-other": fake})
			st := servers.Wait(t.Context())[0]
			require.NoError(t, st.Err)
			sb := newSandbox(t)
			r, faux := newTestRuntime(t, sb)
			requests := make(chan []ai.Message, 1)
			search := ai.FauxAssistantMessage(
				ai.FauxContentBlocks{ai.FauxToolCall("ToolSearch", map[string]any{"query": st.Tools[0].Name}, &ai.FauxToolCallOptions{ID: "search"})},
				ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)},
			)
			faux.SetResponses([]ai.FauxResponseStep{search, callTool(st.Tools[0].Name, "mcp-call"), captureRequest(requests, "done")})
			rules, err := policy.NewRules(sb.ws, sb.kopiHome, tool.Specs(st.Tools), policy.Source{Name: "test", Allow: tc.allow})
			require.NoError(t, err)
			tools := append([]tool.Tool{tool.NewToolSearch(st.Tools, RankTools, LoadTools)}, st.Tools...)
			sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, Tools: tools, Authority: policy.NewAuthority(), Approve: tc.approve, Rules: rules})
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
			_, err = sess.Send(t.Context(), Prompt{Text: "read"}, nil, nil)
			require.NoError(t, err)
			msgs := <-requests
			texts := toolResultTexts(msgs)
			require.Len(t, texts, 2)
			assert.Contains(t, texts[0], "Loaded 1 tool")
			result := lastToolResult(t, msgs)
			assert.Equal(t, tc.wantCalls, calls.Load())
			if tc.wantCalls == 1 {
				assert.Equal(t, "MCP reply", result)
			} else {
				assert.Contains(t, result, policy.ErrDenied.Error())
				assert.NotContains(t, result, "MCP reply")
			}
		})
	}
}

func TestReviewDenialBlocksToolAndReportsReason(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	requests := make(chan []ai.Message, 1)
	call := ai.FauxAssistantMessage(
		ai.FauxContentBlocks{ai.FauxToolCall("Exec", map[string]any{}, &ai.FauxToolCallOptions{ID: "call-1"})},
		ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)},
	)
	faux.SetResponses([]ai.FauxResponseStep{call, captureRequest(requests, "ok")})

	var ran atomic.Int32
	authority := policy.NewAuthority()
	authority.EnterExecute()
	sess, err := r.Open(t.Context(), OpenOptions{
		Model: fauxModel, Tools: []tool.Tool{execTool(&ran)}, Authority: authority, Approve: allowAll,
		Review: func(context.Context, policy.Request) (policy.Review, error) {
			return policy.Review{Verdict: policy.VerdictDeny, Reason: "too risky"}, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	_, err = sess.Send(t.Context(), Prompt{Text: "run it"}, nil, nil)
	require.NoError(t, err)
	assert.Contains(t, lastToolResult(t, <-requests), "too risky")
	assert.Zero(t, ran.Load())
}

func TestExecutionModeRunsExclusiveReadToolAlone(t *testing.T) {
	read := tool.Tool{Capability: policy.Read}
	exclusive := tool.Tool{Capability: policy.Read, Exclusive: true}

	assert.Equal(t, agent.ToolModeParallel, agentTool{tool: read}.ExecutionMode())
	assert.Equal(t, agent.ToolModeSequential, agentTool{tool: exclusive}.ExecutionMode())
}
