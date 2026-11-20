package app

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

var fallbackAction = tool.Action{Verb: "Ran", One: "tool", Many: "tools"}

// display fills the transcript fields of tool events from each tool's Display.
type display struct {
	tools map[string]tool.Tool
}

func newDisplay(tools []tool.Tool) *display {
	d := &display{tools: make(map[string]tool.Tool, len(tools))}
	for _, t := range tools {
		d.tools[t.Name] = t
	}
	return d
}

// apply returns ev with its display fields filled; other events pass through.
func (d *display) apply(ev event.Event) event.Event {
	switch e := ev.(type) {
	case event.ToolStart:
		t := d.tools[e.Name]
		args := json.RawMessage(e.Args)
		e.Title = d.title(e.Name, args)
		if t.Display.Done != nil {
			e.Done = t.Display.Done(args)
		}
		e.Action = t.Display.Action
		if e.Action == (tool.Action{}) {
			e.Action = fallbackAction
		}
		if t.Display.Change != nil {
			e.Change, e.Preview = t.Display.Change(args)
		}
		e.Solo = t.Display.Solo
		e.Unsandboxed = t.Unsandboxed != nil && t.Unsandboxed(args)
		return e
	case event.ToolEnd:
		if e.Kind == event.EndInterrupted {
			e.Summary, e.Body = interrupted, ""
			return e
		}
		if e.IsError {
			e.Kind, e.Summary, e.Body = d.failure(e.Name, e.Output)
			return e
		}
		display := d.tools[e.Name].Display
		switch {
		case display.Summary != nil:
			e.Summary = display.Summary(e.Output)
		case display.Change == nil:
			e.Summary = lineCount(e.Output)
		}
		switch {
		case display.Change != nil:
		case display.Body != nil:
			e.Body = display.Body(e.Output)
		default:
			e.Body = e.Output
		}
		return e
	}
	return ev
}

// failure is how a call that returned an error reads: refused, rejected,
// interrupted or failed.
func (d *display) failure(name, text string) (event.EndKind, string, string) {
	if kind, summary, body := classify(name, text); kind != event.EndRan {
		return kind, summary, body
	}
	if failure := d.tools[name].Display.Failure; failure != nil {
		summary, body := failure(text)
		return event.EndRan, summary, body
	}
	if line := strings.TrimRight(text, "\n"); line != "" && !strings.Contains(line, "\n") {
		return event.EndRan, line, ""
	}
	return event.EndRan, "failed", text
}

func (d *display) title(name string, args json.RawMessage) string {
	t := d.tools[name]
	switch {
	case t.Display.Title != nil:
		return t.Display.Title(args)
	case t.Label != "":
		return t.Label
	}
	return name
}

func (d *display) approval(r policy.Request) event.ApprovalRequested {
	ev := event.ApprovalRequested{CallID: r.CallID, Title: d.title(r.Tool, r.Args), Rule: r.Rule, Review: r.Review, Edits: r.Edits}
	if preview := d.tools[r.Tool].Display.Preview; preview != nil {
		ev.Preview = preview(r.Args)
	}
	return ev
}

func lineCount(output string) string {
	if output == "" {
		return "no output"
	}
	n := strings.Count(strings.TrimSuffix(output, "\n"), "\n") + 1
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}
