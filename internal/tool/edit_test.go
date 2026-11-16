package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

func newEditTool(t *testing.T) (Tool, string) {
	t.Helper()
	dir := t.TempDir()
	return NewEdit(newWorkspace(t, dir, nil)), dir
}

func runEdit(t *testing.T, tool Tool, path, oldString, newString string, replaceAll bool) (string, error) {
	t.Helper()
	args, err := json.Marshal(map[string]any{
		"file_path": path, "old_string": oldString, "new_string": newString, "replace_all": replaceAll,
	})
	require.NoError(t, err)
	return tool.Run(context.Background(), args)
}

func readContent(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func TestEditDescribesItself(t *testing.T) {
	tool, _ := newEditTool(t)

	assert.Equal(t, "Edit", tool.Name)
	assert.Equal(t, policy.Write, tool.Capability)
	assert.Equal(t, []string{"file_path", "old_string", "new_string"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	assert.Equal(t, Action{Verb: "Edited", One: "file", Many: "files"}, tool.Display.Action)
	assert.Equal(t, "Edit a.txt", tool.Display.Title(json.RawMessage(`{"file_path":"a.txt"}`)))
	_, err := json.Marshal(tool.Parameters)
	require.NoError(t, err)
}

func TestEditReplacesSingleMatch(t *testing.T) {
	tool, dir := newEditTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha\nbeta\ngamma\n")

	out, err := runEdit(t, tool, "a.txt", "beta", "BETA", false)
	require.NoError(t, err)
	assert.Equal(t, "replaced 1 occurrence in a.txt", out)
	assert.Equal(t, "1 occurrence", tool.Display.Summary(out))
	assert.Equal(t, "alpha\nBETA\ngamma\n", readContent(t, filepath.Join(dir, "a.txt")))
}

func TestEditDeletesWithEmptyNewString(t *testing.T) {
	tool, dir := newEditTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha\nbeta\n")

	_, err := runEdit(t, tool, "a.txt", "beta\n", "", false)
	require.NoError(t, err)
	assert.Equal(t, "alpha\n", readContent(t, filepath.Join(dir, "a.txt")))
}

func TestEditRefusesAmbiguousMatchWithoutReplaceAll(t *testing.T) {
	tool, dir := newEditTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "x x x")

	_, err := runEdit(t, tool, "a.txt", "x", "y", false)
	require.ErrorIs(t, err, ErrAmbiguousMatch)
	assert.Contains(t, err.Error(), "3 matches")
	assert.Equal(t, "x x x", readContent(t, filepath.Join(dir, "a.txt")))
}

func TestEditReplacesAll(t *testing.T) {
	tool, dir := newEditTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "x x x")

	out, err := runEdit(t, tool, "a.txt", "x", "y", true)
	require.NoError(t, err)
	assert.Equal(t, "replaced 3 occurrences in a.txt", out)
	assert.Equal(t, "3 occurrences", tool.Display.Summary(out))
	assert.Equal(t, "y y y", readContent(t, filepath.Join(dir, "a.txt")))
}

func TestEditRefusesUneditableTargets(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	require.NoError(t, os.Mkdir(filepath.Join(home, "sub"), 0o755))
	writeFile(t, filepath.Join(home, "bin"), "ELF\x00x")
	writeFile(t, filepath.Join(home, "big.txt"), "x"+strings.Repeat(" ", maxEditBytes))
	writeFile(t, filepath.Join(home, "a.txt"), "alpha")
	tool := NewEdit(newWorkspace(t, home, protectedStores(home, kopiHome)))

	for _, tc := range []struct {
		name, path, oldString string
		want                  error
	}{
		{"not found", "a.txt", "omega", ErrNoMatch},
		{"binary", "bin", "x", ErrBinary},
		{"over size cap", "big.txt", "x", ErrTooLarge},
		{"protected", ".ssh/id_x", "private", ErrProtected},
		{"directory", "sub", "x", ErrIsDirectory},
		{"missing", "missing.txt", "x", fs.ErrNotExist},
		{"outside", "../x", "x", ErrOutsideWorkspace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runEdit(t, tool, tc.path, tc.oldString, "y", false)
			require.ErrorIs(t, err, tc.want)
			assert.Empty(t, out)
		})
	}
	assert.Equal(t, "alpha", readContent(t, filepath.Join(home, "a.txt")))
	assert.Equal(t, "private key", readContent(t, filepath.Join(home, ".ssh", "id_x")))
}

func TestEditRejectsInvalidArgs(t *testing.T) {
	tool, dir := newEditTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha")

	for _, tc := range []struct {
		name, args string
	}{
		{"bad json", `{"file_path":`},
		{"trailing data", `{"file_path":"a.txt","old_string":"a","new_string":"b"} {}`},
		{"unknown field", `{"file_path":"a.txt","old_string":"a","new_string":"b","count":1}`},
		{"missing path", `{"old_string":"a","new_string":"b"}`},
		{"missing old_string", `{"file_path":"a.txt","new_string":"b"}`},
		{"empty old_string", `{"file_path":"a.txt","old_string":"","new_string":"b"}`},
		{"missing new_string", `{"file_path":"a.txt","old_string":"a"}`},
		{"identical strings", `{"file_path":"a.txt","old_string":"alpha","new_string":"alpha"}`},
		{"replace_all wrong type", `{"file_path":"a.txt","old_string":"a","new_string":"b","replace_all":"yes"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Run(context.Background(), json.RawMessage(tc.args))
			require.ErrorIs(t, err, ErrInvalidArgs)
		})
	}
	assert.Equal(t, "alpha", readContent(t, filepath.Join(dir, "a.txt")))
}

func TestEditStopsWhenCancelled(t *testing.T) {
	tool, dir := newEditTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tool.Run(ctx, json.RawMessage(`{"file_path":"a.txt","old_string":"alpha","new_string":"beta"}`))
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "alpha", readContent(t, filepath.Join(dir, "a.txt")))
}

func TestEditDisplayPreview(t *testing.T) {
	tool, _ := newEditTool(t)

	assert.Equal(t, []PreviewLine{
		{Kind: Removed, Text: "a"},
		{Kind: Removed, Text: "b"},
		{Kind: Added, Text: "c"},
	}, tool.Display.Preview(json.RawMessage(`{"file_path":"a.txt","old_string":"a\nb","new_string":"c"}`)))

	args, err := json.Marshal(map[string]string{"old_string": numberedLines(11), "new_string": numberedLines(12)})
	require.NoError(t, err)
	preview := tool.Display.Preview(args)
	require.Len(t, preview, 22)
	for i := range 10 {
		assert.Equal(t, PreviewLine{Kind: Removed, Text: fmt.Sprintf("line %d", i+1)}, preview[i])
		assert.Equal(t, PreviewLine{Kind: Added, Text: fmt.Sprintf("line %d", i+1)}, preview[11+i])
	}
	assert.Equal(t, PreviewLine{Kind: Context, Text: "… 1 more line"}, preview[10])
	assert.Equal(t, PreviewLine{Kind: Context, Text: "… 2 more lines"}, preview[21])
}

func TestEditTarget(t *testing.T) {
	tool, dir := newEditTool(t)

	got, err := tool.Target(json.RawMessage(`{"file_path":"a.txt"}`))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "a.txt"), got.Path)
}
