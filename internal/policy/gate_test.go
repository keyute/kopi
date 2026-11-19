package policy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pathTarget(args json.RawMessage) (Target, error) {
	var a struct{ Path string }
	if err := json.Unmarshal(args, &a); err != nil {
		return Target{}, err
	}
	return Target{Path: a.Path, Real: a.Path}, nil
}

var testSpecs = map[string]ToolSpec{
	"Read": {Capability: Read, Target: pathTarget},
	"Edit": {Capability: Write, Target: pathTarget},
	"Bash": {Capability: Exec},
}

// approver answers every request with answer and reason and records it.
type approver struct {
	answer Answer
	reason string
	reqs   []Request
}

func (f *approver) approve(_ context.Context, r Request) (Decision, error) {
	f.reqs = append(f.reqs, r)
	return Decision{Answer: f.answer, Reason: f.reason}, nil
}

func newTestGate(rules *Rules, approve Approver) (*Authority, *Gate) {
	a := NewAuthority()
	return a, NewGate(a, testSpecs, rules, approve, nil)
}

func pathArgs(p string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"path": p})
	return b
}

func TestUnknownToolIsDenied(t *testing.T) {
	_, g := newTestGate(nil, nil)
	assert.ErrorIs(t, g.Before(t.Context(), "c1", "Nope", nil), ErrUnknownTool)
}

func TestReadIsAllowedInBothModes(t *testing.T) {
	a, g := newTestGate(nil, nil)
	require.NoError(t, g.Before(t.Context(), "c1", "Read", pathArgs("/x")))
	require.NoError(t, admitApproved(g, "c1", "Read"))
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c2", "Read", pathArgs("/x")))
	require.NoError(t, admitApproved(g, "c2", "Read"))
}

func TestReadGrantSurvivesModeChanges(t *testing.T) {
	a, g := newTestGate(nil, nil)
	require.NoError(t, g.Before(t.Context(), "c1", "Read", pathArgs("/x")))
	a.EnterExecute()
	a.EnterPlan()
	assert.NoError(t, admitApproved(g, "c1", "Read"))
}

func TestPlanDeniesNonReadBeforeRules(t *testing.T) {
	d := newDirs(t)
	f := &approver{answer: Allow}
	_, g := newTestGate(d.rules(t, Source{Name: "config", Allow: []string{"Edit", "*"}}), f.approve)
	for _, tool := range []string{"Edit", "Bash"} {
		err := g.Before(t.Context(), "c1", tool, pathArgs("/x"))
		require.ErrorIs(t, err, ErrDenied)
		assert.Contains(t, err.Error(), `tool "`+tool+`" needs execute mode; the session is in plan mode`)
		if tool == "Bash" {
			assert.Contains(t, err.Error(), "runs only in the OS sandbox, which is off or unavailable")
		} else {
			assert.NotContains(t, err.Error(), "sandbox")
		}
	}
	assert.Empty(t, f.reqs)
}

func TestExecuteAdmitsEveryCapabilityOnceApproved(t *testing.T) {
	a, g := newTestGate(nil, (&approver{answer: Allow}).approve)
	a.EnterExecute()
	for _, tool := range []string{"Read", "Edit", "Bash"} {
		require.NoError(t, g.Before(t.Context(), "c-"+tool, tool, pathArgs("/x")), tool)
		assert.NoError(t, admitApproved(g, "c-"+tool, tool), tool)
	}
}

func TestExecGrantIsRevokedOnReturnToPlan(t *testing.T) {
	a, g := newTestGate(nil, (&approver{answer: Allow}).approve)
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Bash", nil))
	a.EnterPlan()
	assert.ErrorIs(t, admitApproved(g, "c1", "Bash"), ErrRevoked)
}

func TestStaleExecGrantIsRevokedAfterReturningToExecute(t *testing.T) {
	a, g := newTestGate(nil, (&approver{answer: Allow}).approve)
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Bash", nil))
	a.EnterPlan()
	a.EnterExecute()
	assert.ErrorIs(t, admitApproved(g, "c1", "Bash"), ErrRevoked)
}

func TestDenyRuleNamesRuleAndSource(t *testing.T) {
	d := newDirs(t)
	f := &approver{answer: Allow}
	a, g := newTestGate(d.rules(t, Source{Name: "config", Deny: []string{"Edit(secret)"}}), f.approve)
	a.EnterExecute()
	err := g.Before(t.Context(), "c1", "Edit", pathArgs(filepath.Join(d.ws, "secret")))
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), "Edit(secret) (config)")
	assert.Empty(t, f.reqs)
	assert.ErrorIs(t, admitApproved(g, "c1", "Edit"), ErrNoGrant)
}

func TestDefaultAsks(t *testing.T) {
	for _, tc := range []struct {
		tool  string
		edits bool
	}{{"Edit", true}, {"Bash", false}} {
		f := &approver{answer: Allow}
		a, g := newTestGate(nil, f.approve)
		a.EnterExecute()
		args := pathArgs("/x")
		require.NoError(t, g.Before(t.Context(), "c1", tc.tool, args))
		assert.Equal(t, []Request{{CallID: "c1", Tool: tc.tool, Args: args, Edits: tc.edits}}, f.reqs)
		assert.NoError(t, admitApproved(g, "c1", tc.tool))
	}
}

// sandboxedGate is a gate with one exec tool, Shell, with spec, and rules that
// may name it.
func sandboxedGate(t *testing.T, spec ToolSpec, approve Approver, src Source) (*Authority, *Gate) {
	t.Helper()
	specs := map[string]ToolSpec{"Shell": spec}
	d := newDirs(t)
	rules, err := NewRules(d.ws, d.home, specs, src)
	require.NoError(t, err)
	a := NewAuthority()
	return a, NewGate(a, specs, rules, approve, nil)
}

var (
	sandboxed     = ToolSpec{Capability: Exec, Sandboxed: true}
	planSandboxed = ToolSpec{Capability: Exec, Sandboxed: true, PlanSandboxed: true}
)

func TestSandboxedRunsWithoutAskingInExecute(t *testing.T) {
	a, g := sandboxedGate(t, sandboxed, nil, Source{Name: "config"})
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Shell", nil))
	assert.NoError(t, admitApproved(g, "c1", "Shell"))

	f := &approver{answer: Reject}
	a, g = sandboxedGate(t, sandboxed, f.approve, Source{Name: "config"})
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Shell", nil))
	assert.Empty(t, f.reqs)
}

func TestSandboxedStillObeysRulesAndPlan(t *testing.T) {
	f := &approver{answer: Allow}
	a, g := sandboxedGate(t, sandboxed, f.approve, Source{Name: "config", Ask: []string{"Shell"}})
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Shell", nil))
	assert.Equal(t, []Request{{CallID: "c1", Tool: "Shell", Rule: "Shell (config)"}}, f.reqs)

	f = &approver{answer: Allow}
	a, g = sandboxedGate(t, sandboxed, f.approve, Source{Name: "config", Deny: []string{"Shell"}})
	a.EnterExecute()
	err := g.Before(t.Context(), "c1", "Shell", nil)
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), "Shell (config)")
	assert.Empty(t, f.reqs)

	_, g = sandboxedGate(t, sandboxed, f.approve, Source{Name: "config", Allow: []string{"Shell"}})
	err = g.Before(t.Context(), "c1", "Shell", nil)
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), "needs execute mode")
	assert.Empty(t, f.reqs)
}

func TestPlanSandboxedRunsInPlanWithoutAsking(t *testing.T) {
	f := &approver{answer: Reject}
	_, g := sandboxedGate(t, planSandboxed, f.approve, Source{Name: "config"})
	require.NoError(t, g.Before(t.Context(), "c1", "Shell", nil))
	assert.Empty(t, f.reqs)
	mode, err := g.Admit("c1", "Shell", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, Plan, mode)
}

func TestPlanSandboxedObeysRulesInPlan(t *testing.T) {
	f := &approver{answer: Allow}
	_, g := sandboxedGate(t, planSandboxed, f.approve, Source{Name: "config", Ask: []string{"Shell"}})
	require.NoError(t, g.Before(t.Context(), "c1", "Shell", nil))
	assert.Equal(t, []Request{{CallID: "c1", Tool: "Shell", Rule: "Shell (config)"}}, f.reqs)

	f = &approver{answer: Allow}
	_, g = sandboxedGate(t, planSandboxed, f.approve, Source{Name: "config", Deny: []string{"Shell"}})
	err := g.Before(t.Context(), "c1", "Shell", nil)
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), "Shell (config)")
	assert.Empty(t, f.reqs)
}

func TestPlanSandboxedGrantIsRevokedOnModeChange(t *testing.T) {
	for _, tc := range []struct {
		name          string
		execute       bool
		before, after []Mode
	}{
		{"plan to execute", false, nil, []Mode{Execute}},
		{"execute to plan", true, nil, []Mode{Plan}},
		{"plan to execute to plan", false, nil, []Mode{Execute, Plan}},
		{"inside the approver", false, []Mode{Execute, Plan}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a *Authority
			enter := func(modes []Mode) {
				for _, m := range modes {
					if m == Execute {
						a.EnterExecute()
					} else {
						a.EnterPlan()
					}
				}
			}
			a, g := sandboxedGate(t, planSandboxed, func(context.Context, Request) (Decision, error) {
				enter(tc.before)
				return Decision{Answer: Allow}, nil
			}, Source{Name: "config", Ask: []string{"Shell"}})
			if tc.execute {
				a.EnterExecute()
			}
			require.NoError(t, g.Before(t.Context(), "c1", "Shell", nil))
			enter(tc.after)
			mode, err := g.Admit("c1", "Shell", nil, nil)
			assert.ErrorIs(t, err, ErrRevoked)
			assert.Equal(t, Plan, mode)
		})
	}
}

func TestAdmitReturnsTheAdmittedMode(t *testing.T) {
	for _, tc := range []struct {
		tool    string
		execute bool
		want    Mode
	}{
		{"Shell", false, Plan},
		{"Shell", true, Execute},
		{"Read", false, Plan},
		{"Read", true, Execute},
	} {
		specs := map[string]ToolSpec{"Shell": planSandboxed, "Read": {Capability: Read}}
		a := NewAuthority()
		g := NewGate(a, specs, nil, nil, nil)
		if tc.execute {
			a.EnterExecute()
		}
		require.NoError(t, g.Before(t.Context(), "c1", tc.tool, nil))
		mode, err := g.Admit("c1", tc.tool, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, tc.want, mode, tc.tool)
	}
	a, g := newTestGate(nil, nil)
	a.EnterExecute()
	mode, err := g.Admit("c1", "Read", nil, nil)
	require.ErrorIs(t, err, ErrNoGrant)
	assert.Equal(t, Plan, mode)
}

func TestAskRuleOffersNoEdits(t *testing.T) {
	d := newDirs(t)
	f := &approver{answer: Allow}
	a, g := newTestGate(d.rules(t, Source{Name: "config", Ask: []string{"Edit(*.sql)"}}), f.approve)
	a.EnterExecute()
	args := pathArgs(filepath.Join(d.ws, "db/q.sql"))
	require.NoError(t, g.Before(t.Context(), "c1", "Edit", args))
	assert.Equal(t, []Request{{CallID: "c1", Tool: "Edit", Args: args, Rule: "Edit(*.sql) (config)"}}, f.reqs)
}

func TestAskRuleAppliesToRead(t *testing.T) {
	d := newDirs(t)
	f := &approver{answer: Allow}
	_, g := newTestGate(d.rules(t, Source{Name: "config", Ask: []string{"Read(.env)"}}), f.approve)
	require.NoError(t, g.Before(t.Context(), "c1", "Read", pathArgs(filepath.Join(d.ws, ".env"))))
	assert.Len(t, f.reqs, 1)
}

func TestAllowRuleSkipsApproval(t *testing.T) {
	d := newDirs(t)
	a, g := newTestGate(d.rules(t, Source{Name: "config", Allow: []string{"Edit(docs/**)"}}), nil)
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Edit", pathArgs(filepath.Join(d.ws, "docs/a.md"))))
	assert.NoError(t, admitApproved(g, "c1", "Edit"))
}

func TestRejectedCallIsRefused(t *testing.T) {
	a, g := newTestGate(nil, (&approver{answer: Reject}).approve)
	a.EnterExecute()
	err := g.Before(t.Context(), "c1", "Edit", pathArgs("/x"))
	require.ErrorIs(t, err, ErrRejected)
	assert.Contains(t, err.Error(), "stop and wait for their instructions")
	assert.ErrorIs(t, admitApproved(g, "c1", "Edit"), ErrNoGrant)
}

func TestRejectedCallWithReasonCarriesIt(t *testing.T) {
	a, g := newTestGate(nil, (&approver{answer: Reject, reason: "use b.go"}).approve)
	a.EnterExecute()
	err := g.Before(t.Context(), "c1", "Edit", pathArgs("/x"))
	require.ErrorIs(t, err, ErrRejected)
	assert.Equal(t, "tool \"Edit\": rejected by the user, with this feedback:\nuse b.go", err.Error())
	assert.NotContains(t, err.Error(), "stop and wait")
	assert.ErrorIs(t, admitApproved(g, "c1", "Edit"), ErrNoGrant)
}

func TestReasonOnAllowIsIgnored(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer Answer
	}{{"allow", Allow}, {"allow edits", AllowEdits}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &approver{answer: tc.answer, reason: "ignored"}
			a, g := newTestGate(nil, f.approve)
			a.EnterExecute()
			require.NoError(t, g.Before(t.Context(), "c1", "Edit", pathArgs("/x")))
			require.Len(t, f.reqs, 1)
			require.True(t, f.reqs[0].Edits)
			assert.NoError(t, admitApproved(g, "c1", "Edit"))
			if tc.answer == AllowEdits {
				require.NoError(t, g.Before(t.Context(), "c2", "Edit", pathArgs("/y")))
				assert.Len(t, f.reqs, 1)
			}
		})
	}
}

func TestAllowEditsSkipsLaterDefaultEditAsksOnly(t *testing.T) {
	d := newDirs(t)
	f := &approver{answer: AllowEdits}
	rules := d.rules(t, Source{Name: "config", Deny: []string{"Edit(secret)"}})
	a, g := newTestGate(rules, f.approve)
	a.EnterExecute()
	ws := func(p string) json.RawMessage { return pathArgs(filepath.Join(d.ws, p)) }

	require.NoError(t, g.Before(t.Context(), "c1", "Edit", ws("a.go")))
	require.Len(t, f.reqs, 1)
	f.answer = Allow
	require.NoError(t, g.Before(t.Context(), "c2", "Edit", ws("b.go")))
	assert.Len(t, f.reqs, 1)

	require.NoError(t, g.Before(t.Context(), "c3", "Edit", ws(".git/config")))
	require.Len(t, f.reqs, 2)
	assert.Equal(t, "Edit(.git) (built-in)", f.reqs[1].Rule)
	require.NoError(t, g.Before(t.Context(), "c4", "Bash", nil))
	assert.Len(t, f.reqs, 3)
	assert.ErrorIs(t, g.Before(t.Context(), "c5", "Edit", ws("secret")), ErrDenied)
}

func TestBuiltInGitAskSurvivesAllEditsInsideGitWorkspace(t *testing.T) {
	for _, sub := range []string{".git", ".git/hooks"} {
		t.Run(sub, func(t *testing.T) {
			d := newDirs(t)
			ordinary := filepath.Join(d.ws, "a.go")
			d.ws = filepath.Join(d.ws, sub)
			require.NoError(t, os.MkdirAll(d.ws, 0o755))
			f := &approver{answer: AllowEdits}
			a := NewAuthority()
			g := NewGate(a, map[string]ToolSpec{
				"Edit":  {Capability: Write, Target: pathTarget},
				"Write": {Capability: Write, Target: pathTarget},
			}, d.rules(t), f.approve, nil)
			a.EnterExecute()
			require.NoError(t, g.Before(t.Context(), "initial", "Edit", pathArgs(ordinary)))
			require.NoError(t, admitApproved(g, "initial", "Edit"))
			require.Len(t, f.reqs, 1)
			require.True(t, f.reqs[0].Edits)
			f.answer = Reject
			for _, tool := range []string{"Edit", "Write"} {
				err := g.Before(t.Context(), tool, tool, pathArgs(filepath.Join(d.ws, "pre-commit")))
				require.ErrorIs(t, err, ErrRejected)
				assert.ErrorIs(t, admitApproved(g, tool, tool), ErrNoGrant)
			}
			require.Len(t, f.reqs, 3)
			assert.Equal(t, "Edit(.git) (built-in)", f.reqs[1].Rule)
			assert.Equal(t, "Write(.git) (built-in)", f.reqs[2].Rule)
			assert.False(t, f.reqs[1].Edits)
			assert.False(t, f.reqs[2].Edits)
		})
	}
}

func TestBuiltInMCPFileAskSurvivesAllEdits(t *testing.T) {
	d := newDirs(t)
	f := &approver{answer: AllowEdits}
	a := NewAuthority()
	g := NewGate(a, map[string]ToolSpec{
		"Edit":  {Capability: Write, Target: pathTarget},
		"Write": {Capability: Write, Target: pathTarget},
	}, d.rules(t), f.approve, nil)
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "initial", "Edit", pathArgs(filepath.Join(d.ws, "a.go"))))
	require.NoError(t, admitApproved(g, "initial", "Edit"))
	require.Len(t, f.reqs, 1)
	require.True(t, f.reqs[0].Edits)
	f.answer = Reject
	for _, tool := range []string{"Edit", "Write"} {
		err := g.Before(t.Context(), tool, tool, pathArgs(filepath.Join(d.ws, "sub", ".mcp.json")))
		require.ErrorIs(t, err, ErrRejected)
		assert.ErrorIs(t, admitApproved(g, tool, tool), ErrNoGrant)
	}
	require.Len(t, f.reqs, 3)
	assert.Equal(t, "Edit(.mcp.json) (built-in)", f.reqs[1].Rule)
	assert.Equal(t, "Write(.mcp.json) (built-in)", f.reqs[2].Rule)
	assert.False(t, f.reqs[1].Edits)
	assert.False(t, f.reqs[2].Edits)
}

func TestAllowEditsNotOfferedGrantsOnlyTheCall(t *testing.T) {
	f := &approver{answer: AllowEdits}
	a, g := newTestGate(nil, f.approve)
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Bash", nil))
	require.NoError(t, g.Before(t.Context(), "c2", "Edit", pathArgs("/x")))
	assert.Len(t, f.reqs, 2)
}

func TestCancelledApprovalRefuses(t *testing.T) {
	waiting := make(chan struct{})
	a, g := newTestGate(nil, func(ctx context.Context, _ Request) (Decision, error) {
		close(waiting)
		<-ctx.Done()
		return Decision{}, ctx.Err()
	})
	a.EnterExecute()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- g.Before(ctx, "c1", "Edit", pathArgs("/x")) }()
	<-waiting
	cancel()
	err := <-done
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrRejected)
	assert.ErrorIs(t, admitApproved(g, "c1", "Edit"), ErrNoGrant)
}

func TestNoApproverDeniesNamingTheAllowRule(t *testing.T) {
	a, g := newTestGate(nil, nil)
	a.EnterExecute()
	err := g.Before(t.Context(), "c1", "Edit", pathArgs("/docs/a[1].md"))
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), `tool "Edit" needs approval and this run cannot ask for it; add an allow rule to permissions.allow in config.json or pass --allowed-tools, such as Edit(/docs/a\[1\].md)`)
	d := newDirs(t)
	a, allowed := newTestGate(d.rules(t, Source{Name: "--allowed-tools", Allow: []string{`Edit(/docs/a\[1\].md)`}}), nil)
	a.EnterExecute()
	assert.NoError(t, allowed.Before(t.Context(), "c1", "Edit", pathArgs("/docs/a[1].md")))

	err = g.Before(t.Context(), "c2", "Bash", nil)
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), "or pass --allowed-tools, such as Bash: ")
}

func TestNoApproverDeniesNamingTheAskRule(t *testing.T) {
	d := newDirs(t)
	a, g := newTestGate(d.rules(t), nil)
	a.EnterExecute()
	err := g.Before(t.Context(), "c1", "Edit", pathArgs(filepath.Join(d.ws, ".git/config")))
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), `rule Edit(.git) (built-in) requires approval for tool "Edit" and this run cannot ask for it`)
	assert.NotContains(t, err.Error(), "allowed-tools")
}

func TestModeChangeDuringApprovalRevokes(t *testing.T) {
	var a *Authority
	a, g := newTestGate(nil, func(context.Context, Request) (Decision, error) {
		a.EnterPlan()
		a.EnterExecute()
		return Decision{Answer: Allow}, nil
	})
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Edit", pathArgs("/x")))
	assert.ErrorIs(t, admitApproved(g, "c1", "Edit"), ErrRevoked)
}

func TestTargetErrorRefuses(t *testing.T) {
	f := &approver{answer: Allow}
	a, g := newTestGate(nil, f.approve)
	a.EnterExecute()
	err := g.Before(t.Context(), "c1", "Edit", json.RawMessage(`not json`))
	var syntax *json.SyntaxError
	require.ErrorAs(t, err, &syntax)
	assert.Empty(t, f.reqs)
	assert.ErrorIs(t, admitApproved(g, "c1", "Edit"), ErrNoGrant)
}

func TestAdmitWithoutBeforeHasNoGrant(t *testing.T) {
	_, g := newTestGate(nil, nil)
	assert.ErrorIs(t, admitApproved(g, "c1", "Read"), ErrNoGrant)
}

func TestGrantIsConsumedOnce(t *testing.T) {
	_, g := newTestGate(nil, nil)
	require.NoError(t, g.Before(t.Context(), "c1", "Read", pathArgs("/x")))
	require.NoError(t, admitApproved(g, "c1", "Read"))
	assert.ErrorIs(t, admitApproved(g, "c1", "Read"), ErrNoGrant)
}

func TestGrantForOneToolIsNotAdmittedForAnother(t *testing.T) {
	a, g := newTestGate(nil, nil)
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Read", pathArgs("/x")))
	assert.ErrorIs(t, admitApproved(g, "c1", "Bash"), ErrNoGrant)
}

func TestNewGateCopiesTools(t *testing.T) {
	tools := map[string]ToolSpec{"Read": {Capability: Read}}
	g := NewGate(NewAuthority(), tools, nil, nil, nil)
	tools["Read"] = ToolSpec{Capability: Exec}
	tools["Bash"] = ToolSpec{Capability: Read}
	assert.NoError(t, g.Before(t.Context(), "c1", "Read", nil))
	assert.ErrorIs(t, g.Before(t.Context(), "c2", "Bash", nil), ErrUnknownTool)
}

func admitApproved(g *Gate, id, tool string) error {
	gr := g.grants[id]
	_, err := g.Admit(id, tool, gr.args, gr.target)
	return err
}

func TestGrantBindsArgumentsAndTarget(t *testing.T) {
	for _, change := range []string{"arguments", "target", "missing target"} {
		t.Run(change, func(t *testing.T) {
			a, g := newTestGate(nil, (&approver{answer: Allow}).approve)
			a.EnterExecute()
			args := pathArgs("/allowed")
			require.NoError(t, g.Before(t.Context(), "call", "Edit", args))
			target := &Target{Path: "/allowed", Real: "/allowed"}
			switch change {
			case "arguments":
				args = pathArgs("/other")
			case "target":
				target.Real = "/denied"
			case "missing target":
				target = nil
			}
			_, err := g.Admit("call", "Edit", args, target)
			assert.ErrorIs(t, err, ErrChangedCall)
			_, err = g.Admit("call", "Edit", pathArgs("/allowed"), &Target{Path: "/allowed", Real: "/allowed"})
			assert.ErrorIs(t, err, ErrNoGrant)
		})
	}
}

func TestPendingApprovalBindsOriginalTarget(t *testing.T) {
	waiting, answer := make(chan struct{}), make(chan struct{})
	target := Target{Path: "/link", Real: "/allowed"}
	a := NewAuthority()
	a.EnterExecute()
	g := NewGate(a, map[string]ToolSpec{"Edit": {Capability: Write, Target: func(json.RawMessage) (Target, error) { return target, nil }}}, nil,
		func(context.Context, Request) (Decision, error) {
			close(waiting)
			<-answer
			return Decision{Answer: Allow}, nil
		}, nil)
	args := pathArgs("/link")
	done := make(chan error, 1)
	go func() { done <- g.Before(t.Context(), "call", "Edit", args) }()
	<-waiting
	target.Real = "/denied"
	close(answer)
	require.NoError(t, <-done)
	_, err := g.Admit("call", "Edit", args, &target)
	assert.ErrorIs(t, err, ErrChangedCall)
}

func TestGrantCopiesArgumentsBeforeApproval(t *testing.T) {
	a, g := newTestGate(nil, func(_ context.Context, r Request) (Decision, error) {
		copy(r.Args, pathArgs("/changed"))
		return Decision{Answer: Allow}, nil
	})
	a.EnterExecute()
	args := pathArgs("/allowed")
	require.NoError(t, g.Before(t.Context(), "call", "Edit", args))
	original := append(json.RawMessage(nil), args...)
	copy(args, pathArgs("/changed"))
	_, err := g.Admit("call", "Edit", original, &Target{Path: "/allowed", Real: "/allowed"})
	assert.NoError(t, err)
}

// mcpGate is a gate with an Ask read tool and an Ask exec tool, as MCP
// servers' tools are, and rules that may name them.
func mcpGate(t *testing.T, approve Approver, src Source) (*Authority, *Gate) {
	t.Helper()
	specs := map[string]ToolSpec{
		"mcp__s__read": {Capability: Read, Ask: true},
		"mcp__s__exec": {Capability: Exec, Ask: true},
	}
	d := newDirs(t)
	rules, err := NewRules(d.ws, d.home, specs, src)
	require.NoError(t, err)
	a := NewAuthority()
	return a, NewGate(a, specs, rules, approve, nil)
}

func TestAskToolAsksWithNoRuleEvenForRead(t *testing.T) {
	f := &approver{answer: Allow}
	_, g := mcpGate(t, f.approve, Source{Name: "config"})
	require.NoError(t, g.Before(t.Context(), "c1", "mcp__s__read", nil))
	assert.Equal(t, []Request{{CallID: "c1", Tool: "mcp__s__read"}}, f.reqs)
	assert.NoError(t, admitApproved(g, "c1", "mcp__s__read"))

	f = &approver{answer: Allow}
	a, g := mcpGate(t, f.approve, Source{Name: "config"})
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c2", "mcp__s__exec", nil))
	assert.Equal(t, []Request{{CallID: "c2", Tool: "mcp__s__exec"}}, f.reqs)
	assert.NoError(t, admitApproved(g, "c2", "mcp__s__exec"))

	_, g = mcpGate(t, nil, Source{Name: "config"})
	err := g.Before(t.Context(), "c1", "mcp__s__read", nil)
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), "or pass --allowed-tools, such as mcp__s__read: ")
}

func TestAskToolFollowsRules(t *testing.T) {
	for _, tc := range []struct{ tool, rule string }{
		{"mcp__s__read", "mcp__s__read"},
		{"mcp__s__read", "mcp__s__*"},
		{"mcp__s__read", "mcp__*"},
		{"mcp__s__exec", "mcp__s__exec"},
		{"mcp__s__exec", "mcp__s__*"},
	} {
		f := &approver{answer: Reject}
		a, g := mcpGate(t, f.approve, Source{Name: "config", Allow: []string{tc.rule}})
		if tc.tool == "mcp__s__exec" {
			a.EnterExecute()
		}
		require.NoError(t, g.Before(t.Context(), "c1", tc.tool, nil), tc.rule)
		assert.Empty(t, f.reqs, tc.rule)
	}

	f := &approver{answer: Allow}
	_, g := mcpGate(t, f.approve, Source{Name: "config", Deny: []string{"mcp__s__*"}})
	err := g.Before(t.Context(), "c1", "mcp__s__read", nil)
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), "mcp__s__* (config)")
	assert.Empty(t, f.reqs)
}

func TestPlanRefusesAskExecButAsksAboutAskRead(t *testing.T) {
	f := &approver{answer: Allow}
	_, g := mcpGate(t, f.approve, Source{Name: "config", Allow: []string{"mcp__s__exec"}})
	err := g.Before(t.Context(), "c1", "mcp__s__exec", nil)
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), "needs execute mode")
	assert.NotContains(t, err.Error(), "sandbox")
	assert.Empty(t, f.reqs)

	require.NoError(t, g.Before(t.Context(), "c2", "mcp__s__read", nil))
	assert.Equal(t, []Request{{CallID: "c2", Tool: "mcp__s__read"}}, f.reqs)
}

func TestAllowEditsGrantCoversEveryGateOnTheAuthority(t *testing.T) {
	f := &approver{answer: AllowEdits}
	a, first := newTestGate(nil, f.approve)
	second := NewGate(a, testSpecs, nil, f.approve, nil)
	a.EnterExecute()

	require.NoError(t, first.Before(t.Context(), "c1", "Edit", pathArgs("/x")))
	require.Len(t, f.reqs, 1)
	require.NoError(t, second.Before(t.Context(), "c2", "Edit", pathArgs("/y")))
	assert.Len(t, f.reqs, 1, "the grant given through one gate covers the other")

	a.ClearEdits()
	require.NoError(t, second.Before(t.Context(), "c3", "Edit", pathArgs("/z")))
	assert.Len(t, f.reqs, 2)
}

func planSpecs(targetErr error) map[string]ToolSpec {
	target := func(args json.RawMessage) (Target, error) {
		if targetErr != nil {
			return Target{}, targetErr
		}
		var a struct {
			Path string
			Plan bool
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return Target{}, err
		}
		return Target{Path: a.Path, Real: a.Path, PlanFile: a.Plan}, nil
	}
	return map[string]ToolSpec{"Edit": {Capability: Write, Target: target}}
}

func planArgs(p string, plan bool) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"path": p, "plan": plan})
	return b
}

func TestPlanFileIsAdmittedInPlanWithoutAsking(t *testing.T) {
	f := &approver{answer: Allow}
	a := NewAuthority()
	g := NewGate(a, planSpecs(nil), nil, f.approve, nil)
	args := planArgs("/home/plans/s.md", true)
	require.NoError(t, g.Before(t.Context(), "c1", "Edit", args))
	assert.NoError(t, admitApproved(g, "c1", "Edit"))
	assert.Empty(t, f.reqs)
}

func TestPlanFileIsNotAskedInExecuteWithoutEditsGrant(t *testing.T) {
	f := &approver{answer: Allow}
	a := NewAuthority()
	a.EnterExecute()
	g := NewGate(a, planSpecs(nil), nil, f.approve, nil)
	require.NoError(t, g.Before(t.Context(), "c1", "Edit", planArgs("/home/plans/s.md", true)))
	assert.NoError(t, admitApproved(g, "c1", "Edit"))
	require.NoError(t, g.Before(t.Context(), "c2", "Edit", planArgs("/x", false)))
	assert.Len(t, f.reqs, 1)
}

func TestNonPlanWriteIsStillRefusedInPlan(t *testing.T) {
	a := NewAuthority()
	g := NewGate(a, planSpecs(nil), nil, nil, nil)
	err := g.Before(t.Context(), "c1", "Edit", planArgs("/x", false))
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), `tool "Edit" needs execute mode; the session is in plan mode`)
}

func TestTargetErrorInPlanKeepsTheModeRefusal(t *testing.T) {
	boom := errors.New("boom")
	g := NewGate(NewAuthority(), planSpecs(boom), nil, nil, nil)
	err := g.Before(t.Context(), "c1", "Edit", planArgs("/x", false))
	require.ErrorIs(t, err, ErrDenied)
	assert.NotErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "needs execute mode")

	a := NewAuthority()
	a.EnterExecute()
	g = NewGate(a, planSpecs(boom), nil, nil, nil)
	err = g.Before(t.Context(), "c2", "Edit", planArgs("/x", false))
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "failed to resolve the path")
}

func TestPlanFileThatFailsToResolveReportsTheResolveError(t *testing.T) {
	boom := errors.New("boom")
	specs := map[string]ToolSpec{"Edit": {Capability: Write, Target: func(json.RawMessage) (Target, error) {
		return Target{Path: "/home/plans/s.md", PlanFile: true}, boom
	}}}
	err := NewGate(NewAuthority(), specs, nil, nil, nil).Before(t.Context(), "c1", "Edit", planArgs("/home/plans/s.md", true))
	require.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), "failed to resolve the path")
}

func TestRulesStillApplyToThePlanFile(t *testing.T) {
	d := newDirs(t)
	args := planArgs("/home/plans/s.md", true)

	g := NewGate(NewAuthority(), planSpecs(nil), d.rules(t, Source{Name: "config", Deny: []string{"Edit"}}), nil, nil)
	err := g.Before(t.Context(), "c1", "Edit", args)
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), "denied by rule")

	f := &approver{answer: Allow}
	g = NewGate(NewAuthority(), planSpecs(nil), d.rules(t, Source{Name: "config", Ask: []string{"Edit"}}), f.approve, nil)
	require.NoError(t, g.Before(t.Context(), "c2", "Edit", args))
	assert.Len(t, f.reqs, 1)
}

func TestAdmitAfterModeChangeKeepsPlanFileAndRevokesOthers(t *testing.T) {
	a := NewAuthority()
	a.EnterExecute()
	g := NewGate(a, planSpecs(nil), nil, (&approver{answer: Allow}).approve, nil)
	require.NoError(t, g.Before(t.Context(), "plan", "Edit", planArgs("/home/plans/s.md", true)))
	require.NoError(t, g.Before(t.Context(), "other", "Edit", planArgs("/x", false)))
	a.EnterPlan()
	assert.NoError(t, admitApproved(g, "plan", "Edit"))
	assert.ErrorIs(t, admitApproved(g, "other", "Edit"), ErrRevoked)
}

func TestPlanFileFlagIsPartOfTheBoundTarget(t *testing.T) {
	g := NewGate(NewAuthority(), planSpecs(nil), nil, nil, nil)
	args := planArgs("/home/plans/s.md", true)
	require.NoError(t, g.Before(t.Context(), "c1", "Edit", args))
	_, err := g.Admit("c1", "Edit", args, &Target{Path: "/home/plans/s.md", Real: "/home/plans/s.md"})
	assert.ErrorIs(t, err, ErrChangedCall)
}

// optOutSpec is a sandboxed exec tool whose calls opt out with {"unsandboxed":true}.
var optOutSpec = ToolSpec{Capability: Exec, Sandboxed: true, PlanSandboxed: true, Unsandboxed: func(args json.RawMessage) bool {
	var a struct{ Unsandboxed bool }
	_ = json.Unmarshal(args, &a)
	return a.Unsandboxed
}}

var (
	plainCall  = json.RawMessage(`{"unsandboxed":false}`)
	optOutCall = json.RawMessage(`{"unsandboxed":true}`)
)

func admitArgs(g *Gate, id string, args json.RawMessage) error {
	_, err := g.Admit(id, "Shell", args, nil)
	return err
}

func TestUnsandboxedCallIsRefusedInPlanBeforeRules(t *testing.T) {
	f := &approver{answer: Allow}
	_, g := sandboxedGate(t, optOutSpec, f.approve, Source{Name: "config", Allow: []string{"Shell"}})

	err := g.Before(t.Context(), "c1", "Shell", optOutCall)
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), `tool "Shell" needs execute mode; the session is in plan mode, where it never runs outside the OS sandbox`)
	assert.Empty(t, f.reqs)

	require.NoError(t, g.Before(t.Context(), "c2", "Shell", plainCall))
	assert.NoError(t, admitArgs(g, "c2", plainCall))
	assert.Empty(t, f.reqs)
}

func TestUnsandboxedCallAsksInExecute(t *testing.T) {
	f := &approver{answer: Allow}
	a, g := sandboxedGate(t, optOutSpec, f.approve, Source{Name: "config"})
	a.EnterExecute()

	require.NoError(t, g.Before(t.Context(), "c1", "Shell", plainCall))
	assert.Empty(t, f.reqs)

	require.NoError(t, g.Before(t.Context(), "c2", "Shell", optOutCall))
	assert.Equal(t, []Request{{CallID: "c2", Tool: "Shell", Args: optOutCall}}, f.reqs)
	mode, err := g.Admit("c2", "Shell", optOutCall, nil)
	require.NoError(t, err)
	assert.Equal(t, Execute, mode)
}

func TestUnsandboxedCallObeysRules(t *testing.T) {
	f := &approver{answer: Allow}
	a, g := sandboxedGate(t, optOutSpec, f.approve, Source{Name: "config", Allow: []string{"Shell"}})
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Shell", optOutCall))
	assert.Empty(t, f.reqs)

	a, g = sandboxedGate(t, optOutSpec, f.approve, Source{Name: "config", Deny: []string{"Shell"}})
	a.EnterExecute()
	err := g.Before(t.Context(), "c1", "Shell", optOutCall)
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), "Shell (config)")
	assert.Empty(t, f.reqs)

	a, g = sandboxedGate(t, optOutSpec, f.approve, Source{Name: "config", Ask: []string{"Shell"}})
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Shell", optOutCall))
	assert.Equal(t, []Request{{CallID: "c1", Tool: "Shell", Args: optOutCall, Rule: "Shell (config)"}}, f.reqs)
}

func TestUnsandboxedCallWithoutApproverIsDenied(t *testing.T) {
	a, g := sandboxedGate(t, optOutSpec, nil, Source{Name: "config"})
	a.EnterExecute()

	err := g.Before(t.Context(), "c1", "Shell", optOutCall)

	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), `tool "Shell" needs approval and this run cannot ask for it`)
	assert.NotContains(t, err.Error(), "add an allow rule")
}

func TestUnsandboxedGrantIsRevokedOnModeChange(t *testing.T) {
	// Exec's generation check alone revokes these; forCall in Admit is defence in depth.
	a, g := sandboxedGate(t, optOutSpec, nil, Source{Name: "config", Allow: []string{"Shell"}})
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Shell", optOutCall))
	a.EnterPlan()
	assert.ErrorIs(t, admitArgs(g, "c1", optOutCall), ErrRevoked)

	var b *Authority
	b, g = sandboxedGate(t, optOutSpec, func(context.Context, Request) (Decision, error) {
		b.EnterPlan()
		return Decision{Answer: Allow}, nil
	}, Source{Name: "config"})
	b.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Shell", optOutCall))
	mode, err := g.Admit("c1", "Shell", optOutCall, nil)
	assert.ErrorIs(t, err, ErrRevoked)
	assert.Equal(t, Plan, mode)
}

func TestUnsandboxedFlagIsPartOfTheBoundCall(t *testing.T) {
	a, g := sandboxedGate(t, optOutSpec, nil, Source{Name: "config", Allow: []string{"Shell"}})
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Shell", plainCall))

	assert.ErrorIs(t, admitArgs(g, "c1", optOutCall), ErrChangedCall)
}
