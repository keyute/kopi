package tui

import (
	"context"
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// TrustRequest is what the trust prompt shows. Every field may come from an
// untrusted file.
type TrustRequest struct {
	Folder string
	// Parent is offered as a choice unless it is empty.
	Parent string
	// Servers has one line per server, such as "name: command args".
	Servers []string
}

type TrustChoice int

const (
	TrustCancelled TrustChoice = iota
	TrustFolder
	TrustParent
	TrustDeny
)

// AskTrust asks whether to trust req's folder, before the main UI starts. The
// cancel and interrupt keys and ctx's end return TrustCancelled.
func AskTrust(ctx context.Context, in io.Reader, out io.Writer, opts Options, req TrustRequest) (TrustChoice, error) {
	m := newTrustModel(req, opts.Keys)
	p := tea.NewProgram(m, tea.WithInput(in), tea.WithOutput(out), tea.WithoutSignalHandler())
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-stopped:
		case <-ctx.Done():
			p.Quit()
		}
	}()
	if _, err := p.Run(); err != nil {
		return TrustCancelled, fmt.Errorf("failed to run the trust prompt: %w", err)
	}
	return m.choice, nil
}

type trustModel struct {
	req           TrustRequest
	keys          keyMap
	th            theme
	width, height int
	dialog        *dialog
	// choices[i] is what option i picks.
	choices []TrustChoice
	choice  TrustChoice
}

func newTrustModel(req TrustRequest, keys map[string][]string) *trustModel {
	m := &trustModel{req: req, keys: newKeyMap(keys), th: mocha(), dialog: &dialog{title: "Do you trust this folder?"}}
	add := func(label string, c TrustChoice) {
		m.dialog.options = append(m.dialog.options, option{label: label})
		m.choices = append(m.choices, c)
	}
	add("Yes, trust this folder", TrustFolder)
	if req.Parent != "" {
		add("Yes, trust the parent folder ("+oneLine(req.Parent)+")", TrustParent)
	}
	add("No, don't trust", TrustDeny)
	return m
}

func (m *trustModel) Init() tea.Cmd { return tea.RequestBackgroundColor }

func (m *trustModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.BackgroundColorMsg:
		if !msg.IsDark() {
			m.th = latte()
		}
	case tea.KeyPressMsg:
		if key.Matches(msg, m.keys.cancel, m.keys.interrupt) {
			return m, tea.Quit
		}
		if picked, _ := m.dialog.key(msg, &m.keys); picked {
			m.choice = m.choices[m.dialog.sel]
			return m, tea.Quit
		}
	}
	return m, nil
}

// View is on the alternate screen so the terminal is left as it was for the
// main UI.
func (m *trustModel) View() tea.View {
	v := tea.View{AltScreen: true}
	if m.width == 0 || m.height == 0 {
		return v
	}
	rows := m.rows()
	// Past the height the top is cut so the options stay visible.
	v.SetContent(strings.Join(rows[max(0, len(rows)-m.height):], "\n"))
	return v
}

func (m *trustModel) rows() []string {
	none, width := lipgloss.NewStyle(), m.width-2
	body := hang("", none, oneLine(m.req.Folder), none.Bold(true), width)
	body = append(body, "")
	body = append(body, hang("", none, "Its .mcp.json declares MCP servers that Kopi would start. They run with your permissions.", none, width)...)
	for _, s := range m.req.Servers {
		body = append(body, hang("  ", none, oneLine(s), fg(m.th.muted), width)...)
	}
	m.dialog.body = body
	return m.dialog.rows(m.width, m.height, 1, m.th)
}
