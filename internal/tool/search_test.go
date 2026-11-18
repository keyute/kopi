package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

func TestSearchTargetDefaultsToWorkspace(t *testing.T) {
	dir := t.TempDir()
	ws := newWorkspace(t, dir, nil)
	target := searchTarget(ws)

	for _, raw := range []string{`{}`, `{"path":""}`, `not json`} {
		got, err := target(json.RawMessage(raw))
		require.NoError(t, err)
		assert.Equal(t, policy.Target{Path: ws.given, Real: ws.realDir}, got)
	}
	got, err := target(json.RawMessage(`{"path":"sub"}`))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(ws.realDir, "sub"), got.Real)
}

func TestSearchRootResolvesSymlinkedDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "real", "sub", "a.go"), "")
	require.NoError(t, os.Symlink(filepath.Join("real", "sub"), filepath.Join(dir, "link")))
	ws := newWorkspace(t, dir, nil)

	target, rel, info, err := ws.searchRoot("link")
	require.NoError(t, err)
	assert.Equal(t, "real/sub", rel)
	assert.Equal(t, filepath.Join(ws.given, "link"), target.Path)
	assert.Equal(t, filepath.Join(ws.realDir, "real", "sub"), target.Real)
	assert.True(t, info.IsDir())

	_, rel, info, err = ws.searchRoot(".")
	require.NoError(t, err)
	assert.Equal(t, ".", rel)
	assert.True(t, info.IsDir())
}

func TestSearchRootRefusesProtectedAndOutside(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))

	_, _, _, err := ws.searchRoot(".ssh")
	require.ErrorIs(t, err, ErrProtected)
	_, _, _, err = ws.searchRoot("..")
	require.ErrorIs(t, err, ErrOutsideWorkspace)
}

func TestSearchToolsRefuseKopiHomeWithoutAdmitting(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))
	for _, tc := range []struct {
		tool Tool
		args string
	}{
		{NewGlob(ws, nil), fmt.Sprintf(`{"pattern":"*","path":%q}`, kopiHome)},
		{NewGrep(ws, nil), fmt.Sprintf(`{"pattern":"x","path":%q}`, kopiHome)},
		{NewLS(ws, nil), fmt.Sprintf(`{"path":%q}`, kopiHome)},
	} {
		t.Run(tc.tool.Name, func(t *testing.T) {
			admitted := false
			_, err := tc.tool.Run(context.Background(), json.RawMessage(tc.args), func(policy.Target) (policy.Mode, error) {
				admitted = true
				return policy.Plan, nil
			})
			require.ErrorIs(t, err, ErrProtected)
			assert.False(t, admitted)
		})
	}
}

func TestOpenWalkedRefusesSwappedFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		swap func(dir string) error
	}{
		{"renamed over", func(dir string) error {
			return os.Rename(filepath.Join(dir, "other.txt"), filepath.Join(dir, "a.txt"))
		}},
		{"symlink", func(dir string) error {
			if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
				return err
			}
			return os.Symlink("other.txt", filepath.Join(dir, "a.txt"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "a.txt"), "a")
			writeFile(t, filepath.Join(dir, "other.txt"), "other")
			ws := newWorkspace(t, dir, nil)
			info, err := ws.root.Lstat("a.txt")
			require.NoError(t, err)

			f, err := ws.openWalked("a.txt", info)
			require.NoError(t, err)
			require.NoError(t, f.Close())

			require.NoError(t, tc.swap(dir))
			f, err = ws.openWalked("a.txt", info)
			require.ErrorIs(t, err, ErrTargetChanged)
			assert.Nil(t, f)
		})
	}
}

func TestCompileGlob(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		match   []string
		miss    []string
	}{
		{"*.go", []string{"b.go", "a/b.go"}, []string{"a/b.md"}},
		{"cmd/*.go", []string{"cmd/a.go"}, []string{"x/cmd/a.go", "cmd/x/a.go"}},
		{"*.{go,md}", []string{"a.go", "x/a.md"}, []string{"a.txt"}},
		{"", []string{"a", "x/y.z"}, nil},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			match, err := compileGlob(tc.pattern)
			require.NoError(t, err)
			for _, p := range tc.match {
				assert.True(t, match(p), p)
			}
			for _, p := range tc.miss {
				assert.False(t, match(p), p)
			}
		})
	}
	for _, pattern := range []string{"[a", "/abs/*.go"} {
		_, err := compileGlob(pattern)
		require.ErrorIs(t, err, ErrInvalidArgs, pattern)
	}
}

func TestUnder(t *testing.T) {
	root := policy.Target{Path: "/ws/link", Real: "/real/ws/sub"}

	sub, target := under(root, "sub", "sub/a/b.go")
	assert.Equal(t, "a/b.go", sub)
	assert.Equal(t, policy.Target{Path: "/ws/link/a/b.go", Real: "/real/ws/sub/a/b.go"}, target)

	sub, _ = under(root, ".", "a/b.go")
	assert.Equal(t, "a/b.go", sub)
}

func TestNewestFirst(t *testing.T) {
	now := time.Now()
	got := []found{{path: "b", modTime: now}, {path: "old", modTime: now.Add(-time.Hour)}, {path: "a", modTime: now}}
	slices.SortFunc(got, newestFirst)
	assert.Equal(t, []string{"a", "b", "old"}, foundPaths(got))
}

func TestKeepNewestStaysBounded(t *testing.T) {
	now := time.Now()
	var kept []found
	for i := range 10 {
		kept = keepNewest(kept, found{path: fmt.Sprint(i), modTime: now.Add(time.Duration(i) * time.Second)}, 2)
		assert.Less(t, len(kept), 4)
	}
	slices.SortFunc(kept, newestFirst)
	assert.Equal(t, []string{"9", "8"}, foundPaths(kept[:2]))
}

func foundPaths(list []found) []string {
	var paths []string
	for _, f := range list {
		paths = append(paths, f.path)
	}
	return paths
}

func TestFoundSummary(t *testing.T) {
	for output, want := range map[string]string{
		"Found 3 files\na\nb\nc\n":           "3 files",
		"Found 15 matches in 2 files\na:1:x": "15 matches in 2 files",
		"No matches found":                   "no matches",
		"No files found\n":                   "no files",
	} {
		assert.Equal(t, want, foundSummary(output), output)
	}
}

func TestSearchTitle(t *testing.T) {
	title := searchTitle("Grep")
	assert.Equal(t, "Grep TODO in internal", title(json.RawMessage(`{"pattern":"TODO","path":"internal"}`)))
	assert.Equal(t, "Grep TODO", title(json.RawMessage(`{"pattern":"TODO"}`)))
	assert.Equal(t, "Grep", title(json.RawMessage(`{"path":"internal"}`)))
}

func TestOpenWalkedRefusesFileMovedIntoProtection(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "allowed", "file"), "secret")
	ws := newWorkspace(t, dir, []string{filepath.Join(dir, "protected")})
	info, err := ws.root.Lstat("allowed/file")
	require.NoError(t, err)
	require.NoError(t, os.Rename(filepath.Join(dir, "allowed"), filepath.Join(dir, "protected")))
	require.NoError(t, os.Symlink("protected", filepath.Join(dir, "allowed")))

	f, err := ws.openWalked("allowed/file", info)
	require.ErrorIs(t, err, ErrProtected)
	assert.Nil(t, f)
}

func TestOpenWalkedRefusesSameInodeSymlinkSwap(t *testing.T) {
	for _, parent := range []bool{false, true} {
		for _, protected := range []bool{false, true} {
			t.Run(fmt.Sprintf("parent=%t/protected=%t", parent, protected), func(t *testing.T) {
				dir := t.TempDir()
				name, moved := "allowed.txt", "denied.txt"
				if parent {
					name, moved = "allowed", "denied"
				}
				var stores []string
				if protected {
					stores = []string{filepath.Join(dir, "kopi-home")}
					require.NoError(t, os.Mkdir(stores[0], 0o755))
					moved = filepath.Join("kopi-home", moved)
				}
				p := name
				if parent {
					p = filepath.Join(name, "file.txt")
				}
				writeFile(t, filepath.Join(dir, p), "needle secret")
				ws := newWorkspace(t, dir, stores)
				info, err := ws.root.Lstat(p)
				require.NoError(t, err)
				require.NoError(t, os.Rename(filepath.Join(dir, name), filepath.Join(dir, moved)))
				require.NoError(t, os.Symlink(moved, filepath.Join(dir, name)))
				f, err := ws.openWalked(filepath.ToSlash(p), info)
				if protected {
					require.ErrorIs(t, err, ErrProtected)
				} else {
					require.ErrorIs(t, err, ErrTargetChanged)
				}
				assert.Nil(t, f)
			})
		}
	}
}

func TestSearchToolsRefuseSameInodeDirectorySwapDuringAdmission(t *testing.T) {
	for _, name := range []string{"Glob", "Grep", "LS"} {
		for _, parent := range []bool{false, true} {
			for _, protected := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/parent=%t/protected=%t", name, parent, protected), func(t *testing.T) {
					dir := t.TempDir()
					p := "allowed"
					if parent {
						p = "allowed/sub"
					}
					writeFile(t, filepath.Join(dir, p, "secret.txt"), "needle secret")
					moved := "denied"
					var stores []string
					if protected {
						stores = []string{filepath.Join(dir, "kopi-home")}
						require.NoError(t, os.Mkdir(stores[0], 0o755))
						moved = "kopi-home/denied"
					}
					ws := newWorkspace(t, dir, stores)
					var tool Tool
					args := fmt.Sprintf(`{"path":%q}`, p)
					switch name {
					case "Glob":
						tool = NewGlob(ws, nil)
						args = fmt.Sprintf(`{"path":%q,"pattern":"*"}`, p)
					case "Grep":
						tool = NewGrep(ws, nil)
						args = fmt.Sprintf(`{"path":%q,"pattern":"needle","output_mode":"content"}`, p)
					case "LS":
						tool = NewLS(ws, nil)
					}
					res, err := tool.Run(t.Context(), json.RawMessage(args), func(policy.Target) (policy.Mode, error) {
						require.NoError(t, os.Rename(filepath.Join(dir, "allowed"), filepath.Join(dir, moved)))
						require.NoError(t, os.Symlink(moved, filepath.Join(dir, "allowed")))
						return policy.Execute, nil
					})
					require.Error(t, err)
					assert.Empty(t, res.Text)
				})
			}
		}
	}
}

func TestSearchToolsOpenResolvedSymlinkRoot(t *testing.T) {
	for _, name := range []string{"Glob", "Grep", "LS"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "real", "file.txt"), "needle")
			require.NoError(t, os.Symlink("real", filepath.Join(dir, "link")))
			ws := newWorkspace(t, dir, nil)
			var tool Tool
			args := `{"path":"link"}`
			switch name {
			case "Glob":
				tool, args = NewGlob(ws, nil), `{"path":"link","pattern":"*"}`
			case "Grep":
				tool, args = NewGrep(ws, nil), `{"path":"link","pattern":"needle"}`
			case "LS":
				tool = NewLS(ws, nil)
			}
			res, err := tool.Run(t.Context(), json.RawMessage(args), allowTarget)
			require.NoError(t, err)
			assert.Contains(t, res.Text, "file.txt")
		})
	}
}
