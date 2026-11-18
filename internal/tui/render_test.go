package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

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
				safe := transcript.Call{ID: "call", State: state, Title: clean, Summary: clean, Output: clean}
				unsafe := safe
				unsafe.Title, unsafe.Summary, unsafe.Output = attack+clean, attack+clean, attack+clean
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
			got := m.lines[len(m.lines)-1].text
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
			got := m.lines[len(m.lines)-1].text
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
	return event.ToolEnd{CallID: id, Name: "Agent", Summary: "launched", Output: "Agent t1 started."}
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
