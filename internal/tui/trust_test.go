package tui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
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
	m := newTrustModel(req, config.DefaultKeys(), testThemes)
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
		"  Its .mcp.json declares MCP servers that Kopi would start.",
		"  They run with your permissions.",
		"    docs: npx -y docs-mcp",
		"    remote: https://example.com/mcp",
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
	), trustScreen(newTrustTestModel(both)))
}

// trustReach is every row the prompt shows while the page keys scroll its lists
// from the top to the end.
func trustReach(m *trustModel) []string {
	var seen []string
	for prev := ""; ; trustPress(m, pgDown) {
		rows := trustScreen(m)
		if strings.Join(rows, "\n") == prev {
			return seen
		}
		prev = strings.Join(rows, "\n")
		seen = append(seen, rows...)
	}
}

func TestTrustRowsScrollTheInstructionFilesBelowTheServers(t *testing.T) {
	req := trustReq
	req.Servers = []string{"a: a-mcp", "b: b-mcp", "c: c-mcp", "d: d-mcp", "e: e-mcp"}
	req.Instructions = []string{"/w/AGENTS.md"}
	m := newTrustTestModel(req)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 19})
	rows := trustScreen(m)
	assert.Len(t, rows, 19)
	assert.Equal(t, []string{"  Its .mcp.json declares MCP servers that Kopi would start.", "  They run with your permissions.", "    a: a-mcp", "    b: b-mcp", "    c: c-mcp", "    d: d-mcp", "    e: e-mcp", "  … 4 more lines", "  " + scrollHint(&m.keys)}, rows[4:13])
	assert.Contains(t, trustReach(m), "    /w/AGENTS.md")
}

func TestTrustRowsScrollInstructionFilesToTheHeight(t *testing.T) {
	req := TrustRequest{Folder: "/w/proj", Parent: "/w"}
	for i := range 20 {
		req.Instructions = append(req.Instructions, fmt.Sprintf("/w/%d/AGENTS.md", i))
	}
	m := newTrustTestModel(req)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
	rows := trustScreen(m)
	assert.Len(t, rows, 16)
	assert.Equal(t, strings.Repeat("─", 60), rows[0])
	assert.Equal(t, "  Do you trust this folder?", rows[1])
	assert.Equal(t, "  /w/proj", rows[2])
	assert.Equal(t, []string{"    /w/0/AGENTS.md", "    /w/1/AGENTS.md", "  … 18 more lines", "  " + scrollHint(&m.keys)}, rows[6:10])
	assert.Equal(t, "  enter to choose · esc to decide later", rows[15])
	reached := trustReach(m)
	for i := range 20 {
		assert.Contains(t, reached, fmt.Sprintf("    /w/%d/AGENTS.md", i))
	}
	rows = trustScreen(m)
	assert.Len(t, rows, 16)
	assert.Equal(t, strings.Repeat("─", 60), rows[0])
	assert.Equal(t, "❭ 1. Yes, trust this folder", rows[11])
	assert.Equal(t, "  enter to choose · esc to decide later", rows[15])
}

func TestTrustRowsScrollBack(t *testing.T) {
	req := trustReq
	req.Servers = []string{"a: a-mcp", "b: b-mcp", "c: c-mcp", "d: d-mcp", "e: e-mcp"}
	m := newTrustTestModel(req)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 14})
	top := trustScreen(m)
	assert.NotContains(t, top, "    e: e-mcp")
	trustReach(m)
	assert.Contains(t, trustScreen(m), "    e: e-mcp")
	for range 10 {
		trustPress(m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	}
	assert.Equal(t, top, trustScreen(m))
}

func TestTrustRowsShowTheScrollHintOnlyWhileCut(t *testing.T) {
	m := newTrustTestModel(trustReq)
	assert.NotContains(t, strings.Join(trustScreen(m), "\n"), "to scroll")
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	assert.Contains(t, trustScreen(m), "  "+scrollHint(&m.keys))
}

func TestTrustRowsKeepTheMarkerWithoutTheHintWhenTheHintWouldStarveTheLists(t *testing.T) {
	req := TrustRequest{Folder: "/w/proj", Parent: "/w", Instructions: []string{"/w/proj/AGENTS.md"}, Servers: []string{"a: a-mcp", "b: b-mcp", "c: c-mcp"}}
	m := newTrustTestModel(req)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 9})
	screen := strings.Join(trustScreen(m), "\n")
	assert.Contains(t, screen, "  … 11 more lines")
	assert.NotContains(t, screen, scrollHint(&m.keys))
}

func TestTrustRowsScrollWrappedInstructionPaths(t *testing.T) {
	path := "/w/" + strings.Repeat("a", 60) + "/AGENTS.md"
	req := TrustRequest{Folder: "/w/proj", Parent: "/w", Instructions: []string{path, path, "/w/AGENTS.md"}}
	m := newTrustTestModel(req)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
	rows := trustScreen(m)
	assert.Equal(t, "  /w/proj", rows[2])
	assert.Equal(t, []string{"    /w/" + strings.Repeat("a", 53), "    " + strings.Repeat("a", 7) + "/AGENTS.md", "  … 3 more lines"}, rows[6:9])
	assert.Contains(t, trustReach(m), "    /w/AGENTS.md")

	m.Update(tea.WindowSizeMsg{Width: 60, Height: 19})
	rows = trustScreen(m)
	assert.Equal(t, rows[8:10], rows[10:12])
	assert.Contains(t, rows, "    /w/AGENTS.md")
	assert.NotContains(t, strings.Join(rows, "\n"), "more lines")
}

func TestTrustRowsScrollWrappedInstructionPathsAtANarrowWidth(t *testing.T) {
	path := "/w/" + strings.Repeat("a", 30) + "/AGENTS.md"
	req := TrustRequest{Folder: "/w/proj"}
	for range 20 {
		req.Instructions = append(req.Instructions, path)
	}
	m := newTrustTestModel(req)
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 24})
	rows := trustScreen(m)
	require.Len(t, rows, 24)
	assert.Equal(t, strings.Repeat("─", 30), rows[0])
	assert.Equal(t, "  Do you trust this folder?", rows[1])
	assert.Equal(t, "  /w/proj", rows[2])
	assert.Equal(t, []string{"  … 30 more lines", "  " + scrollHint(&m.keys)}, rows[17:19])
	assert.Equal(t, []string{
		"",
		"❭ 1. Yes, trust this folder",
		"  2. No, don't trust",
		strings.Repeat("─", 30),
		"  enter to choose · esc to de…",
	}, rows[19:])
	reached := trustReach(m)
	assert.Contains(t, reached, "    /w/"+strings.Repeat("a", 23))
	assert.Contains(t, reached, "    "+strings.Repeat("a", 7)+"/AGENTS.md")
}

func TestTrustRowsKeepTheServersInViewWhileInstructionFilesScroll(t *testing.T) {
	req := trustReq
	for range 20 {
		req.Instructions = append(req.Instructions, "/w/AGENTS.md")
	}
	m := newTrustTestModel(req)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 19})
	rows := trustScreen(m)
	assert.Len(t, rows, 19)
	assert.Equal(t, "  Do you trust this folder?", rows[1])
	assert.Equal(t, "  /w/proj", rows[2])
	assert.Contains(t, rows, "  Its .mcp.json declares MCP servers that Kopi would start.")
	assert.Contains(t, rows, "  They run with your permissions.")
	assert.Contains(t, rows, "    docs: npx -y docs-mcp")
	assert.Contains(t, rows, "  … 20 more lines")
	assert.Contains(t, rows, "  "+scrollHint(&m.keys))
	assert.Contains(t, trustReach(m), "    /w/AGENTS.md")
}

func TestTrustRowsListSkillFolders(t *testing.T) {
	rule := strings.Repeat("─", 60)
	skillsOnly := TrustRequest{Folder: "/w/proj", Parent: "/w", Skills: []string{"/w/.agents/skills", "/w/proj/.agents/skills"}}
	assert.Equal(t, append(make([]string, 17),
		rule,
		"  Do you trust this folder?",
		"  /w/proj",
		"",
		"  Kopi would load the skills in these folders:",
		"    /w/.agents/skills",
		"    /w/proj/.agents/skills",
		"",
		"❭ 1. Yes, trust this folder",
		"  2. Yes, trust the parent folder (/w)",
		"  3. No, don't trust",
		rule,
		"  enter to choose · esc to decide later",
	), trustScreen(newTrustTestModel(skillsOnly)))

	all := trustReq
	all.Instructions = []string{"/w/AGENTS.md"}
	all.Skills = []string{"/w/proj/.agents/skills"}
	rows := trustScreen(newTrustTestModel(all))
	assert.Equal(t, []string{
		"  Its .mcp.json declares MCP servers that Kopi would start.",
		"  They run with your permissions.",
		"    docs: npx -y docs-mcp",
		"    remote: https://example.com/mcp",
		"",
		"  Kopi would add these instruction files to the model's",
		"  instructions:",
		"    /w/AGENTS.md",
		"",
		"  Kopi would load the skills in these folders:",
		"    /w/proj/.agents/skills",
		"",
	}, rows[slices.Index(rows, "  Do you trust this folder?")+3:][:12])
}

func TestTrustRowsScrollSkillFolders(t *testing.T) {
	req := TrustRequest{Folder: "/w/proj", Skills: []string{"/a/.agents/skills", "/b/.agents/skills", "/c/.agents/skills"}}
	m := newTrustTestModel(req)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	rows := trustScreen(m)
	assert.Len(t, rows, 12)
	assert.NotContains(t, rows, "    /c/.agents/skills")
	reached := trustReach(m)
	assert.Contains(t, reached, "    /a/.agents/skills")
	assert.Contains(t, reached, "    /c/.agents/skills")
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

func TestTrustRowsScrollTheServersToTheHeight(t *testing.T) {
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
		"  … 4 more lines",
		"  " + scrollHint(&m.keys),
		"",
		"❭ 1. Yes, trust this folder",
		"  2. Yes, trust the parent folder (/w)",
		"  3. No, don't trust",
		rule,
		"  enter to choose · esc to decide later",
	}, trustScreen(m))

	for _, height := range []int{12, 14, 16, 17} {
		m.Update(tea.WindowSizeMsg{Width: 60, Height: height})
		rows := trustScreen(m)
		assert.Len(t, rows, height)
		assert.Equal(t, rule, rows[len(rows)-2], "height %d", height)
		assert.Contains(t, rows, "❭ 1. Yes, trust this folder", "height %d", height)
		assert.Contains(t, trustReach(m), "    e: e-mcp", "height %d", height)
	}
}

func TestTrustPagingReachesTheLastServerAndReturns(t *testing.T) {
	req := trustReq
	req.Servers = []string{"a: a-mcp", "b: b-mcp", "c: c-mcp", "d: d-mcp", "e: e-mcp"}
	m := newTrustTestModel(req)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 14})
	top := trustScreen(m)
	assert.NotContains(t, top, "    e: e-mcp")
	for range 5 {
		assert.False(t, trustPress(m, pgDown))
	}
	assert.Contains(t, trustScreen(m), "    e: e-mcp")
	for range 5 {
		assert.False(t, trustPress(m, tea.KeyPressMsg{Code: tea.KeyPgUp}))
	}
	assert.Equal(t, top, trustScreen(m))
}

func TestTrustRowsWrapOncePerWidthAndTheme(t *testing.T) {
	m := newTrustTestModel(trustReq)
	m.rows()
	require.NotEmpty(t, m.body)
	body := &m.body[0]
	m.rows()
	assert.Same(t, body, &m.body[0])
	m.Update(tea.WindowSizeMsg{Width: 50, Height: 30})
	m.rows()
	assert.NotSame(t, body, &m.body[0])
}

func TestTrustRowsKeepBothRulesWhenTheFixedRowsDoNotFit(t *testing.T) {
	for _, instructions := range [][]string{nil, {"/w/AGENTS.md", "/w/proj/AGENTS.md"}} {
		req := trustReq
		req.Instructions = instructions
		m := newTrustTestModel(req)
		m.Update(tea.WindowSizeMsg{Width: 60, Height: 5})
		rows := trustScreen(m)
		rule := strings.Repeat("─", 60)
		assert.Equal(t, []string{rule, "❭ 1. Yes, trust this folder", "  1 of 3", rule, "  enter to choose · esc to decide later"}, rows)
	}
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
			m := newTrustModel(trustReq, tc.keys, testThemes)
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
