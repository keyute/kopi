package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/skill"
	"github.com/keyute/kopi/internal/tool"
	"github.com/keyute/kopi/internal/transcript"
)

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		41 * time.Second:                          "41s",
		1500 * time.Millisecond:                   "1s",
		72 * time.Second:                          "1m 12s",
		time.Hour + 2*time.Minute + 5*time.Second: "1h 2m",
	} {
		assert.Equal(t, want, formatDuration(d))
	}
}

func TestRenderStripsUntrustedTerminalControls(t *testing.T) {
	for name, attack := range map[string]string{
		"DCS":        "\x1bPsecret\x1b\\",
		"tmux OSC52": "\x1bPtmux;\x1b\x1b]52;c;c2VjcmV0\a\x1b\\",
		"APC":        "\x1b_secret\x1b\\",
		"SOS":        "\x1bXsecret\x1b\\",
		"PM":         "\x1b^secret\x1b\\",
		"CSI":        "\x1b[2J",
		"OSC":        "\x1b]52;c;c2VjcmV0\a",
		"controls":   "\x00\x01\a\b\v\f\r\x0e\x0f\x7f\u0080\u0085\u009b\u009f",
	} {
		t.Run(name, func(t *testing.T) {
			const width = 24
			clean := "ordinary café 世界\n\tmore words wrap here"
			for _, state := range []transcript.State{transcript.OK, transcript.Failed} {
				safe := transcript.Call{ID: "call", State: state, Title: clean, Summary: clean, Body: clean}
				unsafe := safe
				unsafe.Title, unsafe.Summary, unsafe.Body = attack+clean, attack+clean, attack+clean
				fold := blockFolds{expanded: []bool{true}, open: true}
				want := renderRow(safe, fold, width, mocha())
				got := renderRow(unsafe, fold, width, mocha())
				assert.Equal(t, want, got)
				for _, l := range got {
					assert.LessOrEqual(t, ansi.StringWidth(l.text), width)
				}
				assert.NotEqual(t, ansi.Strip(got[0].text), got[0].text, "Kopi styling survives")
			}
			for _, sealed := range []bool{false, true} {
				for _, open := range []bool{false, true} {
					g := transcript.Group{Sealed: sealed, Calls: []transcript.Call{{ID: "call", Title: clean, Summary: clean, Action: tool.Action{Verb: "Read", One: "file"}}}}
					fold := blockFolds{open: open, expanded: []bool{false}}
					want := renderGroup(g, fold, width, mocha())
					g.Calls[0].Title = attack + clean
					g.Calls[0].Summary = attack + clean
					g.Calls[0].Action.Verb = attack + "Read"
					assert.Equal(t, want, renderGroup(g, fold, width, mocha()))
				}
			}
			assert.Equal(t, shadedBox("❯ ", shade(mocha()), clean, nil, width, mocha()), shadedBox("❯ ", shade(mocha()), attack+clean, nil, width, mocha()))
			md := &markdown{width: width}
			assert.Equal(t, md.render(clean), md.render(attack+clean), "plain markdown fallback")
		})
	}
}

func TestWrapPreservesPlainText(t *testing.T) {
	assert.Equal(t, []string{"café 世界", "    tab", "ordinary", "words wrap"}, wrap("café 世界\n\ttab\nordinary words wrap", 10))
	assert.Equal(t, "a\nb", strings.Join(wrap("a\nb", 10), "\n"))
}

func TestTaskRow(t *testing.T) {
	th := mocha()
	for _, tc := range []struct {
		status, want string
		bullet, text lipgloss.Style
	}{
		{"completed", "• shell finished · make build · 12s", fg(th.success), lipgloss.NewStyle()},
		{"failed", "• shell failed · make build · 12s", fg(th.error), fg(th.error)},
		{"stopped", "• shell stopped · make build · 12s", fg(th.error), fg(th.error)},
	} {
		t.Run(tc.status, func(t *testing.T) {
			m, _ := newTestModel(t, 60, 20)
			send(m, event.TaskEnded{ID: "t1", Command: "make build\nmake test", Status: tc.status, Elapsed: 12 * time.Second})
			assert.Equal(t, []string{"", tc.want}, body(m))
			got := m.view().lines[len(m.view().lines)-1].text
			assert.Equal(t, tc.bullet.Render("• ")+tc.text.Render(strings.TrimPrefix(tc.want, "• ")), got)
		})
	}
}

func TestFailedShellRowShowsItsReason(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, event.TaskEnded{ID: "t1", Command: "make build", Status: "failed", Elapsed: 12 * time.Second, Reason: "exit code 2"})
	assert.Equal(t, []string{"", "• shell failed · make build · 12s", "  exit code 2"}, body(m))
}

func TestShellRowOpensToItsOutput(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, event.TaskEnded{ID: "t1", Command: "make build", Status: "completed", Elapsed: 12 * time.Second, Result: "step 1\rstep 2\nbuilt"})
	clickText(t, m, "• shell finished · make build · 12s")
	assert.Equal(t, []string{"", "• shell finished · make build · 12s", "    step 2", "    built"}, body(m))
}

func TestAgentTaskRowOpensToItsReply(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, event.TaskEnded{ID: "t3", Agent: "explore", Command: "look around", Status: "completed", Elapsed: 134 * time.Second, Result: "found it"})
	clickText(t, m, "• explore finished · look around · 2m 14s")
	assert.Equal(t, []string{"", "• explore finished · look around · 2m 14s", "    found it"}, body(m))
}

func TestAgentTaskRow(t *testing.T) {
	th := mocha()
	for _, tc := range []struct {
		status, want string
		bullet, text lipgloss.Style
	}{
		{"completed", "• explore finished · look around · 2m 14s", fg(th.success), lipgloss.NewStyle()},
		{"failed", "• explore failed · look around · 2m 14s", fg(th.error), fg(th.error)},
		{"stopped", "• explore stopped · look around · 2m 14s", fg(th.error), fg(th.error)},
	} {
		t.Run(tc.status, func(t *testing.T) {
			m, _ := newTestModel(t, 60, 20)
			send(m, event.TaskEnded{ID: "t3", Agent: "explore", Command: "look around\nmore", Status: tc.status, Elapsed: 134 * time.Second})
			assert.Equal(t, []string{"", tc.want}, body(m))
			got := m.view().lines[len(m.view().lines)-1].text
			assert.Equal(t, tc.bullet.Render("• ")+tc.text.Render(strings.TrimPrefix(tc.want, "• ")), got)
		})
	}
}

func TestWorkflowTaskRow(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, event.TaskEnded{ID: "t3", Agent: "workflow", Command: "audit concurrency", Status: "completed", Elapsed: 134 * time.Second})
	assert.Equal(t, []string{"", "• workflow finished · audit concurrency · 2m 14s"}, body(m))
}

func workflowEnd(status string, agents, failed int, reason, result string) event.TaskEnded {
	return event.TaskEnded{ID: "t3", Agent: "workflow", Command: "audit route handlers", Status: status, Elapsed: 363 * time.Second, Agents: agents, Failed: failed, Reason: reason, Result: result}
}

func TestWorkflowEndRowShowsItsAgents(t *testing.T) {
	th := mocha()
	for name, tc := range map[string]struct {
		ev   event.TaskEnded
		want []string
	}{
		"finished": {workflowEnd("completed", 9, 0, "", ""), []string{"• workflow finished · audit route handlers · 6m 3s", "  ↳ 9 agents"}},
		"one":      {workflowEnd("completed", 1, 0, "", ""), []string{"• workflow finished · audit route handlers · 6m 3s", "  ↳ 1 agent"}},
		"failures": {workflowEnd("completed", 9, 1, "", ""), []string{"• workflow finished · audit route handlers · 6m 3s", "  ↳ 9 agents · 1 failed"}},
		"failed": {
			workflowEnd("failed", 2, 2, "first line of why\nsecond line of why is long enough to wrap around the width", ""),
			[]string{
				"• workflow failed · audit route handlers · 6m 3s", "  ↳ 2 agents · 2 failed", "  first line of why",
				"  second line of why is long enough to wrap around the width",
			},
		},
		"stopped":   {workflowEnd("stopped", 3, 3, "", ""), []string{"• workflow stopped · audit route handlers · 6m 3s", "  ↳ 3 agents · 3 failed"}},
		"no agents": {workflowEnd("completed", 0, 0, "", ""), []string{"• workflow finished · audit route handlers · 6m 3s"}},
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := newTestModel(t, 60, 20)
			send(m, tc.ev)
			assert.Equal(t, append([]string{""}, tc.want...), body(m))
		})
	}

	ls := renderTask(transcript.Task{Status: "failed", Agents: 1, Reason: "why"}, false, 40, th)
	require.Len(t, ls, 3)
	assert.Equal(t, fg(th.dim).Render("  ↳ ")+fg(th.dim).Render("1 agent"), ls[1].text)
	assert.Equal(t, fg(th.error).Render("  ")+fg(th.error).Render("why"), ls[2].text)
}

func TestTaskWithAResultOpensToItVerbatim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 60, 30)
		send(m, workflowEnd("completed", 2, 0, "", "line one\n  indented line two that is long enough to wrap"))
		head := []string{"", "• workflow finished · audit route handlers · 6m 3s", "  ↳ 2 agents"}
		require.Equal(t, head, body(m))

		clickText(t, m, "• workflow finished · audit route handlers · 6m 3s")
		assert.Equal(t, append(head, "    line one", "      indented line two that is long enough to wrap"), body(m))

		clickText(t, m, "• workflow finished · audit route handlers · 6m 3s")
		assert.Equal(t, head, body(m))
	})
}

func TestTaskWithoutAResultIsInert(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, workflowEnd("completed", 2, 0, "", ""))
	assert.Equal(t, hit{}, m.view().lines[len(m.view().lines)-2].hit)
}

func TestWorkflowLaunchRowOpensToTheScript(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		script := []tool.PreviewLine{{Kind: tool.Context, Text: "const a = 1"}, {Kind: tool.Context, Text: "await agent(a)"}}
		m, _ := newTestModel(t, 40, 30)
		send(m, event.ToolStart{CallID: "w", Name: "Workflow", Title: "Workflow › audit", Done: "Launched workflow › audit", Preview: script},
			event.ToolEnd{CallID: "w", Name: "Workflow", Summary: "launched"})
		collapsed := []string{"", "• Launched workflow › audit", "  ↳ launched"}
		require.Equal(t, collapsed, body(m))

		clickText(t, m, "• Launched workflow › audit")
		assert.Equal(t, append(collapsed, "      const a = 1", "      await agent(a)"), body(m))
	})
}

func TestAgentTaskRowCutsALongDescription(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	send(m, event.TaskEnded{ID: "t3", Agent: "explore", Command: strings.Repeat("x", 100), Status: "completed", Elapsed: time.Minute})
	assert.Equal(t, []string{"", "• explore finished · " + strings.Repeat("x", 10) + "… · 1m 0s"}, body(m))
}

func agentStart(id, description string) event.ToolStart {
	return event.ToolStart{CallID: id, Name: "Agent", Title: "explore › " + description, Action: tool.Action{Verb: "Launched", One: "agent", Many: "agents"}}
}

func agentEnd(id string) event.ToolEnd {
	return event.ToolEnd{CallID: id, Name: "Agent", Summary: "launched", Output: "Agent t1 started.", Body: "Agent t1 started."}
}

func TestAgentToolRowAndGroup(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, agentStart("a", "look"), agentEnd("a"))
	assert.Equal(t, []string{"", "• explore › look", "  ↳ launched"}, body(m))

	m, _ = newTestModel(t, 60, 20)
	send(m, agentStart("a", "look"), agentEnd("a"), agentStart("b", "dig"), agentEnd("b"))
	assert.Equal(t, []string{"", "• Launched 2 agents", "  ↳ explore › look · launched", "  ↳ explore › dig · launched"}, body(m))
}

func TestTaskRowCutsALongCommand(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	send(m, event.TaskEnded{ID: "t1", Command: strings.Repeat("x", 100), Status: "completed", Elapsed: time.Minute})
	assert.Equal(t, []string{"", "• shell finished · " + strings.Repeat("x", 12) + "… · 1m 0s"}, body(m))
}

func TestPlanBlockDrawsAMutedTitleAndMarkdownAndRerendersWhenEdited(t *testing.T) {
	var tr transcript.Transcript
	tr.Apply(event.PlanRequested{Token: 1, Plan: "# Heading\n\n- first step"})
	var c renderCache
	text := func() []string {
		var out []string
		for _, l := range c.lines(tr.Blocks()[0], blockFolds{}, nil, 40, mocha()) {
			out = append(out, strings.TrimRight(ansi.Strip(l.text), " "))
		}
		return out
	}

	got := text()
	assert.Equal(t, "Plan", got[0])
	assert.Equal(t, fg(mocha().muted).Render("Plan"), c.lines(tr.Blocks()[0], blockFolds{}, nil, 40, mocha())[0].text)
	joined := strings.Join(got[1:], "\n")
	assert.Contains(t, joined, "Heading")
	assert.NotContains(t, joined, "# Heading", "the Markdown is rendered")
	assert.Contains(t, joined, "first step")

	tr.Apply(event.PlanRequested{Token: 1, Plan: "# Heading\n\n- second step"})
	joined = strings.Join(text()[1:], "\n")
	assert.Contains(t, joined, "second step")
	assert.NotContains(t, joined, "first step")
}

func TestOKRowShowsItsDoneForm(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, event.ToolStart{CallID: "a", Name: "Bash", Title: "Bash ls", Done: "Ran ls", Action: tool.Action{Verb: "Ran", One: "command", Many: "commands"}},
		event.ToolEnd{CallID: "a", Name: "Bash", Summary: "2 lines", Output: "a\nb", Body: "a\nb"})
	assert.Equal(t, []string{"", "• Ran ls", "  ↳ 2 lines"}, body(m))
}

func TestGroupSentenceCountsUnsandboxedCalls(t *testing.T) {
	bash := tool.Action{Verb: "Ran", One: "command", Many: "commands"}
	events := []event.Event{
		event.ToolStart{CallID: "a", Name: "Bash", Title: "Bash a", Done: "Ran a", Action: bash},
		event.ToolEnd{CallID: "a", Name: "Bash", Summary: "no output"},
		event.ToolStart{CallID: "b", Name: "Bash", Title: "Bash (unsandboxed) b", Done: "Ran b · unsandboxed", Action: bash, Unsandboxed: true},
		event.ToolEnd{CallID: "b", Name: "Bash", Summary: "no output"},
	}
	m, _ := newTestModel(t, 60, 20)
	send(m, events...)
	assert.Equal(t, []string{"", "• Ran 2 commands (1 unsandboxed)", "  ↳ Ran a · no output", "  ↳ Ran b · unsandboxed · no output"}, body(m))

	send(m, event.TextDelta{Text: "Done."})
	assert.Equal(t, []string{"", "▸ Ran 2 commands (1 unsandboxed)", "", "Done."}, body(m))
}

func TestGroupSentenceFollowsItsRowsVerbs(t *testing.T) {
	plan := tool.Action{Verb: "Read", One: "the plan"}
	web := tool.Action{Verb: "Searched", One: "the web"}
	shell := tool.Action{Verb: "Started", One: "shell", Many: "shells"}
	agents := tool.Action{Verb: "Checked on", One: "agent", Many: "agents"}
	call := func(id, name, title string, action tool.Action) []event.Event {
		return []event.Event{
			event.ToolStart{CallID: id, Name: name, Title: title, Done: title, Action: action},
			event.ToolEnd{CallID: id, Name: name, Done: title, Action: action},
		}
	}
	var events []event.Event
	for _, c := range [][]event.Event{
		{start("a"), ok("a", "")},
		{start("b"), ok("b", "")},
		call("c", "Read", "Read the plan", plan),
		call("d", "WebSearch", "Searched the web for x", web),
		call("e", "WebSearch", "Searched the web for y", web),
		call("f", "WebSearch", "Searched the web for z", web),
		call("g", "Bash", "Started make in background", shell),
		call("h", "TaskOutput", "Checked on researcher › survey peers", agents),
		call("i", "TaskOutput", "Checked on explorer › map the repo", agents),
	} {
		events = append(events, c...)
	}
	sentence := "Read 2 files, read the plan, searched the web 3 times, started 1 shell, checked on 2 agents"
	members := []string{"  ↳ a.go", "  ↳ b.go", "  ↳ Read the plan", "  ↳ Searched the web for x", "  ↳ Searched the web for y", "  ↳ Searched the web for z", "  ↳ Started make in background", "  ↳ Checked on researcher › survey peers", "  ↳ Checked on explorer › map the repo"}
	m, _ := newTestModel(t, 120, 30)
	send(m, events...)
	assert.Equal(t, append([]string{"", "• " + sentence}, members...), body(m))

	send(m, event.TextDelta{Text: "Done."})
	assert.Equal(t, []string{"", "▸ " + sentence, "", "Done."}, body(m))
}

func TestTaskOutputRowWithoutANameLineReadsAsATask(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, event.ToolStart{CallID: "a", Name: "TaskOutput", Title: "TaskOutput t3", Done: "Checked task t3", Action: tool.Action{Verb: "Checked", One: "task", Many: "tasks"}},
		event.ToolEnd{CallID: "a", Name: "TaskOutput", Summary: "completed (exit code 0)", Output: "Task t3 completed (exit code 0).\nout", Body: "out"})
	assert.Equal(t, []string{"", "• Checked task t3", "  ↳ completed (exit code 0)"}, body(m))
}

func TestAgentRowOpensToItsPromptOnlyWithAPreview(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prompt := []tool.PreviewLine{{Kind: tool.Context, Text: "look for the bug"}, {Kind: tool.Context, Text: "report the file"}}
		launched := func(id string) event.ToolEnd { return event.ToolEnd{CallID: id, Name: "Agent", Summary: "launched"} }
		m, _ := newTestModel(t, 60, 30)
		send(m, event.ToolStart{CallID: "a", Name: "Agent", Title: "explore › look", Preview: prompt}, launched("a"))
		collapsed := []string{"", "• explore › look", "  ↳ launched"}
		require.Equal(t, collapsed, body(m))

		clickText(t, m, "• explore › look")
		assert.Equal(t, append(collapsed, "      look for the bug", "      report the file"), body(m))

		m, _ = newTestModel(t, 60, 30)
		send(m, agentStart("b", "look"), launched("b"))
		assert.Equal(t, hit{}, m.view().lines[len(m.view().lines)-2].hit)
	})
}

func TestRejectedRowShowsFeedbackInFull(t *testing.T) {
	th := mocha()
	m, _ := newTestModel(t, 60, 20)
	send(m, start("a"), event.ToolEnd{CallID: "a", IsError: true, Kind: event.EndRejected, Summary: "rejected", Body: "use b\ninstead"})
	assert.Equal(t, []string{"", "• a.go", "  ↳ rejected", "    use b", "    instead"}, body(m))
	lines := m.view().lines
	assert.Equal(t, fg(th.warning).Render("• ")+"a.go", lines[len(lines)-4].text)
	for _, l := range lines {
		assert.Equal(t, hit{}, l.hit)
	}

	m, _ = newTestModel(t, 60, 20)
	send(m, start("a"), event.ToolEnd{CallID: "a", IsError: true, Kind: event.EndRejected, Summary: "rejected"})
	assert.Equal(t, []string{"", "• a.go", "  ↳ rejected"}, body(m))
}

func TestDeniedRowHasNoBodyAndNoHit(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, start("a"), event.ToolEnd{CallID: "a", IsError: true, Kind: event.EndDenied, Summary: "denied · plan mode", Body: "refused"})
	assert.Equal(t, []string{"", "• a.go", "  ↳ denied · plan mode"}, body(m))
	for _, l := range m.view().lines {
		assert.Equal(t, hit{}, l.hit)
	}
}

func TestFailedRowDrawsErrorSummaryAndCapsItsBody(t *testing.T) {
	th := mocha()
	five := "1\n2\n\n3\n4\n5\n"
	got := renderRow(transcript.Call{ID: "a", State: transcript.Failed, Title: "t", Summary: "failed", Body: five}, blockFolds{expanded: []bool{false}}, 40, th)
	require.Len(t, got, 8)
	assert.Equal(t, line{text: fg(th.error).Render("  ↳ ") + fg(th.error).Render("failed")}, got[1])
	assert.Equal(t, hit{}, got[0].hit, "five lines are shown in full, so there is nothing to open")

	six := transcript.Call{ID: "a", State: transcript.Failed, Title: "t", Summary: "failed", Body: five + "6\n"}
	got = renderRow(six, blockFolds{expanded: []bool{false}}, 40, th)
	assert.Equal(t, hit{kind: hitOutput, id: "a"}, got[0].hit)
	assert.Equal(t, hit{}, got[1].hit)
	var text []string
	for _, l := range got {
		text = append(text, ansi.Strip(l.text))
	}
	assert.Equal(t, []string{"• t", "  ↳ failed", "    1", "    2", "    … 2 more lines", "    5", "    6"}, text)
}

func TestEditRowOpensToAllItsChangedLines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var preview []tool.PreviewLine
		want := []string{"", "• Edited a.go", "  ↳ +30 -30"}
		for i := range 30 {
			preview = append(preview, tool.PreviewLine{Kind: tool.Removed, Text: fmt.Sprintf("old%d", i)})
		}
		for i := range 30 {
			preview = append(preview, tool.PreviewLine{Kind: tool.Added, Text: fmt.Sprintf("new%d", i)})
		}
		m, _ := newTestModel(t, 40, 80)
		send(m, event.ToolStart{CallID: "a", Name: "Edit", Title: "Edit a.go", Done: "Edited a.go", Change: "+30 -30", Preview: preview},
			event.ToolEnd{CallID: "a", Name: "Edit", Output: "replaced 1 occurrence"})
		require.Equal(t, want, body(m))

		clickText(t, m, "• Edited a.go")
		got := body(m)
		require.Len(t, got, 3+60)
		assert.Equal(t, "    - old0", got[3])
		assert.Equal(t, "    - old29", got[32])
		assert.Equal(t, "    + new0", got[33])
		assert.Equal(t, "    + new29", got[62])
	})
}

func TestCarriageReturnsKeepWhatFollowsTheLast(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	send(m, start("a"), okOut("a", "3 lines", "10%\r50%\r100%\r\nline\r\nx\ry\r"))
	press(m, ctrlO)
	assert.Equal(t, []string{"", "• a.go", "  ↳ 3 lines", "    100%", "    line", "    y"}, body(m))

	var pv []tool.PreviewLine
	pv = append(pv, tool.PreviewLine{Kind: tool.Added, Text: "a\rb"})
	assert.Equal(t, line{text: "    " + fg(mocha().success).Render("+ ") + fg(mocha().success).Render("ab")}, renderRow(transcript.Call{State: transcript.OK, Title: "t", Preview: pv}, blockFolds{expanded: []bool{true}}, 40, mocha())[1])
}

func TestSoloRowBreaksAGroup(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, start("a"), ok("a", "1 line"), start("b"), ok("b", "1 line"),
		event.ToolStart{CallID: "q", Name: "AskUserQuestion", Title: "Asked about scope", Solo: true},
		event.ToolEnd{CallID: "q", Name: "AskUserQuestion", Summary: "all"},
		start("c"), ok("c", "1 line"))
	assert.Equal(t, []string{"", "▸ Read 2 files", "", "• Asked about scope", "  ↳ all", "", "• c.go", "  ↳ 1 line"}, body(m))
}

func bashStart(id, command string) event.ToolStart {
	return event.ToolStart{CallID: id, Name: "Bash", Title: "Bash first …", Command: command, Action: tool.Action{Verb: "Ran", One: "command", Many: "commands"}}
}

func TestOpenedBashRowShowsTheWholeCommandBeforeItsOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		send(m, bashStart("a", "first\nsecond line"), event.ToolEnd{CallID: "a", Name: "Bash", Summary: "1 line", Body: "out"})
		require.Equal(t, []string{"", "• Bash first …", "  ↳ 1 line"}, body(m))

		clickText(t, m, "• Bash first …")

		assert.Equal(t, []string{"", "• Bash first …", "  ↳ 1 line", "    $ first", "      second line", "    out"}, body(m))
	})
}

func TestBashRowWithACutCommandAndNoOutputOpens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		send(m, bashStart("a", "first\nsecond"), event.ToolEnd{CallID: "a", Name: "Bash", Summary: "no output"})

		clickText(t, m, "• Bash first …")

		assert.Equal(t, []string{"", "• Bash first …", "  ↳ no output", "    $ first", "      second"}, body(m))
	})
}

func TestCommandLinesKeepWhatBodyTextWouldDrop(t *testing.T) {
	th := mocha()
	c := transcript.Call{ID: "a", Command: "echo a\rb\n\n  c"}
	assert.Equal(t, []string{"    $ echo ab", "", "        c"}, plainRows(lineTexts(commandLines(c, 40, th))))
	assert.Nil(t, commandLines(transcript.Call{}, 40, th))
}

func TestRefusedAndFailedRowsOpenToTheCommand(t *testing.T) {
	for name, end := range map[string]event.ToolEnd{
		"failed":      {CallID: "a", Name: "Bash", IsError: true, Summary: "exit status 1", Output: "boom", Body: "boom"},
		"denied":      {CallID: "a", Name: "Bash", IsError: true, Kind: event.EndDenied, Summary: "denied", Body: "refused"},
		"rejected":    {CallID: "a", Name: "Bash", IsError: true, Kind: event.EndRejected, Summary: "rejected"},
		"interrupted": {CallID: "a", Name: "Bash", Kind: event.EndInterrupted},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, _ := newTestModel(t, 40, 30)
				send(m, bashStart("a", "first\nsecond"), end)
				closed := body(m)
				require.Equal(t, "• Bash first …", closed[1])

				clickText(t, m, "• Bash first …")
				open := body(m)
				at := slices.Index(open, "    $ first")
				require.Greater(t, at, 2, "the command follows the summary")
				assert.Equal(t, "      second", open[at+1])

				clickText(t, m, "• Bash first …")
				assert.Equal(t, closed, body(m))

				press(m, ctrlO)
				assert.Equal(t, open, body(m))
			})
		})
	}
}

func TestRefusedRowsWithoutACommandStayInert(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, end := range []event.ToolEnd{
			{CallID: "a", IsError: true, Kind: event.EndDenied, Summary: "denied"},
			{CallID: "a", IsError: true, Kind: event.EndRejected, Summary: "rejected"},
			{CallID: "a", Kind: event.EndInterrupted},
		} {
			m, _ := newTestModel(t, 40, 30)
			send(m, start("a"), end)
			want := body(m)
			for _, l := range m.view().lines {
				assert.Equal(t, hit{}, l.hit)
			}

			clickText(t, m, "• a.go")
			press(m, ctrlO)

			assert.Equal(t, want, body(m))
		}
	})
}

func TestFailedRowWithACommandAndAShortBodyOpens(t *testing.T) {
	th := mocha()
	c := transcript.Call{ID: "a", State: transcript.Failed, Title: "t", Summary: "failed", Body: "boom", Command: "a\nb"}
	got := renderRow(c, blockFolds{expanded: []bool{false}}, 40, th)
	assert.Equal(t, hit{kind: hitOutput, id: "a"}, got[0].hit)
	assert.Equal(t, []string{"• t", "  ↳ failed", "    boom"}, plainRows(lineTexts(got)))

	got = renderRow(c, blockFolds{expanded: []bool{true}}, 40, th)
	assert.Equal(t, []string{"• t", "  ↳ failed", "    $ a", "      b", "    boom"}, plainRows(lineTexts(got)))
}

func lineTexts(ls []line) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.text
	}
	return out
}

func TestOpenedGroupMemberShowsItsCommand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 60, 30)
		send(m, bashStart("a", "first\nsecond"), event.ToolEnd{CallID: "a", Name: "Bash", Summary: "1 line", Body: "out"},
			bashStart("b", "x"), event.ToolEnd{CallID: "b", Name: "Bash", Summary: "1 line", Body: "out"},
			event.TextDelta{Text: "Done."})
		clickText(t, m, "▸ Ran 2 commands")

		clickText(t, m, "  ↳ Bash first … · 1 line")

		assert.Equal(t, []string{"", "▾ Ran 2 commands", "  ↳ Bash first … · 1 line", "    $ first", "      second", "    out", "  ↳ Bash first … · 1 line", "", "Done."}, body(m))
	})
}

func TestShadedBoxStylesSpansOnWrappedRows(t *testing.T) {
	th := mocha()
	set, _ := skill.NewSet("skill", []skill.Skill{{Name: "a"}}, nil)
	spans := func(text string) []app.Span { return app.Spans(text, set, true) }
	text := "hello world /skill:a ultracode end"

	flat := shadedBox("❯ ", shade(th), text, nil, 20, th)
	got := shadedBox("❯ ", shade(th), text, spans, 20, th)

	require.Len(t, got, len(flat))
	for i := range got {
		assert.Equal(t, ansi.Strip(flat[i]), ansi.Strip(got[i]))
	}
	assert.Equal(t, flat[1], got[1])
	assert.Contains(t, got[2], spanStyle(th, app.KindCommand).Render("/skill:a"))
	assert.Contains(t, got[2], spanStyle(th, app.KindCommand).Render("ultracode"))
}
