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

func TestPlanRequestedAppendsABlockAndTheSameTokenReplacesItsText(t *testing.T) {
	var tr Transcript
	tr.Apply(start("a"))
	tr.Apply(event.PlanRequested{Token: 1, Plan: "# v1"})
	tr.Apply(end("a"))
	assert.Equal(t, []Block{row("a", OK), Plan{key: "item:1", Text: "# v1"}}, tr.Blocks(), "the tool row is an ordinary row")

	tr.Apply(event.PlanRequested{Token: 1, Plan: "# v2"})
	tr.Apply(event.PlanEnded{Token: 1})
	assert.Equal(t, []Block{row("a", OK), Plan{key: "item:1", Text: "# v2"}}, tr.Blocks(), "the block outlives the dialog")

	tr.Apply(event.PlanRequested{Token: 2, Plan: "# next"})
	assert.Equal(t, []Block{row("a", OK), Plan{key: "item:1", Text: "# v2"}, Plan{key: "item:2", Text: "# next"}}, tr.Blocks())
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
