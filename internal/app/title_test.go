package app

import (
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
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

func TestCleanTitle(t *testing.T) {
	for _, tc := range []struct{ name, reply, want string }{
		{"first non-empty line", "\n  \n Fix it \nsecond", "Fix it"},
		{"quotes", " “'Fix it'” ", "Fix it"},
		{"control characters", "Fix\x00 \x1b[31mit", "Fix [31mit"},
		{"only quotes", "\"\"", ""},
		{"blank", " \n\t", ""},
		{"capped by runes", strings.Repeat("é", titleRunes+5), strings.Repeat("é", titleRunes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cleanTitle(tc.reply))
		})
	}
}
