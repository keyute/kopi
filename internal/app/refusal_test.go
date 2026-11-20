package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
)

// gateError is the error a real gate gives one Shell call. Every text under
// test comes from policy.Gate, so a rewording there fails here.
func gateError(t *testing.T, plan bool, args string, rules policy.Source, approve policy.Approver, review policy.Reviewer) error {
	t.Helper()
	optOut := func(args json.RawMessage) bool {
		var a struct{ Unsandboxed bool }
		_ = json.Unmarshal(args, &a)
		return a.Unsandboxed
	}
	specs := map[string]policy.ToolSpec{
		"Shell": {Capability: policy.Exec},
		"Box":   {Capability: policy.Exec, Sandboxed: true, PlanSandboxed: true, Unsandboxed: optOut},
		"Edit":  {Capability: policy.Write},
	}
	r, err := policy.NewRules(t.TempDir(), t.TempDir(), specs, rules)
	require.NoError(t, err)
	a := policy.NewAuthority()
	if !plan {
		a.EnterExecute()
	}
	g := policy.NewGate(a, specs, r, approve, review)
	name := "Shell"
	switch args {
	case "edit":
		name, args = "Edit", "{}"
	case "box":
		name, args = "Box", `{"unsandboxed":true}`
	default:
		args = "{}"
	}
	err = g.Before(t.Context(), "c1", name, json.RawMessage(args))
	require.Error(t, err)
	return err
}

func answer(a policy.Answer, reason string) policy.Approver {
	return func(context.Context, policy.Request) (policy.Decision, error) {
		return policy.Decision{Answer: a, Reason: reason}, nil
	}
}

func verdict(v policy.Verdict, reason string) policy.Reviewer {
	return func(context.Context, policy.Request) (policy.Review, error) {
		return policy.Review{Verdict: v, Reason: reason}, nil
	}
}

func TestClassifyGateErrors(t *testing.T) {
	none := policy.Source{Name: "config"}
	for _, tc := range []struct {
		name          string
		tool          string
		err           func(t *testing.T) error
		kind          event.EndKind
		summary, body string
	}{
		{"deny rule", "Shell", func(t *testing.T) error {
			return gateError(t, false, "", policy.Source{Name: "config", Deny: []string{"Shell"}}, nil, nil)
		}, event.EndDenied, "denied by rule Shell (config)", ""},
		{"plan mode", "Edit", func(t *testing.T) error { return gateError(t, true, "edit", none, nil, nil) }, event.EndDenied, "denied · plan mode", ""},
		{"plan mode without the sandbox", "Shell", func(t *testing.T) error { return gateError(t, true, "", none, nil, nil) }, event.EndDenied, "denied · plan mode needs the sandbox, which is off or unavailable", ""},
		{"plan mode unsandboxed", "Box", func(t *testing.T) error { return gateError(t, true, "box", none, nil, nil) }, event.EndDenied, "denied · plan mode never runs a command outside the sandbox", ""},
		{"nil approver", "Shell", func(t *testing.T) error { return gateError(t, false, "", none, nil, nil) }, event.EndDenied, "denied · this run cannot ask for approval", ""},
		{"ask rule and nil approver", "Shell", func(t *testing.T) error {
			return gateError(t, false, "", policy.Source{Name: "config", Ask: []string{"Shell"}}, nil, nil)
		}, event.EndDenied, "denied · rule Shell (config) asks and this run cannot ask", ""},
		{"reviewer deny", "Shell", func(t *testing.T) error {
			return gateError(t, false, "", none, nil, verdict(policy.VerdictDeny, "too risky"))
		}, event.EndDenied, "denied by the classifier: too risky", ""},
		{"reviewer deny without reason", "Shell", func(t *testing.T) error {
			return gateError(t, false, "", none, nil, verdict(policy.VerdictDeny, ""))
		}, event.EndDenied, "denied by the classifier", ""},
		{"reviewer undecided", "Shell", func(t *testing.T) error {
			return gateError(t, false, "", none, nil, verdict(policy.VerdictAsk, ""))
		}, event.EndDenied, "denied · the classifier could not decide and this run cannot ask", ""},
		{"reject with feedback", "Shell", func(t *testing.T) error {
			return gateError(t, false, "", none, answer(policy.Reject, "use make\nnot go"), nil)
		}, event.EndRejected, "rejected", "use make\nnot go"},
		{"reject", "Shell", func(t *testing.T) error { return gateError(t, false, "", none, answer(policy.Reject, ""), nil) }, event.EndRejected, "rejected", ""},
		{"review cancelled", "Shell", func(t *testing.T) error {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			a := policy.NewAuthority()
			a.EnterExecute()
			g := policy.NewGate(a, map[string]policy.ToolSpec{"Shell": {Capability: policy.Exec}}, nil, nil, verdict(policy.VerdictAllow, ""))
			err := g.Before(ctx, "c1", "Shell", json.RawMessage("{}"))
			require.Error(t, err)
			return err
		}, event.EndInterrupted, "interrupted", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err(t)
			kind, summary, body, ok := classify(tc.tool, err.Error())
			require.True(t, ok, err.Error())
			assert.Equal(t, tc.kind, kind, err.Error())
			assert.Equal(t, tc.summary, summary)
			assert.Equal(t, tc.body, body)
		})
	}
}

func TestClassifyRevokedGrant(t *testing.T) {
	specs := map[string]policy.ToolSpec{"Shell": {Capability: policy.Exec}}
	a := policy.NewAuthority()
	a.EnterExecute()
	g := policy.NewGate(a, specs, nil, answer(policy.Allow, ""), nil)
	require.NoError(t, g.Before(t.Context(), "c1", "Shell", json.RawMessage("{}")))
	a.EnterPlan()
	_, err := g.Admit("c1", "Shell", json.RawMessage("{}"), nil)
	require.ErrorIs(t, err, policy.ErrRevoked)

	kind, summary, body, ok := classify("Shell", err.Error())
	assert.True(t, ok)
	assert.Equal(t, event.EndDenied, kind)
	assert.Equal(t, "denied · the mode changed after approval", summary)
	assert.Empty(t, body)
}

// The Bash tool's own plan-mode refusals, which the gate does not produce.
func TestClassifyToolDenials(t *testing.T) {
	for text, summary := range map[string]string{
		"failed to run command: an unsandboxed command runs only in execute mode: " + policy.ErrDenied.Error():                            "denied · plan mode never runs a command outside the sandbox",
		"failed to run command: plan mode runs commands only in the OS sandbox, which is off or unavailable: " + policy.ErrDenied.Error(): "denied · plan mode needs the sandbox, which is off or unavailable",
		"something else: " + policy.ErrDenied.Error(): "denied",
	} {
		kind, got, _, ok := classify("Bash", text)
		assert.True(t, ok, text)
		assert.Equal(t, event.EndDenied, kind)
		assert.Equal(t, summary, got)
	}
}

func TestClassifyCancellation(t *testing.T) {
	for _, text := range []string{context.Canceled.Error(), "failed to run: " + context.Canceled.Error()} {
		kind, summary, _, ok := classify("Bash", text)
		assert.True(t, ok, text)
		assert.Equal(t, event.EndInterrupted, kind)
		assert.Equal(t, "interrupted", summary)
	}
}

func TestClassifyLeavesFailuresAlone(t *testing.T) {
	for _, text := range []string{
		"exit status 1",
		context.Canceled.Error() + " later",
		`tool "Other": rejected by the user`,
		errors.New("boom").Error(),
	} {
		_, _, _, ok := classify("Bash", text)
		assert.False(t, ok, text)
	}
}
