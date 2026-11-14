package transcript

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

var readAction = tool.Action{Verb: "Read", One: "file", Many: "files"}

func start(id string) event.ToolStart {
	return event.ToolStart{CallID: id, Name: "Read", Title: "Read " + id, Action: readAction}
}

func end(id string) event.ToolEnd {
	return event.ToolEnd{CallID: id, Name: "Read", Output: "out " + id, Summary: "sum " + id}
}

func fail(id string) event.ToolEnd {
	return event.ToolEnd{CallID: id, Name: "Read", Output: "err " + id, IsError: true}
}

func call(id string, state State) Call {
	c := Call{ID: id, Title: "Read " + id, Action: readAction, State: state}
	switch state {
	case OK:
		c.Output, c.Summary = "out "+id, "sum "+id
	case Failed:
		c.Output = "err " + id
	}
	return c
}

func row(id string, state State) Row { return Row{Call: call(id, state)} }

func group(key string, sealed bool, ids ...string) Group {
	g := Group{key: "group:" + key, Sealed: sealed}
	for _, id := range ids {
		g.Calls = append(g.Calls, call(id, OK))
	}
	return g
}

func TestBlocks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []event.Event
		want   []Block
	}{
		{
			name:   "prompt then text",
			events: []event.Event{event.TurnStart{Prompt: "hi"}, event.TextDelta{Text: "hel"}, event.TextDelta{Text: "lo"}},
			want:   []Block{User{key: "item:0", Text: "hi"}, Assistant{key: "item:1", Text: "hello"}},
		},
		{
			name:   "two ok calls then text seal the group",
			events: []event.Event{start("a"), end("a"), start("b"), end("b"), event.TextDelta{Text: "done"}},
			want:   []Block{group("a", true, "a", "b"), Assistant{key: "item:2", Text: "done"}},
		},
		{
			name:   "group is live while last",
			events: []event.Event{start("a"), end("a"), start("b"), end("b")},
			want:   []Block{group("a", false, "a", "b")},
		},
		{
			name:   "single ok call is a row",
			events: []event.Event{start("a"), end("a"), event.TextDelta{Text: "x"}},
			want:   []Block{row("a", OK), Assistant{key: "item:1", Text: "x"}},
		},
		{
			name:   "running row below a live group",
			events: []event.Event{start("a"), end("a"), start("b"), end("b"), start("c")},
			want:   []Block{group("a", false, "a", "b"), row("c", Running)},
		},
		{
			name:   "running call joins the group on success",
			events: []event.Event{start("a"), end("a"), start("b"), end("b"), start("c"), end("c")},
			want:   []Block{group("a", false, "a", "b", "c")},
		},
		{
			name:   "failed call is its own row and splits the stretch",
			events: []event.Event{start("a"), end("a"), start("b"), end("b"), start("c"), fail("c"), start("d"), end("d")},
			want:   []Block{group("a", true, "a", "b"), row("c", Failed), row("d", OK)},
		},
		{
			name:   "parallel starts group in start order",
			events: []event.Event{start("a"), start("b"), end("b"), end("a")},
			want:   []Block{group("a", false, "a", "b")},
		},
		{
			name:   "parallel starts before either settles",
			events: []event.Event{start("a"), start("b"), end("b")},
			want:   []Block{row("b", OK), row("a", Running)},
		},
		{
			name:   "whitespace text between calls is invisible",
			events: []event.Event{start("a"), end("a"), event.TextDelta{Text: "\n\n"}, start("b"), end("b")},
			want:   []Block{group("a", false, "a", "b")},
		},
		{
			name: "thinking and mode changes split nothing",
			events: []event.Event{
				start("a"), end("a"),
				event.ThinkingDelta{Text: "hmm"},
				event.ModeChanged{Mode: policy.Mode(0)},
				event.SessionChanged{SessionID: "s"},
				start("b"), end("b"),
			},
			want: []Block{group("a", false, "a", "b")},
		},
		{
			name:   "notice splits the stretch",
			events: []event.Event{start("a"), end("a"), start("b"), end("b"), event.Notice{Text: "retrying"}, start("c"), end("c"), start("d"), end("d")},
			want:   []Block{group("a", true, "a", "b"), Notice{key: "item:2", Text: "retrying"}, group("c", false, "c", "d")},
		},
		{
			name: "turn end leaves a running call unfinished",
			events: []event.Event{
				event.TurnStart{Prompt: "go"},
				start("a"),
				event.TurnEnd{Outcome: event.Outcome{Status: event.Interrupted}, Elapsed: time.Second},
			},
			want: []Block{
				User{key: "item:0", Text: "go"},
				row("a", Unfinished),
				Turn{key: "item:2", Outcome: event.Outcome{Status: event.Interrupted}, Elapsed: time.Second},
			},
		},
		{
			name:   "tool end without start is ignored",
			events: []event.Event{end("ghost"), event.TextDelta{Text: "x"}},
			want:   []Block{Assistant{key: "item:0", Text: "x"}},
		},
		{
			name:   "error block",
			events: []event.Event{event.Error{Err: assert.AnError}},
			want:   []Block{Error{key: "item:0", Text: assert.AnError.Error()}},
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

func TestBlockKeysStable(t *testing.T) {
	events := []event.Event{
		event.TurnStart{Prompt: "go"},
		event.TextDelta{Text: "look"},
		start("a"), end("a"), start("b"), end("b"),
		start("c"), fail("c"),
		event.Notice{Text: "n"},
		start("d"), end("d"),
		event.TextDelta{Text: "ok"},
		event.TurnEnd{},
		event.TurnStart{Prompt: "again"},
		start("e"),
	}
	var tr Transcript
	// Item-derived blocks never regroup, so every key they ever had must survive.
	itemKeys := map[string]bool{}
	for _, ev := range events {
		tr.Apply(ev)
		keys := map[string]bool{}
		for _, b := range tr.Blocks() {
			assert.False(t, keys[b.Key()], "duplicate key %s", b.Key())
			keys[b.Key()] = true
			switch b.(type) {
			case Row, Group:
			default:
				itemKeys[b.Key()] = true
			}
		}
	}
	final := map[string]bool{}
	for _, b := range tr.Blocks() {
		final[b.Key()] = true
	}
	for key := range itemKeys {
		assert.True(t, final[key], "lost key %s", key)
	}
	for _, key := range []string{"item:0", "item:1", "group:a", "call:c", "item:5", "call:d", "item:7", "item:8", "item:9", "call:e"} {
		assert.True(t, final[key], "missing key %s", key)
	}
}
