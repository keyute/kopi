package tui

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
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
			assert.Equal(t, shadedBox("❯ ", shade(mocha()), clean, width, mocha()), shadedBox("❯ ", shade(mocha()), attack+clean, width, mocha()))
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
		{"completed", "• task t1 finished · make build · 12s", fg(th.success), lipgloss.NewStyle()},
		{"failed", "• task t1 failed · make build · 12s", fg(th.error), fg(th.error)},
		{"stopped", "• task t1 stopped · make build · 12s", fg(th.error), fg(th.error)},
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
	assert.Equal(t, []string{"", "• task t1 finished · " + strings.Repeat("x", 10) + "… · 1m 0s"}, body(m))
}

func TestPlanBlockDrawsAMutedTitleAndMarkdownAndRerendersWhenEdited(t *testing.T) {
	var tr transcript.Transcript
	tr.Apply(event.PlanRequested{Token: 1, Plan: "# Heading\n\n- first step"})
	var c renderCache
	text := func() []string {
		var out []string
		for _, l := range c.lines(tr.Blocks()[0], blockFolds{}, 40, mocha()) {
			out = append(out, strings.TrimRight(ansi.Strip(l.text), " "))
		}
		return out
	}

	got := text()
	assert.Equal(t, "Plan", got[0])
	assert.Equal(t, fg(mocha().muted).Render("Plan"), c.lines(tr.Blocks()[0], blockFolds{}, 40, mocha())[0].text)
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
