package transcript

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/event"
)

func TestApplyIgnoresEndOfSettledCall(t *testing.T) {
	var tr Transcript
	tr.Apply(start("a"))
	tr.Apply(event.TurnEnd{})
	tr.Apply(end("a"))
	assert.Equal(t, []Block{row("a", Unfinished), Turn{key: "item:1"}}, tr.Blocks())
}

func TestTurnEndKeepsFailureReason(t *testing.T) {
	failed := event.Outcome{Status: event.Failed, Message: "failed to send prompt: boom"}
	var tr Transcript
	tr.Apply(event.TurnEnd{Outcome: failed})
	assert.Equal(t, []Block{Turn{key: "item:0", Outcome: failed}}, tr.Blocks())
}

func TestSteeredMessageDrawsAsRestored(t *testing.T) {
	var live Transcript
	for _, ev := range []event.Event{
		event.TurnStart{Prompt: "go"},
		start("a"), end("a"),
		event.QueueChanged{Pending: []string{"x"}},
		event.QueueChanged{},
		event.UserMessage{Text: "x"},
		event.TextDelta{Text: "ok"},
	} {
		live.Apply(ev)
	}
	var restored Transcript
	restored.Restore([]event.Event{event.TurnStart{Prompt: "go"}, start("a"), end("a"), event.TurnStart{Prompt: "x"}, event.TextDelta{Text: "ok"}})

	want := []Block{
		User{key: "item:0", Text: "go"}, row("a", OK), User{key: "item:2", Text: "x"}, Assistant{key: "item:3", Text: "ok"},
	}
	assert.Equal(t, want, live.Blocks())
	assert.Equal(t, want, restored.Blocks())
}

func TestWakeTurnAddsNoUserBlock(t *testing.T) {
	var tr Transcript
	tr.Apply(event.TurnStart{})
	tr.Apply(event.TextDelta{Text: "it finished"})
	tr.Apply(event.TurnEnd{})
	assert.Equal(t, []Block{Assistant{key: "item:0", Text: "it finished"}, Turn{key: "item:1"}}, tr.Blocks())
}

func TestTaskEndedDrawsAsRestored(t *testing.T) {
	events := []event.Event{
		event.TurnStart{Prompt: "go"},
		start("a"), end("a"),
		event.TaskEnded{ID: "t1", Command: "go test ./...", Status: "failed", Elapsed: 3 * time.Second},
		start("b"), end("b"),
		event.TurnEnd{},
	}
	var live, restored Transcript
	for _, ev := range events {
		live.Apply(ev)
	}
	restored.Restore(events)

	want := []Block{
		User{key: "item:0", Text: "go"},
		row("a", OK),
		Task{key: "item:2", ID: "t1", Command: "go test ./...", Status: "failed", Elapsed: 3 * time.Second},
		row("b", OK),
		Turn{key: "item:4"},
	}
	assert.Equal(t, want, live.Blocks())
	assert.Equal(t, want, restored.Blocks())
}

func TestAgentTaskEndedCarriesTheAgent(t *testing.T) {
	events := []event.Event{
		event.TaskEnded{ID: "t2", Agent: "explore", Command: "look around", Status: "completed", Elapsed: 2 * time.Minute},
	}
	var live, restored Transcript
	live.Apply(events[0])
	restored.Restore(events)

	want := []Block{Task{key: "item:0", ID: "t2", Agent: "explore", Command: "look around", Status: "completed", Elapsed: 2 * time.Minute}}
	assert.Equal(t, want, live.Blocks())
	assert.Equal(t, want, restored.Blocks())
}

func TestQueueChangedAddsNoBlock(t *testing.T) {
	var tr Transcript
	tr.Apply(event.QueueChanged{Pending: []string{"x"}})
	assert.Empty(t, tr.Blocks())
}

func TestRestore(t *testing.T) {
	for _, tc := range []struct {
		name          string
		history, live []event.Event
		want          []Block
	}{
		{
			name:    "last group is sealed",
			history: []event.Event{start("a"), end("a"), start("b"), end("b")},
			want:    []Block{group("a", true, "a", "b")},
		},
		{
			name:    "live call does not join a historical group",
			history: []event.Event{start("a"), end("a"), start("b"), end("b")},
			live:    []event.Event{start("c"), end("c"), start("d"), end("d")},
			want:    []Block{group("a", true, "a", "b"), group("c", false, "c", "d")},
		},
		{
			name:    "history ending mid-call is unfinished",
			history: []event.Event{event.TurnStart{Prompt: "go"}, start("a")},
			want:    []Block{User{key: "item:0", Text: "go"}, row("a", Unfinished)},
		},
		{
			name:    "live text does not extend historical text",
			history: []event.Event{event.TextDelta{Text: "old"}},
			live:    []event.Event{event.TextDelta{Text: "new"}},
			want:    []Block{Assistant{key: "item:0", Text: "old"}, Assistant{key: "item:1", Text: "new"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tr Transcript
			tr.Restore(tc.history)
			for _, ev := range tc.live {
				tr.Apply(ev)
			}
			assert.Equal(t, tc.want, tr.Blocks())
		})
	}
}

func TestRunningIsTheLatestRunningCallsTitle(t *testing.T) {
	var tr Transcript
	_, ok := tr.Running()
	assert.False(t, ok)

	tr.Apply(start("a"))
	tr.Apply(start("b"))
	title, ok := tr.Running()
	assert.True(t, ok)
	assert.Equal(t, "Read b", title)

	tr.Apply(end("b"))
	title, _ = tr.Running()
	assert.Equal(t, "Read a", title)

	tr.Apply(event.TurnEnd{})
	_, ok = tr.Running()
	assert.False(t, ok, "an unfinished call is not running")
}
