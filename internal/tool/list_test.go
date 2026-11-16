package tool

import (
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
	ws := newWorkspace(t, home, DefaultProtected(home, kopiHome))

	got, err := ws.List(100)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{".env", "notes.txt"}, got)
}

func TestListIsEmptyInWorkspaceInsideProtectedStore(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	dir := filepath.Join(home, ".ssh", "project")
	writeFile(t, filepath.Join(dir, "readme.txt"), "hello")
	ws := newWorkspace(t, dir, DefaultProtected(home, kopiHome))

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
			ws := newWorkspace(t, home, DefaultProtected(home, kopiHome))

			got, err := ws.List(100)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
