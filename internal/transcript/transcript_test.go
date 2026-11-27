package transcript

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
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
		event.QueueChanged{Pending: []event.Queued{{Text: "x"}}},
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

func TestUserCommandDrawsAsRestored(t *testing.T) {
	cmd := event.UserCommand{Command: "make", State: event.CommandExited, Elapsed: time.Second, Exit: "exit 2", Failed: true, Output: "boom"}
	events := []event.Event{start("a"), end("a"), cmd, start("b"), end("b")}
	var live, restored Transcript
	for _, ev := range events {
		live.Apply(ev)
	}
	restored.Restore(events)

	want := []Block{row("a", OK), Command{key: "item:1", UserCommand: cmd}, row("b", OK)}
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
	tr.Apply(event.QueueChanged{Pending: []event.Queued{{Text: "x"}}})
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

func TestPlanRequestedLandsOnTheRunningSoloCallAndSurvivesItsEnd(t *testing.T) {
	planStart := func(id string) event.ToolStart {
		return event.ToolStart{CallID: id, Name: "ExitPlanMode", Title: "Plan approval", Solo: true}
	}
	plans := func(tr *Transcript) map[string]string {
		out := map[string]string{}
		for _, b := range tr.Blocks() {
			if r, ok := b.(Row); ok {
				out[r.Call.ID] = r.Call.Plan
			}
		}
		return out
	}
	var tr Transcript
	tr.Apply(planStart("p"))
	tr.Apply(start("a"))
	tr.Apply(event.PlanRequested{Token: 1, Plan: "# v1"})
	assert.Equal(t, map[string]string{"a": "", "p": "# v1"}, plans(&tr), "a plain call does not take the plan")

	tr.Apply(event.PlanRequested{Token: 1, Plan: "# v2"})
	assert.Equal(t, "# v2", plans(&tr)["p"], "a repeat replaces the text")

	tr.Apply(event.PlanEnded{Token: 1})
	tr.Apply(end("p"))
	tr.Apply(end("a"))
	assert.Equal(t, "# v2", plans(&tr)["p"], "the plan outlives the call's end")

	before := tr.Blocks()
	tr.Apply(event.PlanRequested{Token: 2, Plan: "# stray"})
	assert.Equal(t, before, tr.Blocks(), "a plan with no solo call running is dropped")

	tr.Apply(planStart("q"))
	tr.Apply(event.PlanRequested{Token: 2, Plan: "# next"})
	assert.Equal(t, map[string]string{"a": "", "p": "# v2", "q": "# next"}, plans(&tr))
}

func TestToolEndSwapsTitleOnSuccessOnly(t *testing.T) {
	started := event.ToolStart{CallID: "a", Title: "Write a.go", Done: "Wrote a.go", Change: "+2 -1", Preview: []tool.PreviewLine{{Kind: tool.Added, Text: "x"}}}
	var tr Transcript
	tr.Apply(started)
	title, ok := tr.Running()
	assert.True(t, ok)
	assert.Equal(t, "Write a.go", title)

	tr.Apply(event.ToolEnd{CallID: "a", Summary: "2 occurrences"})
	assert.Equal(t, []Block{Row{Call: Call{
		ID: "a", Title: "Wrote a.go", Summary: "+2 -1 · 2 occurrences", State: OK, Preview: started.Preview,
	}}}, tr.Blocks())

	started.CallID = "b"
	tr.Apply(started)
	tr.Apply(event.ToolEnd{CallID: "b", Output: "boom\nbang", IsError: true, Summary: "failed", Body: "boom\nbang"})
	row := tr.Blocks()[1].(Row)
	assert.Equal(t, Call{ID: "b", Title: "Write a.go", Summary: "failed", Body: "boom\nbang", State: Failed}, row.Call)
}

func TestToolEndOutcomeOverridesTheStartOnSuccessOnly(t *testing.T) {
	searched := tool.Action{Verb: "Searched", One: "the web"}
	started := event.ToolStart{CallID: "a", Title: "Fetch", Done: "Fetched", Action: readAction}
	for _, tc := range []struct {
		name  string
		end   event.ToolEnd
		title string
		act   tool.Action
		state State
	}{
		{"overrides", event.ToolEnd{CallID: "a", Done: "Searched x", Action: searched}, "Searched x", searched, OK},
		{"zero keeps the start's", event.ToolEnd{CallID: "a"}, "Fetched", readAction, OK},
		{"failed ignores", event.ToolEnd{CallID: "a", IsError: true, Done: "Searched x", Action: searched}, "Fetch", readAction, Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tr Transcript
			tr.Apply(started)
			tr.Apply(tc.end)
			c := tr.Blocks()[0].(Row).Call
			assert.Equal(t, tc.title, c.Title)
			assert.Equal(t, tc.act, c.Action)
			assert.Equal(t, tc.state, c.State)
		})
	}
}

func TestToolEndKeepsTheCommandWhateverTheOutcome(t *testing.T) {
	for kind, end := range map[string]event.ToolEnd{
		"ok":          {CallID: "a", Summary: "1 line"},
		"failed":      {CallID: "a", IsError: true, Summary: "failed"},
		"denied":      {CallID: "a", IsError: true, Kind: event.EndDenied},
		"rejected":    {CallID: "a", IsError: true, Kind: event.EndRejected},
		"interrupted": {CallID: "a", Kind: event.EndInterrupted},
	} {
		events := []event.Event{event.ToolStart{CallID: "a", Title: "Bash x …", Command: "x\ny"}, end}
		var live, restored Transcript
		for _, ev := range events {
			live.Apply(ev)
		}
		restored.Restore(events)
		assert.Equal(t, "x\ny", live.Blocks()[0].(Row).Call.Command, kind)
		assert.Equal(t, live.Blocks(), restored.Blocks(), kind)
	}
}

func TestToolEndSummaryJoinsOnlyWhatIsSet(t *testing.T) {
	for _, tc := range []struct{ change, summary, want string }{
		{"+1", "", "+1"},
		{"", "3 lines", "3 lines"},
		{"", "", ""},
	} {
		var tr Transcript
		tr.Apply(event.ToolStart{CallID: "a", Change: tc.change})
		tr.Apply(event.ToolEnd{CallID: "a", Summary: tc.summary})
		assert.Equal(t, tc.want, tr.Blocks()[0].(Row).Call.Summary)
	}
}

func TestToolEndKindsSetState(t *testing.T) {
	for kind, want := range map[event.EndKind]State{
		event.EndRan:         Failed,
		event.EndDenied:      Denied,
		event.EndRejected:    Rejected,
		event.EndInterrupted: Unfinished,
	} {
		var tr Transcript
		tr.Apply(event.ToolStart{CallID: "a", Done: "Done"})
		tr.Apply(event.ToolEnd{CallID: "a", IsError: true, Kind: kind, Summary: "s", Body: "b"})
		c := tr.Blocks()[0].(Row).Call
		assert.Equal(t, want, c.State)
		assert.Empty(t, c.Title)
		assert.Equal(t, "s", c.Summary)
		assert.Equal(t, "b", c.Body)
	}
}

func TestRestoreMatchesApply(t *testing.T) {
	events := []event.Event{
		event.TurnStart{Prompt: "go"},
		event.ToolStart{CallID: "a", Title: "Write a", Done: "Wrote a", Change: "+1"},
		event.ToolEnd{CallID: "a"},
		event.ToolStart{CallID: "b", Title: "Bash x", Done: "Ran x", Unsandboxed: true},
		event.ToolEnd{CallID: "b", IsError: true, Kind: event.EndDenied, Summary: "denied"},
		event.ToolStart{CallID: "c", Title: "Ask", Solo: true},
		event.ToolEnd{CallID: "c", Summary: "answered"},
	}
	var live, restored Transcript
	for _, ev := range events {
		live.Apply(ev)
	}
	restored.Restore(events)
	assert.Equal(t, live.Blocks(), restored.Blocks())
}

func TestGroupCountsTheSubjectsOfItsCalls(t *testing.T) {
	edited := tool.Action{Verb: "Edited", One: "file", Many: "files"}
	events := []event.Event{event.TurnStart{Prompt: "go"}}
	for i, subject := range []string{"a.go", "b.go", "a.go"} {
		id := strconv.Itoa(i)
		events = append(events,
			event.ToolStart{CallID: id, Name: "Edit", Title: "Edit " + subject, Done: "Edited " + subject, Action: edited},
			event.ToolEnd{CallID: id, Name: "Edit", Subject: subject})
	}
	var live, restored Transcript
	for _, ev := range events {
		live.Apply(ev)
	}
	restored.Restore(events)

	for _, tr := range []Transcript{live, restored} {
		var sentences []string
		for _, b := range tr.Blocks() {
			if g, ok := b.(Group); ok {
				sentences = append(sentences, g.Sentence())
			}
		}
		assert.Equal(t, []string{"Edited 2 files"}, sentences)
	}
}

func TestRestoreMatchesApplyForOutcomeRows(t *testing.T) {
	events := []event.Event{
		event.TurnStart{Prompt: "go"},
		event.ToolStart{CallID: "a", Name: "TaskStop", Title: "TaskStop t1", Done: "Stopped task t1", Action: tool.Action{Verb: "Stopped", One: "task", Many: "tasks"}},
		event.ToolEnd{CallID: "a", Name: "TaskStop", Done: "TaskStop researcher › survey peers", Action: tool.Action{Verb: "Checked on", One: "agent", Many: "agents"}, Summary: "already completed"},
		event.TextDelta{Text: "x"},
		event.ToolStart{CallID: "b", Name: "Bash", Title: "Bash make", Done: "Ran make", Action: tool.Action{Verb: "Ran", One: "command", Many: "commands"}},
		event.ToolEnd{CallID: "b", Name: "Bash", Done: "Moved make to background", Action: tool.Action{Verb: "Started", One: "shell", Many: "shells"}},
		event.TextDelta{Text: "x"},
		event.ToolStart{CallID: "c", Name: "WebFetch", Title: "WebFetch example.com", Done: "Fetched example.com", Action: tool.Action{Verb: "Fetched", One: "page", Many: "pages"}},
		event.ToolEnd{CallID: "c", Name: "WebFetch", Done: "WebFetch example.com", Summary: "redirected", Action: tool.Action{Verb: "Hit", One: "redirect", Many: "redirects"}},
		event.TextDelta{Text: "x"},
		event.ToolStart{CallID: "d", Name: "Edit", Title: "Edit plan.md", Done: "Edited plan.md", Change: "+1 -1", Action: tool.Action{Verb: "Edited", One: "file", Many: "files"}},
		event.ToolEnd{CallID: "d", Name: "Edit", Done: "Edited the plan", Action: tool.Action{Verb: "Edited", One: "the plan"}},
	}
	var live, restored Transcript
	for _, ev := range events {
		live.Apply(ev)
	}
	restored.Restore(events)

	assert.Equal(t, live.Blocks(), restored.Blocks())
	titles := []string{}
	for _, b := range live.Blocks() {
		if row, ok := b.(Row); ok {
			titles = append(titles, row.Call.Title)
		}
	}
	assert.Equal(t, []string{"TaskStop researcher › survey peers", "Moved make to background", "WebFetch example.com", "Edited the plan"}, titles)
}

func TestWorkflowTaskEndedCarriesItsOutcome(t *testing.T) {
	ev := event.TaskEnded{ID: "t4", Agent: "workflow", Command: "audit", Status: "failed", Elapsed: time.Minute, Agents: 9, Failed: 1, Reason: "boom", Result: "{}"}
	var live, restored Transcript
	live.Apply(ev)
	restored.Restore([]event.Event{ev})

	want := []Block{Task{key: "item:0", ID: "t4", Agent: "workflow", Command: "audit", Status: "failed", Elapsed: time.Minute, Agents: 9, Failed: 1, Reason: "boom", Result: "{}"}}
	assert.Equal(t, want, live.Blocks())
	assert.Equal(t, want, restored.Blocks())
}

func TestLastReply(t *testing.T) {
	apply := func(evs ...event.Event) *Transcript {
		var tr Transcript
		for _, ev := range evs {
			tr.Apply(ev)
		}
		return &tr
	}
	end := func(text string) event.ReplyEnd { return event.ReplyEnd{Text: text} }
	assert.Empty(t, apply().LastReply())
	assert.Empty(t, apply(event.TurnStart{Prompt: "go"}).LastReply())

	assert.Equal(t, "first", apply(
		event.TurnStart{Prompt: "a"}, event.TextDelta{Text: "first"}, end("first"), event.TurnEnd{},
		event.TurnStart{Prompt: "b"}, event.TextDelta{Text: "sec"},
	).LastReply(), "a reply still being written is skipped")

	assert.Equal(t, "before", apply(
		event.TurnStart{Prompt: "a"}, event.TextDelta{Text: "before"}, end("before"), start("a"),
	).LastReply(), "text ended before a tool call is a reply")

	assert.Equal(t, "first", apply(
		event.TextDelta{Text: "first"}, end("first"), event.TextDelta{Text: " \n"}, end(" \n"),
	).LastReply(), "blank text does not replace the earlier reply")

	assert.Equal(t, "\n second\n", apply(
		event.TurnStart{Prompt: "a"}, event.TextDelta{Text: "\n second\n"}, end("\n second\n"), event.TurnEnd{},
	).LastReply(), "raw text is not trimmed")

	assert.Equal(t, "Hello world", apply(
		event.TextDelta{Text: "Hel"}, end("Hel"), event.Notice{Text: "retrying"}, event.TextDelta{Text: "Hello world"}, end("Hello world"),
	).LastReply(), "a retried reply replaces the failed one")

	events := []event.Event{
		event.TurnStart{Prompt: "a"},
		event.TextDelta{Text: "first"},
		end("first"), start("a"),
		event.TextDelta{Text: "last"},
		end("last"),
		event.TurnEnd{},
	}
	var restored Transcript
	restored.Restore(events)
	assert.Equal(t, apply(events...).LastReply(), restored.LastReply())
	assert.Equal(t, "last", restored.LastReply())
}

func TestCopyTargetReplacesAndClears(t *testing.T) {
	var tr Transcript
	tr.Apply(event.ReplyEnd{Text: "later"})
	tr.Apply(event.CopyTarget{Text: "earlier"})
	assert.Equal(t, "earlier", tr.LastReply())
	tr.Apply(event.CopyTarget{})
	assert.Empty(t, tr.LastReply())
}

func TestLastReplyInterruptedByNotice(t *testing.T) {
	events := []event.Event{
		event.TextDelta{Text: "before\n```go\n"},
		event.Notice{Text: "notice"},
		event.TextDelta{Text: "code\n```\nafter"},
		event.ReplyEnd{Text: "before\n```go\ncode\n```\nafter"},
		event.TurnEnd{},
	}
	var live, restored Transcript
	for _, ev := range events {
		live.Apply(ev)
	}
	restored.Restore(events)
	want := "before\n```go\ncode\n```\nafter"
	assert.Equal(t, want, live.LastReply())
	assert.Equal(t, want, restored.LastReply())
	assert.Equal(t, []Block{
		Assistant{key: "item:0", Text: "before\n```go\n"},
		Notice{key: "item:1", Text: "notice"},
		Assistant{key: "item:2", Text: "code\n```\nafter"},
		Turn{key: "item:3"},
	}, live.Blocks())
}
