package tool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

type outputFixture struct {
	kopiHome, outputsDir string
	outputs              *Outputs
	ws, plain            *Workspace
}

// newOutputFixture puts Kopi's home inside the workspace, as when the
// workspace is the user's home, so a refused path ends in ErrProtected.
func newOutputFixture(t *testing.T) outputFixture {
	t.Helper()
	root := t.TempDir()
	kopiHome := filepath.Join(root, "home")
	writeFile(t, filepath.Join(kopiHome, "config.json"), "{}")
	outputsDir := filepath.Join(kopiHome, "outputs")
	outputs := NewOutputs(outputsDir)
	require.NoError(t, outputs.Select("s1"))
	plain := newWorkspace(t, root, []string{kopiHome})
	return outputFixture{kopiHome: kopiHome, outputsDir: outputsDir, outputs: outputs, ws: plain.WithOutputs(outputs), plain: plain}
}

// numbered is count lines of 11 bytes each, "line NNNNN\n", counting from 0.
func numbered(count int) string {
	var b strings.Builder
	for i := range count {
		fmt.Fprintf(&b, "line %05d\n", i)
	}
	return b.String()
}

// spill writes text through a buffer of o and returns what the model is told
// and the saved file's path.
func spill(t *testing.T, o *Outputs, text string) (string, string) {
	t.Helper()
	b := o.buffer()
	n, err := b.Write([]byte(text))
	require.NoError(t, err)
	require.Equal(t, len(text), n)
	b.close()
	return b.String(), b.file()
}

// toolText runs tl with args and returns its text.
func toolText(t *testing.T, tl Tool, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	res, err := tl.Run(t.Context(), raw, admitIn(policy.Plan))
	return res.Text, err
}

func TestOutputBufferSavesNothingAtOrUnderTheThreshold(t *testing.T) {
	f := newOutputFixture(t)
	b := f.outputs.buffer()
	_, err := b.Write([]byte(strings.Repeat("x", 2*maxOutputPart)))
	require.NoError(t, err)
	assert.Empty(t, b.file())
	assert.NoDirExists(t, filepath.Join(f.outputsDir, "s1"))

	_, err = b.Write([]byte("x"))
	require.NoError(t, err)
	b.close()
	assert.NotEmpty(t, b.file())
}

func TestOutputBufferKeepSavesShortOutputFromThenOn(t *testing.T) {
	f := newOutputFixture(t)
	b := f.outputs.buffer()
	_, err := b.Write([]byte("early\n"))
	require.NoError(t, err)
	require.Empty(t, b.file())

	b.keep()
	b.keep()
	_, err = b.Write([]byte("late\n"))
	require.NoError(t, err)

	data, err := os.ReadFile(b.file())
	require.NoError(t, err)
	assert.Equal(t, "early\nlate\n", string(data))
	files, err := filepath.Glob(filepath.Join(f.outputsDir, "s1", "*"))
	require.NoError(t, err)
	require.Len(t, files, 1)

	spilled := f.outputs.buffer()
	_, err = spilled.Write([]byte(numbered(5000)))
	require.NoError(t, err)
	spilled.keep()
	data, err = os.ReadFile(spilled.file())
	require.NoError(t, err)
	assert.Equal(t, numbered(5000), string(data), "a file that already exists is not written twice")

	var none outputBuffer
	none.keep()
	assert.Empty(t, none.file())
}

func TestOutputBufferSavesEveryByteInOrder(t *testing.T) {
	text := numbered(20000)
	for _, size := range []int{1, 1000, 4099, 70000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newOutputFixture(t)
			b := f.outputs.buffer()
			for rest := []byte(text); len(rest) > 0; {
				n := min(size, len(rest))
				w, err := b.Write(rest[:n])
				require.NoError(t, err)
				require.Equal(t, n, w)
				rest = rest[n:]
			}
			b.close()

			data, err := os.ReadFile(b.file())
			require.NoError(t, err)
			assert.Equal(t, text, string(data))
			assert.Equal(t, filepath.Join(f.outputsDir, "s1"), filepath.Dir(b.file()))
			assert.Equal(t, ".txt", filepath.Ext(b.file()))
		})
	}
}

func TestOutputBufferFileModes(t *testing.T) {
	f := newOutputFixture(t)
	_, path := spill(t, f.outputs, numbered(5000))

	dir, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o700), dir.Mode().Perm())
	file, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o600), file.Mode().Perm())
}

func TestOutputBufferNoticeNamesTheFile(t *testing.T) {
	f := newOutputFixture(t)
	text := numbered(5000)

	notice, path := spill(t, f.outputs, text)

	cut := len(text) - 2*maxOutputPart
	want := fmt.Sprintf("\n[… %d bytes cut; full output: %s; use Read with offset=1364 to continue …]\n", cut, path)
	assert.Equal(t, text[:maxOutputPart]+want+text[len(text)-maxOutputPart:], notice)
}

func TestOutputBufferStopsFileAtTheLimit(t *testing.T) {
	f := newOutputFixture(t)
	b := f.outputs.buffer()
	b.limit = 40000
	text := numbered(10000)
	for rest := []byte(text); len(rest) > 0; {
		n := min(3000, len(rest))
		w, err := b.Write(rest[:n])
		require.NoError(t, err)
		require.Equal(t, n, w)
		rest = rest[n:]
	}
	b.close()

	data, err := os.ReadFile(b.file())
	require.NoError(t, err)
	assert.Equal(t, text[:40000], string(data))
	out := b.String()
	assert.Contains(t, out, fmt.Sprintf("first 40000 bytes of output: %s; use Read with offset=1364 to continue", b.file()))
	assert.NotContains(t, out, "full output")
	assert.True(t, strings.HasSuffix(out, text[len(text)-maxOutputPart:]))
}

func TestOutputBufferWithoutAFileKeepsToday(t *testing.T) {
	f := newOutputFixture(t)
	// A file where the session folder belongs makes creating it fail.
	writeFile(t, filepath.Join(f.outputsDir, "s1"), "in the way")
	text := numbered(5000)

	notice, path := spill(t, f.outputs, text)

	assert.Empty(t, path)
	assert.Equal(t, text[:maxOutputPart]+fmt.Sprintf("\n[… %d bytes cut …]\n", len(text)-2*maxOutputPart)+text[len(text)-maxOutputPart:], notice)
}

func TestOutputsNilSavesNothing(t *testing.T) {
	var o *Outputs
	b := o.buffer()
	_, err := b.Write([]byte(numbered(5000)))
	require.NoError(t, err)
	assert.Empty(t, b.file())
	assert.Empty(t, o.sessionDir())
	_, ok := o.holds("/x/y")
	assert.False(t, ok)
}

func TestOutputsSelectRefusesUnusableIDs(t *testing.T) {
	f := newOutputFixture(t)
	for _, id := range []string{"", "a/b", ".hidden", ".."} {
		assert.Error(t, f.outputs.Select(id), id)
		assert.Empty(t, f.outputs.sessionDir(), id)
	}
	require.NoError(t, f.outputs.Select("s2"))
	assert.Equal(t, filepath.Join(f.outputsDir, "s2"), f.outputs.sessionDir())
	assert.NoDirExists(t, filepath.Join(f.outputsDir, "s2"))
}

func TestOutputsBufferKeepsItsSessionFolder(t *testing.T) {
	f := newOutputFixture(t)
	b := f.outputs.buffer()
	require.NoError(t, f.outputs.Select("s2"))

	_, err := b.Write([]byte(numbered(5000)))
	require.NoError(t, err)
	b.close()

	assert.Equal(t, filepath.Join(f.outputsDir, "s1"), filepath.Dir(b.file()))
}

func TestReadReachesASavedOutputWithAnOffset(t *testing.T) {
	f := newOutputFixture(t)
	notice, path := spill(t, f.outputs, numbered(5000))
	require.Contains(t, notice, "offset=1364")

	res, err := readTool(t, f.ws, path)
	require.NoError(t, err)
	assert.Contains(t, res.Text, "     1\tline 00000\n")

	raw := fmt.Sprintf(`{"file_path":%q,"offset":1364,"limit":2}`, path)
	res, err = NewRead(f.ws).Run(t.Context(), []byte(raw), admitIn(policy.Plan))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.Text, "  1364\tline 01363\n  1365\tline 01364\n"), res.Text)
}

func TestSavedOutputTargetMatchesWhatReadAdmits(t *testing.T) {
	f := newOutputFixture(t)
	_, path := spill(t, f.outputs, numbered(5000))

	target, err := f.ws.Target(path)
	require.NoError(t, err)
	assert.False(t, target.PlanFile)
	admitted, err := runTool(t, NewRead(f.ws), map[string]any{"file_path": path})
	require.NoError(t, err)
	assert.Equal(t, []policy.Target{target}, admitted)
}

func TestSavedOutputIsShownByItsLeaf(t *testing.T) {
	f := newOutputFixture(t)
	_, path := spill(t, f.outputs, numbered(5000))

	assert.Equal(t, "output "+filepath.Base(path), f.ws.shown(path))
	assert.NotEqual(t, "output "+filepath.Base(path), f.plain.shown(path))
}

func TestSavedOutputsRefuseEverythingElse(t *testing.T) {
	f := newOutputFixture(t)
	_, path := spill(t, f.outputs, numbered(5000))
	require.NoError(t, f.outputs.Select("s2"))
	cases := map[string]string{
		"other session": path,
		"config":        filepath.Join(f.kopiHome, "config.json"),
		"folder":        filepath.Join(f.outputsDir, "s2"),
		"outputs dir":   f.outputsDir,
		"dotdot":        filepath.Join(f.outputsDir, "s2", "..", "s1", filepath.Base(path)),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := readTool(t, f.ws, p)
			require.ErrorIs(t, err, ErrProtected)
			assert.NotContains(t, res.Text, "line")
			_, err = f.ws.prepare(p)
			assert.ErrorIs(t, err, ErrProtected)
		})
	}
}

func TestPlainWorkspaceRefusesSavedOutput(t *testing.T) {
	f := newOutputFixture(t)
	_, path := spill(t, f.outputs, numbered(5000))

	_, err := readTool(t, f.plain, path)
	assert.ErrorIs(t, err, ErrProtected)
	_, err = runTool(t, NewGrep(f.plain, nil), map[string]any{"pattern": "line", "path": path})
	assert.ErrorIs(t, err, ErrProtected)
}

func TestSavedOutputRefusesASymlinkedLeaf(t *testing.T) {
	f := newOutputFixture(t)
	victim := filepath.Join(t.TempDir(), "victim.txt")
	writeFile(t, victim, "victim")
	link := filepath.Join(f.outputsDir, "s1", "link.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o700))
	require.NoError(t, os.Symlink(victim, link))

	res, err := readTool(t, f.ws, link)
	require.Error(t, err)
	assert.NotContains(t, res.Text, "victim")
	text, err := toolText(t, NewGrep(f.ws, nil), map[string]any{"pattern": "victim", "path": link})
	require.Error(t, err)
	assert.NotContains(t, text, "victim")
}

func TestSavedOutputRefusesASymlinkedFolder(t *testing.T) {
	f := newOutputFixture(t)
	elsewhere := t.TempDir()
	writeFile(t, filepath.Join(elsewhere, "a.txt"), "outside")
	require.NoError(t, os.MkdirAll(f.outputsDir, 0o700))
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(f.outputsDir, "s1")))

	res, err := readTool(t, f.ws, filepath.Join(f.outputsDir, "s1", "a.txt"))
	require.ErrorIs(t, err, ErrProtected)
	assert.NotContains(t, res.Text, "outside")
}

func TestGrepRefusesASymlinkedSessionFolder(t *testing.T) {
	f := newOutputFixture(t)
	elsewhere := t.TempDir()
	writeFile(t, filepath.Join(elsewhere, "a.txt"), "outside")
	require.NoError(t, os.MkdirAll(f.outputsDir, 0o700))
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(f.outputsDir, "s1")))

	text, err := toolText(t, NewGrep(f.ws, nil), map[string]any{"pattern": "outside", "path": filepath.Join(f.outputsDir, "s1", "a.txt")})
	require.ErrorIs(t, err, ErrProtected)
	assert.NotContains(t, text, "outside")
}

func TestSavedOutputIsReadOnly(t *testing.T) {
	f := newOutputFixture(t)
	text := numbered(5000)
	_, path := spill(t, f.outputs, text)

	_, err := runTool(t, NewWrite(f.ws), map[string]any{"file_path": path, "content": "x"})
	assert.ErrorIs(t, err, ErrProtected)
	_, err = runTool(t, NewEdit(f.ws), map[string]any{"file_path": path, "old_string": "line 00000", "new_string": "x"})
	assert.ErrorIs(t, err, ErrProtected)
	_, err = runTool(t, NewEdit(f.ws), map[string]any{"file_path": path, "old_string": "absent", "new_string": "x"})
	assert.ErrorIs(t, err, ErrProtected)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, text, string(data))
}

func TestGrepFindsAMatchInTheCutMiddleOfASavedOutput(t *testing.T) {
	f := newOutputFixture(t)
	text := strings.Replace(numbered(5000), "line 02500", "NEEDLE 02500", 1)
	notice, path := spill(t, f.outputs, text)
	require.NotContains(t, notice, "NEEDLE")

	res, err := toolText(t, NewGrep(f.ws, nil), map[string]any{"pattern": "NEEDLE", "path": path, "output_mode": "content"})

	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("Found 1 match in 1 file\n%s:2501:NEEDLE 02500\n", path), res)
}

func TestGrepRefusesTheOutputsFolderAndOtherSessions(t *testing.T) {
	f := newOutputFixture(t)
	_, path := spill(t, f.outputs, numbered(5000))
	require.NoError(t, f.outputs.Select("s2"))
	for name, p := range map[string]string{
		"other session": path,
		"folder":        filepath.Dir(path),
		"outputs dir":   f.outputsDir,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := toolText(t, NewGrep(f.ws, nil), map[string]any{"pattern": "line", "path": p})
			assert.ErrorIs(t, err, ErrProtected)
		})
	}
}

func TestGrepWalkFromTheWorkspaceRootSkipsSavedOutput(t *testing.T) {
	f := newOutputFixture(t)
	_, path := spill(t, f.outputs, strings.Replace(numbered(5000), "line 02500", "NEEDLE 02500", 1))

	text, err := toolText(t, NewGrep(f.ws, nil), map[string]any{"pattern": "NEEDLE|line", "path": ".", "output_mode": "content"})

	require.NoError(t, err)
	assert.NotContains(t, text, filepath.Base(path))
	assert.NotContains(t, text, "NEEDLE")
}

func TestOutputsPrune(t *testing.T) {
	f := newOutputFixture(t)
	now := time.Now()
	old, fresh := filepath.Join(f.outputsDir, "old", "a.txt"), filepath.Join(f.outputsDir, "mixed", "fresh.txt")
	stale := filepath.Join(f.outputsDir, "mixed", "stale.txt")
	writeFile(t, old, "x")
	writeFile(t, fresh, "x")
	writeFile(t, stale, "x")
	writeFile(t, filepath.Join(f.outputsDir, "empty", ".keep"), "x")
	require.NoError(t, os.Remove(filepath.Join(f.outputsDir, "empty", ".keep")))
	aged := now.Add(-outputMaxAge - time.Hour)
	for _, p := range []string{old, stale} {
		require.NoError(t, os.Chtimes(p, aged, aged))
	}
	edge := filepath.Join(f.outputsDir, "mixed", "edge.txt")
	writeFile(t, edge, "x")
	exact := now.Add(-outputMaxAge)
	require.NoError(t, os.Chtimes(edge, exact, exact))
	target := filepath.Join(t.TempDir(), "target.txt")
	writeFile(t, target, "x")
	require.NoError(t, os.Chtimes(target, aged, aged))
	link := filepath.Join(f.outputsDir, "mixed", "link.txt")
	require.NoError(t, os.Symlink(target, link))
	top := filepath.Join(f.outputsDir, "top.txt")
	writeFile(t, top, "x")
	require.NoError(t, os.Chtimes(top, aged, aged))

	f.outputs.Prune(now)

	assert.NoFileExists(t, old)
	assert.NoDirExists(t, filepath.Join(f.outputsDir, "old"))
	assert.NoFileExists(t, stale)
	assert.FileExists(t, fresh)
	assert.FileExists(t, edge)
	_, err := os.Lstat(link)
	assert.NoError(t, err)
	assert.FileExists(t, target)
	assert.FileExists(t, top)
	assert.NoDirExists(t, filepath.Join(f.outputsDir, "empty"))
}

func TestOutputsPruneMissingFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "none")
	NewOutputs(dir).Prune(time.Now())
	assert.NoDirExists(t, dir)
}

func TestSavedOutputKeepsRawBytes(t *testing.T) {
	f := newOutputFixture(t)
	text := "\xff" + numbered(5000)

	notice, path := spill(t, f.outputs, text)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(data, []byte{0xff}))
	assert.True(t, strings.HasPrefix(notice, "�"))
}

func TestOutputsSymlinkedFolderIsNotUsed(t *testing.T) {
	f := newOutputFixture(t)
	elsewhere := t.TempDir()
	writeFile(t, filepath.Join(elsewhere, "s1", "a.txt"), "outside")
	old := filepath.Join(elsewhere, "gone", "b.txt")
	writeFile(t, old, "outside")
	aged := time.Now().Add(-outputMaxAge - time.Hour)
	require.NoError(t, os.Chtimes(old, aged, aged))
	require.NoError(t, os.Symlink(elsewhere, f.outputsDir))

	res, err := readTool(t, f.ws, filepath.Join(f.outputsDir, "s1", "a.txt"))
	require.ErrorIs(t, err, ErrProtected)
	assert.NotContains(t, res.Text, "outside")

	f.outputs.Prune(time.Now())
	assert.FileExists(t, old)

	// No file is created through the link.
	require.NoError(t, os.Remove(filepath.Join(elsewhere, "s1", "a.txt")))
	require.NoError(t, os.Remove(filepath.Join(elsewhere, "s1")))
	notice, path := spill(t, f.outputs, numbered(5000))
	assert.Empty(t, path)
	assert.Contains(t, notice, " bytes cut …]")
	assert.NoDirExists(t, filepath.Join(elsewhere, "s1"))
}

func TestOutputBufferWithNothingSavedBeyondTheHeadKeepsToday(t *testing.T) {
	f := newOutputFixture(t)
	b := f.outputs.buffer()
	b.limit = 0
	text := numbered(5000)
	_, err := b.Write([]byte(text))
	require.NoError(t, err)
	b.close()

	assert.Empty(t, b.file())
	assert.Contains(t, b.String(), fmt.Sprintf("\n[… %d bytes cut …]\n", len(text)-2*maxOutputPart))
}

func TestOutputBufferRefusesASymlinkedSessionFolder(t *testing.T) {
	f := newOutputFixture(t)
	elsewhere := t.TempDir()
	require.NoError(t, os.MkdirAll(f.outputsDir, 0o700))
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(f.outputsDir, "s1")))
	text := numbered(5000)

	notice, path := spill(t, f.outputs, text)

	assert.Empty(t, path)
	assert.Contains(t, notice, fmt.Sprintf("\n[… %d bytes cut …]\n", len(text)-2*maxOutputPart))
	entries, err := os.ReadDir(elsewhere)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
