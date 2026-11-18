package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

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
		case <-t.Context().Done():
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
	_, err := h.c.Submit(t.Context(), "/login "+loginProvider)
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
	<-run.done

	evs := h.rec.all()
	assert.Contains(t, evs, event.Activity{Text: "Logging in to " + loginProvider + "…"})
	assert.Contains(t, evs, event.Notice{Text: pigruntimetest.LoginProgress})
	assert.Contains(t, evs, event.LoginEnded{Token: req.Token})
	assert.Empty(t, ofType[event.Error](evs))
	assert.Equal(t, event.Activity{}, evs[len(evs)-2])
	assert.True(t, h.loggedIn(t))
}

func TestLoginInterruptCancelsWithoutStoring(t *testing.T) {
	h := openLoginHarness(t, Config{})
	run, req := h.startLogin(t)
	assert.True(t, h.c.Interrupt())
	h.notice(t, "Login cancelled.")
	<-run.done

	evs := h.rec.all()
	assert.Contains(t, evs, event.LoginEnded{Token: req.Token})
	assert.Empty(t, ofType[event.Error](evs))
	assert.False(t, h.c.AnswerLogin(req.Token, "late"))
	assert.False(t, h.loggedIn(t))
	assert.False(t, h.c.Interrupt())
}

func TestLoginRefusals(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h := openLoginHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})

	_, err := h.c.Submit(t.Context(), "/login")
	assert.ErrorIs(t, err, ErrMissingArgument)
	_, err = h.c.Submit(t.Context(), "/logout")
	assert.ErrorIs(t, err, ErrMissingArgument)

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), "/login "+loginProvider)
	assert.ErrorIs(t, err, ErrWorkStillRunning)
	close(release)
	wait(t, w.Done())

	run, _ := h.startLogin(t)
	_, err = h.c.Submit(t.Context(), "/login "+loginProvider)
	assert.ErrorIs(t, err, ErrLoggingIn)
	_, err = h.c.Submit(t.Context(), "/logout "+loginProvider)
	assert.ErrorIs(t, err, ErrLoggingIn)
	_, err = h.c.Submit(t.Context(), "hello")
	assert.ErrorIs(t, err, ErrLoggingIn)
	_, err = h.c.Prompt(t.Context(), "hello")
	assert.ErrorIs(t, err, ErrLoggingIn)
	_, err = h.c.Submit(t.Context(), "/new")
	assert.ErrorIs(t, err, ErrLoggingIn)
	_, err = h.c.Submit(t.Context(), "/resume some-id")
	assert.ErrorIs(t, err, ErrLoggingIn)

	h.c.Interrupt()
	<-run.done
	w, err = h.c.Submit(t.Context(), "hello")
	require.NoError(t, err)
	require.NotNil(t, w)
	wait(t, w.Done())
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
	_, err := h.c.Submit(t.Context(), "/login "+loginProvider)
	assert.ErrorIs(t, err, ErrNoSession)
	assert.False(t, h.loggedIn(t))
}

func TestLoginAndLogoutArguments(t *testing.T) {
	h := openLoginHarness(t, Config{})
	cs, err := h.c.Arguments("login")
	require.NoError(t, err)
	assert.Contains(t, cs, Candidate{Value: loginProvider, Description: "Test sign-in"})
	assert.NotContains(t, values(cs), fauxProvider)
	cs, err = h.c.Arguments("logout")
	require.NoError(t, err)
	assert.Empty(t, cs)

	run, req := h.startLogin(t)
	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	<-run.done

	cs, err = h.c.Arguments("login")
	require.NoError(t, err)
	assert.Contains(t, cs, Candidate{Value: loginProvider, Description: "Test sign-in (logged in)"})
	cs, err = h.c.Arguments("logout")
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
	_, err := h.c.Submit(t.Context(), "/logout "+loginProvider)
	assert.ErrorIs(t, err, pigruntime.ErrNotLoggedIn)

	run, req := h.startLogin(t)
	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	<-run.done
	require.True(t, h.loggedIn(t))

	_, err = h.c.Submit(t.Context(), "/logout "+loginProvider)
	require.NoError(t, err)
	h.notice(t, "Logged out of "+loginProvider+".")
	assert.False(t, h.loggedIn(t))
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
	_, err := h.c.Submit(t.Context(), "hello")
	assert.ErrorIs(t, err, ErrLoggingIn)

	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	h.next(t, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
	h.notice(t, "Logged in to "+loginProvider+".")
	<-run.done
	assert.NotEmpty(t, h.c.Status().SessionID)
	assert.Empty(t, ofType[event.Error](h.rec.all()))
}

func TestFirstRunLoginCancelThenLoginOpens(t *testing.T) {
	h := firstRunHarness(t, Config{})
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	run := h.loginRun()
	require.NotNil(t, run)
	h.next(t, func(ev event.Event) bool { _, ok := ev.(event.LoginRequested); return ok })
	assert.True(t, h.c.Interrupt())
	ev := h.next(t, func(ev event.Event) bool { _, ok := ev.(event.Error); return ok }).(event.Error)
	assert.ErrorIs(t, ev.Err, pigruntime.ErrNoCredential)
	assert.ErrorContains(t, ev.Err, "run /login "+loginProvider)
	<-run.done
	assert.Empty(t, ofType[event.SessionChanged](h.rec.all()))
	_, err := h.c.Submit(t.Context(), "hello")
	assert.ErrorIs(t, err, ErrNoSession)

	run, req := h.startLogin(t)
	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	h.next(t, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
	h.notice(t, "Logged in to "+loginProvider+".")
	<-run.done
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

func TestStartUnattendedKeepsMissingCredentialError(t *testing.T) {
	h := firstRunHarness(t, Config{Unattended: true})
	require.ErrorIs(t, h.c.Start(t.Context(), OpenRequest{}), pigruntime.ErrNoCredential)
	assert.Nil(t, h.loginRun())
}

func TestFirstRunLoginDoesNotRetryAnOpenThatStarted(t *testing.T) {
	var calls atomic.Int32
	h := firstRunHarness(t, Config{Servers: func(context.Context) ([]pigruntime.ServerStatus, error) {
		calls.Add(1)
		return nil, errors.New("servers down")
	}})
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	run := h.loginRun()
	require.NotNil(t, run)
	req := h.next(t, func(ev event.Event) bool { _, ok := ev.(event.LoginRequested); return ok }).(event.LoginRequested)
	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	h.next(t, func(ev event.Event) bool { e, ok := ev.(event.Error); return ok && e.Err.Error() == "servers down" })
	<-run.done
	assert.Empty(t, h.c.Status().SessionID)

	run, req = h.startLogin(t)
	require.True(t, h.c.AnswerLogin(req.Token, "synthetic-code"))
	<-run.done

	assert.Equal(t, int32(1), calls.Load())
	assert.Empty(t, h.c.Status().SessionID)
}
