package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

func newLSTool(t *testing.T) (Tool, string) {
	t.Helper()
	dir := t.TempDir()
	return NewLS(newWorkspace(t, dir, nil), nil), dir
}

func runLS(t *testing.T, tool Tool, args string) (string, error) {
	t.Helper()
	res, err := tool.Run(context.Background(), json.RawMessage(args), allowTarget)
	return res.Text, err
}

func TestLSDescribesItself(t *testing.T) {
	tool, _ := newLSTool(t)

	assert.Equal(t, "LS", tool.Name)
	assert.Equal(t, "LS", tool.Label)
	assert.Equal(t, policy.Read, tool.Capability)
	assert.Equal(t, "object", tool.Parameters["type"])
	assert.NotContains(t, tool.Parameters, "required")
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	_, err := json.Marshal(tool.Parameters)
	require.NoError(t, err)
}

func TestLSListsOneDirectorySorted(t *testing.T) {
	tool, dir := newLSTool(t)
	writeFile(t, filepath.Join(dir, ".gitignore"), "ignored.log\n")
	writeFile(t, filepath.Join(dir, "ignored.log"), "")
	writeFile(t, filepath.Join(dir, ".git", "config"), "")
	writeFile(t, filepath.Join(dir, "b.txt"), "")
	writeFile(t, filepath.Join(dir, "a", "nested.txt"), "")

	out, err := runLS(t, tool, `{}`)
	require.NoError(t, err)
	assert.Equal(t, ".git/\n.gitignore\na/\nb.txt\nignored.log", out)

	out, err = runLS(t, tool, `{"path":"a"}`)
	require.NoError(t, err)
	assert.Equal(t, "nested.txt", out)
}

func TestLSShowsSymlinksWithoutFollowing(t *testing.T) {
	tool, dir := newLSTool(t)
	writeFile(t, filepath.Join(dir, "real", "inside.txt"), "")
	require.NoError(t, os.Symlink("real", filepath.Join(dir, "dirlink")))
	require.NoError(t, os.Symlink("missing", filepath.Join(dir, "dangling")))
	require.NoError(t, os.Symlink("/", filepath.Join(dir, "outside")))

	out, err := runLS(t, tool, `{}`)
	require.NoError(t, err)
	assert.Equal(t, "dangling\ndirlink\noutside\nreal/", out)
}

func TestLSOmitsUnresolvableSymlinks(t *testing.T) {
	tool, dir := newLSTool(t)
	writeFile(t, filepath.Join(dir, "f.txt"), "")
	require.NoError(t, os.Symlink("loop", filepath.Join(dir, "loop")))
	require.NoError(t, os.Symlink("f.txt/x", filepath.Join(dir, "bad")))

	out, err := runLS(t, tool, `{}`)
	require.NoError(t, err)
	assert.Equal(t, "f.txt", out)
}

func TestLSOmitsProtectedAndHidden(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	writeFile(t, filepath.Join(home, "notes.txt"), "")
	writeFile(t, filepath.Join(home, "secret.txt"), "")
	require.NoError(t, os.Symlink("secret.txt", filepath.Join(home, "alias")))
	realHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	secret := filepath.Join(realHome, "secret.txt")
	hidden, calls := recordHidden(func(t policy.Target) bool { return t.Real == secret })
	tool := NewLS(newWorkspace(t, home, protectedStores(home, kopiHome)), hidden)

	out, err := runLS(t, tool, `{}`)
	require.NoError(t, err)
	assert.Equal(t, ".env\nnotes.txt", out)

	root := policy.Target{Path: home, Real: realHome}
	assert.Contains(t, *calls, hiddenCall{tool: "LS", root: root, t: policy.Target{Path: filepath.Join(home, "alias"), Real: secret}})
	for _, c := range *calls {
		assert.Equal(t, "LS", c.tool)
		assert.Equal(t, root, c.root)
	}
}

func TestLSRefusesDirectoryThatBecameStoreDuringApproval(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	writeFile(t, filepath.Join(home, "a", "key"), "secret")
	store := filepath.Join(t.TempDir(), "store")
	tool := NewLS(newWorkspace(t, home, append(protectedStores(home, kopiHome), store)), nil)

	res, err := tool.Run(context.Background(), json.RawMessage(`{"path":"a"}`), func(policy.Target) (policy.Mode, error) {
		require.NoError(t, os.Symlink(filepath.Join(home, "a"), store))
		return policy.Execute, nil
	})
	require.ErrorIs(t, err, ErrProtected)
	assert.Empty(t, res.Text)
}

func TestLSTruncatesAt500(t *testing.T) {
	tool, dir := newLSTool(t)
	for i := range maxLSEntries + 10 {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%03d", i)), "")
	}

	out, err := runLS(t, tool, `{}`)
	require.NoError(t, err)
	lines := strings.Split(out, "\n")
	require.Len(t, lines, maxLSEntries+1)
	assert.Equal(t, "f499", lines[maxLSEntries-1])
	assert.Equal(t, "[Showing 500 of 510 entries.]", lines[maxLSEntries])
}

func TestLSEmptyDirectory(t *testing.T) {
	tool, dir := newLSTool(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "empty"), 0o755))

	out, err := runLS(t, tool, `{"path":"empty"}`)
	require.NoError(t, err)
	assert.Equal(t, "(empty directory)", out)
}

func TestLSRefusesFile(t *testing.T) {
	tool, dir := newLSTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "")

	out, err := runLS(t, tool, `{"path":"a.txt"}`)
	require.ErrorIs(t, err, ErrNotDirectory)
	assert.Empty(t, out)
}

func TestLSRejectsInvalidArgs(t *testing.T) {
	tool, _ := newLSTool(t)

	for _, tc := range []struct {
		name, args string
	}{
		{"unknown field", `{"path":".","pattern":"x"}`},
		{"path wrong type", `{"path":1}`},
		{"trailing data", `{} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runLS(t, tool, tc.args)
			require.ErrorIs(t, err, ErrInvalidArgs)
		})
	}
}

func TestLSDisplay(t *testing.T) {
	tool, dir := newLSTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "")
	writeFile(t, filepath.Join(dir, "one", "b.txt"), "")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "empty"), 0o755))
	for i := range maxLSEntries + 1 {
		writeFile(t, filepath.Join(dir, "many", fmt.Sprintf("f%03d", i)), "")
	}

	assert.Equal(t, Action{Verb: "Listed", One: "directory", Many: "directories"}, tool.Display.Action)
	assert.Equal(t, "LS internal", tool.Display.Title(json.RawMessage(`{"path":"internal"}`)))
	assert.Equal(t, "LS", tool.Display.Title(json.RawMessage(`{}`)))
	assert.Equal(t, "LS", tool.Display.Title(json.RawMessage(`{"path":`)))
	assert.Equal(t, "Listed internal", tool.Display.Done(json.RawMessage(`{"path":"internal"}`)))
	assert.Equal(t, "Listed .", tool.Display.Done(json.RawMessage(`{}`)))
	assert.Empty(t, tool.Display.Body(emptyDirNotice))
	assert.Equal(t, "a.txt", tool.Display.Body("a.txt"))
	for _, tc := range []struct {
		args, want string
	}{
		{`{}`, "4 entries"},
		{`{"path":"one"}`, "1 entry"},
		{`{"path":"empty"}`, "empty"},
		{`{"path":"many"}`, "500 entries"},
	} {
		out, err := runLS(t, tool, tc.args)
		require.NoError(t, err)
		assert.Equal(t, tc.want, tool.Display.Summary(out))
	}
}
