package tui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
)

func TestWorkingRowShowsWhileTurnRuns(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return clock }
	idle := m.transcriptHeight()
	working := func() string { s := screen(m); return s[len(s)-5] }

	send(m, event.TurnStart{Prompt: "go"})
	assert.Equal(t, "⠋ Working… 0s", working())
	assert.Equal(t, idle-1, m.transcriptHeight())
	assert.Len(t, screen(m), 20)
	assert.Empty(t, screen(m)[len(screen(m))-6], "the blank row sits above the working row")

	clock = clock.Add(72 * time.Second)
	_, cmd := m.Update(m.spinner.Tick())
	assert.NotNil(t, cmd, "the spinner keeps ticking while busy")
	assert.Equal(t, "⠙ Working… 1m 12s", working())

	send(m, event.TurnEnd{Elapsed: 72 * time.Second})
	assert.Equal(t, idle, m.transcriptHeight())
	assert.Len(t, screen(m), 20)
	assert.NotContains(t, screen(m), "⠙ Working… 1m 12s")
	_, cmd = m.Update(m.spinner.Tick())
	require.Nil(t, cmd, "ticking stops once idle")
}
