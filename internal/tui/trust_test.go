package tui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/config"
)

var trustReq = TrustRequest{Folder: "/w/proj", Parent: "/w", Servers: []string{"docs: npx -y docs-mcp", "remote: https://example.com/mcp"}}

func newTrustTestModel(req TrustRequest) *trustModel {
	m := newTrustModel(req, config.DefaultKeys())
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	return m
}

// trustScreen is the view's text, one string per row, with trailing blanks trimmed.
func trustScreen(m *trustModel) []string {
	return plainRows(strings.Split(m.View().Content, "\n"))
}

// trustPress sends msg and reports whether the program quit.
func trustPress(m *trustModel, msg tea.KeyPressMsg) bool {
	_, cmd := m.Update(msg)
	if cmd == nil {
		return false
	}
	_, quit := cmd().(tea.QuitMsg)
	return quit
}

func TestTrustRows(t *testing.T) {
	rule := strings.Repeat("─", 60)
	assert.Equal(t, append(make([]string, 16),
		rule,
		"  Do you trust this folder?",
		"  /w/proj",
		"",
		"  Its .mcp.json declares MCP servers that Kopi would start.",
		"  They run with your permissions.",
		"    docs: npx -y docs-mcp",
		"    remote: https://example.com/mcp",
		"",
		"❭ 1. Yes, trust this folder",
		"  2. Yes, trust the parent folder (/w)",
		"  3. No, don't trust",
		rule,
		"  enter to choose · esc to decide later",
	), trustScreen(newTrustTestModel(trustReq)))
	assert.True(t, newTrustTestModel(trustReq).View().AltScreen)
}

func TestTrustRowsListInstructionFiles(t *testing.T) {
	rule := strings.Repeat("─", 60)
	files := []string{"/w/AGENTS.md", "/w/proj/CLAUDE.md"}
	filesOnly := TrustRequest{Folder: "/w/proj", Parent: "/w", Instructions: files}
	assert.Equal(t, append(make([]string, 16),
		rule,
		"  Do you trust this folder?",
		"  /w/proj",
		"",
		"  Kopi would add these instruction files to the model's",
		"  instructions:",
		"    /w/AGENTS.md",
		"    /w/proj/CLAUDE.md",
		"",
		"❭ 1. Yes, trust this folder",
		"  2. Yes, trust the parent folder (/w)",
		"  3. No, don't trust",
		rule,
		"  enter to choose · esc to decide later",
	), trustScreen(newTrustTestModel(filesOnly)))

	both := trustReq
	both.Instructions = files
	assert.Equal(t, append(make([]string, 11),
		rule,
		"  Do you trust this folder?",
		"  /w/proj",
		"",
		"  Kopi would add these instruction files to the model's",
		"  instructions:",
		"    /w/AGENTS.md",
		"    /w/proj/CLAUDE.md",
		"",
		"  Its .mcp.json declares MCP servers that Kopi would start.",
		"  They run with your permissions.",
		"    docs: npx -y docs-mcp",
		"    remote: https://example.com/mcp",
		"",
		"❭ 1. Yes, trust this folder",
		"  2. Yes, trust the parent folder (/w)",
		"  3. No, don't trust",
		rule,
		"  enter to choose · esc to decide later",
	), trustScreen(newTrustTestModel(both)))
}

func TestTrustRowsCapTheServersBelowTheInstructionFiles(t *testing.T) {
	req := trustReq
	req.Servers = []string{"a: a-mcp", "b: b-mcp", "c: c-mcp", "d: d-mcp", "e: e-mcp"}
	req.Instructions = []string{"/w/AGENTS.md"}
	m := newTrustTestModel(req)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 19})
	rows := trustScreen(m)
	assert.Len(t, rows, 19)
	assert.Equal(t, []string{"    /w/AGENTS.md", "", "  Its .mcp.json declares MCP servers that Kopi would start.", "  They run with your permissions.", "    a: a-mcp", "    b: b-mcp", "    … 3 more servers"}, rows[6:13])
}

func TestTrustChoices(t *testing.T) {
	noParent := trustReq
	noParent.Parent = ""
	for _, tt := range []struct {
		name string
		req  TrustRequest
		keys []tea.KeyPressMsg
		want TrustChoice
	}{
		{name: "enter trusts the folder", req: trustReq, keys: []tea.KeyPressMsg{enter}, want: TrustFolder},
		{name: "down then enter trusts the parent", req: trustReq, keys: []tea.KeyPressMsg{down, enter}, want: TrustParent},
		{name: "digit 3 denies", req: trustReq, keys: []tea.KeyPressMsg{digit('3')}, want: TrustDeny},
		{name: "without a parent 2 denies", req: noParent, keys: []tea.KeyPressMsg{digit('2')}, want: TrustDeny},
		{name: "without a parent 3 does nothing", req: noParent, keys: []tea.KeyPressMsg{digit('3'), up, enter}, want: TrustFolder},
		{name: "cancel", req: trustReq, keys: []tea.KeyPressMsg{down, ctrlC}, want: TrustCancelled},
		{name: "interrupt cancels", req: trustReq, keys: []tea.KeyPressMsg{escape}, want: TrustCancelled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newTrustTestModel(tt.req)
			quit := false
			for _, k := range tt.keys {
				require.False(t, quit, "a key after the prompt ended")
				quit = trustPress(m, k)
			}
			assert.True(t, quit)
			assert.Equal(t, tt.want, m.choice)
		})
	}
}

func TestTrustRowsHideTheParentOption(t *testing.T) {
	req := trustReq
	req.Parent = ""
	rows := trustScreen(newTrustTestModel(req))
	assert.Equal(t, []string{"❭ 1. Yes, trust this folder", "  2. No, don't trust"}, rows[len(rows)-4:len(rows)-2])
	assert.NotContains(t, strings.Join(rows, "\n"), "parent")
}

func TestTrustRowsKeepHostileTextOnItsLine(t *testing.T) {
	clean := trustScreen(newTrustTestModel(trustReq))
	for name, attack := range map[string]string{
		"CSI":      "\x1b[2J",
		"OSC":      "\x1b]52;c;c2VjcmV0\a",
		"controls": "\x00\a\b\v\f\x7f\u009b",
	} {
		t.Run(name, func(t *testing.T) {
			req := trustReq
			req.Servers = []string{attack + "docs: npx -y docs-mcp", "remote: https://example.com/mcp" + attack}
			req.Folder = attack + req.Folder
			assert.Equal(t, clean, trustScreen(newTrustTestModel(req)))
		})
	}

	req := trustReq
	req.Servers = []string{"docs: a\nfake: b\r\nc", "remote: https://example.com/mcp"}
	rows := trustScreen(newTrustTestModel(req))
	assert.Contains(t, rows, "    docs: a fake: b c", "a line break cannot start a new server line")
	for _, r := range newTrustTestModel(req).rows() {
		assert.LessOrEqual(t, ansi.StringWidth(r), 60)
	}
}

func TestTrustRowsCapTheServersToTheHeight(t *testing.T) {
	req := trustReq
	req.Servers = []string{"a: a-mcp", "b: b-mcp", "c: c-mcp", "d: d-mcp", "e: e-mcp"}
	m := newTrustTestModel(req)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 15})
	rule := strings.Repeat("─", 60)
	assert.Equal(t, []string{
		rule,
		"  Do you trust this folder?",
		"  /w/proj",
		"",
		"  Its .mcp.json declares MCP servers that Kopi would start.",
		"  They run with your permissions.",
		"    a: a-mcp",
		"    b: b-mcp",
		"    … 3 more servers",
		"",
		"❭ 1. Yes, trust this folder",
		"  2. Yes, trust the parent folder (/w)",
		"  3. No, don't trust",
		rule,
		"  enter to choose · esc to decide later",
	}, trustScreen(m))

	m.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	assert.Contains(t, trustScreen(m), "    … 5 more servers", "with no room left the count still shows")
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 14})
	assert.Contains(t, trustScreen(m), "    … 4 more servers")
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
	assert.Contains(t, trustScreen(m), "    … 2 more servers")
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 17})
	assert.Contains(t, trustScreen(m), "    e: e-mcp", "the last server takes the row the count would")
}

func TestTrustRowsCutTheTopWhenTheFixedRowsDoNotFit(t *testing.T) {
	m := newTrustTestModel(trustReq)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 5})
	rows := trustScreen(m)
	assert.Equal(t, []string{"❭ 1. Yes, trust this folder", "  2. Yes, trust the parent folder (/w)", "  3. No, don't trust", strings.Repeat("─", 60), "  enter to choose · esc to decide later"}, rows)
}

func TestTrustHintNamesBoundKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys map[string][]string
		want string
	}{
		{"rebound", map[string][]string{"submit": {"ctrl+j", "enter"}, "interrupt": {"ctrl+g"}}, "  ctrl+j to choose · ctrl+g to decide later"},
		{"no interrupt", map[string][]string{"submit": {"enter"}}, "  enter to choose"},
		{"neither", map[string][]string{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTrustModel(trustReq, tc.keys)
			m.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
			rows := trustScreen(m)
			assert.Equal(t, tc.want, rows[len(rows)-1])
		})
	}
}

func TestAskTrustReturnsCancelledWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan TrustChoice, 1)
	errs := make(chan error, 1)
	// The input never ends, so only ctx stops the prompt.
	in, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	go func() {
		c, err := AskTrust(ctx, in, &lockedBuffer{}, testOptions(), trustReq)
		errs <- err
		done <- c
	}()
	cancel()
	select {
	case err := <-errs:
		require.NoError(t, err)
		assert.Equal(t, TrustCancelled, <-done)
	case <-time.After(waitLimit):
		t.Fatal("AskTrust did not return")
	}
}

func TestAskTrustReturnsThePick(t *testing.T) {
	c, err := AskTrust(t.Context(), strings.NewReader("3"), &bytes.Buffer{}, testOptions(), trustReq)
	require.NoError(t, err)
	assert.Equal(t, TrustDeny, c)
}
