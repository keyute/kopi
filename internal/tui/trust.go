package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

// TrustRequest is what the trust prompt shows. Every field may come from an
// untrusted file.
type TrustRequest struct {
	Folder string
	// Parent is offered as a choice unless it is empty.
	Parent string
	// Servers has one line per server, such as "name: command args".
	Servers []string
	// Instructions are the paths of the folder's instruction files.
	Instructions []string
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
	// Discard already queued input, not establish a render/readiness boundary.
	// Keys arriving during Bubble Tea startup can still be accepted before the
	// first draw; that timing limitation is accepted.
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		if err := discardInput(f); err != nil {
			return TrustCancelled, fmt.Errorf("failed to discard typed-ahead input before the trust prompt: %w", err)
		}
	}
	// Started only once Run is certain: Quit blocks until Run reads it.
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
	rows := append(m.rows(), m.hint())
	// rows caps the servers to the height; only when the other rows alone do
	// not fit is the top cut so the options stay visible. Short of the height,
	// blank rows above put the dialog at the bottom, where approvals are drawn.
	rows = rows[max(0, len(rows)-m.height):]
	v.SetContent(strings.Join(append(make([]string, m.height-len(rows)), rows...), "\n"))
	return v
}

// hint is the footer row naming the bound keys.
func (m *trustModel) hint() string {
	var parts []string
	if ks := m.keys.submit.Keys(); len(ks) > 0 {
		parts = append(parts, ks[0]+" to choose")
	}
	if ks := m.keys.interrupt.Keys(); len(ks) > 0 {
		parts = append(parts, ks[0]+" to decide later")
	}
	if len(parts) == 0 {
		return ""
	}
	return fg(m.th.dim).Render(ansi.Truncate("  "+strings.Join(parts, " · "), m.width, "…"))
}

func (m *trustModel) rows() []string {
	none, width := lipgloss.NewStyle(), m.width-2
	body := hang("", none, oneLine(m.req.Folder), none.Bold(true), width)
	if len(m.req.Instructions) > 0 {
		body = append(body, "")
		body = append(body, hang("", none, "Kopi would add these instruction files to the model's instructions:", none, width)...)
		for _, path := range m.req.Instructions {
			body = append(body, hang("  ", none, oneLine(path), fg(m.th.muted), width)...)
		}
	}
	if len(m.req.Servers) > 0 {
		body = append(body, "")
		body = append(body, hang("", none, "Its .mcp.json declares MCP servers that Kopi would start. They run with your permissions.", none, width)...)
		m.dialog.body = body
		// The footer row is the one below the dialog.
		fixed := len(m.dialog.rows(m.width, m.height, 1, m.th)) + 1
		body = append(body, m.serverRows(m.height-fixed, width)...)
	}
	m.dialog.body = body
	return m.dialog.rows(m.width, m.height, 1, m.th)
}

// serverRows draws whole server lines within limit rows; when they do not all
// fit, the last row counts the servers left out. The count shows even past
// limit, so the top cut drops a top row rather than every server unseen.
func (m *trustModel) serverRows(limit, width int) []string {
	var rows []string
	for i, s := range m.req.Servers {
		lines := hang("  ", lipgloss.NewStyle(), oneLine(s), fg(m.th.muted), width)
		budget := limit - len(rows)
		if i != len(m.req.Servers)-1 {
			budget--
		}
		if len(lines) <= budget {
			rows = append(rows, lines...)
			continue
		}
		more := fmt.Sprintf("… %d more servers", len(m.req.Servers)-i)
		if len(m.req.Servers)-i == 1 {
			more = "… 1 more server"
		}
		return append(rows, hang("  ", fg(m.th.dim), more, fg(m.th.dim), width)[0])
	}
	return rows
}
