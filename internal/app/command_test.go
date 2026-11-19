package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		line, name, args string
		isCommand        bool
	}{
		{line: "/plan", name: "plan", isCommand: true},
		{line: "/resume abc", name: "resume", args: "abc", isCommand: true},
		{line: "  /status  ", name: "status", isCommand: true},
		{line: "/frobnicate now", name: "frobnicate", args: "now", isCommand: true},
		{line: "//x"},
		{line: "explain this repo"},
		{line: ""},
	} {
		t.Run(tc.line, func(t *testing.T) {
			name, args, isCommand := parse(tc.line)
			assert.Equal(t, tc.name, name)
			assert.Equal(t, tc.args, args)
			assert.Equal(t, tc.isCommand, isCommand)
		})
	}
}

func TestArgumentsListsSessionsForResume(t *testing.T) {
	h, _ := openHarness(t, Config{})
	cs, err := h.c.Arguments("resume")
	require.NoError(t, err)
	assert.Empty(t, cs)

	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("answer")})
	h.turn(t, "first line\nsecond line")
	cs, err = h.c.Arguments("resume")
	require.NoError(t, err)
	require.Len(t, cs, 1)
	assert.Equal(t, h.c.Status().SessionID, cs[0].Value)
	assert.NotEmpty(t, cs[0].Label)
	assert.Contains(t, cs[0].Description, "first line")
	assert.NotContains(t, cs[0].Description, "second line")

	for _, name := range []string{"plan", "nope"} {
		cs, err := h.c.Arguments(name)
		require.NoError(t, err)
		assert.Nil(t, cs, name)
	}
}

func TestTasksCommandShowsTasksIdleAndMidTurn(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})

	_, err := h.c.Submit(t.Context(), Main, "/tasks")
	assert.ErrorIs(t, err, ErrShowTasks)

	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	got, err := h.c.Submit(t.Context(), Main, "/tasks")
	assert.ErrorIs(t, err, ErrShowTasks)
	assert.Nil(t, got)
	assert.Empty(t, h.c.TakeQueued(Main))
	close(release)
	wait(t, w.Done())
}

func TestPlanCommandEntersPlanModeAndOpensThePlanFile(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	plans := tool.NewPlans(filepath.Join(sb.Home, "plans"))
	h := newHarness(t, sb, Config{Plans: plans})
	notices := subscribeTo[event.Notice](h.c)

	_, err := h.c.Submit(t.Context(), Main, "/plan open")
	require.NoError(t, err, "without a session")
	assert.Equal(t, event.Notice{Text: "This session has no plan file yet."}, receiveEvent(t, notices))

	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	h.c.EnterExecute()
	_, err = h.c.Submit(t.Context(), Main, "/plan open")
	require.NoError(t, err, "before the plan file exists")
	assert.Equal(t, event.Notice{Text: "This session has no plan file yet."}, receiveEvent(t, notices))
	assert.Equal(t, policy.Execute, h.c.Status().Mode)

	require.NoError(t, os.WriteFile(h.c.PlanFile(), []byte("# Plan\n"), 0o600))
	_, err = h.c.Submit(t.Context(), Main, "/plan open")
	assert.ErrorIs(t, err, ErrOpenPlan)
	assert.Equal(t, policy.Execute, h.c.Status().Mode)

	_, err = h.c.Submit(t.Context(), Main, "/plan something")
	assert.ErrorIs(t, err, ErrUnknownArgument)
	assert.Equal(t, policy.Execute, h.c.Status().Mode)

	_, err = h.c.Submit(t.Context(), Main, "/plan")
	require.NoError(t, err)
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
}
