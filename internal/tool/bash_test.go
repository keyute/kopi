package tool

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/sandbox"
)

func newBashTool(t *testing.T) (Tool, string) {
	t.Helper()
	dir := t.TempDir()
	return NewBash(NewShell(dir, nil), NewShell(dir, nil)), dir
}

func TestBashDescribesItself(t *testing.T) {
	tool, _ := newBashTool(t)

	assert.Equal(t, "Bash", tool.Name)
	assert.Equal(t, policy.Exec, tool.Capability)
	assert.Nil(t, tool.Target)
	assert.False(t, tool.Sandboxed)
	assert.False(t, tool.PlanSandboxed)
	assert.NotContains(t, tool.Description, "sandbox")
	assert.Equal(t, []string{"command"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	assert.Equal(t, Action{Verb: "Ran", One: "command", Many: "commands"}, tool.Display.Action)
	_, err := json.Marshal(tool.Parameters)
	require.NoError(t, err)
	sandboxed := NewBash(NewShell(t.TempDir(), &sandbox.Sandbox{}), NewShell(t.TempDir(), &sandbox.Sandbox{}))
	assert.True(t, sandboxed.Sandboxed)
	assert.True(t, sandboxed.PlanSandboxed)
	assert.Contains(t, sandboxed.Description, "in execute mode, the workspace")
	assert.False(t, NewBash(NewShell(t.TempDir(), &sandbox.Sandbox{}), NewShell(t.TempDir(), nil)).PlanSandboxed)
}

func TestBashRunsCommand(t *testing.T) {
	tool, _ := newBashTool(t)

	out, err := tool.Run(context.Background(), json.RawMessage(`{"command":"echo one; echo two","timeout":60000}`), allowTarget)

	require.NoError(t, err)
	assert.Equal(t, "one\ntwo\n", out.Text)
	assert.Equal(t, "2 lines", tool.Display.Summary(out.Text))
}

func TestBashTimeoutIsMilliseconds(t *testing.T) {
	tool, _ := newBashTool(t)

	_, err := tool.Run(context.Background(), json.RawMessage(`{"command":"sleep 600","timeout":1}`), allowTarget)

	require.ErrorIs(t, err, ErrTimedOut)
	assert.ErrorContains(t, err, "after 1ms")
}

func TestBashRejectsInvalidArgs(t *testing.T) {
	tool, _ := newBashTool(t)

	for _, tc := range []struct {
		name, args string
	}{
		{"bad json", `{"command":`},
		{"missing command", `{}`},
		{"empty command", `{"command":""}`},
		{"zero timeout", `{"command":"true","timeout":0}`},
		{"timeout too long", `{"command":"true","timeout":600001}`},
		{"unknown field", `{"command":"true","description":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Run(context.Background(), json.RawMessage(tc.args), allowTarget)
			require.ErrorIs(t, err, ErrInvalidArgs)
		})
	}
}

func TestBashDoesNotRunWithoutAdmission(t *testing.T) {
	tool, dir := newBashTool(t)
	refused := errors.New("refused")

	_, err := tool.Run(context.Background(), json.RawMessage(`{"command":"touch marker"}`), func(policy.Target) (policy.Mode, error) { return policy.Plan, refused })

	require.ErrorIs(t, err, refused)
	assert.NoFileExists(t, filepath.Join(dir, "marker"))
}

func TestBashDoesNotStartWhenAdmissionCancels(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	// A Start attempt must fail differently from cancellation.
	sh.shell = filepath.Join(sh.dir, "missing-shell")
	tool := NewBash(sh, NewShell(sh.dir, nil))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	admitted := false

	out, err := tool.Run(ctx, json.RawMessage(`{"command":"echo unexpected"}`), func(policy.Target) (policy.Mode, error) {
		admitted = true
		cancel()
		return policy.Execute, nil
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, context.Canceled.Error(), err.Error())
	assert.Empty(t, out.Text)
	assert.True(t, admitted)
}

func TestBashRunsInTheShellOfTheAdmittedMode(t *testing.T) {
	execDir, planDir := t.TempDir(), t.TempDir()
	tool := NewBash(NewShell(execDir, &sandbox.Sandbox{}), NewShell(planDir, &sandbox.Sandbox{}))
	for _, tc := range []struct {
		file string
		mode policy.Mode
	}{
		{"plan", policy.Plan},
		{"execute", policy.Execute},
		{"unknown", policy.Mode(7)},
	} {
		_, err := tool.Run(t.Context(), json.RawMessage(`{"command":"touch `+tc.file+`"}`), func(policy.Target) (policy.Mode, error) {
			return tc.mode, nil
		})
		require.NoError(t, err, tc.file)
	}
	assert.Equal(t, []string{"plan", "unknown"}, listDir(t, planDir))
	assert.Equal(t, []string{"execute"}, listDir(t, execDir))
}

func TestBashRefusesPlanModeWithoutPlanSandbox(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sandboxed bool
	}{
		{"unsandboxed exec shell", false},
		{"sandboxed exec shell", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execDir, planDir := t.TempDir(), t.TempDir()
			var sb *sandbox.Sandbox
			if tc.sandboxed {
				sb = &sandbox.Sandbox{}
			}
			tool := NewBash(NewShell(execDir, sb), NewShell(planDir, nil))

			_, err := tool.Run(t.Context(), json.RawMessage(`{"command":"touch marker"}`), func(policy.Target) (policy.Mode, error) {
				return policy.Plan, nil
			})

			require.ErrorIs(t, err, policy.ErrDenied)
			assert.False(t, tool.PlanSandboxed)
			assert.NoFileExists(t, filepath.Join(execDir, "marker"))
			assert.NoFileExists(t, filepath.Join(planDir, "marker"))
		})
	}
}

func TestBashDisplayTitle(t *testing.T) {
	tool, _ := newBashTool(t)

	assert.Equal(t, "Bash go test ./...", tool.Display.Title(json.RawMessage(`{"command":"go test ./..."}`)))
	assert.Equal(t, "Bash ls", tool.Display.Title(json.RawMessage(`{"command":"ls\n"}`)))
	assert.Equal(t, "Bash cd a …", tool.Display.Title(json.RawMessage(`{"command":"cd a\nls"}`)))
	assert.Equal(t, "Bash", tool.Display.Title(json.RawMessage(`{"command":`)))

	exact, err := json.Marshal(map[string]string{"command": strings.Repeat("é", maxBashTitleRunes)})
	require.NoError(t, err)
	assert.Equal(t, "Bash "+strings.Repeat("é", maxBashTitleRunes), tool.Display.Title(exact))

	long, err := json.Marshal(map[string]string{"command": strings.Repeat("a", maxBashTitleRunes-1) + "日本語"})
	require.NoError(t, err)
	title := tool.Display.Title(long)
	assert.Equal(t, "Bash "+strings.Repeat("a", maxBashTitleRunes-1)+"日 …", title)
	assert.True(t, utf8.ValidString(title))
}

func TestBashDisplaySummary(t *testing.T) {
	tool, _ := newBashTool(t)

	assert.Equal(t, "no output", tool.Display.Summary(noOutputNotice))
	assert.Equal(t, "1 line", tool.Display.Summary("hi\n"))
	assert.Equal(t, "3 lines", tool.Display.Summary("a\nb\nc"))
}

func TestBashDisplayPreview(t *testing.T) {
	unsandboxed, _ := newBashTool(t)
	sandboxed := NewBash(NewShell(t.TempDir(), &sandbox.Sandbox{}), NewShell(t.TempDir(), nil))
	oneLine := json.RawMessage(`{"command":"ls"}`)
	twoLines := json.RawMessage(`{"command":"cd a\nls\n"}`)
	long, err := json.Marshal(map[string]string{"command": strings.Repeat("echo\n", maxPreviewLines+5)})
	require.NoError(t, err)
	wideCommand := strings.Repeat("x", maxBashTitleRunes+1)
	wide, err := json.Marshal(map[string]string{"command": wideCommand})
	require.NoError(t, err)

	for _, bash := range []Tool{unsandboxed, sandboxed} {
		assert.Empty(t, bash.Display.Preview(oneLine))
		assert.Equal(t, []PreviewLine{{Kind: Context, Text: "cd a"}, {Kind: Context, Text: "ls"}},
			bash.Display.Preview(twoLines))
		longPreview := bash.Display.Preview(long)
		require.NotEmpty(t, longPreview)
		assert.Equal(t, PreviewLine{Kind: Context, Text: "echo"}, longPreview[0])
		assert.Equal(t, []PreviewLine{{Kind: Context, Text: wideCommand}}, bash.Display.Preview(wide))
	}
}
