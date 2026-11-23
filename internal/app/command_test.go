package app

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
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

func TestEffortCommandSetsTheLevelForAReasoningModel(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	rt := pigruntimetest.NewRuntime(t, sb)
	thinker := ai.NewFauxProvider(ai.FauxConfig{ProviderID: fauxProvider, Models: []ai.FauxModelDefinition{{ID: "thinker", Reasoning: true}}})
	require.NoError(t, rt.RegisterProvider(thinker.Provider()))
	h := buildHarness(t, rt, thinker, sb, Config{Model: fauxProvider + "/thinker"})
	_, err := h.c.Submit(t.Context(), Main, "/effort high")
	require.ErrorIs(t, err, ErrNoSession)
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	changed, notices := subscribeTo[event.EffortChanged](h.c), subscribeTo[event.Notice](h.c)

	for arg, want := range map[string]error{"": ErrMissingArgument, "loud": ErrUnknownArgument} {
		_, err = h.c.Submit(t.Context(), Main, "/effort "+arg)
		assert.ErrorIs(t, err, want, arg)
	}

	// The faux model tops out at high, so max clamps to it.
	_, err = h.c.Submit(t.Context(), Main, "/effort max")
	require.NoError(t, err)
	receiveEvent(t, changed)
	assert.Equal(t, event.Notice{Text: "Effort set to high for this session."}, receiveEvent(t, notices))

	_, err = h.c.Submit(t.Context(), Main, "/effort low")
	require.NoError(t, err)
	assert.Equal(t, "low", h.c.Agents()[0].Effort)
	receiveEvent(t, changed)
	assert.Equal(t, event.Notice{Text: "Effort set to low for this session."}, receiveEvent(t, notices))
	cs, err := h.c.Arguments("effort")
	require.NoError(t, err)
	var current []string
	for _, cand := range cs {
		if cand.Description == "current" {
			current = append(current, cand.Value)
		}
	}
	assert.Equal(t, []string{"low"}, current)
	assert.Equal(t, h.c.main.sess.Efforts(), values(cs))

	for _, level := range []string{"off", "high"} {
		_, err = h.c.Submit(t.Context(), Main, "/effort "+level)
		require.NoError(t, err)
		receiveEvent(t, changed)
		assert.Equal(t, event.Notice{Text: "Effort set to " + level + " for this session."}, receiveEvent(t, notices))
	}

	// A switch holds the old session until it finishes.
	switched := subscribeTo[event.SessionChanged](h.c)
	h.c.life.Lock()
	_, err = h.c.Submit(t.Context(), Main, "/new")
	assert.NoError(t, err)
	_, err = h.c.Submit(t.Context(), Main, "/effort high")
	assert.ErrorIs(t, err, ErrBusy)
	h.c.life.Unlock()
	receiveEvent(t, switched)
	assertSwitchDone(t, h)
	assert.Equal(t, "medium", h.c.Agents()[0].Effort)
}

func TestEffortCommandRefusesAModelThatDoesNotReason(t *testing.T) {
	h, _ := openHarness(t, Config{})
	notices := subscribeTo[event.Notice](h.c)
	_, err := h.c.Submit(t.Context(), Main, "/effort high")
	require.NoError(t, err)
	assert.Equal(t, event.Notice{Text: "This model does not reason."}, receiveEvent(t, notices))
	assert.Empty(t, h.c.Agents()[0].Effort)
	cs, err := h.c.Arguments("effort")
	require.NoError(t, err)
	assert.Empty(t, cs)
}

func TestModelCommandSwitchesModelAndKeepsItAcrossNew(t *testing.T) {
	pigruntimetest.ClearEnv(t)
	h, _ := openHarness(t, Config{ConfigFile: "/kopi/config.json"})
	other, otherRef := pigruntimetest.RegisterFaux(t, h.c.rt, "kopi-other")
	changed := subscribeTo[event.ModelChanged](h.c)
	notices := subscribeTo[event.Notice](h.c)
	sessions := subscribeTo[event.SessionChanged](h.c)

	_, err := h.c.Submit(t.Context(), Main, "/model "+otherRef)
	require.NoError(t, err)

	assert.Equal(t, event.ModelChanged{Model: otherRef}, receiveEvent(t, changed))
	assert.Equal(t, otherRef, h.c.Model())
	assert.Equal(t, event.Notice{Text: "Using " + otherRef + " until kopi exits. Set \"model\" in /kopi/config.json to keep it."}, receiveEvent(t, notices))
	assertModelChangeDone(t, h)

	_, err = h.c.Submit(t.Context(), Main, "/new")
	require.NoError(t, err)
	receiveEvent(t, sessions)
	requests := make(chan pigruntimetest.Request, 1)
	other.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "answer")})
	h.turn(t, "hello")
	receive(t, requests)
	assert.Equal(t, otherRef, h.c.Model())
}

func TestModelCommandKeepsTheChoiceAcrossResume(t *testing.T) {
	pigruntimetest.ClearEnv(t)
	h, _ := openHarness(t, Config{})
	other, otherRef := pigruntimetest.RegisterFaux(t, h.c.rt, "kopi-other")
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("first answer")})
	h.turn(t, "first question")
	savedID := h.c.Status().SessionID
	changed := subscribeTo[event.ModelChanged](h.c)
	sessions := subscribeTo[event.SessionChanged](h.c)

	_, err := h.c.Submit(t.Context(), Main, "/model "+otherRef)
	require.NoError(t, err)
	receiveEvent(t, changed)
	assertModelChangeDone(t, h)
	_, err = h.c.Submit(t.Context(), Main, "/resume "+savedID)
	require.NoError(t, err)
	assert.Equal(t, savedID, receiveEvent(t, sessions).SessionID)
	assertSwitchDone(t, h)
	requests := make(chan pigruntimetest.Request, 1)
	other.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "answer")})
	h.turn(t, "hello")

	receive(t, requests)
	assert.Equal(t, otherRef, h.c.Model())
}

func TestModelCommandCarriesTheEffortToTheNewModel(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	rt := pigruntimetest.NewRuntime(t, sb)
	thinker := ai.NewFauxProvider(ai.FauxConfig{ProviderID: fauxProvider, Models: []ai.FauxModelDefinition{{ID: "thinker", Reasoning: true}}})
	require.NoError(t, rt.RegisterProvider(thinker.Provider()))
	other := ai.NewFauxProvider(ai.FauxConfig{ProviderID: "kopi-other", Models: []ai.FauxModelDefinition{{ID: "thinker", Reasoning: true}}})
	require.NoError(t, rt.RegisterProvider(other.Provider()))
	h := buildHarness(t, rt, thinker, sb, Config{Model: fauxProvider + "/thinker"})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	changed := subscribeTo[event.ModelChanged](h.c)
	_, err := h.c.Submit(t.Context(), Main, "/effort low")
	require.NoError(t, err)
	require.Equal(t, "low", h.c.Agents()[0].Effort)

	_, err = h.c.Submit(t.Context(), Main, "/model kopi-other/thinker")
	require.NoError(t, err)
	receiveEvent(t, changed)
	assertModelChangeDone(t, h)

	assert.Equal(t, "low", h.c.Agents()[0].Effort, "the level asked for survives the switch rather than the default")
}

func TestModelCommandReadsTheNewProvidersLimits(t *testing.T) {
	pigruntimetest.ClearEnv(t)
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{Limits: true})
	_, otherRef := pigruntimetest.RegisterFaux(t, h.c.rt, "kopi-other")
	limits := subscribeTo[event.LimitsChanged](h.c)
	changed := subscribeTo[event.ModelChanged](h.c)
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	receiveEvent(t, limits)
	require.Equal(t, fauxProvider, h.c.Status().Limits.Provider)

	_, err := h.c.Submit(t.Context(), Main, "/model "+otherRef)
	require.NoError(t, err)
	receiveEvent(t, changed)

	receiveEvent(t, limits)
	assert.Equal(t, "kopi-other", h.c.Status().Limits.Provider, "the minute's throttle does not hold back the new provider")
}

func TestModelCommandRefusesDuringNew(t *testing.T) {
	pigruntimetest.ClearEnv(t)
	h, _ := openHarness(t, Config{})
	_, otherRef := pigruntimetest.RegisterFaux(t, h.c.rt, "kopi-other")
	switched := subscribeTo[event.SessionChanged](h.c)

	// A switch holds the old session until it finishes.
	h.c.life.Lock()
	_, err := h.c.Submit(t.Context(), Main, "/new")
	require.NoError(t, err)
	_, err = h.c.Submit(t.Context(), Main, "/model "+otherRef)
	assert.ErrorIs(t, err, ErrBusy)
	h.c.life.Unlock()

	receiveEvent(t, switched)
	assertSwitchDone(t, h)
	assert.NotEqual(t, otherRef, h.c.Model())
}

func TestModelCommandFailures(t *testing.T) {
	pigruntimetest.ClearEnv(t)
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	before := h.c.Model()

	_, err := h.c.Submit(t.Context(), Main, "/model")
	assert.ErrorIs(t, err, ErrMissingArgument)

	_, err = h.c.Submit(t.Context(), Main, "/model sonnet")
	assert.ErrorIs(t, err, ErrUnknownArgument)
	assert.Equal(t, before, h.c.Model())

	errs := subscribeTo[event.Error](h.c)
	_, err = h.c.Submit(t.Context(), Main, "/model "+fauxProvider+"/missing")
	require.NoError(t, err)
	assert.ErrorIs(t, receiveEvent(t, errs).Err, pigruntime.ErrUnknownModel)
	assert.Equal(t, before, h.c.Model())

	_, otherRef := pigruntimetest.RegisterFaux(t, h.c.rt, "kopi-other")
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})
	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), Main, "/model "+otherRef)
	assert.ErrorIs(t, err, ErrWorkStillRunning)
	assert.Equal(t, before, h.c.Model())
	close(release)
	wait(t, w.Done())
}

func TestModelCommandHoldsTurnsAndSwitchesUntilTheChangeEnds(t *testing.T) {
	pigruntimetest.ClearEnv(t)
	h, _ := openHarness(t, Config{Wake: true})
	other, otherRef := pigruntimetest.RegisterFaux(t, h.c.rt, "kopi-other")
	changed := subscribeTo[event.ModelChanged](h.c)
	ends := turnEnds(h.c)
	other.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("noted")})

	// The change's goroutine waits for life, as it would for the runtime.
	h.c.life.Lock()
	_, err := h.c.Submit(t.Context(), Main, "/model "+otherRef)
	require.NoError(t, err)
	h.tasks.end(endedTask("t1"))
	for _, text := range []string{"/model " + otherRef, "/new", "/resume some-id", "/login " + fauxProvider, "/logout " + fauxProvider, "/effort high", "hello"} {
		_, err = h.c.Submit(t.Context(), Main, text)
		assert.ErrorIs(t, err, ErrBusy, text)
	}
	assert.Zero(t, other.CallCount()+h.faux.CallCount(), "the notice does not wake main during the change")
	h.c.life.Unlock()

	receiveEvent(t, changed)
	receiveEnd(t, ends)
	assert.Equal(t, 1, other.CallCount(), "the woken turn runs on the new model")
	assertSwitchDone(t, h)
}

func TestModelChangeThatAcquiresLifeAfterCloseChangesNothing(t *testing.T) {
	pigruntimetest.ClearEnv(t)
	h, _ := openHarness(t, Config{})
	_, otherRef := pigruntimetest.RegisterFaux(t, h.c.rt, "kopi-other")
	before := h.c.Model()

	// The change's goroutine waits for life. Close sets closed under mu before
	// it releases life, so the goroutine can only run after it; setting it here
	// fixes that order without relying on which waiter the mutex wakes first.
	h.c.life.Lock()
	_, err := h.c.Submit(t.Context(), Main, "/model "+otherRef)
	require.NoError(t, err)
	h.c.mu.Lock()
	h.c.closed = true
	h.c.mu.Unlock()
	h.c.life.Unlock()

	// The change's goroutine ends with endModelChange, which takes mu.
	deadline := time.Now().Add(waitLimit)
	for !modelChangeEnded(h.c) {
		require.True(t, time.Now().Before(deadline), "the model change never ended")
		runtime.Gosched()
	}
	assert.Equal(t, before, h.c.Model())
}

// assertModelChangeDone waits for the model change that emitted its events to release the controller.
func assertModelChangeDone(t *testing.T, h *harness) {
	t.Helper()
	h.c.life.Lock()
	defer h.c.life.Unlock()
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	assert.False(t, h.c.changingModel)
}

func modelChangeEnded(c *Controller) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.changingModel
}

func TestModelCommandLeavesRunningSubagentsAlone(t *testing.T) {
	pigruntimetest.ClearEnv(t)
	def := agentDef("explorer")
	def.Model = ""
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{}, def)
	other, otherRef := pigruntimetest.RegisterFaux(t, a.c.rt, "kopi-other")
	gate := make(chan struct{})
	oldRequests := make(chan pigruntimetest.Request, 1)
	a.faux.SetResponses([]ai.FauxResponseStep{gated(gate, "first"), pigruntimetest.Capture(t, oldRequests, "second")})
	newRequests := make(chan pigruntimetest.Request, 1)
	other.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, newRequests, "done")})
	changed := subscribeTo[event.ModelChanged](a.c)
	id := a.launch(t, "explorer", "find it", "go")

	_, err := a.c.Submit(t.Context(), Main, "/model "+otherRef)
	require.NoError(t, err)
	receiveEvent(t, changed)
	_, err = a.c.message(t.Context(), id, "also check b")
	assert.NoError(t, err, "the change does not touch the subagent")
	close(gate)
	texts := receive(t, oldRequests).UserTexts
	assert.Equal(t, "also check b", texts[len(texts)-1], "the running subagent keeps the old model")
	assert.Zero(t, other.CallCount(), "the running subagent did not move to the new model")

	a.launch(t, "explorer", "find more", "again")
	assert.Equal(t, []string{"again"}, receive(t, newRequests).UserTexts, "a later subagent inherits the new model")
}

func TestModelCommandReportsAFailedSaveAfterSwitching(t *testing.T) {
	pigruntimetest.ClearEnv(t)
	h, sb := openHarness(t, Config{})
	_, otherRef := pigruntimetest.RegisterFaux(t, h.c.rt, "kopi-other")
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("answer")})
	h.turn(t, "hello")
	var files []string
	require.NoError(t, filepath.WalkDir(sb.KopiHome, func(path string, _ fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}
		return err
	}))
	require.Len(t, files, 1)
	// The write is refused as the file is opened for appending.
	require.NoError(t, os.Chmod(files[0], 0o400))
	t.Cleanup(func() { _ = os.Chmod(files[0], 0o600) })
	if f, err := os.OpenFile(files[0], os.O_WRONLY|os.O_APPEND, 0); err == nil {
		_ = f.Close()
		t.Skip("file permissions do not stop this user from appending")
	}
	changed := subscribeTo[event.ModelChanged](h.c)
	errs := subscribeTo[event.Error](h.c)
	notices := subscribeTo[event.Notice](h.c)

	_, err := h.c.Submit(t.Context(), Main, "/model "+otherRef)
	require.NoError(t, err)

	assert.Equal(t, event.ModelChanged{Model: otherRef}, receiveEvent(t, changed))
	assert.ErrorIs(t, receiveEvent(t, errs).Err, pigruntime.ErrModelNotSaved)
	assert.Empty(t, notices)
	assert.Equal(t, otherRef, h.c.Model())
}

func TestModelCommandWithoutCredentialPointsAtLogin(t *testing.T) {
	pigruntimetest.ClearEnv(t)
	h, _ := openHarness(t, Config{})
	pigruntimetest.RegisterLoginProvider(t, h.c.rt, "kopi-login")
	before := h.c.Model()
	errs := subscribeTo[event.Error](h.c)

	_, err := h.c.Submit(t.Context(), Main, "/model kopi-login/login-1")
	require.NoError(t, err)

	got := receiveEvent(t, errs).Err
	assert.ErrorIs(t, got, pigruntime.ErrNoCredential)
	assert.EqualError(t, got, `failed to change model to "kopi-login/login-1": provider "kopi-login": no credential: run /login kopi-login`)
	assert.Equal(t, before, h.c.Model())
}

func TestArgumentsListsAvailableModelsForModel(t *testing.T) {
	pigruntimetest.ClearEnv(t)
	h, _ := openHarness(t, Config{})
	_, otherRef := pigruntimetest.RegisterFaux(t, h.c.rt, "kopi-other")

	cs, err := h.c.Arguments("model")

	require.NoError(t, err)
	assert.Equal(t, []Candidate{
		{Value: fauxProvider + "/faux-1", Description: "Faux Model (current)"},
		{Value: otherRef, Description: "Faux Model"},
	}, cs)
}
