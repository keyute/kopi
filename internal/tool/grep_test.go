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

func newGrepTool(t *testing.T) (Tool, string) {
	t.Helper()
	dir := t.TempDir()
	return NewGrep(newWorkspace(t, dir, nil), nil), dir
}

func runGrep(t *testing.T, tool Tool, args string) (string, error) {
	t.Helper()
	res, err := tool.Run(context.Background(), json.RawMessage(args), allowTarget)
	return res.Text, err
}

// setGrepModTime gives path a modification time age before a fixed instant.
func setGrepModTime(t *testing.T, path string, age time.Duration) {
	t.Helper()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(-age)
	require.NoError(t, os.Chtimes(path, when, when))
}

func TestGrepDescribesItself(t *testing.T) {
	tool, _ := newGrepTool(t)

	assert.Equal(t, "Grep", tool.Name)
	assert.Equal(t, policy.Read, tool.Capability)
	assert.Equal(t, "object", tool.Parameters["type"])
	assert.Equal(t, []string{"pattern"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	_, err := json.Marshal(tool.Parameters)
	require.NoError(t, err)
}

func TestGrepFilesWithMatchesNewestFirst(t *testing.T) {
	tool, dir := newGrepTool(t)
	for name, age := range map[string]time.Duration{"old.txt": 3 * time.Hour, "sub/new.txt": time.Hour, "mid.txt": 2 * time.Hour} {
		writeFile(t, filepath.Join(dir, name), "a\nneedle\n")
		setGrepModTime(t, filepath.Join(dir, name), age)
	}
	writeFile(t, filepath.Join(dir, "other.txt"), "nothing here\n")

	out, err := runGrep(t, tool, `{"pattern":"need+le"}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 3 files\nsub/new.txt\nmid.txt\nold.txt\n", out)
}

func TestGrepContentWithContext(t *testing.T) {
	tool, dir := newGrepTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "one\nmatch A\ntwo\nthree\nfour\nmatch B\nmatch C\nfive\nsix\n")
	writeFile(t, filepath.Join(dir, "b.txt"), "match D\n")

	out, err := runGrep(t, tool, `{"pattern":"match","output_mode":"content","context":1}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 4 matches in 2 files\n"+
		"a.txt-1-one\n"+
		"a.txt:2:match A\n"+
		"a.txt-3-two\n"+
		"--\n"+
		"a.txt-5-four\n"+
		"a.txt:6:match B\n"+
		"a.txt:7:match C\n"+
		"a.txt-8-five\n"+
		"--\n"+
		"b.txt:1:match D\n", out)

	out, err = runGrep(t, tool, `{"pattern":"match [AD]","output_mode":"content"}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 2 matches in 2 files\na.txt:2:match A\nb.txt:1:match D\n", out)
}

func TestGrepContentMarksCutLines(t *testing.T) {
	tool, dir := newGrepTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "match"+strings.Repeat("x", maxLineChars)+"\n")

	out, err := runGrep(t, tool, `{"pattern":"match","output_mode":"content"}`)
	require.NoError(t, err)
	want := "Found 1 match in 1 file\na.txt:1:match" + strings.Repeat("x", maxLineChars-len("match")) + lineCutMarker + "\n"
	assert.Equal(t, want, out)
}

func TestGrepCount(t *testing.T) {
	tool, dir := newGrepTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "x\nx\nx\ny\n")
	setGrepModTime(t, filepath.Join(dir, "a.txt"), time.Hour)
	writeFile(t, filepath.Join(dir, "b.txt"), "y\nx\n")
	setGrepModTime(t, filepath.Join(dir, "b.txt"), 2*time.Hour)
	writeFile(t, filepath.Join(dir, "c.txt"), "y\n")

	out, err := runGrep(t, tool, `{"pattern":"x","output_mode":"count"}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 4 matches in 2 files\na.txt:3\nb.txt:1\n", out)

	out, err = runGrep(t, tool, `{"pattern":"x","output_mode":"count","limit":1}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 4 matches in 2 files\na.txt:3\n"+
		"[Showing 1 of 2 files, most recently modified first; a narrower path, glob or pattern lists the others.]\n", out)

	out, err = runGrep(t, tool, `{"pattern":"x","output_mode":"count","path":"b.txt"}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 match in 1 file\nb.txt:1\n", out)
}

func TestGrepCaseInsensitive(t *testing.T) {
	tool, dir := newGrepTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "NEEDLE\n")

	out, err := runGrep(t, tool, `{"pattern":"needle"}`)
	require.NoError(t, err)
	assert.Equal(t, "No matches found", out)

	out, err = runGrep(t, tool, `{"pattern":"needle","case_insensitive":true}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 file\na.txt\n", out)
}

func TestGrepGlobFilter(t *testing.T) {
	tool, dir := newGrepTool(t)
	for i, name := range []string{"src/a.go", "src/deep/b.go", "c.md", "src/d.ts"} {
		writeFile(t, filepath.Join(dir, name), "needle\n")
		setGrepModTime(t, filepath.Join(dir, name), time.Duration(i)*time.Hour)
	}

	for _, tc := range []struct {
		name, args, want string
	}{
		{"name at any depth", `{"pattern":"needle","glob":"*.go"}`, "Found 2 files\nsrc/a.go\nsrc/deep/b.go\n"},
		{"path pattern", `{"pattern":"needle","glob":"src/*.go"}`, "Found 1 file\nsrc/a.go\n"},
		{"relative to path", `{"pattern":"needle","path":"src","glob":"deep/*.go"}`, "Found 1 file\nsrc/deep/b.go\n"},
		{"no file matches", `{"pattern":"needle","glob":"*.rs"}`, "No matches found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runGrep(t, tool, tc.args)
			require.NoError(t, err)
			assert.Equal(t, tc.want, out)
		})
	}
}

func TestGrepSkipsBinarySymlinksAndHidden(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	writeFile(t, filepath.Join(home, "a.txt"), "needle\n")
	writeFile(t, filepath.Join(home, "bin"), "needle\x00\n")
	require.NoError(t, os.Symlink("a.txt", filepath.Join(home, "link")))
	writeFile(t, filepath.Join(home, ".gitignore"), "ignored.txt\n")
	writeFile(t, filepath.Join(home, "ignored.txt"), "needle\n")
	writeFile(t, filepath.Join(home, ".ssh", "notes"), "needle\n")
	writeFile(t, filepath.Join(kopiHome, "notes"), "needle\n")
	writeFile(t, filepath.Join(home, "hidden.txt"), "needle\n")
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))
	type call struct {
		tool    string
		root, t policy.Target
	}
	var calls []call
	tool := NewGrep(ws, func(name string, root, t policy.Target) bool {
		calls = append(calls, call{name, root, t})
		return filepath.Base(t.Real) == "hidden.txt"
	})

	for _, mode := range []string{"files_with_matches", "content", "count"} {
		t.Run(mode, func(t *testing.T) {
			calls = nil
			out, err := runGrep(t, tool, fmt.Sprintf(`{"pattern":"needle","output_mode":%q}`, mode))
			require.NoError(t, err)
			want := map[string]string{
				"files_with_matches": "Found 1 file\na.txt\n",
				"content":            "Found 1 match in 1 file\na.txt:1:needle\n",
				"count":              "Found 1 match in 1 file\na.txt:1\n",
			}[mode]
			assert.Equal(t, want, out)
			root := policy.Target{Path: ws.given, Real: ws.realDir}
			assert.Contains(t, calls, call{"Grep", root, policy.Target{
				Path: filepath.Join(ws.given, "hidden.txt"),
				Real: filepath.Join(ws.realDir, "hidden.txt"),
			}})
		})
	}
}

func TestGrepSearchesSingleFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), "sub/\n")
	writeFile(t, filepath.Join(dir, "sub", "a.txt"), "x\nneedle\n")
	writeFile(t, filepath.Join(dir, "sub", "b.txt"), "needle\n")
	ws := newWorkspace(t, dir, nil)
	tool := NewGrep(ws, func(string, policy.Target, policy.Target) bool { return true })

	out, err := runGrep(t, tool, `{"pattern":"needle","path":"sub/a.txt","glob":"*.md","output_mode":"content"}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 match in 1 file\nsub/a.txt:2:needle\n", out)

	out, err = runGrep(t, tool, fmt.Sprintf(`{"pattern":"needle","path":%q}`, filepath.Join(dir, "sub", "a.txt")))
	require.NoError(t, err)
	assert.Equal(t, "Found 1 file\nsub/a.txt\n", out)
}

func TestGrepTruncatesAtLimit(t *testing.T) {
	tool, dir := newGrepTool(t)
	for i := range 5 {
		name := fmt.Sprintf("f%d.txt", i)
		writeFile(t, filepath.Join(dir, name), "needle\nneedle\n")
		setGrepModTime(t, filepath.Join(dir, name), time.Duration(i)*time.Hour)
	}

	out, err := runGrep(t, tool, `{"pattern":"needle","limit":2}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 5 files\nf0.txt\nf1.txt\n"+
		"[Showing 2 of 5 files, most recently modified first; a narrower path, glob or pattern lists the others.]\n", out)

	out, err = runGrep(t, tool, `{"pattern":"needle","output_mode":"content","limit":3}`)
	require.NoError(t, err)
	assert.Equal(t, "Found 3 matches in 2 files\nf0.txt:1:needle\nf0.txt:2:needle\nf1.txt:1:needle\n"+
		"[Showing 3 matching lines; more match. A narrower path, glob or pattern, or a higher limit, lists them.]\n", out)

	out, err = runGrep(t, tool, `{"pattern":"needle","output_mode":"content","limit":10}`)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "Found 10 matches in 5 files\n"))
	assert.NotContains(t, out, "higher limit")
}

func TestGrepStopsAtByteCap(t *testing.T) {
	tool, dir := newGrepTool(t)
	line := "needle" + strings.Repeat("y", 1000)
	writeFile(t, filepath.Join(dir, "a.txt"), strings.Repeat(line+"\n", 500))

	out, err := runGrep(t, tool, `{"pattern":"needle","output_mode":"content","limit":1000}`)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(out), maxReadBytes)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	shown := len(lines) - 2
	assert.Less(t, shown, 500)
	assert.Equal(t, fmt.Sprintf("Found %d matches in 1 file", shown), lines[0])
	assert.Equal(t, fmt.Sprintf("[Showing %d matching lines; more match. A narrower path, glob or pattern lists them.]", shown), lines[len(lines)-1])
}

func TestGrepNoticeOmitsHigherLimitAtMaximum(t *testing.T) {
	tool, dir := newGrepTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), strings.Repeat("needle\n", 1001))

	out, err := runGrep(t, tool, `{"pattern":"needle","output_mode":"content","limit":1000}`)
	require.NoError(t, err)
	assert.Contains(t, out, "[Showing 1000 matching lines; more match. A narrower path, glob or pattern lists them.]")
	assert.NotContains(t, out, "higher limit")
}

func TestGrepRejectsInvalidArgs(t *testing.T) {
	tool, dir := newGrepTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "needle\n")

	for _, tc := range []struct {
		name, args string
	}{
		{"missing pattern", `{}`},
		{"empty pattern", `{"pattern":""}`},
		{"bad regex", `{"pattern":"a("}`},
		{"bad glob", `{"pattern":"a","glob":"[a"}`},
		{"unknown output_mode", `{"pattern":"a","output_mode":"lines"}`},
		{"negative context", `{"pattern":"a","context":-1}`},
		{"zero limit", `{"pattern":"a","limit":0}`},
		{"unknown field", `{"pattern":"a","file_path":"a.txt"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runGrep(t, tool, tc.args)
			require.ErrorIs(t, err, ErrInvalidArgs)
		})
	}
}

func TestGrepStopsWhenCancelled(t *testing.T) {
	tool, dir := newGrepTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "needle\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tool.Run(ctx, json.RawMessage(`{"pattern":"needle"}`), allowTarget)
	require.ErrorIs(t, err, context.Canceled)
}

func TestGrepSearchesAdmittedDirectoryAfterRetarget(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "allowed", "file"), "needle alpha\n")
	writeFile(t, filepath.Join(dir, "denied", "file"), "needle secret\n")
	ws := newWorkspace(t, dir, nil)
	tool := NewGrep(ws, nil)

	res, err := tool.Run(t.Context(), json.RawMessage(`{"pattern":"needle","path":"allowed","output_mode":"content"}`), func(target policy.Target) (policy.Mode, error) {
		assert.Equal(t, filepath.Join(ws.realDir, "allowed"), target.Real)
		require.NoError(t, os.Rename(filepath.Join(dir, "allowed"), filepath.Join(dir, "pinned")))
		require.NoError(t, os.Symlink("denied", filepath.Join(dir, "allowed")))
		return policy.Execute, nil
	})
	if err == nil {
		assert.NotContains(t, res.Text, "secret")
	}
}

func TestGrepDoesNotReadBeforeAdmission(t *testing.T) {
	tool, dir := newGrepTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "needle\n")

	res, err := tool.Run(t.Context(), json.RawMessage(`{"pattern":"needle"}`), func(policy.Target) (policy.Mode, error) {
		return policy.Plan, policy.ErrDenied
	})
	require.ErrorIs(t, err, policy.ErrDenied)
	assert.Empty(t, res.Text)
}

func TestGrepDisplay(t *testing.T) {
	tool, dir := newGrepTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "x\nx\n")
	writeFile(t, filepath.Join(dir, "b.txt"), "x\n")

	assert.Equal(t, Action{Verb: "Searched for", One: "pattern", Many: "patterns"}, tool.Display.Action)
	assert.Equal(t, "Grep x in sub", tool.Display.Title(json.RawMessage(`{"pattern":"x","path":"sub"}`)))
	assert.Equal(t, "Grep x", tool.Display.Title(json.RawMessage(`{"pattern":"x"}`)))
	for _, tc := range []struct {
		name, args, want string
	}{
		{"files", `{"pattern":"x"}`, "2 files"},
		{"content", `{"pattern":"x","output_mode":"content"}`, "3 matches in 2 files"},
		{"count", `{"pattern":"x","output_mode":"count","path":"b.txt"}`, "1 match in 1 file"},
		{"no matches", `{"pattern":"z"}`, "no matches"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runGrep(t, tool, tc.args)
			require.NoError(t, err)
			assert.Equal(t, tc.want, tool.Display.Summary(out))
		})
	}
}
