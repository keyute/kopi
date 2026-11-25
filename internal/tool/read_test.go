package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

func newReadTool(t *testing.T) (Tool, string) {
	t.Helper()
	dir := t.TempDir()
	return NewRead(newWorkspace(t, dir, nil)), dir
}

func runRead(t *testing.T, tool Tool, args string) (string, error) {
	t.Helper()
	res, err := tool.Run(context.Background(), json.RawMessage(args), allowTarget)
	return res.Text, err
}

func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func TestReadDescribesItself(t *testing.T) {
	tool, _ := newReadTool(t)

	assert.Equal(t, "Read", tool.Name)
	assert.Equal(t, policy.Read, tool.Capability)
	assert.Equal(t, "object", tool.Parameters["type"])
	assert.Equal(t, []string{"file_path"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	_, err := json.Marshal(tool.Parameters)
	require.NoError(t, err)
}

func TestReadNumbersLines(t *testing.T) {
	tool, dir := newReadTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha\nbeta\n\ngamma")

	out, err := runRead(t, tool, `{"file_path":"a.txt"}`)
	require.NoError(t, err)
	assert.Equal(t, "     1\talpha\n     2\tbeta\n     3\t\n     4\tgamma\n", out)
}

func TestReadHonoursOffsetAndLimit(t *testing.T) {
	tool, dir := newReadTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), numberedLines(10))

	for _, tc := range []struct {
		name, args, want string
	}{
		{
			name: "offset to end",
			args: `{"file_path":"a.txt","offset":9}`,
			want: "     9\tline 9\n    10\tline 10\n",
		},
		{
			name: "offset and limit",
			args: `{"file_path":"a.txt","offset":3,"limit":2}`,
			want: "     3\tline 3\n     4\tline 4\n" +
				"[Showing lines 3-4. More lines remain; use offset=5 to continue.]\n",
		},
		{
			name: "limit reaching the last line",
			args: `{"file_path":"a.txt","offset":8,"limit":3}`,
			want: "     8\tline 8\n     9\tline 9\n    10\tline 10\n",
		},
		{
			name: "offset past end",
			args: `{"file_path":"a.txt","offset":11}`,
			want: "(offset 11 is past the end of the file, which has 10 lines)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runRead(t, tool, tc.args)
			require.NoError(t, err)
			assert.Equal(t, tc.want, out)
		})
	}
}

func TestReadStopsAtLineLimitWithNotice(t *testing.T) {
	tool, dir := newReadTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), numberedLines(maxReadLines+5))

	for _, args := range []string{
		`{"file_path":"a.txt"}`,
		`{"file_path":"a.txt","limit":5000}`,
	} {
		out, err := runRead(t, tool, args)
		require.NoError(t, err)
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		require.Len(t, lines, maxReadLines+1)
		assert.Equal(t, "  2000\tline 2000", lines[maxReadLines-1])
		assert.Equal(t, "[Showing lines 1-2000. More lines remain; use offset=2001 to continue.]", lines[maxReadLines])
	}
}

func TestReadCutsLongLines(t *testing.T) {
	tool, dir := newReadTool(t)
	long := strings.Repeat("é", maxLineChars+50)
	writeFile(t, filepath.Join(dir, "a.txt"), long+"\nnext\n")

	out, err := runRead(t, tool, `{"file_path":"a.txt"}`)
	require.NoError(t, err)
	want := "     1\t" + strings.Repeat("é", maxLineChars) + lineCutMarker + "\n     2\tnext\n"
	assert.Equal(t, want, out)
}

func TestReadCutsUnboundedLine(t *testing.T) {
	tool, dir := newReadTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), strings.Repeat("x", 1<<20)+"\nnext")

	out, err := runRead(t, tool, `{"file_path":"a.txt"}`)
	require.NoError(t, err)
	want := "     1\t" + strings.Repeat("x", maxLineChars) + lineCutMarker + "\n     2\tnext\n"
	assert.Equal(t, want, out)
}

func TestReadStopsAtByteCapWithNotice(t *testing.T) {
	tool, dir := newReadTool(t)
	line := strings.Repeat("y", 1000)
	writeFile(t, filepath.Join(dir, "a.txt"), strings.Repeat(line+"\n", 500))

	out, err := runRead(t, tool, `{"file_path":"a.txt"}`)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(out), maxReadBytes)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	shown := len(lines) - 1
	assert.Less(t, shown, 500)
	assert.Equal(t, fmt.Sprintf("[Showing lines 1-%d. More lines remain; use offset=%d to continue.]", shown, shown+1), lines[shown])
}

func TestReadReturnsNoticeForEmptyFile(t *testing.T) {
	tool, dir := newReadTool(t)
	writeFile(t, filepath.Join(dir, "empty.txt"), "")

	out, err := runRead(t, tool, `{"file_path":"empty.txt"}`)
	require.NoError(t, err)
	assert.Equal(t, "(empty file)", out)
}

const pngSignature = "\x89PNG\r\n\x1a\n"

func TestReadReturnsImage(t *testing.T) {
	tool, dir := newReadTool(t)
	content := pngSignature + "\x00\x00\x00\rIHDR"
	writeFile(t, filepath.Join(dir, "a.png"), content)

	res, err := tool.Run(context.Background(), json.RawMessage(`{"file_path":"a.png","offset":5}`), allowTarget)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("(image/png, %d bytes)", len(content)), res.Text)
	assert.Equal(t, []Image{{MimeType: "image/png", Data: []byte(content)}}, res.Images)
}

func TestReadRefusesOversizedImage(t *testing.T) {
	tool, dir := newReadTool(t)
	path := filepath.Join(dir, "big.png")
	writeFile(t, path, pngSignature)
	require.NoError(t, os.Truncate(path, MaxImageBytes+1))

	out, err := runRead(t, tool, `{"file_path":"big.png"}`)
	require.ErrorIs(t, err, ErrTooLarge)
	assert.Empty(t, out)
}

func TestReadDisplayTitle(t *testing.T) {
	tool, _ := newReadTool(t)

	assert.Equal(t, Action{Verb: "Read", One: "file", Many: "files"}, tool.Display.Action)
	for _, tc := range []struct {
		name, args, want string
	}{
		{"relative path", `{"file_path":"cmd/root.go"}`, "Read cmd/root.go"},
		{"absolute path", `{"file_path":"/w/a.txt"}`, "Read /w/a.txt"},
		{"offset only", `{"file_path":"a.txt","offset":40}`, "Read a.txt:40-"},
		{"limit only", `{"file_path":"a.txt","limit":50}`, "Read a.txt:1-50"},
		{"offset and limit", `{"file_path":"a.txt","offset":40,"limit":50}`, "Read a.txt:40-89"},
		{"missing path", `{}`, "Read"},
		{"bad json", `{"file_path":`, "Read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tool.Display.Title(json.RawMessage(tc.args)))
		})
	}
}

func TestReadDisplaySummary(t *testing.T) {
	tool, dir := newReadTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), numberedLines(10))
	writeFile(t, filepath.Join(dir, "one.txt"), "only")
	writeFile(t, filepath.Join(dir, "empty.txt"), "")
	writeFile(t, filepath.Join(dir, "big.txt"), numberedLines(maxReadLines+5))
	writeFile(t, filepath.Join(dir, "a.png"), pngSignature+"\x00\x00\x00\rIHDR")

	for _, tc := range []struct {
		name, args, want string
	}{
		{"whole file", `{"file_path":"a.txt"}`, "10 lines"},
		{"single line", `{"file_path":"one.txt"}`, "1 line"},
		{"range with more remaining", `{"file_path":"a.txt","offset":3,"limit":2}`, "2 lines"},
		{"line cap with notice", `{"file_path":"big.txt"}`, "2000 lines"},
		{"empty file", `{"file_path":"empty.txt"}`, "empty file"},
		{"offset past end", `{"file_path":"a.txt","offset":11}`, "past end of file"},
		{"image", `{"file_path":"a.png"}`, "image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runRead(t, tool, tc.args)
			require.NoError(t, err)
			assert.Equal(t, tc.want, tool.Display.Summary(out))
		})
	}
}

func TestReadDisplayBody(t *testing.T) {
	tool, _ := newReadTool(t)

	assert.Equal(t, "1  x\n", tool.Display.Body("     1\tx\n"))
	assert.Empty(t, tool.Display.Body(emptyFileNotice))
	assert.Empty(t, tool.Display.Body("(offset 11 is past the end of the file, which has 10 lines)"))
	assert.Empty(t, tool.Display.Body("(image/png, 20 bytes)"))
}

func TestReadBodyAlignsNumbersToTheWidest(t *testing.T) {
	for name, tc := range map[string]struct{ output, want string }{
		"one digit":    {"     1\ta\n     2\tb\n", "1  a\n2  b\n"},
		"two digits":   {"     9\ta\n    10\tb\n", " 9  a\n10  b\n"},
		"three digits": {"    99\ta\n   100\tb\n", " 99  a\n100  b\n"},
		"four digits":  {"   999\ta\n  1000\tb\n", " 999  a\n1000  b\n"},
		"notice":       {"    98\ta\n    99\t\n   100\tc\n[Showing lines 98-100. More lines remain; use offset=101 to continue.]\n", " 98  a\n 99  \n100  c\n[Showing lines 98-100. More lines remain; use offset=101 to continue.]\n"},
		"cut marker":   {"     1\t" + strings.Repeat("x", 3) + lineCutMarker + "\n", "1  xxx" + lineCutMarker + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, readBody(tc.output))
		})
	}
}

func TestReadSummaryOmittedImageIsNotAnImage(t *testing.T) {
	tool, _ := newReadTool(t)
	assert.Equal(t, "image omitted", tool.Display.Summary(imageNotice+"20 bytes)\n"+OmittedImageNote))
}

func TestReadRefusesUnreadableTargets(t *testing.T) {
	tool, dir := newReadTool(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	writeFile(t, filepath.Join(dir, "bin"), "ELF\x00\x01\x02")

	for _, tc := range []struct {
		name, path string
		want       error
	}{
		{"directory", "sub", ErrIsDirectory},
		{"binary", "bin", ErrBinary},
		{"missing", "missing.txt", fs.ErrNotExist},
		{"outside", "../x", ErrOutsideWorkspace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runRead(t, tool, fmt.Sprintf(`{"file_path":%q}`, tc.path))
			require.ErrorIs(t, err, tc.want)
			assert.Empty(t, out)
		})
	}
}

func TestReadRejectsInvalidArgs(t *testing.T) {
	tool, dir := newReadTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha\n")

	for _, tc := range []struct {
		name, args string
	}{
		{"bad json", `{"file_path":`},
		{"not an object", `"a.txt"`},
		{"trailing data", `{"file_path":"a.txt"} {}`},
		{"unknown field", `{"file_path":"a.txt","pattern":"x"}`},
		{"missing path", `{}`},
		{"empty path", `{"file_path":""}`},
		{"offset zero", `{"file_path":"a.txt","offset":0}`},
		{"limit zero", `{"file_path":"a.txt","limit":0}`},
		{"negative limit", `{"file_path":"a.txt","limit":-1}`},
		{"path wrong type", `{"file_path":1}`},
		{"offset wrong type", `{"file_path":"a.txt","offset":"1"}`},
		{"fractional limit", `{"file_path":"a.txt","limit":1.5}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runRead(t, tool, tc.args)
			require.ErrorIs(t, err, ErrInvalidArgs)
		})
	}
}

func TestReadReturnsContextErrorWhenCancelled(t *testing.T) {
	tool, dir := newReadTool(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tool.Run(ctx, json.RawMessage(`{"file_path":"a.txt"}`), allowTarget)
	require.ErrorIs(t, err, context.Canceled)
}

func TestReadLineStopsDiscardingWhenCancelled(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 1<<10)), 16)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := readLine(ctx, r)
	require.ErrorIs(t, err, context.Canceled)
}

func TestReadTarget(t *testing.T) {
	tool, dir := newReadTool(t)
	realDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)

	got, err := tool.Target(json.RawMessage(`{"file_path":"sub/a.txt","limit":3}`))
	require.NoError(t, err)
	assert.Equal(t, policy.Target{Path: filepath.Join(dir, "sub", "a.txt"), Real: filepath.Join(realDir, "sub", "a.txt")}, got)
	other := t.TempDir()
	got, err = tool.Target(json.RawMessage(fmt.Sprintf(`{"file_path":%q}`, other+"/sub/../b.txt")))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(other, "b.txt"), got.Path)
	_, err = tool.Target(json.RawMessage(`{}`))
	require.ErrorIs(t, err, ErrInvalidArgs)
}

func allowTarget(policy.Target) (policy.Mode, error) { return policy.Execute, nil }

func TestToolsDoNotReadContentBeforeAdmission(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "binary"), "secret\x00")
	ws := newWorkspace(t, dir, nil)
	args := map[string]string{
		"Read": `{"file_path":"binary"}`,
		"Edit": `{"file_path":"binary","old_string":"secret","new_string":"new"}`,
		"Glob": `{"pattern":"*"}`,
		"Grep": `{"pattern":"secret"}`,
		"LS":   `{}`,
	}
	for _, tool := range []Tool{NewRead(ws), NewEdit(ws), NewGlob(ws, nil), NewGrep(ws, nil), NewLS(ws, nil)} {
		t.Run(tool.Name, func(t *testing.T) {
			out, err := tool.Run(t.Context(), json.RawMessage(args[tool.Name]), func(policy.Target) (policy.Mode, error) { return policy.Plan, policy.ErrDenied })
			require.ErrorIs(t, err, policy.ErrDenied)
			assert.Empty(t, out)
			assert.Equal(t, "secret\x00", readContent(t, filepath.Join(dir, "binary")))
		})
	}
}

func TestWalkingToolsListOnlyAfterAdmission(t *testing.T) {
	args := map[string]string{
		"Glob": `{"pattern":"*"}`,
		"Grep": `{"pattern":"needle"}`,
		"LS":   `{}`,
	}
	for _, name := range []string{"Glob", "Grep", "LS"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			ws := newWorkspace(t, dir, nil)
			tools := map[string]Tool{"Glob": NewGlob(ws, nil), "Grep": NewGrep(ws, nil), "LS": NewLS(ws, nil)}
			out, err := tools[name].Run(t.Context(), json.RawMessage(args[name]), func(policy.Target) (policy.Mode, error) {
				writeFile(t, filepath.Join(dir, "late.txt"), "needle\n")
				return policy.Execute, nil
			})
			require.NoError(t, err)
			assert.Contains(t, out.Text, "late.txt")
		})
	}
}

func TestToolsUseAdmittedHandlesAfterParentRetarget(t *testing.T) {
	for _, name := range []string{"Read", "Write", "Edit"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "allowed", "file"), "alpha")
			writeFile(t, filepath.Join(dir, "denied", "file"), "secret")
			ws := newWorkspace(t, dir, nil)
			tools := map[string]Tool{"Read": NewRead(ws), "Write": NewWrite(ws), "Edit": NewEdit(ws)}
			args := map[string]string{"file_path": "allowed/file"}
			if name == "Write" {
				args["content"] = "updated"
			}
			if name == "Edit" {
				args["old_string"], args["new_string"] = "alpha", "updated"
			}
			raw, err := json.Marshal(args)
			require.NoError(t, err)
			out, err := tools[name].Run(t.Context(), raw, func(target policy.Target) (policy.Mode, error) {
				assert.Equal(t, filepath.Join(ws.realDir, "allowed", "file"), target.Real)
				require.NoError(t, os.Rename(filepath.Join(dir, "allowed"), filepath.Join(dir, "pinned")))
				require.NoError(t, os.Symlink("denied", filepath.Join(dir, "allowed")))
				return policy.Execute, nil
			})
			require.NoError(t, err)
			if name == "Read" {
				assert.Contains(t, out.Text, "alpha")
				assert.NotContains(t, out.Text, "secret")
			} else {
				assert.Equal(t, "updated", readContent(t, filepath.Join(dir, "pinned", "file")))
			}
			assert.Equal(t, "secret", readContent(t, filepath.Join(dir, "denied", "file")))
			assert.Equal(t, []string{"file"}, listDir(t, filepath.Join(dir, "pinned")))
		})
	}
}

func TestReadDisplayOutcome(t *testing.T) {
	ws, plan, outputs := newPlanWorkspace(t, t.TempDir())
	d := NewRead(ws).Display

	_, action := d.Outcome(json.RawMessage(`{"file_path":`+strconv.Quote(plan)+`}`), "")
	assert.Equal(t, Action{Verb: "Read", One: "the plan"}, action)
	_, action = d.Outcome(json.RawMessage(`{"file_path":`+strconv.Quote(filepath.Join(outputs, "x.txt"))+`}`), "")
	assert.Equal(t, Action{Verb: "Read", One: "output file", Many: "output files"}, action)
	_, action = d.Outcome(json.RawMessage(`{"file_path":"a.go"}`), "")
	assert.Equal(t, Action{}, action)
	_, action = d.Outcome(json.RawMessage(`{"file_path":"plan"}`), "")
	assert.Equal(t, Action{}, action)

	assert.Equal(t, "a.go", d.Subject(json.RawMessage(`{"file_path":"a.go","offset":40,"limit":50}`), ""), "whatever range was read")
}

func TestReadReachesAnAddedDirectory(t *testing.T) {
	tool, dir := newReadTool(t)
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "a.txt"), "hello\n")
	args := fmt.Sprintf(`{"file_path":%q}`, filepath.Join(other, "a.txt"))

	_, err := runRead(t, tool, args)
	require.ErrorIs(t, err, ErrOutsideWorkspace)
	ws := newWorkspace(t, dir, nil)
	require.NoError(t, ws.Add(Dir{Given: other, Real: realPath(t, other)}))
	tool = NewRead(ws)

	out, err := runRead(t, tool, args)
	require.NoError(t, err)
	assert.Equal(t, "     1\thello\n", out)
	got, err := tool.Target(json.RawMessage(args))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(other, "a.txt"), got.Path)
	assert.Equal(t, filepath.Join(realPath(t, other), "a.txt"), got.Real)
	_, err = runRead(t, tool, `{"file_path":"a.txt"}`)
	require.ErrorIs(t, err, fs.ErrNotExist)
}
