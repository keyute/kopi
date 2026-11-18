package app

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

func TestDisplayUsesToolDisplay(t *testing.T) {
	shown := testTool("Show", policy.Read, nil)
	shown.Display = tool.Display{
		Title:   func(args json.RawMessage) string { return "Show " + string(args) },
		Summary: func(output string) string { return "shown " + output },
		Action:  tool.Action{Verb: "Showed", One: "thing", Many: "things"},
	}
	d := newDisplay([]tool.Tool{shown})

	start := d.apply(event.ToolStart{CallID: "c1", Name: "Show", Args: `{"x":1}`})
	end := d.apply(event.ToolEnd{CallID: "c1", Name: "Show", Output: "ok"})

	assert.Equal(t, event.ToolStart{CallID: "c1", Name: "Show", Args: `{"x":1}`, Title: `Show {"x":1}`, Action: shown.Display.Action}, start)
	assert.Equal(t, event.ToolEnd{CallID: "c1", Name: "Show", Output: "ok", Summary: "shown ok"}, end)
}

func TestDisplayFallbacks(t *testing.T) {
	labelled := testTool("Plain", policy.Read, nil)
	labelled.Label = "Plain tool"
	unlabelled := testTool("Bare", policy.Read, nil)
	unlabelled.Label = ""
	d := newDisplay([]tool.Tool{labelled, unlabelled})

	for _, tc := range []struct {
		name, tool, output, title, summary string
	}{
		{"label as title", "Plain", "one\ntwo\n", "Plain tool", "2 lines"},
		{"name as title", "Bare", "one", "Bare", "1 line"},
		{"unknown tool", "Ghost", "", "Ghost", "no output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := d.apply(event.ToolStart{CallID: tc.name, Name: tc.tool}).(event.ToolStart)
			assert.Equal(t, tc.title, start.Title)
			assert.Equal(t, fallbackAction, start.Action)
			end := d.apply(event.ToolEnd{CallID: tc.name, Name: tc.tool, Output: tc.output}).(event.ToolEnd)
			assert.Equal(t, tc.summary, end.Summary)
		})
	}
}

func TestDisplayGivesFailedCallNoSummary(t *testing.T) {
	d := newDisplay(nil)

	d.apply(event.ToolStart{CallID: "c1", Name: "Ghost"})
	end := d.apply(event.ToolEnd{CallID: "c1", Name: "Ghost", Output: "boom", IsError: true})

	assert.Equal(t, event.ToolEnd{CallID: "c1", Name: "Ghost", Output: "boom", IsError: true}, end)
}

func TestDisplaySummarizesWithoutStart(t *testing.T) {
	shown := testTool("Show", policy.Read, nil)
	shown.Display.Summary = func(output string) string { return "shown " + output }
	d := newDisplay([]tool.Tool{shown})

	end := d.apply(event.ToolEnd{CallID: "c1", Name: "Show", Output: "ok"})

	assert.Equal(t, event.ToolEnd{CallID: "c1", Name: "Show", Output: "ok", Summary: "shown ok"}, end)
}

func TestDisplayPassesOtherEventsThrough(t *testing.T) {
	d := newDisplay(nil)
	for _, ev := range []event.Event{
		event.TurnStart{WorkID: "turn-1", Prompt: "hi"},
		event.TextDelta{Text: "x"},
		event.TurnEnd{WorkID: "turn-1"},
	} {
		assert.Equal(t, ev, d.apply(ev))
	}
}

func TestDisplayApprovalCarriesTitleAndPreview(t *testing.T) {
	changed := testTool("Change", policy.Write, nil)
	changed.Display = tool.Display{
		Title: func(args json.RawMessage) string { return "Change " + string(args) },
		Preview: func(args json.RawMessage) []tool.PreviewLine {
			return []tool.PreviewLine{{Kind: tool.Removed, Text: "old"}, {Kind: tool.Added, Text: string(args)}}
		},
	}
	d := newDisplay([]tool.Tool{changed, testTool("Plain", policy.Exec, nil)})

	got := d.approval(policy.Request{CallID: "c1", Tool: "Change", Args: json.RawMessage(`{"x":1}`), Rule: "Change(x) (test)", Edits: true})
	plain := d.approval(policy.Request{CallID: "c2", Tool: "Plain"})

	assert.Equal(t, event.ApprovalRequested{
		CallID: "c1", Title: `Change {"x":1}`,
		Preview: []tool.PreviewLine{{Kind: tool.Removed, Text: "old"}, {Kind: tool.Added, Text: `{"x":1}`}},
		Rule:    "Change(x) (test)", Edits: true,
	}, got)
	assert.Equal(t, event.ApprovalRequested{CallID: "c2", Title: "Plain"}, plain)
}

func TestDisplayApprovalCopiesReview(t *testing.T) {
	d := newDisplay([]tool.Tool{testTool("Plain", policy.Exec, nil)})
	review := &policy.Review{Verdict: policy.VerdictDeny, Reason: "unsafe"}

	got := d.approval(policy.Request{CallID: "c1", Tool: "Plain", Review: review})

	assert.Same(t, review, got.Review)
}
