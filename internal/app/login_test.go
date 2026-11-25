package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/tool"
)

const loginProvider = "kopi-login"

// loginHarness is an open harness with a login provider and a channel of
// every event delivered after it.
type loginHarness struct {
	*harness
	events chan event.Event
}

func openLoginHarness(t *testing.T, cfg Config) *loginHarness {
	t.Helper()
	h, _ := openHarness(t, cfg)
	pigruntimetest.RegisterLoginProvider(t, h.c.rt, loginProvider)
	lh := &loginHarness{harness: h, events: make(chan event.Event, 64)}
	h.c.Subscribe(func(ev event.Event) { lh.events <- ev })
	return lh
}

// next returns the first later event that match accepts.
func (h *loginHarness) next(t *testing.T, match func(event.Event) bool) event.Event {
	t.Helper()
	for {
		select {
		case ev := <-h.events:
			if match(ev) {
				return ev
			}
		case <-time.After(waitLimit):
			t.Fatal("timed out waiting for an event")
		}
	}
}

func (h *loginHarness) notice(t *testing.T, text string) {
	t.Helper()
	h.next(t, func(ev event.Event) bool { n, ok := ev.(event.Notice); return ok && n.Text == text })
}

// startLogin starts /login and returns the run and its first question.
func (h *loginHarness) startLogin(t *testing.T) (*loginRun, event.LoginRequested) {
	t.Helper()
	_, err := h.c.Submit(t.Context(), Main, "/login "+loginProvider)
	require.NoError(t, err)
	req := h.next(t, func(ev event.Event) bool { _, ok := ev.(event.LoginRequested); return ok }).(event.LoginRequested)
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	return h.c.login, req
}

func (h *loginHarness) loggedIn(t *testing.T) bool {
	t.Helper()
	providers, err := h.c.rt.Providers(t.Context())
	require.NoError(t, err)
	for _, p := range providers {
		if p.ID == loginProvider {
			return p.LoggedIn
		}
	}
	t.Fatal("provider not registered")
	return false
}

func TestLoginAnswerStoresCredential(t *testing.T) {
	h := openLoginHarness(t, Config{})
	run, req := h.startLogin(t)
	assert.Equal(t, event.LoginRequested{Token: req.Token, Provider: loginProvider, Message: "Code:"}, req)
	assert.False(t, h.c.AnswerLogin(req.Token+1, "x"))

	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	assert.False(t, h.c.AnswerLogin(req.Token, "again"))
	h.notice(t, "Logged in to "+loginProvider+".")
	wait(t, run.done)

	evs := h.rec.all()
	assert.Contains(t, evs, event.Activity{Text: "Logging in to " + loginProvider + "…"})
	assert.Contains(t, evs, event.Notice{Text: pigruntimetest.LoginProgress})
	assert.Contains(t, evs, event.LoginEnded{Token: req.Token})
	assert.Empty(t, ofType[event.Error](evs))
	assert.Equal(t, event.Activity{}, evs[len(evs)-2])
	assert.True(t, h.loggedIn(t))
}

func TestLoginRequestCarriesOptionsAndSecretPrompt(t *testing.T) {
	const provider = "kopi-prompt"
	h := openLoginHarness(t, Config{})
	pigruntimetest.RegisterPromptingLoginProvider(t, h.c.rt, provider)
	isRequest := func(ev event.Event) bool { _, ok := ev.(event.LoginRequested); return ok }

	_, err := h.c.Submit(t.Context(), Main, "/login "+provider)
	require.NoError(t, err)
	team := h.next(t, isRequest).(event.LoginRequested)
	assert.Equal(t, event.LoginRequested{Token: team.Token, Provider: provider, Message: "Team:", Options: []event.LoginOption{
		{ID: "red", Label: "Red", Description: "warm"}, {ID: "blue", Label: "Blue"},
	}}, team)
	run := h.loginRun()
	require.NotNil(t, run)

	require.True(t, h.c.AnswerLogin(team.Token, "blue"))
	key := h.next(t, isRequest).(event.LoginRequested)
	assert.Equal(t, event.LoginRequested{Token: key.Token, Provider: provider, Message: "Key:", Placeholder: "sk-test", Secret: true}, key)
	require.True(t, h.c.AnswerLogin(key.Token, "synthetic-key"))
	h.notice(t, "Logged in to "+provider+".")
	wait(t, run.done)
	assert.Empty(t, ofType[event.Error](h.rec.all()))
}

func TestLoginEmitsTheSignInNotice(t *testing.T) {
	h := openLoginHarness(t, Config{})
	pigruntimetest.RegisterPromptingLoginProvider(t, h.c.rt, "anthropic")

	_, err := h.c.Submit(t.Context(), Main, "/login anthropic")
	require.NoError(t, err)
	team := h.next(t, func(ev event.Event) bool { _, ok := ev.(event.LoginRequested); return ok }).(event.LoginRequested)
	run := h.loginRun()
	require.NotNil(t, run)
	require.True(t, h.c.AnswerLogin(team.Token, "red"))
	key := h.next(t, func(ev event.Event) bool { r, ok := ev.(event.LoginRequested); return ok && r.Secret }).(event.LoginRequested)
	require.True(t, h.c.AnswerLogin(key.Token, "synthetic-key"))

	n := h.next(t, func(ev event.Event) bool {
		n, ok := ev.(event.Notice)
		return ok && strings.Contains(n.Text, "Anthropic bills")
	}).(event.Notice)
	assert.Contains(t, n.Text, "extra usage")
	wait(t, run.done)
}

func TestLoginFailureReportsAnErrorNotACancel(t *testing.T) {
	h := openLoginHarness(t, Config{})

	_, err := h.c.Submit(t.Context(), Main, "/login no-such-provider")
	require.NoError(t, err)

	ev := h.next(t, func(ev event.Event) bool { _, ok := ev.(event.Error); return ok }).(event.Error)
	assert.ErrorIs(t, ev.Err, pigruntime.ErrUnknownProvider)
	assert.NotContains(t, h.rec.all(), event.Notice{Text: "Login cancelled."})
}

func TestLoginInterruptCancelsWithoutStoring(t *testing.T) {
	h := openLoginHarness(t, Config{})
	run, req := h.startLogin(t)
	assert.True(t, h.c.Interrupt(Main))
	h.notice(t, "Login cancelled.")
	wait(t, run.done)

	evs := h.rec.all()
	assert.Contains(t, evs, event.LoginEnded{Token: req.Token})
	assert.Empty(t, ofType[event.Error](evs))
	assert.False(t, h.c.AnswerLogin(req.Token, "late"))
	assert.False(t, h.loggedIn(t))
	assert.False(t, h.c.Interrupt(Main))
}

func TestLoginInterruptCancelsWhenASubagentIsNamed(t *testing.T) {
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{}, agentDef("explorer"))
	pigruntimetest.RegisterLoginProvider(t, a.c.rt, loginProvider)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("done")})
	a.rootAcks(1)
	ends, requested, loginEnded := turnEnds(a.c), subscribeTo[event.LoginRequested](a.c), subscribeTo[event.LoginEnded](a.c)
	id := a.launch(t, "explorer", "find it", "go")
	a.untilStatus(t, id, tool.TaskCompleted)
	receiveEnd(t, ends)
	_, err := a.c.Submit(t.Context(), Main, "/login "+loginProvider)
	require.NoError(t, err)
	req := receiveEvent(t, requested)

	assert.True(t, a.c.Interrupt(id))

	assert.Equal(t, event.LoginEnded{Token: req.Token}, receiveEvent(t, loginEnded))
	assert.False(t, a.c.AnswerLogin(req.Token, "late"))
}

func TestLoginRefusals(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h := openLoginHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})

	_, err := h.c.Submit(t.Context(), Main, "/login")
	assert.ErrorIs(t, err, ErrMissingArgument)
	_, err = h.c.Submit(t.Context(), Main, "/logout")
	assert.ErrorIs(t, err, ErrMissingArgument)

	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), Main, "/login "+loginProvider)
	assert.ErrorIs(t, err, ErrWorkStillRunning)
	close(release)
	wait(t, w.Done())

	run, _ := h.startLogin(t)
	_, err = h.c.Submit(t.Context(), Main, "/login "+loginProvider)
	assert.ErrorIs(t, err, ErrLoggingIn)
	_, err = h.c.Submit(t.Context(), Main, "/logout "+loginProvider)
	assert.ErrorIs(t, err, ErrLoggingIn)
	_, err = h.c.Submit(t.Context(), Main, "hello")
	assert.ErrorIs(t, err, ErrLoggingIn)
	_, err = h.c.Prompt(t.Context(), "hello")
	assert.ErrorIs(t, err, ErrLoggingIn)
	_, err = h.c.Submit(t.Context(), Main, "/new")
	assert.ErrorIs(t, err, ErrLoggingIn)
	_, err = h.c.Submit(t.Context(), Main, "/resume some-id")
	assert.ErrorIs(t, err, ErrLoggingIn)
	_, err = h.c.Submit(t.Context(), Main, "/model "+fauxProvider+"/faux-1")
	assert.ErrorIs(t, err, ErrLoggingIn)

	h.c.Interrupt(Main)
	wait(t, run.done)
	w, err = h.c.Submit(t.Context(), Main, "hello")
	require.NoError(t, err)
	require.NotNil(t, w)
	wait(t, w.Done())
}

func TestLogoutRefusesWhileASwitchRuns(t *testing.T) {
	h := openLoginHarness(t, Config{})
	// A switch holds life until it finishes.
	h.c.life.Lock()
	_, err := h.c.Submit(t.Context(), Main, "/new")
	require.NoError(t, err)

	_, err = h.c.Submit(t.Context(), Main, "/logout "+loginProvider)
	assert.ErrorIs(t, err, ErrBusy)
	h.c.life.Unlock()
	h.next(t, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
}

func TestCloseEndsRunningLogin(t *testing.T) {
	h := openLoginHarness(t, Config{})
	run, req := h.startLogin(t)
	require.NoError(t, h.c.Close(context.Background()))

	select {
	case <-run.done:
	default:
		t.Fatal("Close returned before the login ended")
	}
	evs := h.rec.all()
	assert.Contains(t, evs, event.LoginEnded{Token: req.Token})
	assert.False(t, h.c.AnswerLogin(req.Token, "late"))
	_, err := h.c.Submit(t.Context(), Main, "/login "+loginProvider)
	assert.ErrorIs(t, err, ErrNoSession)
	assert.False(t, h.loggedIn(t))
}

func TestLoginAndLogoutArguments(t *testing.T) {
	h := openLoginHarness(t, Config{})
	cs, err := h.c.Arguments("login", "")
	require.NoError(t, err)
	assert.Contains(t, cs, Candidate{Value: loginProvider, Description: "Test sign-in"})
	assert.NotContains(t, values(cs), fauxProvider)
	cs, err = h.c.Arguments("logout", "")
	require.NoError(t, err)
	assert.Empty(t, cs)

	run, req := h.startLogin(t)
	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	wait(t, run.done)

	cs, err = h.c.Arguments("login", "")
	require.NoError(t, err)
	assert.Contains(t, cs, Candidate{Value: loginProvider, Description: "Test sign-in (logged in)"})
	cs, err = h.c.Arguments("logout", "")
	require.NoError(t, err)
	assert.Equal(t, []Candidate{{Value: loginProvider}}, cs)

	for _, cmd := range h.c.Commands() {
		if cmd.Name == "login" || cmd.Name == "logout" {
			assert.True(t, cmd.Argument, cmd.Name)
		}
	}
}

func values(cs []Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Value)
	}
	return out
}

func TestLogout(t *testing.T) {
	h := openLoginHarness(t, Config{})
	_, err := h.c.Submit(t.Context(), Main, "/logout "+loginProvider)
	assert.ErrorIs(t, err, pigruntime.ErrNotLoggedIn)

	run, req := h.startLogin(t)
	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	wait(t, run.done)
	require.True(t, h.loggedIn(t))

	_, err = h.c.Submit(t.Context(), Main, "/logout "+loginProvider)
	require.NoError(t, err)
	h.notice(t, "Logged out of "+loginProvider+".")
	assert.False(t, h.loggedIn(t))
}

func TestLoginAndLogoutReadTheLimitsOnlyAfterUsageWasAsked(t *testing.T) {
	h := openLoginHarness(t, Config{})
	limits := subscribeTo[event.LimitsChanged](h.c)
	run, req := h.startLogin(t)
	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	wait(t, run.done)
	h.c.limits.mu.Lock()
	assert.True(t, h.c.limits.attempted.IsZero(), "no read before /usage")
	h.c.limits.mu.Unlock()

	h.c.Usage()
	receiveEvent(t, limits)

	_, err := h.c.Submit(t.Context(), Main, "/logout "+loginProvider)
	require.NoError(t, err)
	receiveEvent(t, limits)
	run, req = h.startLogin(t)
	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	wait(t, run.done)
	receiveEvent(t, limits)
}

// firstRunHarness is a harness whose model is the login provider's, with no
// credential, so Start runs that provider's login.
func firstRunHarness(t *testing.T, cfg Config) *loginHarness {
	t.Helper()
	cfg.Model = loginProvider + "/login-1"
	h := newHarness(t, pigruntimetest.NewSandbox(t), cfg)
	pigruntimetest.RegisterLoginProvider(t, h.c.rt, loginProvider)
	lh := &loginHarness{harness: h, events: make(chan event.Event, 64)}
	h.c.Subscribe(func(ev event.Event) { lh.events <- ev })
	return lh
}

func (h *loginHarness) loginRun() *loginRun {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	return h.c.login
}

func TestStartRunsFirstRunLoginThenOpens(t *testing.T) {
	h := firstRunHarness(t, Config{})
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	run := h.loginRun()
	require.NotNil(t, run)
	req := h.next(t, func(ev event.Event) bool { _, ok := ev.(event.LoginRequested); return ok }).(event.LoginRequested)
	assert.Equal(t, loginProvider, req.Provider)
	_, err := h.c.Submit(t.Context(), Main, "hello")
	assert.ErrorIs(t, err, ErrLoggingIn)

	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	h.next(t, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
	h.notice(t, "Logged in to "+loginProvider+".")
	wait(t, run.done)
	assert.NotEmpty(t, h.c.Status().SessionID)
	assert.Empty(t, ofType[event.Error](h.rec.all()))
}

func TestStartReportsConnectingServersWhileFirstRunLoginIsOpen(t *testing.T) {
	h := firstRunHarness(t, Config{Servers: holdServers("s")})
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	require.NotNil(t, h.loginRun())
	h.next(t, func(ev event.Event) bool { _, ok := ev.(event.LoginRequested); return ok })
	assert.Contains(t, ofType[event.Activity](h.rec.all()), event.Activity{Text: "Connecting to MCP servers…"})
}

func TestFirstRunLoginCancelThenLoginOpens(t *testing.T) {
	h := firstRunHarness(t, Config{})
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	run := h.loginRun()
	require.NotNil(t, run)
	h.next(t, func(ev event.Event) bool { _, ok := ev.(event.LoginRequested); return ok })
	assert.True(t, h.c.Interrupt(Main))
	ev := h.next(t, func(ev event.Event) bool { _, ok := ev.(event.Error); return ok }).(event.Error)
	assert.ErrorIs(t, ev.Err, pigruntime.ErrNoCredential)
	assert.ErrorContains(t, ev.Err, "run /login "+loginProvider)
	wait(t, run.done)
	assert.Empty(t, ofType[event.SessionChanged](h.rec.all()))
	_, err := h.c.Submit(t.Context(), Main, "hello")
	assert.ErrorIs(t, err, ErrNoSession)

	run, req := h.startLogin(t)
	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	h.next(t, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
	h.notice(t, "Logged in to "+loginProvider+".")
	wait(t, run.done)
	assert.NotEmpty(t, h.c.Status().SessionID)
}

func TestCloseEndsFirstRunLogin(t *testing.T) {
	h := firstRunHarness(t, Config{})
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	run := h.loginRun()
	require.NotNil(t, run)
	h.next(t, func(ev event.Event) bool { _, ok := ev.(event.LoginRequested); return ok })
	require.NoError(t, h.c.Close(context.Background()))
	select {
	case <-run.done:
	default:
		t.Fatal("Close returned before the login ended")
	}
	assert.Empty(t, ofType[event.SessionChanged](h.rec.all()))
	assert.False(t, h.loggedIn(t))
}

func TestFirstRunLoginFailureReportsTheCauseThenMissing(t *testing.T) {
	const provider = "kopi-prompt"
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{Model: provider + "/login-1"})
	pigruntimetest.RegisterPromptingLoginProvider(t, h.c.rt, provider)
	lh := &loginHarness{harness: h, events: make(chan event.Event, 64)}
	h.c.Subscribe(func(ev event.Event) { lh.events <- ev })
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	run := lh.loginRun()
	require.NotNil(t, run)
	req := lh.next(t, func(ev event.Event) bool { _, ok := ev.(event.LoginRequested); return ok }).(event.LoginRequested)

	require.True(t, h.c.AnswerLogin(req.Token, "green"))

	isError := func(ev event.Event) bool { _, ok := ev.(event.Error); return ok }
	cause := lh.next(t, isError).(event.Error)
	assert.ErrorContains(t, cause.Err, `invalid choice "green"`)
	missing := lh.next(t, isError).(event.Error)
	assert.ErrorIs(t, missing.Err, pigruntime.ErrNoCredential)
	assert.ErrorContains(t, missing.Err, "run /login "+provider)
	wait(t, run.done)
	assert.NotContains(t, h.rec.all(), event.Notice{Text: "Login cancelled."})
}

func TestFirstRunLoginEndsWhileLifeIsHeld(t *testing.T) {
	h := firstRunHarness(t, Config{})
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	run := h.loginRun()
	require.NotNil(t, run)
	req := h.next(t, func(ev event.Event) bool { _, ok := ev.(event.LoginRequested); return ok }).(event.LoginRequested)

	// Close holds life while it waits for the login to end.
	h.c.life.Lock()
	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	wait(t, run.done)
	h.c.life.Unlock()

	h.next(t, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
	assertSwitchDone(t, h.harness)
}

func TestStartUnattendedKeepsMissingCredentialError(t *testing.T) {
	h := firstRunHarness(t, Config{Unattended: true})
	require.ErrorIs(t, h.c.Start(t.Context(), OpenRequest{}), pigruntime.ErrNoCredential)
	assert.Nil(t, h.loginRun())
}

func TestFirstRunLoginDoesNotRetryAnOpenThatWasTried(t *testing.T) {
	h := firstRunHarness(t, Config{Tools: []tool.Tool{unopenableOnce()}})
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	run := h.loginRun()
	require.NotNil(t, run)
	req := h.next(t, func(ev event.Event) bool { _, ok := ev.(event.LoginRequested); return ok }).(event.LoginRequested)
	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	h.notice(t, "Logged in to "+loginProvider+".")
	h.next(t, func(ev event.Event) bool { e, ok := ev.(event.Error); return ok && errors.Is(e.Err, errOpen) })
	wait(t, run.done)
	assert.Empty(t, h.c.Status().SessionID)

	run, req = h.startLogin(t)
	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	wait(t, run.done)

	assert.Empty(t, h.c.Status().SessionID, "the open would succeed now, and is not tried again")
}

// unsetLoginHarness is an unset-model harness with a login provider and a
// channel of every event delivered after it.
func unsetLoginHarness(t *testing.T, cfg Config, withFaux bool) *loginHarness {
	t.Helper()
	h := newUnsetHarness(t, cfg, withFaux)
	pigruntimetest.RegisterLoginProvider(t, h.c.rt, loginProvider)
	lh := &loginHarness{harness: h, events: make(chan event.Event, 64)}
	h.c.Subscribe(func(ev event.Event) { lh.events <- ev })
	return lh
}

func (h *loginHarness) request(t *testing.T) event.LoginRequested {
	t.Helper()
	return h.next(t, func(ev event.Event) bool { _, ok := ev.(event.LoginRequested); return ok }).(event.LoginRequested)
}

const usingLogin = `Using kopi-login/login-1. Set "model" in /kopi/config.json to choose another.`

func TestStartUnsetModelWithCredentialOpensAndReportsNotice(t *testing.T) {
	h := unsetLoginHarness(t, Config{}, true)

	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))

	h.next(t, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
	h.notice(t, `Using kopi-faux/faux-1. Set "model" in /kopi/config.json to choose another.`)
	assert.Nil(t, h.loginRun())
	assert.Equal(t, "kopi-faux/faux-1", h.c.Status().Model)
}

func TestStartUnsetModelAsksWhichProviderThenOpens(t *testing.T) {
	h := unsetLoginHarness(t, Config{}, false)
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	run := h.loginRun()
	require.NotNil(t, run)

	pick := h.request(t)

	assert.Empty(t, pick.Provider)
	assert.Equal(t, "Log in to a provider:", pick.Message)
	assert.True(t, slices.IsSortedFunc(pick.Options, func(a, b event.LoginOption) int { return strings.Compare(a.ID, b.ID) }))
	i := slices.IndexFunc(pick.Options, func(o event.LoginOption) bool { return o.ID == loginProvider })
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, event.LoginOption{ID: loginProvider, Label: loginProvider, Description: "Test sign-in"}, pick.Options[i])

	require.True(t, h.c.AnswerLogin(pick.Token, loginProvider))
	code := h.request(t)
	assert.Equal(t, loginProvider, code.Provider)
	require.True(t, h.c.AnswerLogin(code.Token, "synthetic-code"))

	h.next(t, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
	h.notice(t, "Logged in to "+loginProvider+".")
	h.notice(t, usingLogin)
	wait(t, run.done)
	assert.Equal(t, "kopi-login/login-1", h.c.Status().Model)
	assert.Empty(t, ofType[event.Error](h.rec.all()))
}

func TestStartUnsetModelFailsOnAnAnswerThatIsNoProvider(t *testing.T) {
	h := unsetLoginHarness(t, Config{}, false)
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	run := h.loginRun()
	require.NotNil(t, run)
	pick := h.request(t)

	require.True(t, h.c.AnswerLogin(pick.Token, "nope"))

	isError := func(ev event.Event) bool { _, ok := ev.(event.Error); return ok }
	cause := h.next(t, isError).(event.Error)
	assert.ErrorIs(t, cause.Err, pigruntime.ErrUnknownProvider)
	missing := h.next(t, isError).(event.Error)
	assert.ErrorIs(t, missing.Err, pigruntime.ErrNoCredential)
	assert.ErrorContains(t, missing.Err, "run /login <provider>")
	wait(t, run.done)
}

func TestUnsetModelInterruptAtThePickThenLoginOpens(t *testing.T) {
	h := unsetLoginHarness(t, Config{}, false)
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	run := h.loginRun()
	require.NotNil(t, run)
	h.request(t)

	assert.True(t, h.c.Interrupt(Main))

	h.notice(t, "Login cancelled.")
	ev := h.next(t, func(ev event.Event) bool { _, ok := ev.(event.Error); return ok }).(event.Error)
	assert.ErrorIs(t, ev.Err, pigruntime.ErrNoCredential)
	assert.ErrorContains(t, ev.Err, "run /login <provider>")
	wait(t, run.done)
	assert.Empty(t, h.c.Status().SessionID)

	run, req := h.startLogin(t)
	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	h.next(t, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
	h.notice(t, usingLogin)
	wait(t, run.done)
	assert.NotEmpty(t, h.c.Status().SessionID)
}

func TestStartUnsetModelUnattendedKeepsMissingCredentialError(t *testing.T) {
	h := unsetLoginHarness(t, Config{Unattended: true}, false)

	require.ErrorIs(t, h.c.Start(t.Context(), OpenRequest{}), pigruntime.ErrNoCredential)
	assert.Nil(t, h.loginRun())
}

func TestUnsetModelNewAfterCancelledPickReportsMissingCredential(t *testing.T) {
	h := unsetLoginHarness(t, Config{}, false)
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	run := h.loginRun()
	require.NotNil(t, run)
	h.request(t)
	assert.True(t, h.c.Interrupt(Main))
	wait(t, run.done)

	err := h.c.NewSession(t.Context())

	require.ErrorIs(t, err, pigruntime.ErrNoCredential)
	assert.ErrorContains(t, err, "run /login <provider>")
	assert.NotContains(t, err.Error(), "unknown model")
}

func TestUnsetModelNewAfterCancelledPickOffersWebSearch(t *testing.T) {
	pigruntimetest.ClearEnv(t)
	sb := pigruntimetest.NewSandbox(t)
	rt := pigruntimetest.NewRuntime(t, sb)
	pigruntimetest.RegisterLoginProvider(t, rt, loginProvider)
	lh := &loginHarness{harness: buildHarness(t, rt, nil, sb, Config{ConfigFile: "/kopi/config.json"}), events: make(chan event.Event, 64)}
	lh.c.Subscribe(func(ev event.Event) { lh.events <- ev })
	require.NoError(t, lh.c.Start(t.Context(), OpenRequest{}))
	run := lh.loginRun()
	require.NotNil(t, run)
	lh.request(t)
	assert.True(t, lh.c.Interrupt(Main))
	wait(t, run.done)
	require.Equal(t, []string{"AskUserQuestion", "WebSearch", "Workflow"}, toolNames(lh.c.tools))

	t.Setenv("OPENAI_API_KEY", "synthetic-key")
	require.NoError(t, lh.c.NewSession(t.Context()))

	lh.next(t, func(ev event.Event) bool {
		n, ok := ev.(event.Notice)
		return ok && strings.HasPrefix(n.Text, "Using openai/")
	})
	assert.True(t, lh.c.main.sess.Callable("WebSearch"))
}
