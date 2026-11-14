package tui

import (
	"testing"
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
	m, _ := newTestModel(t, 40, 20)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return clock }
	idle := m.transcriptHeight()
	working := func() string { s := screen(m); return s[len(s)-6] }

	send(m, event.TurnStart{Prompt: "go"})
	assert.Equal(t, "⠋ Working… 0s", working())
	assert.Equal(t, idle-busyRows, m.transcriptHeight())
	assert.Len(t, screen(m), 20)
	assert.Empty(t, screen(m)[len(screen(m))-7], "a blank row sits above the working row")
	assert.Empty(t, screen(m)[len(screen(m))-5], "and another below it")
	assert.Equal(t, "❯", screen(m)[m.composerTop()])

	clock = clock.Add(72 * time.Second)
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
}
