package app

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
)

// titleHarness is an opened harness whose small model is a second faux
// provider, so title requests are scripted apart from the turns.
func titleHarness(t *testing.T, cfg Config) (*harness, *ai.FauxProviderHandle) {
	t.Helper()
	h, title := unopenedTitleHarness(t, cfg)
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	return h, title
}

func unopenedTitleHarness(t *testing.T, cfg Config) (*harness, *ai.FauxProviderHandle) {
	t.Helper()
	sb := pigruntimetest.NewSandbox(t)
	rt := pigruntimetest.NewRuntime(t, sb)
	faux, model := pigruntimetest.RegisterFaux(t, rt, fauxProvider)
	title, small := pigruntimetest.RegisterFaux(t, rt, "kopi-title")
	cfg.Model, cfg.SmallModel = model, small
	return buildHarness(t, rt, faux, sb, cfg), title
}

func (h *harness) sessionName() string {
	h.c.mu.Lock()
	sess := h.c.main.sess
	h.c.mu.Unlock()
	return sess.Name()
}

func TestFirstPromptNamesSessionOnce(t *testing.T) {
	h, title := titleHarness(t, Config{Title: true})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("one"), pigruntimetest.Text("two")})
	title.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("  \"Fix the\x1b build\"\nignored line"), pigruntimetest.Text("Second")})
	named := subscribeTo[event.SessionNamed](h.c)

	h.turn(t, "fix the build")
	assert.Equal(t, event.SessionNamed{Name: "Fix the build"}, receiveEvent(t, named))
	h.turn(t, "and then more")
	h.c.wg.Wait()

	assert.Equal(t, "Fix the build", h.sessionName())
	assert.Equal(t, 1, title.CallCount())
	assert.Empty(t, named)
}

func TestSuccessfulTitleChargesTheTotal(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	priced := sb.PricedModel(t)
	h := newHarness(t, sb, Config{Title: true, SmallModel: priced})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("one")})
	named := subscribeTo[event.SessionNamed](h.c)

	h.turn(t, "fix the build")
	assert.Equal(t, event.SessionNamed{Name: "priced answer"}, receiveEvent(t, named))
	h.c.wg.Wait()

	assert.Equal(t, "priced answer", h.sessionName())
	assert.Equal(t, pigruntimetest.PricedModelCost, h.c.Status().TotalCost)
}

func TestTitleWorkerStartedAfterSessionSwitchChargesNothing(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	priced := sb.PricedModel(t)
	h := newHarness(t, sb, Config{Title: true, SmallModel: priced})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	named := subscribeTo[event.SessionNamed](h.c)
	h.c.mu.Lock()
	old, bound := h.c.main.sess, h.c.main.bound
	h.c.mu.Unlock()

	require.NoError(t, h.c.NewSession(t.Context()))
	h.c.nameSession(old, bound, "fix the build")

	assert.Zero(t, h.c.Status().TotalCost)
	assert.Empty(t, old.Name())
	assert.Empty(t, h.sessionName())
	assert.Empty(t, named)
	assert.Empty(t, ofType[event.SessionNamed](h.rec.all()))
}

func TestPromptQueuedWhileOpeningNamesSession(t *testing.T) {
	srv := holdServers()
	h, title := unopenedTitleHarness(t, Config{Title: true, Servers: srv.wait})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("one")})
	title.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("Fix the build")})
	named := subscribeTo[event.SessionNamed](h.c)
	queued := subscribeTo[event.QueueChanged](h.c)

	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	w, err := h.c.Submit(t.Context(), Main, "fix the build")
	require.NoError(t, err)
	assert.Nil(t, w)
	receiveEvent(t, queued)
	close(srv.release)

	assert.Equal(t, event.SessionNamed{Name: "Fix the build"}, receiveEvent(t, named))
	assert.Equal(t, 1, title.CallCount())
}

func TestSessionSwitchResetsTitleAndNamedSessionIsNotAsked(t *testing.T) {
	h, title := titleHarness(t, Config{Title: true})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("one"), pigruntimetest.Text("two"), pigruntimetest.Text("three")})
	title.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("First"), pigruntimetest.Text("Second")})
	named := subscribeTo[event.SessionNamed](h.c)
	firstID := h.c.Status().SessionID

	h.turn(t, "one")
	receiveEvent(t, named)
	require.NoError(t, h.c.NewSession(t.Context()))
	h.turn(t, "two")
	assert.Equal(t, event.SessionNamed{Name: "Second"}, receiveEvent(t, named))
	require.NoError(t, h.c.Resume(t.Context(), firstID))
	h.turn(t, "three")
	h.c.wg.Wait()

	assert.Equal(t, "First", h.sessionName())
	assert.Equal(t, 2, title.CallCount())
	assert.Empty(t, named)
	changed := ofType[event.SessionChanged](h.rec.all())
	require.Len(t, changed, 3)
	assert.Equal(t, "First", changed[2].Name)
}

func TestTitleOffAsksNothing(t *testing.T) {
	h, title := titleHarness(t, Config{})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("one")})
	title.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("Unwanted")})

	h.turn(t, "fix the build")
	h.c.wg.Wait()

	assert.Zero(t, title.CallCount())
	assert.Empty(t, h.sessionName())
	assert.Empty(t, ofType[event.SessionNamed](h.rec.all()))
}

func TestFailedTitleLeavesNoNameAndIsNotRetried(t *testing.T) {
	h, title := titleHarness(t, Config{Title: true})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("one"), pigruntimetest.Text("two")})
	title.SetResponses([]ai.FauxResponseStep{
		ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonError), ErrorMessage: "boom"}),
		pigruntimetest.Text("Late"),
	})

	h.turn(t, "fix the build")
	h.turn(t, "again")
	h.c.wg.Wait()

	assert.Equal(t, 1, title.CallCount())
	assert.Empty(t, h.sessionName())
	assert.Empty(t, ofType[event.SessionNamed](h.rec.all()))
}

func TestLongPromptIsCutForTheTitleRequest(t *testing.T) {
	for _, tc := range []struct{ name, prompt, want string }{
		{"ascii", strings.Repeat("a", titlePromptBytes+10), strings.Repeat("a", titlePromptBytes)},
		{"character straddling the cut", strings.Repeat("a", titlePromptBytes-1) + "é" + "tail", strings.Repeat("a", titlePromptBytes-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, title := titleHarness(t, Config{Title: true})
			h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("one")})
			requests := make(chan pigruntimetest.Request, 1)
			title.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "Fix the build")})

			h.turn(t, tc.prompt)
			h.c.wg.Wait()

			require.Len(t, requests, 1)
			assert.Equal(t, []string{tc.want}, (<-requests).UserTexts)
		})
	}
}

func TestCleanTitle(t *testing.T) {
	for _, tc := range []struct{ name, reply, want string }{
		{"first non-empty line", "\n  \n Fix it \nsecond", "Fix it"},
		{"quotes", " “'Fix it'” ", "Fix it"},
		{"control characters", "Fix\x00 \x1b[31mit", "Fix [31mit"},
		{"only quotes", "\"\"", ""},
		{"blank", " \n\t", ""},
		{"capped by bytes without splitting a character", strings.Repeat("é", titleBytes), strings.Repeat("é", titleBytes/2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cleanTitle(tc.reply))
		})
	}
}

// blockedTitle scripts a title reply that signals entered, then waits for
// unblock, which is also closed when the test ends.
func blockedTitle(t *testing.T, title *ai.FauxProviderHandle, reply string, unblock <-chan struct{}) (entered chan struct{}) {
	t.Helper()
	entered = make(chan struct{})
	title.SetResponses([]ai.FauxResponseStep{
		ai.FauxFactoryStep(func(ai.TranscriptContext, ai.StreamOptions, *ai.FauxProviderState, *ai.Model) (ai.AssistantMessage, error) {
			close(entered)
			<-unblock
			return ai.FauxAssistantMessage(ai.FauxContentString(reply), ai.FauxAssistantMessageOptions{}), nil
		}),
	})
	return entered
}

func TestTitleCommitCoversPersistenceAndAnnouncement(t *testing.T) {
	h, title := titleHarness(t, Config{Title: true})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("one")})
	title.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("Fix the build")})
	id := h.c.Status().SessionID
	announced, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	h.c.mu.Lock()
	bound := h.c.main.bound
	h.c.main.bound = func(ev event.Event) {
		bound(ev)
		close(announced)
		<-release
	}
	h.c.mu.Unlock()

	h.turn(t, "fix the build")
	wait(t, announced)
	locked := h.c.titleCommit.TryLock()
	if locked {
		h.c.titleCommit.Unlock()
	}
	assert.False(t, locked, "title commit must cover persistence and announcement")
	op, err := h.c.beginSwitch(nil)
	require.NoError(t, err)
	switching, switched := make(chan struct{}), make(chan error, 1)
	go func() {
		h.c.life.Lock()
		defer h.c.life.Unlock()
		close(switching)
		switched <- h.c.startSwitch(t.Context(), OpenRequest{ResumeID: id}, op)
	}()
	wait(t, switching)
	unblock()
	select {
	case err := <-switched:
		require.NoError(t, err)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for resume")
	}
	h.c.wg.Wait()

	assert.Equal(t, "Fix the build", h.sessionName())
	changed := ofType[event.SessionChanged](h.rec.all())
	require.Len(t, changed, 2)
	assert.Equal(t, "Fix the build", changed[1].Name)
}

func TestTitleAnsweredAfterSessionSwitchIsDropped(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "new session"
		if resume {
			name = "same-session resume"
		}
		t.Run(name, func(t *testing.T) {
			h, title := titleHarness(t, Config{Title: true})
			h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("one")})
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			entered := blockedTitle(t, title, "Stale", release)
			named := subscribeTo[event.SessionNamed](h.c)
			h.c.mu.Lock()
			old := h.c.main.sess
			h.c.mu.Unlock()
			h.c.Subscribe(func(ev event.Event) {
				if _, ok := ev.(event.SessionChanged); !ok {
					return
				}
				locked := h.c.titleCommit.TryLock()
				if locked {
					h.c.titleCommit.Unlock()
				}
				assert.False(t, locked, "replacement must hold title commit through announcement")
				unblock()
			})

			h.turn(t, "fix the build")
			wait(t, entered)
			if resume {
				require.NoError(t, h.c.Resume(t.Context(), old.ID()))
			} else {
				require.NoError(t, h.c.NewSession(t.Context()))
			}
			h.c.wg.Wait()

			assert.Empty(t, old.Name())
			assert.Empty(t, h.sessionName())
			assert.Empty(t, named)
			assert.Empty(t, ofType[event.SessionNamed](h.rec.all()))
			changed := ofType[event.SessionChanged](h.rec.all())
			require.Len(t, changed, 2)
			assert.Empty(t, changed[1].Name)
		})
	}
}

func TestCloseStopsTitleRequest(t *testing.T) {
	h, title := titleHarness(t, Config{Title: true})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("one")})
	// The faux provider cannot see the request's context, so the reply waits on
	// the controller's own title context.
	named := subscribeTo[event.SessionNamed](h.c)
	h.c.mu.Lock()
	sess := h.c.main.sess
	h.c.mu.Unlock()
	entered := blockedTitle(t, title, "Late", h.c.titleCtx.Done())

	h.turn(t, "fix the build")
	wait(t, entered)
	closed := make(chan error, 1)
	go func() { closed <- h.c.Close(t.Context()) }()

	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for Close")
	}
	assert.Empty(t, sess.Name())
	assert.Empty(t, named)
}

func TestAgentAndBlankPromptsDoNotNameSession(t *testing.T) {
	h, title := titleHarness(t, Config{Title: true})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("one")})
	title.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("Fix the build")})
	named := subscribeTo[event.SessionNamed](h.c)

	h.c.queue.Lock()
	h.c.nameSessionLocked([]pigruntime.Prompt{{Text: "from an agent", FromAgent: true}, {Text: " \n"}})
	h.c.queue.Unlock()
	h.c.wg.Wait()
	assert.Zero(t, title.CallCount())

	h.turn(t, "fix the build")
	assert.Equal(t, event.SessionNamed{Name: "Fix the build"}, receiveEvent(t, named))
	assert.Equal(t, 1, title.CallCount())
}
