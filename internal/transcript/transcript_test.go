package transcript

import (
	"testing"

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
