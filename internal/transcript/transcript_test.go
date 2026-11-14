package transcript

import (
	"errors"
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

func TestTurnEndDrawsFailureReasonOnce(t *testing.T) {
	failed := event.Outcome{Status: event.Failed, Message: "failed to send prompt: boom"}
	for _, tc := range []struct {
		name   string
		events []event.Event
		want   []Block
	}{
		{
			name:   "error then turn end with the same text",
			events: []event.Event{event.Error{Err: errors.New(failed.Message)}, event.TurnEnd{Outcome: failed}},
			want: []Block{
				Error{key: "item:0", Text: failed.Message},
				Turn{key: "item:1", Outcome: event.Outcome{Status: event.Failed}},
			},
		},
		{
			name:   "turn end alone keeps its message",
			events: []event.Event{event.TurnEnd{Outcome: failed}},
			want:   []Block{Turn{key: "item:0", Outcome: failed}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tr Transcript
			for _, ev := range tc.events {
				tr.Apply(ev)
			}
			assert.Equal(t, tc.want, tr.Blocks())
		})
	}
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
