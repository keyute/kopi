package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

func admitIn(mode policy.Mode) func(policy.Target) (policy.Mode, error) {
	return func(policy.Target) (policy.Mode, error) { return mode, nil }
}

// reviewing returns a ReviewPlan that answers with a and mode, and counts calls.
func reviewing(t *testing.T, a PlanAnswer, mode policy.Mode, calls *int, plans *Plans, rewrite string) ReviewPlan {
	return func(context.Context, string) (PlanAnswer, policy.Mode, error) {
		*calls++
		if rewrite != "" {
			require.NoError(t, os.WriteFile(plans.Path(), []byte(rewrite), 0o600))
		}
		return a, mode, nil
	}
}

func planFile(t *testing.T, text string) *Plans {
	t.Helper()
	plans, err := NewPlans(filepath.Join(t.TempDir(), "plans"))
	require.NoError(t, err)
	require.NoError(t, plans.Select("s1"))
	if text != "" {
		require.NoError(t, os.WriteFile(plans.Path(), []byte(text), 0o600))
	}
	return plans
}

func TestExitPlanModeDescribesItself(t *testing.T) {
	tool := NewExitPlanMode(nil, nil)

	assert.Equal(t, "ExitPlanMode", tool.Name)
	assert.Equal(t, "Puts the plan in the session's plan file before the user and waits for their answer. It takes no arguments: it reads the plan from that file, which the plan-mode message names. The user approves it, which switches the session to execute mode, asks for changes with feedback, or dismisses it; the result says which, and whether the user edited the file. It works only in plan mode.", tool.Description)
	assert.Equal(t, map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}, tool.Parameters)
	assert.Equal(t, policy.Read, tool.Capability)
	assert.True(t, tool.Exclusive)
	assert.False(t, tool.Ask)
	assert.Nil(t, tool.Target)
	assert.Equal(t, "Plan", tool.Display.Title(json.RawMessage(`{}`)))
}

func TestExitPlanModeRefusesOutsidePlanMode(t *testing.T) {
	plans := planFile(t, "# Plan\n")
	calls := 0
	tool := NewExitPlanMode(plans, reviewing(t, PlanAnswer{Approved: true}, policy.Execute, &calls, plans, ""))

	_, err := tool.Run(t.Context(), json.RawMessage(`{}`), admitIn(policy.Execute))

	assert.ErrorIs(t, err, ErrNotPlanMode)
	assert.Zero(t, calls)
}

func TestExitPlanModeStopsWhenNotAdmitted(t *testing.T) {
	denied := errors.New("denied")
	plans := planFile(t, "# Plan\n")
	calls := 0
	tool := NewExitPlanMode(plans, reviewing(t, PlanAnswer{}, policy.Plan, &calls, plans, ""))

	_, err := tool.Run(t.Context(), json.RawMessage(`{}`), func(policy.Target) (policy.Mode, error) { return policy.Plan, denied })

	assert.ErrorIs(t, err, denied)
	assert.Zero(t, calls)
}

func TestExitPlanModeNeedsAPlan(t *testing.T) {
	for name, text := range map[string]string{"missing": "", "blank": " \n\t"} {
		t.Run(name, func(t *testing.T) {
			plans := planFile(t, text)
			calls := 0
			tool := NewExitPlanMode(plans, reviewing(t, PlanAnswer{}, policy.Plan, &calls, plans, ""))

			_, err := tool.Run(t.Context(), json.RawMessage(`{}`), admitIn(policy.Plan))

			assert.ErrorIs(t, err, ErrNoPlan)
			assert.ErrorContains(t, err, "write the plan to "+plans.Path()+" first")
			assert.Zero(t, calls)
		})
	}

	_, err := NewExitPlanMode(nil, nil).Run(t.Context(), json.RawMessage(`{}`), admitIn(policy.Plan))
	assert.ErrorIs(t, err, ErrNoPlan)
	assert.ErrorContains(t, err, "this session has no plan file")
}

func TestExitPlanModeRefusesArguments(t *testing.T) {
	_, err := NewExitPlanMode(nil, nil).Run(t.Context(), json.RawMessage(`{"plan":"x"}`), admitIn(policy.Plan))

	assert.ErrorIs(t, err, ErrInvalidArgs)
}

func TestExitPlanModeReportsTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answer  PlanAnswer
		mode    policy.Mode
		rewrite string
		want    string
	}{
		{"approved", PlanAnswer{Approved: true}, policy.Execute, "", "The user approved the plan. The session is in execute mode."},
		{"feedback", PlanAnswer{Feedback: "smaller steps"}, policy.Plan, "", "The user asked for changes to the plan. The session is in plan mode. Their feedback:\nsmaller steps"},
		{"dismissed", PlanAnswer{}, policy.Plan, "", "The user dismissed the plan without answering. The session is in plan mode."},
		{"blank feedback dismisses", PlanAnswer{Feedback: " \n"}, policy.Plan, "", "The user dismissed the plan without answering. The session is in plan mode."},
		{"approved and edited", PlanAnswer{Approved: true}, policy.Execute, "# Edited\n", "The user approved the plan. The user edited the plan file before answering. The session is in execute mode."},
		{"feedback and edited", PlanAnswer{Feedback: "x"}, policy.Plan, "# Edited\n", "The user asked for changes to the plan. The user edited the plan file before answering. The session is in plan mode. Their feedback:\nx"},
		{"dismissed and edited", PlanAnswer{}, policy.Plan, "# Edited\n", "The user dismissed the plan without answering. The user edited the plan file before answering. The session is in plan mode."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plans := planFile(t, "# Plan\n")
			calls := 0
			tool := NewExitPlanMode(plans, reviewing(t, tc.answer, tc.mode, &calls, plans, tc.rewrite))

			out, err := tool.Run(t.Context(), json.RawMessage(`{}`), admitIn(policy.Plan))

			require.NoError(t, err)
			assert.Equal(t, tc.want, out.Text)
			assert.Equal(t, 1, calls)
		})
	}
}

func TestExitPlanModeShowsTheFileTextAndPassesReviewErrorThrough(t *testing.T) {
	plans := planFile(t, "# Plan\n")
	failed := errors.New("no screen")
	var shown string
	tool := NewExitPlanMode(plans, func(_ context.Context, plan string) (PlanAnswer, policy.Mode, error) {
		shown = plan
		return PlanAnswer{}, policy.Plan, failed
	})

	_, err := tool.Run(t.Context(), json.RawMessage(`{}`), admitIn(policy.Plan))

	assert.Equal(t, failed, err)
	assert.Equal(t, "# Plan\n", shown)
}

func TestExitPlanModeSummary(t *testing.T) {
	summary := NewExitPlanMode(nil, nil).Display.Summary

	assert.Equal(t, "approved", summary("The user approved the plan. The session is in execute mode."))
	assert.Equal(t, "approved", summary("The user approved the plan. The user edited the plan file before answering. The session is in execute mode."))
	assert.Equal(t, "revision requested", summary("The user asked for changes to the plan. The session is in plan mode. Their feedback:\nx"))
	assert.Equal(t, "dismissed", summary("The user dismissed the plan without answering. The session is in plan mode."))
	assert.Empty(t, summary("something else"))
}
