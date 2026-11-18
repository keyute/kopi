package tool

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListHonoursNestedGitignore(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), "# comment\n*.log\nbuild/\n")
	writeFile(t, filepath.Join(dir, "a.log"), "")
	writeFile(t, filepath.Join(dir, "build", "out"), "")
	writeFile(t, filepath.Join(dir, "sub", ".gitignore"), "gen/\n!keep.log\n")
	writeFile(t, filepath.Join(dir, "sub", "gen", "x.go"), "")
	writeFile(t, filepath.Join(dir, "sub", "keep.log"), "")
	writeFile(t, filepath.Join(dir, "sub", "drop.log"), "")
	writeFile(t, filepath.Join(dir, "other", "keep.log"), "")
	writeFile(t, filepath.Join(dir, "other", "gen", "y.go"), "")
	ws := newWorkspace(t, dir, nil)

	got, err := ws.List(100)
	require.NoError(t, err)
	assert.Equal(t, []string{
		".gitignore", "other/", "sub/",
		"other/gen/", "sub/.gitignore", "sub/keep.log",
		"other/gen/y.go",
	}, got)
}

func TestListKeepsAncestorPatternsSeparateBetweenSiblings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), "*.log\n*.tmp\n*.bak\n")
	for _, name := range []string{"a", "b"} {
		keep, deep := "keep.log", "deep.log"
		if name == "b" {
			keep, deep = "keep.tmp", "deep.tmp"
		}
		writeFile(t, filepath.Join(dir, name, ".gitignore"), "!"+keep+"\n")
		writeFile(t, filepath.Join(dir, name, "deep", ".gitignore"), "!"+deep+"\n")
		for _, sub := range []string{"", "deep", "deep/leaf"} {
			for _, file := range []string{"keep.log", "keep.tmp", "deep.log", "deep.tmp", "drop.bak"} {
				writeFile(t, filepath.Join(dir, name, sub, file), "")
			}
		}
	}
	ws := newWorkspace(t, dir, nil)

	got, err := ws.List(100)
	require.NoError(t, err)
	assert.Equal(t, []string{
		".gitignore", "a/", "b/",
		"a/.gitignore", "a/deep/", "a/keep.log",
		"b/.gitignore", "b/deep/", "b/keep.tmp",
		"a/deep/.gitignore", "a/deep/deep.log", "a/deep/keep.log", "a/deep/leaf/",
		"b/deep/.gitignore", "b/deep/deep.tmp", "b/deep/keep.tmp", "b/deep/leaf/",
		"a/deep/leaf/deep.log", "a/deep/leaf/keep.log",
		"b/deep/leaf/deep.tmp", "b/deep/leaf/keep.tmp",
	}, got)
}

func TestListDoesNotApplySymlinkedGitignore(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ignore-rules"), "hidden.txt\n")
	writeFile(t, filepath.Join(dir, "hidden.txt"), "")
	require.NoError(t, os.Symlink("ignore-rules", filepath.Join(dir, ".gitignore")))
	ws := newWorkspace(t, dir, nil)

	got, err := ws.List(100)
	require.NoError(t, err)
	assert.Equal(t, []string{".gitignore", "hidden.txt", "ignore-rules"}, got)
}

func TestListSkipsGitFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".git"), "gitdir: elsewhere\n")
	writeFile(t, filepath.Join(dir, "sub", ".git"), "gitdir: elsewhere\n")
	writeFile(t, filepath.Join(dir, "sub", "keep.txt"), "")
	ws := newWorkspace(t, dir, nil)

	got, err := ws.List(100)
	require.NoError(t, err)
	assert.Equal(t, []string{"sub/", "sub/keep.txt"}, got)
}

func TestListSkipsGitAndListsHiddenShallowFirst(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(dir, ".hidden"), "")
	writeFile(t, filepath.Join(dir, "a", "b", "c.txt"), "")
	writeFile(t, filepath.Join(dir, "z.txt"), "")
	require.NoError(t, os.Symlink("a", filepath.Join(dir, "link")))
	ws := newWorkspace(t, dir, nil)

	got, err := ws.List(100)
	require.NoError(t, err)
	assert.Equal(t, []string{".hidden", "a/", "link", "z.txt", "a/b/", "a/b/c.txt"}, got)

	got, err = ws.List(3)
	require.NoError(t, err)
	assert.Equal(t, []string{".hidden", "a/", "link"}, got)
}

func TestListSkipsProtectedStores(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	writeFile(t, filepath.Join(home, "notes.txt"), "")
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))

	got, err := ws.List(100)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{".env", "notes.txt"}, got)
}

func TestReadDirRejectsDirectorySwappedForProtectedSymlink(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	dir := filepath.Join(home, "ordinary")
	require.NoError(t, os.Mkdir(dir, 0o755))
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))
	vetted, err := os.Stat(dir)
	require.NoError(t, err)

	require.NoError(t, os.Rename(dir, filepath.Join(home, "ordinary-original")))
	require.NoError(t, os.Symlink(".ssh", dir))

	got, err := ws.readDir("ordinary", vetted)
	require.ErrorIs(t, err, ErrOutsideWorkspace)
	assert.Empty(t, got)
}

func TestListIsEmptyInWorkspaceInsideProtectedStore(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	dir := filepath.Join(home, ".ssh", "project")
	writeFile(t, filepath.Join(dir, "readme.txt"), "hello")
	ws := newWorkspace(t, dir, protectedStores(home, kopiHome))

	got, err := ws.List(100)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListDoesNotApplyGitignoreLinkedToProtectedStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		link func(home string) error
		want []string
	}{
		{
			name: "symlink",
			link: func(home string) error {
				return os.Symlink(filepath.Join("..", ".ssh", "id_x"), filepath.Join(home, "proj", ".gitignore"))
			},
			want: []string{".env", "proj/", "proj/.gitignore", "proj/other.txt", "proj/secret.txt"},
		},
		{
			name: "hard link",
			link: func(home string) error {
				return os.Link(filepath.Join(home, ".netrc"), filepath.Join(home, "proj", ".gitignore"))
			},
			// The link is the protected file itself, so it is not listed either.
			want: []string{".env", "proj/", "proj/other.txt", "proj/secret.txt"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, kopiHome := syntheticHome(t)
			writeFile(t, filepath.Join(home, ".ssh", "id_x"), "secret.txt\n")
			writeFile(t, filepath.Join(home, ".netrc"), "secret.txt\n")
			writeFile(t, filepath.Join(home, "proj", "secret.txt"), "")
			writeFile(t, filepath.Join(home, "proj", "other.txt"), "")
			require.NoError(t, tc.link(home))
			ws := newWorkspace(t, home, protectedStores(home, kopiHome))

			got, err := ws.List(100)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// walked runs walk from dir and returns the paths it visits.
func walked(t *testing.T, ws *Workspace, dir string) []string {
	t.Helper()
	var vetted fs.FileInfo
	if dir != "." {
		var err error
		vetted, err = ws.root.Lstat(dir)
		require.NoError(t, err)
	}
	var got []string
	require.NoError(t, ws.walk(context.Background(), dir, vetted, func(p string, _ fs.FileInfo) bool {
		got = append(got, p)
		return true
	}))
	return got
}

func TestWalkFromSubdirectoryAppliesAncestorGitignore(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), "*.log\n")
	writeFile(t, filepath.Join(dir, "sub", "a.log"), "")
	writeFile(t, filepath.Join(dir, "sub", "a.go"), "")
	ws := newWorkspace(t, dir, nil)

	assert.Equal(t, []string{"sub/a.go"}, walked(t, ws, "sub"))
}

func TestWalkSearchesIgnoredStartDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), "build/\n")
	writeFile(t, filepath.Join(dir, "build", "out", "a.o"), "")
	ws := newWorkspace(t, dir, nil)

	assert.Equal(t, []string{"build/out", "build/out/a.o"}, walked(t, ws, "build"))
	assert.Equal(t, []string{".gitignore"}, walked(t, ws, "."))
}

func TestWalkStopsWhenCancelled(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "")
	ws := newWorkspace(t, dir, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ws.walk(ctx, ".", nil, func(string, fs.FileInfo) bool {
		t.Fatal("visited after cancellation")
		return true
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestWalkSkipsStoreCreatedDuringWalk(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	require.NoError(t, os.Mkdir(filepath.Join(home, "a"), 0o755))
	ws := newWorkspace(t, home, append(protectedStores(home, kopiHome), filepath.Join(home, "a", "store")))

	var got []string
	require.NoError(t, ws.walk(context.Background(), ".", nil, func(p string, _ fs.FileInfo) bool {
		if p == "a" {
			writeFile(t, filepath.Join(home, "a", "store", "key"), "secret")
		}
		got = append(got, p)
		return true
	}))
	assert.Equal(t, []string{".env", "a"}, got)
}

func TestWalkSkipsQueuedDirectoryThatBecameStore(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	writeFile(t, filepath.Join(home, "a", "key"), "secret")
	store := filepath.Join(t.TempDir(), "store")
	ws := newWorkspace(t, home, append(protectedStores(home, kopiHome), store))

	var got []string
	require.NoError(t, ws.walk(context.Background(), ".", nil, func(p string, _ fs.FileInfo) bool {
		if p == "a" {
			require.NoError(t, os.Symlink(filepath.Join(home, "a"), store))
		}
		got = append(got, p)
		return true
	}))
	assert.Equal(t, []string{".env", "a"}, got)
}
