package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
)

var dirReq = event.DirRequested{
	Token:        7,
	Dir:          "/w/other",
	Instructions: []string{"/w/other/AGENTS.md"},
	Servers:      []string{"docs: npx -y docs-server"},
	Skills:       []string{"/w/other/.agents/skills"},
	Saved:        true,
}

func dirRows(ev event.DirRequested) []string {
	km := newKeyMap(config.DefaultKeys())
	a := newDirDialog(ev, &km)
	return plainRows(a.dialog(100, mocha()).rows(100, 100, 100, mocha()))
}

func TestDirRows(t *testing.T) {
	rule := strings.Repeat("─", 100)
	options := []string{"", "❭ 1. Yes, and trust this folder", "  2. Yes, but don't trust it", "  3. No, don't add", "", "  enter to choose · esc to cancel", rule}
	noInstructions := dirReq
	noInstructions.Instructions = nil
	unsaved := dirReq
	unsaved.Saved = false
	bare := dirReq
	bare.Servers, bare.Skills = nil, nil

	assert.Equal(t, append([]string{
		rule,
		"  Add this folder to the workspace?",
		"  /w/other",
		"",
		"  Trusting it adds these instruction files to the model's instructions:",
		"    /w/other/AGENTS.md",
		"  The answer is saved. When Kopi starts in this folder it also covers:",
		"    MCP server docs: npx -y docs-server",
		"    skills in /w/other/.agents/skills",
	}, options...), dirRows(dirReq))
	assert.Equal(t, append([]string{
		rule,
		"  Add this folder to the workspace?",
		"  /w/other",
		"",
		"  It has no instruction files.",
		"  The answer is saved. When Kopi starts in this folder it also covers:",
		"    MCP server docs: npx -y docs-server",
		"    skills in /w/other/.agents/skills",
	}, options...), dirRows(noInstructions))
	assert.Equal(t, append([]string{
		rule,
		"  Add this folder to the workspace?",
		"  /w/other",
		"",
		"  Trusting it adds these instruction files to the model's instructions:",
		"    /w/other/AGENTS.md",
	}, options...), dirRows(unsaved))
	assert.Equal(t, append([]string{
		rule,
		"  Add this folder to the workspace?",
		"  /w/other",
		"",
		"  Trusting it adds these instruction files to the model's instructions:",
		"    /w/other/AGENTS.md",
		"  The answer is saved and also applies when Kopi starts in this folder.",
	}, options...), dirRows(bare))
}

func TestDirAnswers(t *testing.T) {
	for _, tt := range []struct {
		name string
		keys []tea.KeyPressMsg
		want dirAnswer
	}{
		{"enter trusts and adds", []tea.KeyPressMsg{enter}, dirAnswer{7, true, true}},
		{"down then enter adds untrusted", []tea.KeyPressMsg{down, enter}, dirAnswer{7, true, false}},
		{"digit 3 does not add", []tea.KeyPressMsg{digit('3')}, dirAnswer{7, false, false}},
		{"dismiss does not add", []tea.KeyPressMsg{down, escape}, dirAnswer{7, false, false}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, c := newTestModel(t, 70, 30)
			send(m, dirReq)
			for _, k := range tt.keys {
				press(m, k)
			}
			assert.Equal(t, []dirAnswer{tt.want}, c.dirAnswers)
			assert.NotNil(t, dirOf(m), "the prompt stays until DirEnded")
		})
	}
}

func TestDirHintNamesBoundKeys(t *testing.T) {
	km := newKeyMap(map[string][]string{"submit": {"ctrl+j", "enter"}, "interrupt": {"ctrl+g"}})
	a := newDirDialog(dirReq, &km)
	assert.Contains(t, plainRows(a.dialog(100, mocha()).rows(100, 100, 100, mocha())), "  ctrl+j to choose · ctrl+g to cancel")
}

func TestDirRowsKeepHostileTextOnItsLine(t *testing.T) {
	clean := dirRows(dirReq)
	for name, attack := range map[string]string{
		"CSI":      "\x1b[2J",
		"OSC":      "\x1b]52;c;c2VjcmV0\a",
		"controls": "\x00\a\b\v\f\x7f\u009b",
	} {
		t.Run(name, func(t *testing.T) {
			req := dirReq
			req.Dir = attack + req.Dir
			req.Instructions = []string{attack + "/w/other/AGENTS.md"}
			req.Servers = []string{"docs: npx -y docs-server" + attack}
			assert.Equal(t, clean, dirRows(req))
		})
	}

	req := dirReq
	req.Dir = "/w/a\nb"
	req.Servers = []string{"docs: a\nfake: b\r\nc"}
	rows := dirRows(req)
	assert.Contains(t, rows, "  /w/a b", "a line break cannot start a new line")
	assert.Contains(t, rows, "    MCP server docs: a fake: b c")
	for _, r := range rows {
		assert.LessOrEqual(t, ansi.StringWidth(r), 100)
	}
}

func TestDirEndedClosesOnlyItsOwnDialog(t *testing.T) {
	m, _ := newTestModel(t, 70, 30)
	send(m, dirReq)
	require.NotNil(t, dirOf(m))
	assert.Contains(t, strings.Join(screen(m), "\n"), "Add this folder to the workspace?", "drawn with no session open")

	send(m, event.SessionChanged{}, event.TurnEnd{}, event.DirEnded{Token: 8}, event.LoginEnded{Token: 7})
	assert.NotNil(t, dirOf(m))

	send(m, event.DirEnded{Token: 7})
	assert.Nil(t, dirOf(m))
	assert.NotContains(t, strings.Join(screen(m), "\n"), "Add this folder")
}
