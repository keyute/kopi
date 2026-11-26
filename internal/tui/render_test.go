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
				want := renderRow(safe, fold, width, mocha()).rows
				got := renderRow(unsafe, fold, width, mocha()).rows
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
			assert.Equal(t, newMarkdown(width, theme{}).lines("", clean), newMarkdown(width, theme{}).lines("", attack+clean), "markdown drops terminal controls")
		})
	}
}

func TestWrapPreservesPlainText(t *testing.T) {
	assert.Equal(t, []string{"café 世界", "    tab", "ordinary", "words wrap"}, wrap("café 世界\n\ttab\nordinary words wrap", 10))
	assert.Equal(t, "a\nb", strings.Join(wrap("a\nb", 10), "\n"))
}

func TestToneStylesBulletAndLine(t *testing.T) {
	th := mocha()
	for name, tc := range map[string]struct {
		tone         tone
		bullet, line lipgloss.Style
	}{
		"running": {toneRunning, fg(th.muted), fg(th.dim)},
		"ok":      {toneOK, fg(th.success), fg(th.dim)},
		"refused": {toneRefused, fg(th.warning), fg(th.warning)},
		"failed":  {toneFailed, fg(th.error), fg(th.error)},
	} {
		bullet, line := tc.tone.styles(th)
		assert.Equal(t, tc.bullet.Render("•"), bullet.Render("•"), name)
		assert.Equal(t, tc.line.Render("↳"), line.Render("↳"), name)
	}

	for state, want := range map[transcript.State]tone{
		transcript.Running: toneRunning, transcript.OK: toneOK, transcript.Denied: toneRefused,
		transcript.Rejected: toneRefused, transcript.Unfinished: toneRefused, transcript.Failed: toneFailed,
	} {
		assert.Equal(t, want, callTone(state), state)
	}
	for status, want := range map[tool.TaskStatus]tone{
		tool.TaskRunning: toneRunning, tool.TaskCompleted: toneOK, tool.TaskStopped: toneRefused, tool.TaskFailed: toneFailed,
	} {
		assert.Equal(t, want, taskTone(status), status)
	}
}

func TestRowStatesColourTheirBulletAndLine(t *testing.T) {
	th := mocha()
	for name, tc := range map[string]struct {
		call         transcript.Call
		bullet, line lipgloss.Style
		summary      string
	}{
		"ok":         {transcript.Call{State: transcript.OK, Summary: "s"}, fg(th.success), fg(th.dim), "s"},
		"failed":     {transcript.Call{State: transcript.Failed, Summary: "s"}, fg(th.error), fg(th.error), "s"},
		"denied":     {transcript.Call{State: transcript.Denied, Summary: "s"}, fg(th.warning), fg(th.warning), "s"},
		"rejected":   {transcript.Call{State: transcript.Rejected, Summary: "s"}, fg(th.warning), fg(th.warning), "s"},
		"unfinished": {transcript.Call{State: transcript.Unfinished}, fg(th.warning), fg(th.warning), "interrupted"},
	} {
		tc.call.Title = "t"
		got := renderRow(tc.call, blockFolds{expanded: []bool{false}}, 40, th).rows
		require.Len(t, got, 2, name)
		assert.Equal(t, tc.bullet.Render("• ")+fg(th.text).Render("t"), got[0].text, name)
		assert.Equal(t, tc.line.Render("  ↳ ")+tc.line.Render(tc.summary), got[1].text, name)
	}
}

func TestMemberBodyIsTheLoneBodyBelowItsSummaryLine(t *testing.T) {
	th := mocha()
	long := strings.Repeat("word ", 10)
	for name, c := range map[string]transcript.Call{
		"output":       {ID: "a", State: transcript.OK, Title: "Bash ls", Summary: "2 lines", Command: "ls\nmore", Body: "a.go\nb.go"},
		"preview":      {ID: "a", State: transcript.OK, Title: "Edit a.go", Summary: "+1 -1", Preview: []tool.PreviewLine{{Kind: tool.Removed, Text: "old"}, {Kind: tool.Added, Text: "new"}}},
		"prompt":       {ID: "a", State: transcript.OK, Title: "explore", Summary: "launched", Preview: []tool.PreviewLine{{Kind: tool.Context, Text: "look"}}},
		"long output":  {ID: "a", State: transcript.OK, Title: "Bash ls", Summary: "1 line", Body: long},
		"long preview": {ID: "a", State: transcript.OK, Title: "Edit a.go", Summary: "+1", Preview: []tool.PreviewLine{{Kind: tool.Added, Text: long}}},
	} {
		lone := renderRow(c, blockFolds{expanded: []bool{true}}, 40, th).rows
		full := renderGroup(transcript.Group{Sealed: true, Calls: []transcript.Call{c, c}}, blockFolds{open: true, expanded: []bool{true, false}}, 40, th)
		require.Greater(t, len(lone), 2, name)
		assert.Equal(t, lone[2:], full[2:len(lone)], name)
		for _, l := range lone[2:] {
			assert.LessOrEqual(t, ansi.StringWidth(l.text), 40, name)
			assert.True(t, strings.HasPrefix(ansi.Strip(l.text), strings.Repeat(" ", 6)), "%q starts six in", l.text)
		}
	}
}

func TestOutlineBodyIsStepPastTheTextAbove(t *testing.T) {
	th := mocha()
	for _, tc := range []struct {
		name string
		draw func(o *outline)
		col  int
	}{
		{"title", func(o *outline) { o.title("• ", fg(th.text), "t", fg(th.text), hit{}) }, 4},
		{"under", func(o *outline) {
			o.title("• ", fg(th.text), "t", fg(th.text), hit{})
			o.under("u", fg(th.dim), hit{})
		}, 6},
	} {
		o := &outline{width: 40}
		tc.draw(o)
		assert.Equal(t, 40-tc.col, o.bodyWidth(), tc.name)
		o.body([]line{{text: "b"}})
		assert.Equal(t, strings.Repeat(" ", tc.col)+"b", ansi.Strip(o.rows[len(o.rows)-1].text), tc.name)
	}
}

func TestTaskRow(t *testing.T) {
	th := mocha()
	for _, tc := range []struct {
		status, want string
		bullet       lipgloss.Style
	}{
		{"completed", "• shell finished · make build · 12s", fg(th.success)},
		{"failed", "• shell failed · make build · 12s", fg(th.error)},
		{"stopped", "• shell stopped · make build · 12s", fg(th.warning)},
	} {
		t.Run(tc.status, func(t *testing.T) {
			m, _ := newTestModel(t, 60, 20)
			send(m, event.TaskEnded{ID: "t1", Command: "make build\nmake test", Status: tc.status, Elapsed: 12 * time.Second})
			assert.Equal(t, []string{"", tc.want}, body(m))
			got := m.view().lines[len(m.view().lines)-1].text
			assert.Equal(t, tc.bullet.Render("• ")+fg(th.text).Render(strings.TrimPrefix(tc.want, "• ")), got)
		})
	}
}

func TestFailedShellRowShowsItsReason(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, event.TaskEnded{ID: "t1", Command: "make build", Status: "failed", Elapsed: 12 * time.Second, Reason: "exit code 2"})
	assert.Equal(t, []string{"", "• shell failed · make build · 12s", "  ↳ exit code 2"}, body(m))
}

func TestShellRowOpensToItsOutput(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, event.TaskEnded{ID: "t1", Command: "make build", Status: "completed", Elapsed: 12 * time.Second, Result: "step 1\rstep 2\nbuilt"})
	clickText(t, m, "• shell finished · make build · 12s")
	assert.Equal(t, []string{"", "• shell finished · make build · 12s", "    step 2", "    built"}, body(m))
}

func TestTaskRowWithAReasonOpensToItsResultSixIn(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, event.TaskEnded{ID: "t1", Command: "make build", Status: "failed", Elapsed: 12 * time.Second, Reason: "exit 2", Result: "boom"})
	clickText(t, m, "• shell failed · make build · 12s")
	assert.Equal(t, []string{"", "• shell failed · make build · 12s", "  ↳ exit 2", "      boom"}, body(m))
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
		bullet       lipgloss.Style
	}{
		{"completed", "• explore finished · look around · 2m 14s", fg(th.success)},
		{"failed", "• explore failed · look around · 2m 14s", fg(th.error)},
		{"stopped", "• explore stopped · look around · 2m 14s", fg(th.warning)},
	} {
		t.Run(tc.status, func(t *testing.T) {
			m, _ := newTestModel(t, 60, 20)
			send(m, event.TaskEnded{ID: "t3", Agent: "explore", Command: "look around\nmore", Status: tc.status, Elapsed: 134 * time.Second})
			assert.Equal(t, []string{"", tc.want}, body(m))
			got := m.view().lines[len(m.view().lines)-1].text
			assert.Equal(t, tc.bullet.Render("• ")+fg(th.text).Render(strings.TrimPrefix(tc.want, "• ")), got)
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
				"• workflow failed · audit route handlers · 6m 3s", "  ↳ 2 agents · 2 failed · first line of why",
				"    second line of why is long enough to wrap around the", "    width",
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
	require.Len(t, ls, 2)
	assert.Equal(t, fg(th.error).Render("  ↳ ")+fg(th.error).Render("1 agent · why"), ls[1].text)
}

func TestTaskEndRowLineColours(t *testing.T) {
	th := mocha()
	for name, tc := range map[string]struct {
		task  transcript.Task
		style lipgloss.Style
		line  string
	}{
		"stopped workflow":     {transcript.Task{Agent: "workflow", Status: "stopped", Agents: 3, Failed: 3}, fg(th.warning), "3 agents · 3 failed"},
		"workflow w/ failures": {transcript.Task{Agent: "workflow", Status: "completed", Agents: 9, Failed: 1}, fg(th.dim), "9 agents · 1 failed"},
	} {
		ls := renderTask(tc.task, false, 60, th)
		require.Len(t, ls, 2, name)
		assert.Equal(t, tc.style.Render("  ↳ ")+tc.style.Render(tc.line), ls[1].text, name)
	}

	ls := renderTask(transcript.Task{Agent: "explorer", Status: "failed", Reason: "boom"}, false, 60, th)
	require.Len(t, ls, 2)
	assert.Equal(t, "  ↳ boom", ansi.Strip(ls[1].text))
}

func TestTaskWithAResultOpensToItVerbatim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 60, 30)
		send(m, workflowEnd("completed", 2, 0, "", "line one\n  indented line two that is long enough to wrap"))
		head := []string{"", "• workflow finished · audit route handlers · 6m 3s", "  ↳ 2 agents"}
		require.Equal(t, head, body(m))

		clickText(t, m, "• workflow finished · audit route handlers · 6m 3s")
		assert.Equal(t, append(head, "      line one", "        indented line two that is long enough to wrap"), body(m))

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

func TestPlanRowDrawsItsPlanWhileRunningAndOnlyWhenOpenedAfter(t *testing.T) {
	var tr transcript.Transcript
	tr.Apply(event.ToolStart{CallID: "p", Name: "ExitPlanMode", Title: "Plan approval", Solo: true})
	tr.Apply(event.PlanRequested{Token: 1, Plan: "# Heading\n\nfirst step " + strings.Repeat("word ", 30)})
	var c renderCache
	text := func(f blockFolds) []string {
		var out []string
		for _, l := range c.lines(tr.Blocks()[0], f, nil, 40, mocha()) {
			out = append(out, strings.TrimRight(ansi.Strip(l.text), " "))
		}
		return out
	}

	got := text(blockFolds{})
	assert.Equal(t, "• Plan approval", got[0])
	joined := strings.Join(got[1:], "\n")
	assert.NotContains(t, joined, "# Heading", "the Markdown is rendered")
	for _, l := range got[1:] {
		if l != "" {
			assert.True(t, strings.HasPrefix(l, "    "), "%q is four in", l)
		}
	}
	assert.Contains(t, joined, "Heading")
	assert.Contains(t, joined, "first step")

	tr.Apply(event.PlanRequested{Token: 1, Plan: "# Heading\n\nsecond step " + strings.Repeat("word ", 30)})
	joined = strings.Join(text(blockFolds{}), "\n")
	assert.Contains(t, joined, "second step")
	assert.NotContains(t, joined, "first step")

	tr.Apply(event.ToolEnd{CallID: "p", Name: "ExitPlanMode", Kind: event.EndRejected, Summary: "revision requested", Body: "split step 2"})
	closed := blockFolds{expanded: []bool{false}}
	assert.Equal(t, []string{"• Plan approval", "  ↳ revision requested", "      split step 2"}, text(closed), "the feedback shows while the row is closed")
	th := mocha()
	raw := c.lines(tr.Blocks()[0], closed, nil, 40, th)
	assert.Equal(t, fg(th.warning).Render("• ")+fg(th.text).Render("Plan approval"), raw[0].text)
	assert.Equal(t, fg(th.warning).Render("  ↳ ")+fg(th.warning).Render("revision requested"), raw[1].text)

	opened := text(blockFolds{expanded: []bool{true}})
	require.Greater(t, len(opened), 4)
	assert.Equal(t, []string{"• Plan approval", "  ↳ revision requested", "      split step 2"}, opened[:3])
	assert.Contains(t, strings.Join(opened[3:], "\n"), "second step", "the plan follows the feedback")
	assert.Equal(t, []string{"• Plan approval", "  ↳ revision requested", "      split step 2"}, text(closed), "closing takes the plan away")
	for _, l := range opened[3:] {
		if l != "" {
			assert.True(t, strings.HasPrefix(l, "      "), "%q is six in", l)
			assert.LessOrEqual(t, ansi.StringWidth(l), 40, "%q fits the width", l)
		}
	}

	var bare transcript.Transcript
	bare.Apply(event.ToolStart{CallID: "q", Name: "ExitPlanMode", Title: "Plan approval", Solo: true})
	bare.Apply(event.PlanRequested{Token: 2, Plan: "# Heading\n\n- step"})
	bare.Apply(event.ToolEnd{CallID: "q", Name: "ExitPlanMode", Summary: "approved"})
	assert.Equal(t, hit{kind: hitOutput, id: "q"}, c.lines(bare.Blocks()[0], closed, nil, 40, mocha())[0].hit, "the plan alone makes the row open")
}

func TestDismissedPlanRowReadsDismissedAndOpensToItsPlan(t *testing.T) {
	th := mocha()
	var tr transcript.Transcript
	tr.Apply(event.ToolStart{CallID: "p", Name: "ExitPlanMode", Title: "Plan approval", Solo: true})
	tr.Apply(event.PlanRequested{Token: 1, Plan: "# Heading\n\n- first step"})
	tr.Apply(event.ToolEnd{CallID: "p", Name: "ExitPlanMode", Kind: event.EndRejected, Summary: "dismissed"})
	var c renderCache
	text := func(f blockFolds) []string {
		var out []string
		for _, l := range c.lines(tr.Blocks()[0], f, nil, 40, th) {
			out = append(out, strings.TrimRight(ansi.Strip(l.text), " "))
		}
		return out
	}

	closed := blockFolds{expanded: []bool{false}}
	assert.Equal(t, []string{"• Plan approval", "  ↳ dismissed"}, text(closed))
	assert.Equal(t, hit{kind: hitOutput, id: "p"}, c.lines(tr.Blocks()[0], closed, nil, 40, th)[0].hit)

	opened := text(blockFolds{expanded: []bool{true}})
	require.Greater(t, len(opened), 2)
	assert.Equal(t, []string{"• Plan approval", "  ↳ dismissed"}, opened[:2])
	for _, l := range opened[2:] {
		if l != "" {
			assert.True(t, strings.HasPrefix(l, "      ") && !strings.HasPrefix(l, "       "), "%q starts at column six", l)
		}
	}
}

func TestAgentPromptIsFlushAtTheBodyColumn(t *testing.T) {
	th := mocha()
	c := transcript.Call{ID: "a", State: transcript.OK, Title: "explore", Preview: []tool.PreviewLine{{Kind: tool.Context, Text: "look"}}}
	lone := renderRow(c, blockFolds{expanded: []bool{true}}, 40, th).rows
	member := renderGroup(transcript.Group{Sealed: true, Calls: []transcript.Call{c}}, blockFolds{open: true, expanded: []bool{true}}, 40, th)
	assert.Equal(t, "    look", ansi.Strip(lone[len(lone)-1].text), "no ↳ line above it")
	assert.Equal(t, "      look", ansi.Strip(member[len(member)-1].text))
}

func TestPlanRowRewrapsWhenTheWidthChanges(t *testing.T) {
	var tr transcript.Transcript
	tr.Apply(event.ToolStart{CallID: "p", Name: "ExitPlanMode", Title: "Plan approval", Solo: true})
	tr.Apply(event.PlanRequested{Token: 1, Plan: strings.Repeat("word ", 60)})
	var c renderCache
	planLines := func(width int) []string {
		var out []string
		for _, l := range c.lines(tr.Blocks()[0], blockFolds{}, nil, width, mocha())[1:] {
			out = append(out, strings.TrimPrefix(l.text, "    "))
		}
		return out
	}

	narrow, wide := planLines(40), planLines(80)
	assert.Less(t, len(wide), len(narrow))
	for _, l := range wide {
		assert.LessOrEqual(t, ansi.StringWidth(l), 76, "%q fits the width less the indent", l)
	}
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
	task := tool.Action{Verb: "Stopped", One: "task", Many: "tasks"}
	// The start carries the tool's static action; only the end's outcome names the shells and agents.
	callAs := func(id, name, title string, static, outcome tool.Action) []event.Event {
		return []event.Event{
			event.ToolStart{CallID: id, Name: name, Title: title, Done: title, Action: static},
			event.ToolEnd{CallID: id, Name: name, Done: title, Action: outcome},
		}
	}
	call := func(id, name, title string, action tool.Action) []event.Event {
		return callAs(id, name, title, action, action)
	}
	var events []event.Event
	for _, c := range [][]event.Event{
		{start("a"), ok("a", "")},
		{start("b"), ok("b", "")},
		call("c", "Read", "Read the plan", plan),
		call("d", "WebSearch", "Searched the web for x", web),
		call("e", "WebSearch", "Searched the web for y", web),
		call("f", "WebSearch", "Searched the web for z", web),
		callAs("g", "Bash", "Started make in background", tool.Action{}, shell),
		callAs("h", "TaskStop", "TaskStop researcher › survey peers", task, agents),
		callAs("i", "TaskStop", "TaskStop explorer › map the repo", task, agents),
	} {
		events = append(events, c...)
	}
	sentence := "Read 2 files, read the plan, searched the web 3 times, started 1 shell, checked on 2 agents"
	members := []string{"  ↳ a.go", "  ↳ b.go", "  ↳ Read the plan", "  ↳ Searched the web for x", "  ↳ Searched the web for y", "  ↳ Searched the web for z", "  ↳ Started make in background", "  ↳ TaskStop researcher › survey peers", "  ↳ TaskStop explorer › map the repo"}
	m, _ := newTestModel(t, 120, 30)
	send(m, events...)
	assert.Equal(t, append([]string{"", "• " + sentence}, members...), body(m))

	send(m, event.TextDelta{Text: "Done."})
	assert.Equal(t, []string{"", "▸ " + sentence, "", "Done."}, body(m))
}

func TestTaskStopRowWithoutANameLineReadsAsATask(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, event.ToolStart{CallID: "a", Name: "TaskStop", Title: "TaskStop t3", Done: "Stopped task t3", Action: tool.Action{Verb: "Stopped", One: "task", Many: "tasks"}},
		event.ToolEnd{CallID: "a", Name: "TaskStop", Output: "Task t3 stopped."})
	assert.Equal(t, []string{"", "• Stopped task t3"}, body(m))
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
	assert.Equal(t, []string{"", "• a.go", "  ↳ rejected", "      use b", "      instead"}, body(m))
	lines := m.view().lines
	assert.Equal(t, fg(th.warning).Render("• ")+fg(th.text).Render("a.go"), lines[len(lines)-4].text)
	for _, l := range lines {
		assert.Equal(t, hit{}, l.hit)
	}

	m, _ = newTestModel(t, 60, 20)
	send(m, start("a"), event.ToolEnd{CallID: "a", IsError: true, Kind: event.EndRejected, Summary: "rejected"})
	assert.Equal(t, []string{"", "• a.go", "  ↳ rejected"}, body(m))
}

func TestDismissedQuestionRowIsWarningColoured(t *testing.T) {
	th := mocha()
	var tr transcript.Transcript
	tr.Apply(event.ToolStart{CallID: "a", Name: "AskUserQuestion", Title: "AskUserQuestion"})
	tr.Apply(event.ToolEnd{CallID: "a", Name: "AskUserQuestion", Kind: event.EndRejected, Summary: "dismissed", Done: "Asked"})
	var c renderCache
	got := c.lines(tr.Blocks()[0], blockFolds{expanded: []bool{false}}, nil, 40, th)
	require.Len(t, got, 2)
	assert.Equal(t, fg(th.warning).Render("• ")+fg(th.text).Render("AskUserQuestion"), got[0].text)
	assert.Equal(t, fg(th.warning).Render("  ↳ ")+fg(th.warning).Render("dismissed"), got[1].text)
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
	got := renderRow(transcript.Call{ID: "a", State: transcript.Failed, Title: "t", Summary: "failed", Body: five}, blockFolds{expanded: []bool{false}}, 40, th).rows
	require.Len(t, got, 8)
	assert.Equal(t, line{text: fg(th.error).Render("  ↳ ") + fg(th.error).Render("failed")}, got[1])
	assert.Equal(t, hit{}, got[0].hit, "five lines are shown in full, so there is nothing to open")

	six := transcript.Call{ID: "a", State: transcript.Failed, Title: "t", Summary: "failed", Body: five + "6\n"}
	got = renderRow(six, blockFolds{expanded: []bool{false}}, 40, th).rows
	assert.Equal(t, hit{kind: hitOutput, id: "a"}, got[0].hit)
	assert.Equal(t, hit{kind: hitOutput, id: "a"}, got[1].hit)
	var text []string
	for _, l := range got {
		text = append(text, ansi.Strip(l.text))
	}
	assert.Equal(t, []string{"• t", "  ↳ failed", "      1", "      2", "      … 2 more lines", "      5", "      6"}, text)
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
		assert.Equal(t, "      - old0", got[3])
		assert.Equal(t, "      - old29", got[32])
		assert.Equal(t, "      + new0", got[33])
		assert.Equal(t, "      + new29", got[62])
	})
}

func TestCarriageReturnsKeepWhatFollowsTheLast(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	send(m, start("a"), okOut("a", "3 lines", "10%\r50%\r100%\r\nline\r\nx\ry\r"))
	press(m, ctrlO)
	assert.Equal(t, []string{"", "• a.go", "  ↳ 3 lines", "      100%", "      line", "      y"}, body(m))

	var pv []tool.PreviewLine
	pv = append(pv, tool.PreviewLine{Kind: tool.Added, Text: "a\rb"})
	assert.Equal(t, line{text: "    " + fg(mocha().success).Render("+ ") + fg(mocha().success).Render("ab")}, renderRow(transcript.Call{State: transcript.OK, Title: "t", Preview: pv}, blockFolds{expanded: []bool{true}}, 40, mocha()).rows[1])
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

		assert.Equal(t, []string{"", "• Bash first …", "  ↳ 1 line", "      $ first", "        second line", "      out"}, body(m))
	})
}

func TestBashRowWithACutCommandAndNoOutputOpens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		send(m, bashStart("a", "first\nsecond"), event.ToolEnd{CallID: "a", Name: "Bash", Summary: "no output"})

		clickText(t, m, "• Bash first …")

		assert.Equal(t, []string{"", "• Bash first …", "  ↳ no output", "      $ first", "        second"}, body(m))
	})
}

func TestCommandLinesKeepWhatBodyTextWouldDrop(t *testing.T) {
	th := mocha()
	c := transcript.Call{ID: "a", Command: "echo a\rb\n\n  c"}
	assert.Equal(t, []string{"$ echo ab", "", "    c"}, plainRows(lineTexts(commandLines(c, 40, th))))
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
				at := slices.Index(open, "      $ first")
				require.Greater(t, at, 2, "the command follows the summary")
				assert.Equal(t, "        second", open[at+1])

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
	got := renderRow(c, blockFolds{expanded: []bool{false}}, 40, th).rows
	assert.Equal(t, hit{kind: hitOutput, id: "a"}, got[0].hit)
	assert.Equal(t, []string{"• t", "  ↳ failed", "      boom"}, plainRows(lineTexts(got)))

	got = renderRow(c, blockFolds{expanded: []bool{true}}, 40, th).rows
	assert.Equal(t, []string{"• t", "  ↳ failed", "      $ a", "        b", "      boom"}, plainRows(lineTexts(got)))
}

func commandHintRows(c event.UserCommand, o commandOptions, th theme) []string {
	rows := lineTexts(commandRows(c, o, 80, th))
	blank := slices.Index(rows, "")
	return rows[blank+1:]
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

		assert.Equal(t, []string{"", "▾ Ran 2 commands", "  ↳ Bash first … · 1 line", "      $ first", "        second", "      out", "  ↳ Bash first … · 1 line", "", "Done."}, body(m))
	})
}

func TestShadedBoxStylesSpansOnWrappedRows(t *testing.T) {
	th := mocha()
	set, _ := skill.NewSet("skill", []skill.Skill{{Name: "a"}}, nil)
	spans := func(text string) []app.Span { return app.Spans(text, set, true, false) }
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

func TestCommandHint(t *testing.T) {
	th := mocha()
	dim, success, errc, warn := fg(th.dim), fg(th.success), fg(th.error), fg(th.warning)
	for _, tc := range []struct {
		name string
		c    event.UserCommand
		o    commandOptions
		want string
	}{
		{"queued", event.UserCommand{State: event.CommandQueued}, commandOptions{}, "  ↳ queued"},
		{"running", event.UserCommand{State: event.CommandRunning}, commandOptions{Elapsed: 12 * time.Second, BackgroundKey: "ctrl+b", StopKey: "esc"}, "  ↳ running 12s · ctrl+b to background · esc to stop"},
		{"running under a second", event.UserCommand{State: event.CommandRunning}, commandOptions{Elapsed: time.Second - 1, BackgroundKey: "ctrl+b"}, "  ↳ running · ctrl+b to background"},
		{"running without keys", event.UserCommand{State: event.CommandRunning}, commandOptions{Elapsed: 3 * time.Second}, "  ↳ running 3s"},
		{"exited", event.UserCommand{State: event.CommandExited, Exit: "exit 0", Elapsed: 12 * time.Second}, commandOptions{}, "  ↳ exit 0 · 12s"},
		{"exited unread", event.UserCommand{State: event.CommandExited, Exit: "exit 0", Elapsed: 12 * time.Second}, commandOptions{Unread: true}, "  ↳ exit 0 · 12s · unread"},
		{"exited under a second", event.UserCommand{State: event.CommandExited, Exit: "exit 0", Elapsed: 10 * time.Millisecond}, commandOptions{}, "  ↳ exit 0"},
		{"failed", event.UserCommand{State: event.CommandExited, Exit: "exit 1", Failed: true, Elapsed: 12 * time.Second}, commandOptions{Unread: true}, "  ↳ exit 1 · 12s · unread"},
		{"stopped", event.UserCommand{State: event.CommandStopped, Elapsed: 12 * time.Second}, commandOptions{Unread: true}, "  ↳ stopped after 12s · unread"},
		{"stopped under a second", event.UserCommand{State: event.CommandStopped}, commandOptions{}, "  ↳ stopped"},
		{"moved", event.UserCommand{State: event.CommandMoved, Elapsed: 12 * time.Second}, commandOptions{}, "  ↳ moved to background after 12s"},
		{"moved under a second", event.UserCommand{State: event.CommandMoved}, commandOptions{}, "  ↳ moved to background"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := commandHintRows(tc.c, tc.o, th)
			require.Len(t, got, 1)
			assert.Equal(t, tc.want, ansi.Strip(got[0]))
		})
	}

	t.Run("colours", func(t *testing.T) {
		hint := func(c event.UserCommand, o commandOptions) string { return commandHintRows(c, o, th)[0] }
		exited := hint(event.UserCommand{State: event.CommandExited, Exit: "exit 0", Elapsed: 12 * time.Second}, commandOptions{Unread: true})
		assert.Contains(t, exited, success.Render("exit 0"))
		assert.Contains(t, exited, dim.Render(" · 12s · unread"))

		failed := hint(event.UserCommand{State: event.CommandExited, Exit: "exit 1", Failed: true, Elapsed: 12 * time.Second}, commandOptions{})
		assert.Contains(t, failed, errc.Render("exit 1 · 12s"))

		stopped := hint(event.UserCommand{State: event.CommandStopped, Elapsed: 12 * time.Second}, commandOptions{})
		assert.Contains(t, stopped, warn.Render("stopped after 12s"))

		assert.Contains(t, hint(event.UserCommand{State: event.CommandQueued}, commandOptions{}), dim.Render("queued"))
		assert.Contains(t, hint(event.UserCommand{State: event.CommandMoved, Elapsed: 12 * time.Second}, commandOptions{}), dim.Render("moved to background after 12s"))
		assert.Contains(t, hint(event.UserCommand{State: event.CommandRunning}, commandOptions{StopKey: "esc"}), dim.Render("running · esc to stop"))
	})
}

func TestCommandRowsBox(t *testing.T) {
	th := mocha()
	got := commandRows(event.UserCommand{Command: "ls -la", State: event.CommandExited, Exit: "exit 0"}, commandOptions{Lines: 10}, 20, th)
	blank := commandShade(th).Render(strings.Repeat(" ", 20))
	assert.Equal(t, blank, got[0].text)
	assert.Equal(t, blank, got[2].text)
	assert.True(t, strings.HasPrefix(got[1].text, commandShade(th).Foreground(th.error).Render("! ")), "the glyph is error on the command shade")
	assert.Contains(t, got[1].text, commandShade(th).Foreground(th.text).Render("ls -la"+strings.Repeat(" ", 12)))
	assert.Equal(t, line{}, got[3], "a blank row follows the box")
	assert.Equal(t, "  ↳ exit 0", ansi.Strip(got[4].text))
	assert.Contains(t, got[4].text, fg(th.success).Render("exit 0"))
	assert.Len(t, got, 5, "no output, no rows")
}

func TestCommandRowsOutput(t *testing.T) {
	th := mocha()
	numbered := func(n int) string {
		var ls []string
		for i := 1; i <= n; i++ {
			ls = append(ls, fmt.Sprintf("l%d", i))
		}
		return strings.Join(ls, "\n") + "\n\n"
	}
	c := event.UserCommand{Command: "seq", State: event.CommandExited, Exit: "exit 0", Output: numbered(12)}
	h := hit{kind: hitOutput, id: "k"}
	tail := func(from, to int) []string {
		var out []string
		for i := from; i <= to; i++ {
			out = append(out, fmt.Sprintf("      l%d", i))
		}
		return out
	}
	rows := func(c event.UserCommand, o commandOptions) []string {
		var out []string
		for _, l := range commandRows(c, o, 40, th)[4:] {
			out = append(out, ansi.Strip(l.text))
		}
		return out
	}

	assert.Equal(t, append([]string{"  ↳ exit 0", "      … 2 earlier lines"}, tail(3, 12)...), rows(c, commandOptions{Lines: 10}))
	assert.Equal(t, append([]string{"  ↳ exit 0"}, tail(1, 12)...), rows(c, commandOptions{Lines: 10, Expanded: true}))
	c.Output = numbered(10)
	assert.Equal(t, append([]string{"  ↳ exit 0"}, tail(1, 10)...), rows(c, commandOptions{Lines: 10}), "exactly the limit is not cut")
	c.Output = numbered(7)
	assert.Equal(t, append([]string{"  ↳ exit 0", "      … 2 earlier lines"}, tail(3, 7)...), rows(c, commandOptions{Lines: 5}))

	for _, l := range commandRows(c, commandOptions{Lines: 5, Hit: h}, 40, th)[4:5] {
		assert.Equal(t, h, l.hit, "the hint row toggles the output")
	}
	assert.Equal(t, hit{}, commandRows(c, commandOptions{Lines: 5, Hit: h}, 40, th)[1].hit)
	c.Output = ""
	assert.Equal(t, hit{}, commandRows(c, commandOptions{Hit: h}, 40, th)[4].hit, "nothing to toggle")
}

func TestUserCommandBlockShowsInTheTranscript(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, event.UserCommand{Command: "echo hi", State: event.CommandExited, Exit: "exit 0", Elapsed: 2 * time.Second, Output: "hi\n"})
	assert.Equal(t, []string{"", "", "! echo hi", "", "", "  ↳ exit 0 · 2s", "      hi"}, body(m))
}
