package app

import (
	"encoding/json"
	"strings"
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
	assert.Equal(t, event.ToolEnd{CallID: "c1", Name: "Show", Output: "ok", Summary: "shown ok", Body: "ok"}, end)
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

func TestDisplayFailedCall(t *testing.T) {
	failing := testTool("Fail", policy.Exec, nil)
	failing.Display.Failure = func(output string) (string, string) { return "exit 1", "tail of " + output }
	d := newDisplay([]tool.Tool{failing})

	for _, tc := range []struct {
		name string
		end  event.ToolEnd
		want event.ToolEnd
	}{
		{
			"one line is the summary",
			event.ToolEnd{Name: "Ghost", Output: "boom\n", IsError: true},
			event.ToolEnd{Name: "Ghost", Output: "boom\n", IsError: true, Summary: "boom"},
		},
		{
			"several lines are the body",
			event.ToolEnd{Name: "Ghost", Output: "boom\nbang", IsError: true},
			event.ToolEnd{Name: "Ghost", Output: "boom\nbang", IsError: true, Summary: "failed", Body: "boom\nbang"},
		},
		{
			"the tool's Failure",
			event.ToolEnd{Name: "Fail", Output: "x", IsError: true},
			event.ToolEnd{Name: "Fail", Output: "x", IsError: true, Summary: "exit 1", Body: "tail of x"},
		},
		{
			"a refusal wins over Failure",
			event.ToolEnd{Name: "Fail", Output: `tool "Fail" needs execute mode; the session is in plan mode: denied by policy`, IsError: true},
			event.ToolEnd{Name: "Fail", Output: `tool "Fail" needs execute mode; the session is in plan mode: denied by policy`, IsError: true, Kind: event.EndDenied, Summary: "denied · plan mode"},
		},
		{
			"cancellation",
			event.ToolEnd{Name: "Fail", Output: "context canceled", IsError: true},
			event.ToolEnd{Name: "Fail", Output: "context canceled", IsError: true, Kind: event.EndInterrupted, Summary: "interrupted"},
		},
		{
			"a kind set upstream is kept",
			event.ToolEnd{Name: "Fail", Output: "Operation aborted", IsError: true, Kind: event.EndInterrupted},
			event.ToolEnd{Name: "Fail", Output: "Operation aborted", IsError: true, Kind: event.EndInterrupted, Summary: "interrupted"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, d.apply(tc.end))
		})
	}
}

func TestDisplayFillsStartFields(t *testing.T) {
	arg := "x" + strings.Repeat("y", 100)
	change := testTool("Change", policy.Write, nil)
	change.Unsandboxed = func(args json.RawMessage) bool { return string(args) == "{}" }
	change.Display = tool.Display{
		Done: func(json.RawMessage) string { return "Changed" },
		Change: func(json.RawMessage) (string, []tool.PreviewLine) {
			return "+1 -1", []tool.PreviewLine{{Kind: tool.Added, Text: arg[1:]}}
		},
		Solo: true,
	}
	d := newDisplay([]tool.Tool{change, testTool("Plain", policy.Read, nil)})

	start := d.apply(event.ToolStart{CallID: "c1", Name: "Change", Args: "{}"}).(event.ToolStart)
	assert.Equal(t, "Changed", start.Done)
	assert.Equal(t, "+1 -1", start.Change)
	assert.Equal(t, []tool.PreviewLine{{Kind: tool.Added, Text: arg[1:]}}, start.Preview)
	assert.True(t, start.Solo)
	assert.True(t, start.Unsandboxed)

	plain := d.apply(event.ToolStart{CallID: "c2", Name: "Plain", Args: "{}"}).(event.ToolStart)
	assert.Empty(t, plain.Done)
	assert.Empty(t, plain.Change)
	assert.Nil(t, plain.Preview)
	assert.False(t, plain.Solo)
	assert.False(t, plain.Unsandboxed)
}

func TestDisplayBody(t *testing.T) {
	trimmed := testTool("Trim", policy.Read, nil)
	trimmed.Display.Body = strings.ToUpper
	shut := testTool("Shut", policy.Read, nil)
	shut.Display.Body = func(string) string { return "" }
	change := testTool("Change", policy.Write, nil)
	change.Display.Change = func(json.RawMessage) (string, []tool.PreviewLine) { return "+1", nil }
	d := newDisplay([]tool.Tool{trimmed, shut, change, testTool("Plain", policy.Read, nil)})

	for name, want := range map[string]event.ToolEnd{
		"Trim":   {Summary: "1 line", Body: "OUT"},
		"Shut":   {Summary: "1 line"},
		"Change": {},
		"Plain":  {Summary: "1 line", Body: "out"},
	} {
		want.Name, want.Output = name, "out"
		assert.Equal(t, want, d.apply(event.ToolEnd{Name: name, Output: "out"}), name)
	}
}

func TestDisplaySummarizesWithoutStart(t *testing.T) {
	shown := testTool("Show", policy.Read, nil)
	shown.Display.Summary = func(output string) string { return "shown " + output }
	d := newDisplay([]tool.Tool{shown})

	end := d.apply(event.ToolEnd{CallID: "c1", Name: "Show", Output: "ok"})

	assert.Equal(t, event.ToolEnd{CallID: "c1", Name: "Show", Output: "ok", Summary: "shown ok", Body: "ok"}, end)
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
