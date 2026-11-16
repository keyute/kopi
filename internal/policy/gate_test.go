package policy

import (
	"context"
	"encoding/json"
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

// approver answers every request with answer and records it.
type approver struct {
	answer Answer
	reqs   []Request
}

func (f *approver) approve(_ context.Context, r Request) (Answer, error) {
	f.reqs = append(f.reqs, r)
	return f.answer, nil
}

func newTestGate(rules *Rules, approve Approver) (*Authority, *Gate) {
	a := NewAuthority()
	return a, NewGate(a, testSpecs, rules, approve)
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
	require.NoError(t, g.Admit("c1", "Read"))
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c2", "Read", pathArgs("/x")))
	require.NoError(t, g.Admit("c2", "Read"))
}

func TestReadGrantSurvivesModeChanges(t *testing.T) {
	a, g := newTestGate(nil, nil)
	require.NoError(t, g.Before(t.Context(), "c1", "Read", pathArgs("/x")))
	a.EnterExecute()
	a.EnterPlan()
	assert.NoError(t, g.Admit("c1", "Read"))
}

func TestPlanDeniesNonReadBeforeRules(t *testing.T) {
	d := newDirs(t)
	f := &approver{answer: Allow}
	_, g := newTestGate(d.rules(t, Source{Name: "config", Allow: []string{"Edit", "*"}}), f.approve)
	for _, tool := range []string{"Edit", "Bash"} {
		err := g.Before(t.Context(), "c1", tool, pathArgs("/x"))
		require.ErrorIs(t, err, ErrDenied)
		assert.Contains(t, err.Error(), `tool "`+tool+`" needs execute mode; the session is in plan mode`)
	}
	assert.Empty(t, f.reqs)
}

func TestExecuteAdmitsEveryCapabilityOnceApproved(t *testing.T) {
	a, g := newTestGate(nil, (&approver{answer: Allow}).approve)
	a.EnterExecute()
	for _, tool := range []string{"Read", "Edit", "Bash"} {
		require.NoError(t, g.Before(t.Context(), "c-"+tool, tool, pathArgs("/x")), tool)
		assert.NoError(t, g.Admit("c-"+tool, tool), tool)
	}
}

func TestExecGrantIsRevokedOnReturnToPlan(t *testing.T) {
	a, g := newTestGate(nil, (&approver{answer: Allow}).approve)
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Bash", nil))
	a.EnterPlan()
	assert.ErrorIs(t, g.Admit("c1", "Bash"), ErrRevoked)
}

func TestStaleExecGrantIsRevokedAfterReturningToExecute(t *testing.T) {
	a, g := newTestGate(nil, (&approver{answer: Allow}).approve)
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Bash", nil))
	a.EnterPlan()
	a.EnterExecute()
	assert.ErrorIs(t, g.Admit("c1", "Bash"), ErrRevoked)
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
	assert.ErrorIs(t, g.Admit("c1", "Edit"), ErrNoGrant)
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
		assert.NoError(t, g.Admit("c1", tc.tool))
	}
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
	assert.NoError(t, g.Admit("c1", "Edit"))
}

func TestRejectedCallIsRefused(t *testing.T) {
	a, g := newTestGate(nil, (&approver{answer: Reject}).approve)
	a.EnterExecute()
	err := g.Before(t.Context(), "c1", "Edit", pathArgs("/x"))
	require.ErrorIs(t, err, ErrRejected)
	assert.Contains(t, err.Error(), "stop and wait for their instructions")
	assert.ErrorIs(t, g.Admit("c1", "Edit"), ErrNoGrant)
}

func TestAllowEditsSkipsLaterDefaultEditAsksOnly(t *testing.T) {
	d := newDirs(t)
	f := &approver{answer: AllowEdits}
	rules := d.rules(t, BuiltIn, Source{Name: "config", Deny: []string{"Edit(secret)"}})
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
	a, g := newTestGate(nil, func(ctx context.Context, _ Request) (Answer, error) {
		close(waiting)
		<-ctx.Done()
		return Reject, ctx.Err()
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
	assert.ErrorIs(t, g.Admit("c1", "Edit"), ErrNoGrant)
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
	a, g := newTestGate(d.rules(t, BuiltIn), nil)
	a.EnterExecute()
	err := g.Before(t.Context(), "c1", "Edit", pathArgs(filepath.Join(d.ws, ".git/config")))
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), `rule Edit(.git) (built-in) requires approval for tool "Edit" and this run cannot ask for it`)
	assert.NotContains(t, err.Error(), "allowed-tools")
}

func TestModeChangeDuringApprovalRevokes(t *testing.T) {
	var a *Authority
	a, g := newTestGate(nil, func(context.Context, Request) (Answer, error) {
		a.EnterPlan()
		a.EnterExecute()
		return Allow, nil
	})
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Edit", pathArgs("/x")))
	assert.ErrorIs(t, g.Admit("c1", "Edit"), ErrRevoked)
}

func TestTargetErrorRefuses(t *testing.T) {
	f := &approver{answer: Allow}
	a, g := newTestGate(nil, f.approve)
	a.EnterExecute()
	err := g.Before(t.Context(), "c1", "Edit", json.RawMessage(`not json`))
	var syntax *json.SyntaxError
	require.ErrorAs(t, err, &syntax)
	assert.Empty(t, f.reqs)
	assert.ErrorIs(t, g.Admit("c1", "Edit"), ErrNoGrant)
}

func TestAdmitWithoutBeforeHasNoGrant(t *testing.T) {
	_, g := newTestGate(nil, nil)
	assert.ErrorIs(t, g.Admit("c1", "Read"), ErrNoGrant)
}

func TestGrantIsConsumedOnce(t *testing.T) {
	_, g := newTestGate(nil, nil)
	require.NoError(t, g.Before(t.Context(), "c1", "Read", pathArgs("/x")))
	require.NoError(t, g.Admit("c1", "Read"))
	assert.ErrorIs(t, g.Admit("c1", "Read"), ErrNoGrant)
}

func TestGrantForOneToolIsNotAdmittedForAnother(t *testing.T) {
	a, g := newTestGate(nil, nil)
	a.EnterExecute()
	require.NoError(t, g.Before(t.Context(), "c1", "Read", pathArgs("/x")))
	assert.ErrorIs(t, g.Admit("c1", "Bash"), ErrNoGrant)
}

func TestNewGateCopiesTools(t *testing.T) {
	tools := map[string]ToolSpec{"Read": {Capability: Read}}
	g := NewGate(NewAuthority(), tools, nil, nil)
	tools["Read"] = ToolSpec{Capability: Exec}
	tools["Bash"] = ToolSpec{Capability: Read}
	assert.NoError(t, g.Before(t.Context(), "c1", "Read", nil))
	assert.ErrorIs(t, g.Before(t.Context(), "c2", "Bash", nil), ErrUnknownTool)
}
