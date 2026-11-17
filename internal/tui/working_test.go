package tui

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
)

func TestIdleLayout(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	typeText(m, "hi")
	s := screen(m)

	assert.Len(t, s, 20)
	assert.Equal(t, 20-1-chromeRows, m.transcriptHeight())
	h := m.transcriptHeight()
	assert.Equal(t, []string{"", "", "❯ hi", ""}, s[h:h+4], "blank, shaded, composer, shaded")
	assert.Equal(t, h+2, m.composerTop())
	assert.Contains(t, s[len(s)-1], "plan")
}

func TestWorkingRowShowsWhileTurnRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 20)
		idle := m.transcriptHeight()
		working := func() string { s := screen(m); return s[len(s)-6] }

		send(m, event.TurnStart{Prompt: "go"})
		assert.Equal(t, "⠋ Working… 0s", working())
		assert.Equal(t, idle-busyRows, m.transcriptHeight())
		assert.Len(t, screen(m), 20)
		assert.Empty(t, screen(m)[len(screen(m))-7], "a blank row sits above the working row")
		assert.Empty(t, screen(m)[len(screen(m))-5], "and another below it")
		assert.Equal(t, "❯", screen(m)[m.composerTop()])

		time.Sleep(72 * time.Second)
		_, cmd := m.Update(m.spinner.Tick())
		assert.NotNil(t, cmd, "the spinner keeps ticking while busy")
		assert.Equal(t, "⠙ Working… 1m 12s", working())

		send(m, event.TurnEnd{Elapsed: 72 * time.Second})
		assert.Equal(t, idle, m.transcriptHeight())
		assert.Len(t, screen(m), 20)
		assert.NotContains(t, screen(m), "⠙ Working… 1m 12s")
		assert.Equal(t, "❯", screen(m)[m.composerTop()])
		_, cmd = m.Update(m.spinner.Tick())
		require.Nil(t, cmd, "ticking stops once idle")
	})
}

func TestWorkingRowShowsActivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 20)
		idle := m.transcriptHeight()
		working := func() string { s := screen(m); return s[len(s)-6] }

		send(m, event.Activity{Text: "Connecting to MCP servers…"})
		assert.Equal(t, "⠋ Connecting to MCP servers…", working())
		assert.Equal(t, idle-busyRows, m.transcriptHeight())
		_, cmd := m.Update(m.spinner.Tick())
		assert.NotNil(t, cmd, "the spinner ticks during the activity")

		send(m, event.TurnStart{Prompt: "go"})
		assert.Equal(t, "⠙ Working… 0s", working(), "a running turn's text wins")
		send(m, event.TurnEnd{})
		assert.Equal(t, "⠙ Connecting to MCP servers…", working())

		send(m, event.Activity{})
		assert.Equal(t, idle, m.transcriptHeight())
		_, cmd = m.Update(m.spinner.Tick())
		require.Nil(t, cmd, "ticking stops once the activity ends")
	})
}
