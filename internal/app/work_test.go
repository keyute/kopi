package app

import (
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
)

func TestWorkQueueRetainsCompletedWakesInOrder(t *testing.T) {
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{}, agentDef("explorer"))
	q := a.c.FollowWork()
	defer q.Close()
	idle := a.c.FollowWork()
	defer idle.Close()
	a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("initial"), pigruntimetest.ProviderError("wake failed"), pigruntimetest.Text("last")})
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("one"), pigruntimetest.Text("two")})
	initial := a.turn(t, "go")
	ends := turnEnds(a.c)
	for range 2 {
		a.launch(t, "explorer", "find it", "go")
		receiveEnd(t, ends)
		require.False(t, settled(t, idle))
	}
	for i, status := range []event.Status{event.Completed, event.Failed, event.Completed} {
		w, _, _ := q.Next()
		require.NotNil(t, w)
		wait(t, w.Done())
		assert.Equal(t, status, w.Outcome().Status)
		if i == 0 {
			assert.Same(t, initial, w)
		}
	}
	w, live, _ := q.Next()
	assert.Nil(t, w)
	assert.False(t, live)
	q.Close()
	idle.Close()
	assert.Empty(t, a.c.followers)
}

// settled consumes q's turns once each has ended and reports whether any
// subagent is still running.
func settled(t *testing.T, q *WorkQueue) bool {
	t.Helper()
	for {
		w, live, _ := q.Next()
		if w == nil {
			return live
		}
		wait(t, w.Done())
	}
}
