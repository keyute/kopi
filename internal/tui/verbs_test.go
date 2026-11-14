package tui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/event"
)

var brewing = verb{"Brewing", "Brewed"}

func TestTurnUsesPickedVerbOnWorkingRowAndTurnLine(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	m.pickVerb = func() verb { return brewing }

	send(m, event.TurnStart{Prompt: "go"})
	assert.Contains(t, screen(m), "⠋ Brewing… 0s")

	send(m, event.TurnEnd{Elapsed: 41 * time.Second})
	assert.Equal(t, []string{"", "", "❯ go", "", "", "κ Brewed for 41s"}, body(m))

	m.pickVerb = func() verb { return verb{"Roasting", "Roasted"} }
	send(m, event.TurnStart{Prompt: "again"}, event.TurnEnd{Elapsed: 2 * time.Second})
	assert.Equal(t, []string{
		"", "", "❯ go", "", "", "κ Brewed for 41s",
		"", "", "❯ again", "", "", "κ Roasted for 2s",
	}, body(m), "each turn keeps its own verb")
}

func TestInterruptedTurnLineIgnoresVerb(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	m.pickVerb = func() verb { return brewing }

	send(m, event.TurnStart{Prompt: "go"}, event.TurnEnd{Outcome: event.Outcome{Status: event.Interrupted}, Elapsed: 3 * time.Second})

	assert.Contains(t, body(m), "κ Interrupted after 3s")
}

func TestRestoredTurnSaysWorkedAndSessionChangeResetsVerbs(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	m.pickVerb = func() verb { return brewing }
	send(m, event.TurnStart{Prompt: "go"}, event.TurnEnd{Elapsed: 4 * time.Second})
	assert.Contains(t, body(m), "κ Brewed for 4s")

	send(m, event.SessionChanged{SessionID: "s2", Resumed: true, History: []event.Event{
		event.TurnStart{Prompt: "earlier"},
		event.TurnEnd{Elapsed: 5 * time.Second},
	}})

	assert.Equal(t, []string{"", "", "❯ earlier", "", "", "κ Worked for 5s"}, body(m))
	assert.Empty(t, m.folds.verbs)
}
