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
	"github.com/keyute/kopi/internal/policy"
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
	c := &Controller{authority: policy.NewAuthority()}
	r := &runner{c: c, queued: []*entry{{prompt: a}}, told: modeLine(policy.Plan, "", false)}
	c.main = r
	emit := func(event.Event) {}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	entries := r.pull(cancelled, emit)()
	assert.Empty(t, entries)
	assert.Equal(t, []pigruntime.Prompt{a}, r.waiting())

	entries = r.pull(t.Context(), emit)()
	assert.Equal(t, []pigruntime.Entry{{Prompt: a}}, entries)
	assert.Empty(t, r.waiting())
	assert.Equal(t, []*entry{{prompt: a, handed: true}}, r.queued, "a handed prompt stays queued until it is read")
}

func TestHandedPromptsStayQueuedUntilRead(t *testing.T) {
	a, b := pigruntime.Prompt{Text: "a"}, pigruntime.Prompt{Text: "b"}
	agent := pigruntime.Prompt{Text: "agent", FromAgent: true}
	c := &Controller{authority: policy.NewAuthority()}
	r := &runner{c: c, queued: []*entry{{prompt: a}, {prompt: agent}, {prompt: b}}, told: modeLine(policy.Plan, "", false)}
	c.main = r
	var got []event.Event
	emit := func(ev event.Event) { got = append(got, ev) }

	entries := r.pull(t.Context(), emit)()
	assert.Equal(t, []pigruntime.Entry{{Prompt: a}, {Prompt: agent}, {Prompt: b}}, entries)
	assert.Empty(t, got, "the hand-over reports nothing")
	assert.Empty(t, r.takeHuman(emit), "a handed prompt is no longer the human's to take back")
	assert.Empty(t, got)

	late := pigruntime.Prompt{Text: "late"}
	r.queued = append(r.queued, &entry{prompt: late})
	assert.Equal(t, []pigruntime.Prompt{late}, r.takeHuman(emit))
	assert.Equal(t, []event.Event{event.QueueChanged{Pending: []event.Queued{{Text: "a"}, {Text: "b"}}}}, got)

	got = nil
	read := r.reading(nil, emit)
	read(event.TextDelta{Text: "x"})
	read(event.UserMessage{Text: "a"})
	read(event.UserMessage{Text: "agent"})
	read(event.UserMessage{Text: "b"})
	assert.Equal(t, []event.Event{
		event.TextDelta{Text: "x"},
		event.QueueChanged{Pending: []event.Queued{{Text: "b"}}},
		event.UserMessage{Text: "a"},
		event.UserMessage{Text: "agent"},
		event.QueueChanged{Pending: []event.Queued{}},
		event.UserMessage{Text: "b"},
	}, got)
	assert.Empty(t, r.queued)
}

func TestReadingIgnoresAReplacedSession(t *testing.T) {
	c := &Controller{authority: policy.NewAuthority()}
	r := &runner{c: c, sess: &pigruntime.Session{}, queued: []*entry{{prompt: pigruntime.Prompt{Text: "a"}, handed: true}}}
	var got []event.Event
	r.reading(&pigruntime.Session{}, func(ev event.Event) { got = append(got, ev) })(event.UserMessage{Text: "a"})
	assert.Equal(t, []event.Event{event.UserMessage{Text: "a"}}, got)
	assert.Len(t, r.queued, 1)
}

func TestDropHandedKeepsWaitingInput(t *testing.T) {
	a, b := pigruntime.Prompt{Text: "a"}, pigruntime.Prompt{Text: "b"}
	agent := pigruntime.Prompt{Text: "agent", FromAgent: true}
	r := &runner{queued: []*entry{{prompt: a, handed: true}, {prompt: agent, handed: true}, {prompt: b}}}
	var got []event.Event
	emit := func(ev event.Event) { got = append(got, ev) }

	r.dropHanded(emit)
	assert.Equal(t, []pigruntime.Prompt{b}, r.waiting())
	assert.Equal(t, []event.Event{event.QueueChanged{Pending: []event.Queued{{Text: "b"}}}}, got)

	got = nil
	r.dropHanded(emit)
	assert.Empty(t, got, "nothing handed, nothing to report")
}

func TestUnhandReturnsOnlyWhatARecordHanded(t *testing.T) {
	a, b := &entry{cmd: &userCommand{}, handed: true}, &entry{cmd: &userCommand{}, handed: true}
	r := &runner{queued: []*entry{a, b}}
	r.unhand([]*entry{a})
	assert.False(t, a.handed)
	assert.True(t, b.handed, "a newer hand-over's entry stays handed")
}

func TestReclaimedQueuePlansInTypingOrder(t *testing.T) {
	exited := func() *entry { return &entry{cmd: &userCommand{text: "a", state: event.CommandExited}} }
	prompt := func(text string) *entry { return &entry{prompt: pigruntime.Prompt{Text: text}} }
	newRunner := func(es ...*entry) (*runner, func(event.Event)) {
		c := &Controller{authority: policy.NewAuthority()}
		r := &runner{c: c, queued: es, told: modeLine(policy.Plan, "", false)}
		c.main = r
		return r, func(event.Event) {}
	}
	for _, tc := range []struct {
		name    string
		handed  bool
		prompts []string
	}{
		{"handed and never read", true, []string{"P1"}},
		{"still queued", false, []string{"P1", "P2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := exited()
			es := []*entry{a}
			for _, text := range tc.prompts {
				es = append(es, prompt(text))
			}
			for _, e := range es {
				e.handed = tc.handed
			}
			r, emit := newRunner(es...)
			var undelivered []pigruntime.Prompt
			for _, e := range es[1:] {
				undelivered = append(undelivered, e.prompt)
			}

			r.reclaim(emit, true, undelivered)
			require.Empty(t, r.lead(), "the command is ahead of the prompts")
			turn := r.plan(nil, emit)

			require.NotNil(t, turn.record)
			assert.Equal(t, "", turn.text, "the prompt is drawn where the model reads it")
			require.Len(t, turn.record.notices, 1)
			assert.Equal(t, a.cmd.shown(), turn.record.notices[0].Shown)
			assert.Equal(t, es[1:], turn.record.prompts)
			assert.Equal(t, undelivered, turn.parts)
			assert.True(t, a.handed)
		})
	}
	t.Run("a stopped Work hands the prompts back", func(t *testing.T) {
		a, p := exited(), prompt("P1")
		a.handed, p.handed = true, true
		r, emit := newRunner(a, p)
		r.reclaim(emit, false, []pigruntime.Prompt{p.prompt})
		assert.Equal(t, []*entry{a}, r.queued)
		assert.False(t, a.handed)
	})
	t.Run("the mode line goes back with its prompt", func(t *testing.T) {
		p := prompt("P1")
		p.handed = true
		r, emit := newRunner(p)
		r.told, r.toldBy = "line", map[string]delivery{"line": {line: true}}
		r.reclaim(emit, true, []pigruntime.Prompt{{Text: "P1", Mode: "line"}})
		assert.Empty(t, r.told)
		assert.False(t, p.handed)
	})
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

func TestJoinedPromptsMergeToolsWithoutDuplicates(t *testing.T) {
	got := joined([]pigruntime.Prompt{{Text: "one", Tools: []string{"Workflow"}}, {Text: "two"}, {Text: "three", Tools: []string{"Other", "Workflow"}}})
	assert.Equal(t, pigruntime.Prompt{Text: "one\n\ntwo\n\nthree", Tools: []string{"Workflow", "Other"}}, got)
}

func TestJoinedPromptsCarryTheRaisedEffort(t *testing.T) {
	got := joined([]pigruntime.Prompt{{Text: "one"}, {Text: "two", Effort: pigruntime.MaxEffort}, {Text: "three"}})
	assert.Equal(t, pigruntime.Prompt{Text: "one\n\ntwo\n\nthree", Effort: pigruntime.MaxEffort}, got)
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
	assert.Equal(t, []pigruntime.Prompt{agent}, h.c.main.waiting(), "the agent's message stays queued")
	h.c.queue.Unlock()
	assert.True(t, h.c.Interrupt(Main))
	wait(t, w.Done())
}
