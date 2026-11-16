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
	return NewBash(NewShell(dir, nil)), dir
}

func TestBashDescribesItself(t *testing.T) {
	tool, _ := newBashTool(t)

	assert.Equal(t, "Bash", tool.Name)
	assert.Equal(t, policy.Exec, tool.Capability)
	assert.Nil(t, tool.Target)
	assert.False(t, tool.Sandboxed)
	assert.Equal(t, []string{"command"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	assert.Equal(t, Action{Verb: "Ran", One: "command", Many: "commands"}, tool.Display.Action)
	_, err := json.Marshal(tool.Parameters)
	require.NoError(t, err)
	assert.True(t, NewBash(NewShell(t.TempDir(), &sandbox.Sandbox{})).Sandboxed)
}

func TestBashRunsCommand(t *testing.T) {
	tool, _ := newBashTool(t)

	out, err := tool.Run(context.Background(), json.RawMessage(`{"command":"echo one; echo two","timeout":60000}`), allowTarget)

	require.NoError(t, err)
	assert.Equal(t, "one\ntwo\n", out)
	assert.Equal(t, "2 lines", tool.Display.Summary(out))
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

	_, err := tool.Run(context.Background(), json.RawMessage(`{"command":"touch marker"}`), func(policy.Target) error { return refused })

	require.ErrorIs(t, err, refused)
	assert.NoFileExists(t, filepath.Join(dir, "marker"))
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
	sandboxed := NewBash(NewShell(t.TempDir(), &sandbox.Sandbox{}))
	noSandbox := PreviewLine{Kind: Context, Text: "runs without the OS sandbox"}
	oneLine := json.RawMessage(`{"command":"ls"}`)
	twoLines := json.RawMessage(`{"command":"cd a\nls\n"}`)
	long, err := json.Marshal(map[string]string{"command": strings.Repeat("echo\n", maxPreviewLines+5)})
	require.NoError(t, err)
	wideCommand := strings.Repeat("x", maxBashTitleRunes+1)
	wide, err := json.Marshal(map[string]string{"command": wideCommand})
	require.NoError(t, err)

	assert.Equal(t, []PreviewLine{noSandbox}, unsandboxed.Display.Preview(oneLine))
	assert.Equal(t, []PreviewLine{noSandbox, {Kind: Context, Text: "cd a"}, {Kind: Context, Text: "ls"}},
		unsandboxed.Display.Preview(twoLines))
	longPreview := unsandboxed.Display.Preview(long)
	require.NotEmpty(t, longPreview)
	assert.Equal(t, noSandbox, longPreview[0])
	assert.Empty(t, sandboxed.Display.Preview(oneLine))
	assert.Equal(t, []PreviewLine{{Kind: Context, Text: "cd a"}, {Kind: Context, Text: "ls"}},
		sandboxed.Display.Preview(twoLines))
	assert.Equal(t, []PreviewLine{noSandbox, {Kind: Context, Text: wideCommand}}, unsandboxed.Display.Preview(wide))
	assert.Equal(t, []PreviewLine{{Kind: Context, Text: wideCommand}}, sandboxed.Display.Preview(wide))
}
