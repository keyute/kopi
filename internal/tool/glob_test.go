package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

type hiddenCall struct {
	tool    string
	root, t policy.Target
}

// recordHidden returns a Hidden that hides what hide reports and records every call.
func recordHidden(hide func(policy.Target) bool) (Hidden, *[]hiddenCall) {
	var calls []hiddenCall
	return func(tool string, root, t policy.Target) bool {
		calls = append(calls, hiddenCall{tool: tool, root: root, t: t})
		return hide(t)
	}, &calls
}

func newGlobTool(t *testing.T) (Tool, string) {
	t.Helper()
	dir := t.TempDir()
	return NewGlob(newWorkspace(t, dir, nil), nil), dir
}

func runGlob(t *testing.T, tool Tool, args string) (string, error) {
	t.Helper()
	res, err := tool.Run(context.Background(), json.RawMessage(args), allowTarget)
	return res.Text, err
}

func touch(t *testing.T, path string, mod time.Time) {
	t.Helper()
	require.NoError(t, os.Chtimes(path, mod, mod))
}

func TestGlobDescribesItself(t *testing.T) {
	tool, _ := newGlobTool(t)

	assert.Equal(t, "Glob", tool.Name)
	assert.Equal(t, "Glob", tool.Label)
	assert.Equal(t, policy.Read, tool.Capability)
	assert.Equal(t, "object", tool.Parameters["type"])
	assert.Equal(t, []string{"pattern"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	_, err := json.Marshal(tool.Parameters)
	require.NoError(t, err)
}

func TestGlobMatchesNewestFirst(t *testing.T) {
	tool, dir := newGlobTool(t)
	base := time.Now().Add(-time.Hour)
	for name, minutes := range map[string]int{"old.go": 0, "sub/new.go": 2, "mid.go": 1} {
		writeFile(t, filepath.Join(dir, name), "")
		touch(t, filepath.Join(dir, name), base.Add(time.Duration(minutes)*time.Minute))
	}
	writeFile(t, filepath.Join(dir, "readme.md"), "")

	out, err := runGlob(t, tool, `{"pattern":"**/*.go"}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 3 files\nsub/new.go\nmid.go\nold.go", out)
}

func TestGlobPatternWithoutSlashMatchesAtAnyDepth(t *testing.T) {
	tool, dir := newGlobTool(t)
	writeFile(t, filepath.Join(dir, "a", "b", "deep.go"), "")
	writeFile(t, filepath.Join(dir, "a", "b", "deep.txt"), "")

	out, err := runGlob(t, tool, `{"pattern":"*.go"}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 file\na/b/deep.go", out)
}

func TestGlobSearchesBelowPath(t *testing.T) {
	tool, dir := newGlobTool(t)
	writeFile(t, filepath.Join(dir, "top.go"), "")
	writeFile(t, filepath.Join(dir, "sub", "inner", "b.go"), "")
	writeFile(t, filepath.Join(dir, "inner", "c.go"), "")

	out, err := runGlob(t, tool, `{"pattern":"inner/*.go","path":"sub"}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 file\nsub/inner/b.go", out)

	out, err = runGlob(t, tool, fmt.Sprintf(`{"pattern":"*.go","path":%q}`, filepath.Join(dir, "sub")))
	require.NoError(t, err)
	assert.Equal(t, "Found 1 file\nsub/inner/b.go", out)
}

func TestGlobRespectsGitignore(t *testing.T) {
	tool, dir := newGlobTool(t)
	writeFile(t, filepath.Join(dir, ".gitignore"), "*.log\nbuild/\n")
	writeFile(t, filepath.Join(dir, "a.go"), "")
	writeFile(t, filepath.Join(dir, "b.log"), "")
	writeFile(t, filepath.Join(dir, "build", "c.go"), "")
	writeFile(t, filepath.Join(dir, ".git", "config"), "")

	out, err := runGlob(t, tool, `{"pattern":"*"}`)
	require.NoError(t, err)
	lines := strings.Split(out, "\n")
	assert.Equal(t, "Found 2 files", lines[0])
	assert.ElementsMatch(t, []string{".gitignore", "a.go"}, lines[1:])
}

func TestGlobSearchesExplicitGitPath(t *testing.T) {
	tool, dir := newGlobTool(t)
	writeFile(t, filepath.Join(dir, ".git", "config"), "")

	out, err := runGlob(t, tool, `{"pattern":"*","path":".git"}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 file\n.git/config", out)
}

func TestGlobSkipsSymlinksProtectedAndHidden(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	writeFile(t, filepath.Join(home, "notes.txt"), "")
	writeFile(t, filepath.Join(home, "secret.txt"), "")
	require.NoError(t, os.Symlink("notes.txt", filepath.Join(home, "link.txt")))
	realHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	secret := filepath.Join(realHome, "secret.txt")
	hidden, calls := recordHidden(func(t policy.Target) bool { return t.Real == secret })
	tool := NewGlob(newWorkspace(t, home, protectedStores(home, kopiHome)), hidden)

	out, err := runGlob(t, tool, `{"pattern":"*"}`)
	require.NoError(t, err)
	lines := strings.Split(out, "\n")
	assert.Equal(t, "Found 2 files", lines[0])
	assert.ElementsMatch(t, []string{".env", "notes.txt"}, lines[1:])

	root := policy.Target{Path: home, Real: realHome}
	assert.Contains(t, *calls, hiddenCall{
		tool: "Glob",
		root: root,
		t:    policy.Target{Path: filepath.Join(home, "secret.txt"), Real: secret},
	})
	for _, c := range *calls {
		assert.Equal(t, "Glob", c.tool)
		assert.Equal(t, root, c.root)
	}
}

func TestGlobTruncatesAt100WithNotice(t *testing.T) {
	tool, dir := newGlobTool(t)
	for i := range 150 {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), "")
	}

	out, err := runGlob(t, tool, `{"pattern":"*.txt"}`)
	require.NoError(t, err)
	lines := strings.Split(out, "\n")
	require.Len(t, lines, maxGlobResults+2)
	assert.Equal(t, "Found 150 files", lines[0])
	assert.Equal(t, "[Showing 100 of 150 files, most recently modified first; a narrower path or pattern lists the others.]", lines[len(lines)-1])
}

func TestGlobReportsNoFiles(t *testing.T) {
	tool, dir := newGlobTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "")

	out, err := runGlob(t, tool, `{"pattern":"*.go"}`)
	require.NoError(t, err)
	assert.Equal(t, "No files found", out)
}

func TestGlobRefusesFilePath(t *testing.T) {
	tool, dir := newGlobTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "")

	out, err := runGlob(t, tool, `{"pattern":"*","path":"a.txt"}`)
	require.ErrorIs(t, err, ErrNotDirectory)
	assert.Empty(t, out)
}

func TestGlobRejectsInvalidArgs(t *testing.T) {
	tool, _ := newGlobTool(t)

	for _, tc := range []struct {
		name, args string
	}{
		{"missing pattern", `{}`},
		{"empty pattern", `{"pattern":""}`},
		{"invalid pattern", `{"pattern":"[a"}`},
		{"absolute pattern", `{"pattern":"/etc/*"}`},
		{"unknown field", `{"pattern":"*","glob":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runGlob(t, tool, tc.args)
			require.ErrorIs(t, err, ErrInvalidArgs)
		})
	}
}

func TestGlobDisplay(t *testing.T) {
	tool, dir := newGlobTool(t)
	writeFile(t, filepath.Join(dir, "a.go"), "")
	writeFile(t, filepath.Join(dir, "b.go"), "")

	assert.Equal(t, Action{Verb: "Searched for", One: "pattern", Many: "patterns"}, tool.Display.Action)
	assert.Equal(t, "Glob *.go in sub", tool.Display.Title(json.RawMessage(`{"pattern":"*.go","path":"sub"}`)))
	assert.Equal(t, "Glob", tool.Display.Title(json.RawMessage(`{"pattern":`)))
	assert.Equal(t, "Searched for *.go in sub", tool.Display.Done(json.RawMessage(`{"pattern":"*.go","path":"sub"}`)))
	assert.Equal(t, "Searched for *.go", tool.Display.Done(json.RawMessage(`{"pattern":"*.go"}`)))
	assert.Empty(t, tool.Display.Body("No files found"))
	assert.Equal(t, "Found 1 file\na.go", tool.Display.Body("Found 1 file\na.go"))
	for _, tc := range []struct {
		pattern, want string
	}{
		{"*.go", "2 files"},
		{"a.go", "1 file"},
		{"*.rs", "no files"},
	} {
		out, err := runGlob(t, tool, fmt.Sprintf(`{"pattern":%q}`, tc.pattern))
		require.NoError(t, err)
		assert.Equal(t, tc.want, tool.Display.Summary(out))
	}
}

func TestGlobSearchesAnAddedDirectoryWithAbsoluteResults(t *testing.T) {
	dir := t.TempDir()
	ws := newWorkspace(t, dir, nil)
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "sub", "a.go"), "")
	writeFile(t, filepath.Join(dir, "start.go"), "")
	tool := NewGlob(ws, nil)
	args := fmt.Sprintf(`{"pattern":"*.go","path":%q}`, other)

	_, err := runGlob(t, tool, args)
	require.ErrorIs(t, err, ErrOutsideWorkspace)
	require.NoError(t, ws.Add(other, realPath(t, other)))

	out, err := runGlob(t, tool, args)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 file\n"+filepath.Join(realPath(t, other), "sub", "a.go"), out)
	out, err = runGlob(t, tool, `{"pattern":"*.go"}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 file\nstart.go", out)
}

func TestGlobLeavesKopiHomeOutOfAnAddedDirectory(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	writeFile(t, filepath.Join(home, "a.json"), "")
	ws := newWorkspace(t, t.TempDir(), protectedStores(home, kopiHome))
	require.NoError(t, ws.Add(home, realPath(t, home)))

	out, err := runGlob(t, NewGlob(ws, nil), fmt.Sprintf(`{"pattern":"**/*.json","path":%q}`, home))

	require.NoError(t, err)
	assert.Equal(t, "Found 1 file\n"+filepath.Join(realPath(t, home), "a.json"), out)
}
