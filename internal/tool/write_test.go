package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		name, args, path, content, want, summary string
	}{
		{"lines", `{"file_path":"sub/a.txt","content":"one\ntwo\n"}`, "sub/a.txt", "one\ntwo\n", "wrote 2 lines to sub/a.txt", "2 lines"},
		{"one line", `{"file_path":"b.txt","content":"one"}`, "b.txt", "one", "wrote 1 line to b.txt", "1 line"},
		{"empty", `{"file_path":"c.txt","content":""}`, "c.txt", "", "wrote 0 lines to c.txt", "0 lines"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tool.Run(context.Background(), json.RawMessage(tc.args))
			require.NoError(t, err)
			assert.Equal(t, tc.want, out)
			assert.Equal(t, tc.summary, tool.Display.Summary(out))
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
			_, err := tool.Run(context.Background(), json.RawMessage(tc.args))
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
	require.Len(t, preview, 12)
	assert.Equal(t, PreviewLine{Kind: Context, Text: "13 lines"}, preview[0])
	for i, l := range preview[1:11] {
		assert.Equal(t, PreviewLine{Kind: Added, Text: fmt.Sprintf("line %d", i+1)}, l)
	}
	assert.Equal(t, PreviewLine{Kind: Context, Text: "… 3 more lines"}, preview[11])
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

	_, err := tool.Run(ctx, json.RawMessage(`{"file_path":"sub/a.txt","content":"x"}`))
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, listDir(t, dir))
}
