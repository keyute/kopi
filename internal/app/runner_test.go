package app

import (
	"context"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/tool"
)

// unreadRole reports the input another agent sent that a run never read.
type unreadRole struct {
	role
	unread chan []pigruntime.Prompt
}

func (r unreadRole) end(w *Work, unread []pigruntime.Prompt) {
	r.unread <- unread
	r.role.end(w, unread)
}

func TestSettle(t *testing.T) {
	a, b, c, d := pigruntime.Prompt{Text: "a"}, pigruntime.Prompt{Text: "b"}, pigruntime.Prompt{Text: "c"}, pigruntime.Prompt{Text: "d"}
	completed, interrupted, failed := event.Outcome{Status: event.Completed}, event.Outcome{Status: event.Interrupted}, event.Outcome{Status: event.Failed, Message: "boom"}
	ps := func(p ...pigruntime.Prompt) []pigruntime.Prompt { return p }
	for _, tc := range []struct {
		name                           string
		stopped, cancelled, sent       bool
		outcome                        event.Outcome
		undelivered, handBack, pending []pigruntime.Prompt
		wantNext, wantUnsent           []pigruntime.Prompt
	}{
		{name: "completed alone ends", sent: true, outcome: completed},
		{name: "completed continues with undelivered then pending", sent: true, outcome: completed, undelivered: ps(b), pending: ps(c), wantNext: ps(b, c)},
		{name: "stopped hands back unsent parts, undelivered and hand-back", stopped: true, cancelled: true, outcome: interrupted, undelivered: ps(b), handBack: ps(c), wantUnsent: ps(a, b, c)},
		{name: "stopped keeps pending for the next turn", stopped: true, cancelled: true, sent: true, outcome: interrupted, handBack: ps(b), pending: ps(c), wantUnsent: ps(b)},
		{name: "stopped after completing hands back", stopped: true, cancelled: true, sent: true, outcome: completed, handBack: ps(b), wantUnsent: ps(b)},
		{name: "send-now continues with pending", cancelled: true, sent: true, outcome: interrupted, undelivered: ps(b), pending: ps(c, d), wantNext: ps(b, c, d)},
		{name: "send-now before the prompt was read resends it", cancelled: true, outcome: interrupted, pending: ps(b), wantNext: ps(a, b)},
		{name: "send-now continues after a failure", cancelled: true, sent: true, outcome: failed, pending: ps(b), wantNext: ps(b)},
		{name: "failed hands everything back", sent: true, outcome: failed, undelivered: ps(b), pending: ps(c), wantUnsent: ps(b, c)},
		{name: "rejected prompt is handed back", outcome: failed, pending: ps(b), wantUnsent: ps(a, b)},
		{name: "interrupted by the provider hands pending back", sent: true, outcome: interrupted, pending: ps(b), wantUnsent: ps(b)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, unsent := settle(tc.stopped, tc.cancelled, tc.outcome, tc.sent, ps(a), tc.undelivered, tc.handBack, tc.pending)
			assert.Equal(t, tc.wantNext, next)
			assert.Equal(t, tc.wantUnsent, unsent)
		})
	}
}

func TestPullHandsNothingToCancelledSend(t *testing.T) {
	a := pigruntime.Prompt{Text: "a"}
	r := &runner{c: &Controller{}, pending: []pigruntime.Prompt{a}}
	emit := func(event.Event) {}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	prompts, _ := r.pull(cancelled, emit)()
	assert.Empty(t, prompts)
	assert.Equal(t, []pigruntime.Prompt{a}, r.pending)

	prompts, _ = r.pull(t.Context(), emit)()
	assert.Equal(t, []pigruntime.Prompt{a}, prompts)
	assert.Empty(t, r.pending)
}

func TestJoinedPromptsKeepOrder(t *testing.T) {
	i1, i2 := pigruntime.Image{Path: "/1.png"}, pigruntime.Image{Path: "/2.png"}
	got := joined([]pigruntime.Prompt{{Text: "one [image #1]", Images: []pigruntime.Image{i1}}, {Text: "two"}, {Text: "three [image #2]", Images: []pigruntime.Image{i2}}})
	assert.Equal(t, pigruntime.Prompt{Text: "one [image #1]\n\ntwo\n\nthree [image #2]", Images: []pigruntime.Image{i1, i2}}, got)
}

func TestJoinedPromptsMergeSkillsWithoutDuplicates(t *testing.T) {
	got := joined([]pigruntime.Prompt{{Text: "one", Skills: []string{"a", "b"}}, {Text: "two"}, {Text: "three", Skills: []string{"b", "c"}}})
	assert.Equal(t, pigruntime.Prompt{Text: "one\n\ntwo\n\nthree", Skills: []string{"a", "b", "c"}}, got)
}

func TestEndSplitsUnsentInputBySender(t *testing.T) {
	started := make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{blockTool(started)}})
	unread := make(chan []pigruntime.Prompt, 1)
	h.c.main.role = unreadRole{role: h.c.main.role, unread: unread}
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "call-1"))})
	w, err := h.c.Submit(t.Context(), Main, "block")
	require.NoError(t, err)
	wait(t, started)
	agent := pigruntime.Prompt{Text: "from an agent", FromAgent: true}
	_, err = h.c.main.submit(t.Context(), agent, false)
	require.NoError(t, err)
	_, err = h.c.Submit(t.Context(), Main, "from the human")
	require.NoError(t, err)

	assert.True(t, h.c.Interrupt(Main))
	wait(t, w.Done())

	assert.Equal(t, []pigruntime.Prompt{agent}, <-unread)
	assert.Equal(t, []event.TurnEnd{{
		WorkID:  w.ID(),
		Outcome: event.Outcome{Status: event.Interrupted},
		Unsent:  []event.Input{{Text: "from the human"}},
	}}, ofType[event.TurnEnd](untimed(h.rec.all())))
}

func TestQueueChangedAndTakeQueuedSeeOnlyHumanInput(t *testing.T) {
	started := make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{blockTool(started)}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "call-1"))})
	w, err := h.c.Submit(t.Context(), Main, "block")
	require.NoError(t, err)
	wait(t, started)
	agent := pigruntime.Prompt{Text: "from an agent", FromAgent: true}
	_, err = h.c.main.submit(t.Context(), agent, false)
	require.NoError(t, err)
	_, err = h.c.Submit(t.Context(), Main, "from the human")
	require.NoError(t, err)

	assert.Equal(t, []event.Input{{Text: "from the human"}}, h.c.TakeQueued(Main))

	queues := ofType[event.QueueChanged](h.rec.all())
	require.NotEmpty(t, queues)
	for _, q := range queues {
		assert.NotContains(t, q.Pending, "from an agent")
	}
	assert.Empty(t, queues[len(queues)-1].Pending)
	h.c.queue.Lock()
	assert.Equal(t, []pigruntime.Prompt{agent}, h.c.main.pending, "the agent's message stays queued")
	h.c.queue.Unlock()
	assert.True(t, h.c.Interrupt(Main))
	wait(t, w.Done())
}
