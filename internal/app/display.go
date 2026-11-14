package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

var fallbackAction = tool.Action{Verb: "Ran", One: "tool", Many: "tools"}

// display fills the transcript fields of tool events from each tool's
// Display. A ToolEnd's Summary needs the call's arguments, which only its
// ToolStart carries, so they are held by call ID in between. Tool events may
// arrive from concurrent goroutines.
type display struct {
	tools map[string]tool.Tool

	mu   sync.Mutex
	args map[string]json.RawMessage
}

func newDisplay(tools []tool.Tool) *display {
	d := &display{tools: make(map[string]tool.Tool, len(tools)), args: map[string]json.RawMessage{}}
	for _, t := range tools {
		d.tools[t.Name] = t
	}
	return d
}

// apply returns ev with its display fields filled; other events pass through.
func (d *display) apply(ev event.Event) event.Event {
	switch e := ev.(type) {
	case event.ToolStart:
		args := json.RawMessage(e.Args)
		d.mu.Lock()
		d.args[e.CallID] = args
		d.mu.Unlock()
		t := d.tools[e.Name]
		switch {
		case t.Display.Title != nil:
			e.Title = t.Display.Title(args)
		case t.Label != "":
			e.Title = t.Label
		default:
			e.Title = e.Name
		}
		e.Action = t.Display.Action
		if e.Action == (tool.Action{}) {
			e.Action = fallbackAction
		}
		return e
	case event.ToolEnd:
		d.mu.Lock()
		args := d.args[e.CallID]
		delete(d.args, e.CallID)
		d.mu.Unlock()
		if e.IsError {
			return e
		}
		if summary := d.tools[e.Name].Display.Summary; summary != nil {
			e.Summary = summary(args, e.Output)
		} else {
			e.Summary = lineCount(e.Output)
		}
		return e
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
