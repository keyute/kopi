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
		e.Title = d.title(e.Name, json.RawMessage(e.Args))
		e.Action = d.tools[e.Name].Display.Action
		if e.Action == (tool.Action{}) {
			e.Action = fallbackAction
		}
		return e
	case event.ToolEnd:
		if e.IsError {
			return e
		}
		if summary := d.tools[e.Name].Display.Summary; summary != nil {
			e.Summary = summary(e.Output)
		} else {
			e.Summary = lineCount(e.Output)
		}
		return e
	}
	return ev
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
