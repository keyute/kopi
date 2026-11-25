package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

func newWriteTool(t *testing.T) (Tool, string) {
	t.Helper()
	dir := t.TempDir()
	return NewWrite(newWorkspace(t, dir, nil)), dir
}

func TestWriteDescribesItself(t *testing.T) {
	tool, _ := newWriteTool(t)

	assert.Equal(t, "Write", tool.Name)
	assert.Equal(t, policy.Write, tool.Capability)
	assert.Equal(t, []string{"file_path", "content"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	assert.Equal(t, Action{Verb: "Wrote", One: "file", Many: "files"}, tool.Display.Action)
	_, err := json.Marshal(tool.Parameters)
	require.NoError(t, err)
}

func TestWriteCreatesFileAndReports(t *testing.T) {
	tool, dir := newWriteTool(t)

	for _, tc := range []struct {
		name, args, path, content, want string
	}{
		{"lines", `{"file_path":"sub/a.txt","content":"one\ntwo\n"}`, "sub/a.txt", "one\ntwo\n", "wrote 2 lines to sub/a.txt"},
		{"one line", `{"file_path":"b.txt","content":"one"}`, "b.txt", "one", "wrote 1 line to b.txt"},
		{"empty", `{"file_path":"c.txt","content":""}`, "c.txt", "", "wrote 0 lines to c.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tool.Run(context.Background(), json.RawMessage(tc.args), allowTarget)
			require.NoError(t, err)
			assert.Equal(t, tc.want, out.Text)
			got, err := os.ReadFile(filepath.Join(dir, tc.path))
			require.NoError(t, err)
			assert.Equal(t, tc.content, string(got))
		})
	}
}

func TestWriteRejectsInvalidArgs(t *testing.T) {
	tool, dir := newWriteTool(t)

	for _, tc := range []struct {
		name, args string
	}{
		{"bad json", `{"file_path":`},
		{"trailing data", `{"file_path":"a.txt","content":"x"} {}`},
		{"unknown field", `{"file_path":"a.txt","content":"x","mode":"0644"}`},
		{"missing path", `{"content":"x"}`},
		{"empty path", `{"file_path":"","content":"x"}`},
		{"missing content", `{"file_path":"a.txt"}`},
		{"content wrong type", `{"file_path":"a.txt","content":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Run(context.Background(), json.RawMessage(tc.args), allowTarget)
			require.ErrorIs(t, err, ErrInvalidArgs)
		})
	}
	assert.Empty(t, listDir(t, dir))
}

func TestWriteDisplayTitle(t *testing.T) {
	tool, _ := newWriteTool(t)

	assert.Equal(t, "Write cmd/root.go", tool.Display.Title(json.RawMessage(`{"file_path":"cmd/root.go"}`)))
	assert.Equal(t, "Write", tool.Display.Title(json.RawMessage(`{"file_path":`)))
}

func TestWriteDisplayDone(t *testing.T) {
	tool, _ := newWriteTool(t)

	assert.Equal(t, "Wrote cmd/root.go", tool.Display.Done(json.RawMessage(`{"file_path":"cmd/root.go"}`)))
	assert.Equal(t, "Wrote", tool.Display.Done(json.RawMessage(`{"file_path":`)))
	assert.Nil(t, tool.Display.Summary)
	assert.Equal(t, "cmd/root.go", tool.Display.Subject(json.RawMessage(`{"file_path":"cmd/root.go"}`), ""))
}

func TestWriteDisplayChange(t *testing.T) {
	tool, _ := newWriteTool(t)

	summary, lines := tool.Display.Change(json.RawMessage(`{"file_path":"a.txt","content":"one\ntwo\n"}`))
	assert.Equal(t, "+2", summary)
	assert.Equal(t, []PreviewLine{{Kind: Added, Text: "one"}, {Kind: Added, Text: "two"}}, lines)

	args, err := json.Marshal(map[string]string{"file_path": "a.txt", "content": numberedLines(13) + strings.Repeat("é", maxLineChars+5)})
	require.NoError(t, err)
	summary, lines = tool.Display.Change(args)
	assert.Equal(t, "+14", summary)
	require.Len(t, lines, 14)
	assert.Equal(t, PreviewLine{Kind: Added, Text: "line 13"}, lines[12])
	assert.Equal(t, strings.Repeat("é", maxLineChars)+lineCutMarker, lines[13].Text)

	summary, lines = tool.Display.Change(json.RawMessage(`{"file_path":"a.txt","content":""}`))
	assert.Equal(t, "+0", summary)
	assert.Empty(t, lines)
}

func TestWriteDisplayPreview(t *testing.T) {
	tool, _ := newWriteTool(t)

	assert.Equal(t, []PreviewLine{
		{Kind: Context, Text: "2 lines"},
		{Kind: Added, Text: "one"},
		{Kind: Added, Text: "two"},
	}, tool.Display.Preview(json.RawMessage(`{"file_path":"a.txt","content":"one\ntwo\n"}`)))
	assert.Equal(t, []PreviewLine{{Kind: Context, Text: "0 lines"}},
		tool.Display.Preview(json.RawMessage(`{"file_path":"a.txt","content":""}`)))

	args, err := json.Marshal(map[string]string{"file_path": "a.txt", "content": numberedLines(13)})
	require.NoError(t, err)
	preview := tool.Display.Preview(args)
	require.Len(t, preview, 14)
	assert.Equal(t, PreviewLine{Kind: Context, Text: "13 lines"}, preview[0])
	for i, l := range preview[1:] {
		assert.Equal(t, PreviewLine{Kind: Added, Text: fmt.Sprintf("line %d", i+1)}, l)
	}
}

func TestWriteTarget(t *testing.T) {
	tool, dir := newWriteTool(t)

	got, err := tool.Target(json.RawMessage(`{"file_path":"new/a.txt","content":"x"}`))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "new", "a.txt"), got.Path)
	_, err = tool.Target(json.RawMessage(`{}`))
	require.ErrorIs(t, err, ErrInvalidArgs)
}

func TestWriteStopsWhenCancelled(t *testing.T) {
	tool, dir := newWriteTool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tool.Run(ctx, json.RawMessage(`{"file_path":"sub/a.txt","content":"x"}`), allowTarget)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, listDir(t, dir))
}

func TestWriteRevokedBeforeRunLeavesWorkspaceUnchanged(t *testing.T) {
	tool, dir := newWriteTool(t)
	authority := policy.NewAuthority()
	authority.EnterExecute()
	gate := policy.NewGate(authority, map[string]policy.ToolSpec{
		tool.Name: {Capability: tool.Capability, Target: tool.Target},
	}, nil, func(context.Context, policy.Request) (policy.Decision, error) {
		return policy.Decision{Answer: policy.Allow}, nil
	}, nil)
	args := json.RawMessage(`{"file_path":"missing/deep/file","content":"x"}`)
	require.Empty(t, listDir(t, dir))
	require.NoError(t, gate.Before(t.Context(), "call-1", tool.Name, args))
	defer gate.Finish("call-1")
	authority.EnterPlan()

	admitted := false
	_, err := tool.Run(t.Context(), args, func(target policy.Target) (policy.Mode, error) {
		admitted = true
		return gate.Admit("call-1", tool.Name, args, &target)
	})
	assert.True(t, admitted, "Run must reach the admission callback")
	assert.ErrorIs(t, err, policy.ErrRevoked)
	assert.Empty(t, listDir(t, dir))
}

func TestWriteAndEditLongBasename(t *testing.T) {
	dir := t.TempDir()
	name := strings.Repeat("b", 240)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("probe"), 0o600); errors.Is(err, syscall.ENAMETOOLONG) {
		t.Skip("filesystem does not support 240-byte basenames")
	} else {
		require.NoError(t, err)
	}
	require.NoError(t, os.Remove(path))
	ws := newWorkspace(t, dir, nil)
	write := NewWrite(ws)
	for _, content := range []string{"created", "replaced"} {
		raw, err := json.Marshal(map[string]string{"file_path": name, "content": content})
		require.NoError(t, err)
		_, err = write.Run(t.Context(), raw, allowTarget)
		require.NoError(t, err)
		assert.Equal(t, content, readContent(t, path))
		require.NoError(t, os.Chmod(path, 0o640))
	}
	_, err := runEdit(t, NewEdit(ws), name, "replaced", "edited", false)
	require.NoError(t, err)
	assert.Equal(t, "edited", readContent(t, path))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o640), info.Mode().Perm())
	assert.Equal(t, []string{name}, listDir(t, dir))
}

func TestPreviewLineBoundaries(t *testing.T) {
	for _, text := range []string{"", "\n", "\n\n", "x\n\n", "a\r\nb\r\n", numberedLines(9), numberedLines(10), numberedLines(11), strings.Repeat("\n", 100000)} {
		t.Run(fmt.Sprintf("%d bytes", len(text)), func(t *testing.T) {
			lines := []string(nil)
			if text != "" {
				lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
			}
			assert.Equal(t, len(lines), countLines(text))
			for _, kind := range []PreviewKind{Added, Removed} {
				preview := previewLines(kind, text)
				require.Len(t, preview, len(lines))
				for i := range lines {
					assert.Equal(t, PreviewLine{Kind: kind, Text: lines[i]}, preview[i])
				}
			}
			writeRaw, err := json.Marshal(map[string]string{"content": text})
			require.NoError(t, err)
			write := writePreview(writeRaw)
			assert.Equal(t, PreviewLine{Kind: Context, Text: lineCount(len(lines))}, write[0])
			assert.Equal(t, previewLines(Added, text), write[1:])
			editRaw, err := json.Marshal(map[string]string{"old_string": text, "new_string": text})
			require.NoError(t, err)
			assert.Equal(t, append(previewLines(Removed, text), previewLines(Added, text)...), editPreview(editRaw))
		})
	}
	assert.Equal(t, []PreviewLine{{Kind: Context, Text: "0 lines"}}, writePreview(json.RawMessage(`{"content":`)))
	assert.Empty(t, editPreview(json.RawMessage(`{"old_string":`)))
}

func TestWriteDisplayOutcome(t *testing.T) {
	ws, plan, _ := newPlanWorkspace(t, t.TempDir())
	d := NewWrite(ws).Display

	done, action := d.Outcome(json.RawMessage(`{"file_path":`+strconv.Quote(plan)+`}`), "")
	assert.Empty(t, done)
	assert.Equal(t, Action{Verb: "Wrote", One: "the plan"}, action)
	_, action = d.Outcome(json.RawMessage(`{"file_path":"a.go"}`), "")
	assert.Equal(t, Action{}, action)
	_, action = d.Outcome(json.RawMessage(`{"file_path":"plan"}`), "")
	assert.Equal(t, Action{}, action)
}

func TestWriteReachesAnAddedDirectory(t *testing.T) {
	_, dir := newWriteTool(t)
	ws := newWorkspace(t, dir, nil)
	other := t.TempDir()
	path := filepath.Join(other, "sub", "a.txt")
	args := fmt.Sprintf(`{"file_path":%q,"content":"hi"}`, path)
	tool := NewWrite(ws)

	_, err := tool.Run(context.Background(), json.RawMessage(args), allowTarget)
	require.ErrorIs(t, err, ErrOutsideWorkspace)
	require.NoError(t, ws.Add(other, realPath(t, other)))

	var admitted policy.Target
	res, err := tool.Run(context.Background(), json.RawMessage(args), func(t policy.Target) (policy.Mode, error) {
		admitted = t
		return policy.Execute, nil
	})
	require.NoError(t, err)
	assert.Equal(t, "wrote 1 line to "+path, res.Text)
	assert.Equal(t, "hi", readContent(t, path))
	before, err := tool.Target(json.RawMessage(args))
	require.NoError(t, err)
	assert.Equal(t, before, admitted)
}
