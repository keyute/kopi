package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func newWorkspace(t *testing.T, dir string, protected []string) *Workspace {
	t.Helper()
	ws, err := NewWorkspace(dir, protected)
	require.NoError(t, err)
	t.Cleanup(func() { ws.Close() })
	return ws
}

func readAll(t *testing.T, ws *Workspace, path string) string {
	t.Helper()
	f, err := ws.Open(path)
	require.NoError(t, err)
	defer f.Close()
	b, err := io.ReadAll(f)
	require.NoError(t, err)
	return string(b)
}

func TestOpenReadsRelativePathInside(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sub", "a.txt"), "hello")
	ws := newWorkspace(t, dir, nil)

	assert.Equal(t, "hello", readAll(t, ws, "sub/a.txt"))
}

func TestOpenReadsAbsolutePathInside(t *testing.T) {
	dir := t.TempDir()
	realDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	ws := newWorkspace(t, dir, nil)

	assert.Equal(t, "hello", readAll(t, ws, filepath.Join(dir, "a.txt")))
	assert.Equal(t, "hello", readAll(t, ws, filepath.Join(realDir, "a.txt")))
}

func TestOpenAbsoluteWorkspaceRootSpellings(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "data")
	require.NoError(t, os.Mkdir(dir, 0o755))
	alias := filepath.Join(parent, "alias")
	require.NoError(t, os.Symlink("data", alias))
	ws := newWorkspace(t, alias, nil)
	want, err := os.Stat(dir)
	require.NoError(t, err)

	for _, base := range []string{ws.given, ws.realDir} {
		for _, suffix := range []string{"", "/", "///", "/.", "//.//"} {
			path := base + suffix
			t.Run(path, func(t *testing.T) {
				f, err := ws.Open(path)
				require.NoError(t, err)
				defer f.Close()
				got, err := f.Stat()
				require.NoError(t, err)
				assert.True(t, got.IsDir())
				assert.True(t, os.SameFile(want, got))
			})
		}
	}
}

func TestOpenRefusesPathsOutsideWorkspace(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ws")
	writeFile(t, filepath.Join(dir, "sub", "in.txt"), "in")
	writeFile(t, filepath.Join(parent, "out.txt"), "secret")
	require.NoError(t, os.Symlink(filepath.Join(parent, "out.txt"), filepath.Join(dir, "abs-link")))
	require.NoError(t, os.Symlink("../out.txt", filepath.Join(dir, "rel-link")))
	ws := newWorkspace(t, dir, nil)

	for _, tc := range []struct {
		name, path string
	}{
		{"dot-dot escape", "../out.txt"},
		{"nested dot-dot escape", "sub/../../out.txt"},
		{"absolute path outside", filepath.Join(parent, "out.txt")},
		{"absolute symlink outside", "abs-link"},
		{"relative symlink outside", "rel-link"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ws.Open(tc.path)
			require.ErrorIs(t, err, ErrOutsideWorkspace)
			assert.Nil(t, f)
			assert.Contains(t, err.Error(), tc.path)
		})
	}
}

func TestOpenFollowsSymlinkStayingInside(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sub", "a.txt"), "hello")
	require.NoError(t, os.Symlink("sub/a.txt", filepath.Join(dir, "link")))
	ws := newWorkspace(t, dir, nil)

	assert.Equal(t, "hello", readAll(t, ws, "link"))
}

// syntheticHome builds a fake home directory holding credential stores and a
// Kopi home, and returns the home and the Kopi home paths.
func syntheticHome(t *testing.T) (home, kopiHome string) {
	t.Helper()
	home = t.TempDir()
	kopiHome = filepath.Join(home, "kopi-home")
	writeFile(t, filepath.Join(home, ".ssh", "id_x"), "private key")
	writeFile(t, filepath.Join(home, ".aws", "credentials"), "aws secret")
	writeFile(t, filepath.Join(home, ".claude.json"), "{}")
	writeFile(t, filepath.Join(kopiHome, "config.json"), "{}")
	writeFile(t, filepath.Join(home, ".env"), "TOKEN=1")
	return home, kopiHome
}

// protectedStores lists the Kopi home and the stores under home that the tests
// exercise: directories, a file replaced by rename, a nested path and stores
// that do not exist yet.
func protectedStores(home, kopiHome string) []string {
	paths := []string{kopiHome}
	for _, name := range []string{".ssh", ".aws", ".claude.json", ".config/gh", ".codex", ".netrc", ".docker"} {
		paths = append(paths, filepath.Join(home, filepath.FromSlash(name)))
	}
	return paths
}

func TestOpenRefusesProtectedStoresInsideHomeWorkspace(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))

	for _, path := range []string{
		".ssh/id_x",
		".ssh",
		".aws/credentials",
		".claude.json",
		"kopi-home/config.json",
		filepath.Join(home, ".ssh", "id_x"),
	} {
		t.Run(path, func(t *testing.T) {
			f, err := ws.Open(path)
			require.ErrorIs(t, err, ErrProtected)
			assert.Nil(t, f)
			assert.Contains(t, err.Error(), path)
			assert.NotContains(t, err.Error(), "private key")
		})
	}
}

func TestOpenRefusesSymlinkedCredentialStore(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	require.NoError(t, os.Symlink(".ssh", filepath.Join(home, "link")))
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))

	_, err := ws.Open("link/id_x")
	require.ErrorIs(t, err, ErrProtected)
}

func TestOpenRefusesCredentialStoreThatIsSymlink(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	require.NoError(t, os.RemoveAll(filepath.Join(home, ".ssh")))
	writeFile(t, filepath.Join(home, "dotfiles", "ssh", "id_x"), "private key")
	require.NoError(t, os.Symlink(filepath.Join("dotfiles", "ssh"), filepath.Join(home, ".ssh")))
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))

	for _, path := range []string{".ssh/id_x", "dotfiles/ssh/id_x"} {
		f, err := ws.Open(path)
		require.ErrorIs(t, err, ErrProtected, path)
		assert.Nil(t, f)
	}
}

func TestOpenRefusesCaseVariantOfProtectedStore(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	if _, err := os.Stat(filepath.Join(home, ".SSH")); err != nil {
		t.Skip("filesystem is case-sensitive")
	}
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))

	_, err := ws.Open(".SSH/id_x")
	require.ErrorIs(t, err, ErrProtected)
}

func TestOpenRefusesMissingPathsInProtectedStore(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	require.NoError(t, os.Symlink(".ssh", filepath.Join(home, "link")))
	require.NoError(t, os.Symlink("missing", filepath.Join(home, ".ssh", "dangling")))
	require.NoError(t, os.Symlink("../.env", filepath.Join(home, ".ssh", "outward")))
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "x"), filepath.Join(home, ".ssh", "escape")))
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))

	for _, path := range []string{
		".ssh/nonexistent",
		".ssh/nodir/x",
		".ssh/id_x/x",
		"link/nonexistent",
		"kopi-home/nonexistent",
		".ssh/dangling",
		".ssh/outward",
		".ssh/escape",
	} {
		t.Run(path, func(t *testing.T) {
			f, err := ws.Open(path)
			require.ErrorIs(t, err, ErrProtected)
			assert.Nil(t, f)
			assert.NotErrorIs(t, err, fs.ErrNotExist)
		})
	}
}

func TestOpenRefusesFileStoreReplacedByRename(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))
	tmp := filepath.Join(home, ".claude.json.tmp")
	writeFile(t, tmp, `{"token":"new"}`)
	require.NoError(t, os.Rename(tmp, filepath.Join(home, ".claude.json")))

	_, err := ws.Open(".claude.json")
	require.ErrorIs(t, err, ErrProtected)
}

func TestOpenRefusesStoreCreatedAfterStart(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))
	writeFile(t, filepath.Join(home, ".config", "gh", "hosts.yml"), "token")

	for _, path := range []string{".config/gh/hosts.yml", ".config/gh/missing"} {
		_, err := ws.Open(path)
		require.ErrorIs(t, err, ErrProtected, path)
	}
}

func TestOpenRefusesStoreRecreatedAfterStart(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))
	require.NoError(t, os.RemoveAll(filepath.Join(home, ".aws")))
	writeFile(t, filepath.Join(home, ".aws", "credentials"), "aws secret")

	_, err := ws.Open(".aws/credentials")
	require.ErrorIs(t, err, ErrProtected)
}

func TestOpenSkipsProtectedPathUnderRegularFile(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	writeFile(t, filepath.Join(home, ".config"), "not a directory")
	writeFile(t, filepath.Join(home, "notes.txt"), "hello")
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))

	assert.Equal(t, "hello", readAll(t, ws, "notes.txt"))
	require.NoError(t, ws.WriteFile("notes.txt", []byte("updated")))
	assert.Equal(t, "updated", readAll(t, ws, "notes.txt"))
	_, err := ws.Open(".ssh/id_x")
	require.ErrorIs(t, err, ErrProtected)
}

func TestNewWorkspaceRejectsRelativeProtectedPath(t *testing.T) {
	_, err := NewWorkspace(t.TempDir(), []string{".ssh"})
	require.Error(t, err)
}

func TestOpenReadsDotEnvInWorkspace(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))

	assert.Equal(t, "TOKEN=1", readAll(t, ws, ".env"))
}

func TestOpenReportsMissingFile(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	require.NoError(t, os.Mkdir(filepath.Join(home, "sub"), 0o755))
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))

	for _, path := range []string{"missing.txt", "sub/missing.txt", "nodir/missing.txt"} {
		_, err := ws.Open(path)
		require.ErrorIs(t, err, fs.ErrNotExist, path)
		assert.Contains(t, err.Error(), path)
	}
}

func TestNewWorkspaceSkipsMissingProtectedEntries(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	ws := newWorkspace(t, dir, protectedStores(filepath.Join(dir, "no-home"), filepath.Join(dir, "no-kopi")))

	assert.Equal(t, "hello", readAll(t, ws, "a.txt"))
}

func TestOpenRefusesEverythingInWorkspaceInsideProtectedStore(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	dir := filepath.Join(home, ".ssh", "project")
	writeFile(t, filepath.Join(dir, "readme.txt"), "hello")
	ws := newWorkspace(t, dir, protectedStores(home, kopiHome))

	for _, path := range []string{"readme.txt", "."} {
		_, err := ws.Open(path)
		require.ErrorIs(t, err, ErrProtected, path)
	}
}

// listDir returns the names in dir, so a test can assert nothing extra, such
// as a temp file, was left behind.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteFileCreatesFileAndParents(t *testing.T) {
	dir := t.TempDir()
	ws := newWorkspace(t, dir, nil)

	require.NoError(t, ws.WriteFile("a/b/c.txt", []byte("hello")))
	require.NoError(t, ws.WriteFile(filepath.Join(dir, "d.txt"), []byte("")))
	// A file created the usual way shows what the umask allows.
	reference := filepath.Join(t.TempDir(), "ref")
	require.NoError(t, os.WriteFile(reference, nil, 0o666))

	got, err := os.ReadFile(filepath.Join(dir, "a", "b", "c.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
	info, err := os.Stat(filepath.Join(dir, "a", "b", "c.txt"))
	require.NoError(t, err)
	want, err := os.Stat(reference)
	require.NoError(t, err)
	assert.Equal(t, want.Mode().Perm(), info.Mode().Perm())
	assert.Equal(t, []string{"c.txt"}, listDir(t, filepath.Join(dir, "a", "b")))
	assert.Equal(t, []string{"a", "d.txt"}, listDir(t, dir))
}

func TestWriteFileReplacesKeepingMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.sh")
	writeFile(t, path, "old")
	require.NoError(t, os.Chmod(path, 0o750))
	ws := newWorkspace(t, dir, nil)

	require.NoError(t, ws.WriteFile("run.sh", []byte("new")))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o750), info.Mode().Perm())
	assert.Equal(t, []string{"run.sh"}, listDir(t, dir))
}

func TestWriteFileReplacesKeepingPrivateMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private.txt")
	writeFile(t, path, "old")
	require.NoError(t, os.Chmod(path, 0o600))
	ws := newWorkspace(t, dir, nil)

	require.NoError(t, ws.WriteFile("private.txt", []byte("new")))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
	assert.Equal(t, []string{"private.txt"}, listDir(t, dir))
}

func TestWriteFileRefusesPathsOutsideWorkspace(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ws")
	require.NoError(t, os.Mkdir(dir, 0o755))
	writeFile(t, filepath.Join(parent, "out.txt"), "secret")
	require.NoError(t, os.Symlink(filepath.Join(parent, "out.txt"), filepath.Join(dir, "file-link")))
	require.NoError(t, os.Symlink(parent, filepath.Join(dir, "dir-link")))
	ws := newWorkspace(t, dir, nil)

	for _, path := range []string{
		"../out.txt",
		"../new.txt",
		filepath.Join(parent, "new.txt"),
		"file-link",
		"dir-link/new.txt",
	} {
		t.Run(path, func(t *testing.T) {
			err := ws.WriteFile(path, []byte("x"))
			require.ErrorIs(t, err, ErrOutsideWorkspace)
		})
	}
	got, err := os.ReadFile(filepath.Join(parent, "out.txt"))
	require.NoError(t, err)
	assert.Equal(t, "secret", string(got))
	assert.ElementsMatch(t, []string{"out.txt", "ws"}, listDir(t, parent))
}

func TestWriteFileRefusesProtectedStores(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	require.NoError(t, os.Symlink(".ssh", filepath.Join(home, "link")))
	require.NoError(t, os.Symlink(filepath.Join(home, ".config", "gh", "hosts.yml"), filepath.Join(home, "gh-link")))
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))
	before := listDir(t, home)

	for _, path := range []string{
		".ssh/id_x",
		".ssh/new",
		".ssh",
		"link/new",
		".claude.json",
		"kopi-home/config.json",
		// Stores that do not exist yet: the write would create them.
		".config/gh/hosts.yml",
		".codex/auth.json",
		".netrc",
		"gh-link",
		filepath.Join(home, ".docker", "config.json"),
	} {
		t.Run(path, func(t *testing.T) {
			err := ws.WriteFile(path, []byte("x"))
			require.ErrorIs(t, err, ErrProtected)
		})
	}
	assert.Equal(t, before, listDir(t, home))
	assert.Equal(t, []string{"id_x"}, listDir(t, filepath.Join(home, ".ssh")))
	got, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	require.NoError(t, err)
	assert.Equal(t, "{}", string(got))
}

func TestWriteFileRefusesCaseVariantOfMissingStore(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	if _, err := os.Stat(filepath.Join(home, ".SSH")); err != nil {
		t.Skip("filesystem is case-sensitive")
	}
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))
	before := listDir(t, home)

	err := ws.WriteFile(".Config/GH/hosts.yml", []byte("x"))
	require.ErrorIs(t, err, ErrProtected)
	assert.Equal(t, before, listDir(t, home))
}

func TestWriteFileWritesOutsideStores(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	ws := newWorkspace(t, home, protectedStores(home, kopiHome))

	require.NoError(t, ws.WriteFile(".config/other/settings.json", []byte("{}")))
	require.NoError(t, ws.WriteFile(".ssh-notes.txt", []byte("hi")))
}

func TestWriteFileRefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	ws := newWorkspace(t, dir, nil)

	for _, path := range []string{"sub", "."} {
		err := ws.WriteFile(path, []byte("x"))
		require.ErrorIs(t, err, ErrIsDirectory, path)
	}
	assert.Equal(t, []string{"sub"}, listDir(t, dir))
	assert.Empty(t, listDir(t, filepath.Join(dir, "sub")))
}

func TestWriteFileWritesThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sub", "a.txt"), "old")
	require.NoError(t, os.Symlink("sub/a.txt", filepath.Join(dir, "link")))
	require.NoError(t, os.Symlink("sub/new.txt", filepath.Join(dir, "dangling")))
	ws := newWorkspace(t, dir, nil)

	require.NoError(t, ws.WriteFile("link", []byte("new")))
	require.NoError(t, ws.WriteFile("dangling", []byte("created")))

	got, err := os.ReadFile(filepath.Join(dir, "sub", "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
	got, err = os.ReadFile(filepath.Join(dir, "sub", "new.txt"))
	require.NoError(t, err)
	assert.Equal(t, "created", string(got))
	for _, link := range []string{"link", "dangling"} {
		info, err := os.Lstat(filepath.Join(dir, link))
		require.NoError(t, err)
		assert.NotZero(t, info.Mode()&fs.ModeSymlink, link)
	}
	assert.ElementsMatch(t, []string{"a.txt", "new.txt"}, listDir(t, filepath.Join(dir, "sub")))
}

func TestPreparedWritePinsParentAcrossSymlinkReplacement(t *testing.T) {
	for _, suffix := range []string{"file", "missing/deep/file"} {
		t.Run(suffix, func(t *testing.T) {
			home, kopiHome := syntheticHome(t)
			require.NoError(t, os.Mkdir(filepath.Join(home, "ordinary"), 0o755))
			ws := newWorkspace(t, home, []string{kopiHome})
			dest, err := ws.prepareWrite("ordinary/"+suffix, "ordinary/"+suffix)
			require.NoError(t, err)
			defer func() { dest.parent.Close() }()
			before := listDir(t, kopiHome)
			start, done := make(chan struct{}), make(chan error)
			go func() {
				<-start
				if err := os.Rename(filepath.Join(home, "ordinary"), filepath.Join(home, "pinned")); err != nil {
					done <- err
					return
				}
				done <- os.Symlink("kopi-home", filepath.Join(home, "ordinary"))
			}()
			close(start)
			require.NoError(t, <-done)
			err = dest.write(ws, []byte("new"))
			if suffix == "file" {
				require.NoError(t, err)
				assert.Equal(t, "new", readContent(t, filepath.Join(home, "pinned", "file")))
			} else {
				require.ErrorIs(t, err, ErrProtected)
				assert.Equal(t, []string{"missing"}, listDir(t, filepath.Join(home, "pinned")))
				assert.Empty(t, listDir(t, filepath.Join(home, "pinned", "missing")))
			}
			assert.Equal(t, before, listDir(t, kopiHome))
			assert.Equal(t, "{}", readContent(t, filepath.Join(kopiHome, "config.json")))
		})
	}
}

func TestPrepareWriteRejectsReplacementBeforeAcquisition(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	require.NoError(t, os.Mkdir(filepath.Join(home, "ordinary"), 0o755))
	ws := newWorkspace(t, home, []string{kopiHome})
	target, protected, err := ws.resolve("ordinary/missing/file")
	require.NoError(t, err)
	require.False(t, protected)
	require.NoError(t, os.Rename(filepath.Join(home, "ordinary"), filepath.Join(home, "pinned")))
	require.NoError(t, os.Symlink("kopi-home", filepath.Join(home, "ordinary")))
	rel, err := filepath.Rel(ws.realDir, target)
	require.NoError(t, err)
	dest, err := ws.prepareWrite(rel, rel)
	require.ErrorIs(t, err, ErrProtected)
	assert.Nil(t, dest)
	assert.Equal(t, []string{"config.json"}, listDir(t, kopiHome))
}

func TestPreparedWriteVetsExistingMissingChild(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "ordinary", "store")
	writeFile(t, filepath.Join(store, "config.json"), "{}")
	ws := newWorkspace(t, dir, []string{store})
	dest, err := ws.prepareWrite("ordinary/missing/deep/file", "ordinary/missing/deep/file")
	require.NoError(t, err)
	defer func() { dest.parent.Close() }()
	require.NoError(t, os.Symlink("store", filepath.Join(dir, "ordinary", "missing")))
	require.ErrorIs(t, dest.write(ws, []byte("new")), ErrProtected)
	assert.Equal(t, []string{"config.json"}, listDir(t, store))
}

func TestPreparedWriteRefusesAbsentStoreThroughInstalledSymlink(t *testing.T) {
	for _, suffix := range []string{"home", "home/config.json"} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			store := filepath.Join(dir, "ordinary", "store")
			require.NoError(t, os.MkdirAll(store, 0o755))
			ws := newWorkspace(t, dir, []string{filepath.Join(store, "home")})
			path := "ordinary/missing/" + suffix
			dest, err := ws.prepareWrite(path, path)
			require.NoError(t, err)
			defer func() { dest.parent.Close() }()
			require.NoError(t, os.Symlink("store", filepath.Join(dir, "ordinary", "missing")))

			assert.ErrorIs(t, dest.write(ws, []byte("new")), ErrProtected)
			assert.Empty(t, listDir(t, store))
		})
	}
}

func TestWorkspaceAcceptsRedundantAbsoluteSeparators(t *testing.T) {
	dir, realDir := symlinkDotDotLayout(t)
	ws := newWorkspace(t, dir, nil)
	for _, base := range []string{ws.given, ws.realDir} {
		for _, suffix := range []string{"//secrets/key", "///./secrets/key", "//docs/link/../key"} {
			path := base + suffix
			t.Run(path, func(t *testing.T) {
				f, err := ws.Open(path)
				require.NoError(t, err)
				require.NoError(t, f.Close())
				target, err := ws.Target(path)
				require.NoError(t, err)
				assert.Equal(t, filepath.Join(realDir, "secrets", "key"), target.Real)
				require.NoError(t, ws.WriteFile(path, []byte("updated")))
				assert.Equal(t, "updated", readAll(t, ws, path))
			})
		}
	}
	for _, suffix := range []string{"//secrets/", "///./secrets/."} {
		path := dir + suffix
		f, err := ws.Open(path)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		target, err := ws.Target(path)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(realDir, "secrets"), target.Real)
		assert.ErrorIs(t, ws.WriteFile(path, nil), ErrIsDirectory)
	}
}

func TestWorkspaceRejectsNonDirectoryTraversal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "file")
	writeFile(t, filepath.Join(dir, "victim"), "unchanged")
	require.NoError(t, os.Symlink("a.txt", filepath.Join(dir, "link")))
	ws := newWorkspace(t, dir, nil)
	before := listDir(t, dir)
	for _, name := range []string{"a.txt", "link"} {
		for _, suffix := range []string{"/../victim", "/child/../../victim", "/.", "/", "/child"} {
			for _, prefix := range []string{"", dir + "/", dir + "//", dir + "///./"} {
				path := prefix + name + suffix
				t.Run(path, func(t *testing.T) {
					_, err := ws.Target(path)
					assert.ErrorIs(t, err, syscall.ENOTDIR)
					f, err := ws.Open(path)
					assert.ErrorIs(t, err, syscall.ENOTDIR)
					if f != nil {
						f.Close()
					}
					assert.ErrorIs(t, ws.WriteFile(path, []byte("changed")), syscall.ENOTDIR)
					assert.Equal(t, "unchanged", readContent(t, filepath.Join(dir, "victim")))
					assert.Equal(t, before, listDir(t, dir))
				})
			}
		}
	}
}

func TestWorkspaceRejectsMissingTerminalDirectory(t *testing.T) {
	for _, spelling := range []string{"new/", "new/.", "missing/new/", "missing/new/.", "new//.//", "new/child/.."} {
		for _, absolute := range []bool{false, true} {
			for _, symlink := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/absolute=%t/symlink=%t", spelling, absolute, symlink), func(t *testing.T) {
					dir := t.TempDir()
					ws := newWorkspace(t, dir, nil)
					path := spelling
					if absolute {
						path = dir + "/" + path
					}
					if symlink {
						require.NoError(t, os.Symlink(path, filepath.Join(dir, "link")))
						path = "link"
					}
					before := listDir(t, dir)

					_, err := ws.Target(path)
					assert.ErrorIs(t, err, syscall.ENOTDIR)
					assert.ErrorIs(t, ws.WriteFile(path, []byte("changed")), syscall.ENOTDIR)
					assert.Equal(t, before, listDir(t, dir))
					if symlink {
						info, err := os.Lstat(filepath.Join(dir, "link"))
						require.NoError(t, err)
						assert.NotZero(t, info.Mode()&fs.ModeSymlink)
					}
				})
			}
		}
	}
}

func TestTargetResolvesPaths(t *testing.T) {
	parent := t.TempDir()
	realParent, err := filepath.EvalSymlinks(parent)
	require.NoError(t, err)
	dir := filepath.Join(parent, "ws")
	writeFile(t, filepath.Join(dir, "sub", "a.txt"), "x")
	require.NoError(t, os.Symlink("sub", filepath.Join(dir, "dir-link")))
	require.NoError(t, os.Symlink("sub/a.txt", filepath.Join(dir, "file-link")))
	require.NoError(t, os.Symlink("../missing/x", filepath.Join(dir, "sub", "dangling")))
	ws := newWorkspace(t, dir, nil)
	realDir := filepath.Join(realParent, "ws")

	for _, tc := range []struct {
		name, path, wantPath, wantReal string
	}{
		{"relative", "sub/a.txt", filepath.Join(dir, "sub", "a.txt"), filepath.Join(realDir, "sub", "a.txt")},
		{"cleaned", "./sub//../sub/a.txt", filepath.Join(dir, "sub", "a.txt"), filepath.Join(realDir, "sub", "a.txt")},
		{"file symlink", "file-link", filepath.Join(dir, "file-link"), filepath.Join(realDir, "sub", "a.txt")},
		{"missing tail under symlink", "dir-link/new/b.txt", filepath.Join(dir, "dir-link", "new", "b.txt"), filepath.Join(realDir, "sub", "new", "b.txt")},
		{"dangling symlink", "sub/dangling", filepath.Join(dir, "sub", "dangling"), filepath.Join(realDir, "missing", "x")},
		{"outside", filepath.Join(parent, "other"), filepath.Join(parent, "other"), filepath.Join(realParent, "other")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ws.Target(tc.path)
			require.NoError(t, err)
			assert.Equal(t, tc.wantPath, got.Path)
			assert.Equal(t, tc.wantReal, got.Real)
		})
	}
}

// symlinkDotDotLayout builds ws/docs/link -> ../secrets/sub, so "docs/link/.."
// is ws/secrets physically but ws/docs lexically, with a decoy at each.
func symlinkDotDotLayout(t *testing.T) (dir, realDir string) {
	t.Helper()
	dir = t.TempDir()
	realDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	writeFile(t, filepath.Join(dir, "secrets", "key"), "secret")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "secrets", "sub"), 0o755))
	writeFile(t, filepath.Join(dir, "docs", "key"), "decoy")
	require.NoError(t, os.Symlink("../secrets/sub", filepath.Join(dir, "docs", "link")))
	return dir, realDir
}

func TestTargetIsTheFileOpenReads(t *testing.T) {
	dir, realDir := symlinkDotDotLayout(t)
	ws := newWorkspace(t, dir, nil)

	for _, path := range []string{
		"docs/link/../key",
		filepath.Join(dir, "docs", "link") + "/../key",
	} {
		t.Run(path, func(t *testing.T) {
			got, err := ws.Target(path)
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(realDir, "secrets", "key"), got.Real)
			assert.Equal(t, "secret", readAll(t, ws, path))
		})
	}
	got, err := ws.Target("docs/link/../key")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "docs", "key"), got.Path)
}

func TestTargetIsTheFileWriteFileCreates(t *testing.T) {
	dir, realDir := symlinkDotDotLayout(t)
	ws := newWorkspace(t, dir, nil)

	for _, tc := range []struct{ path, want string }{
		{"docs/link/../new", "secrets/new"},
		{filepath.Join(dir, "docs", "link") + "/../abs", "secrets/abs"},
		// os.Root cannot open through a missing component, but WriteFile
		// creates it, so the walk goes on past it physically.
		{"missing/../docs/link/../deep/x", "secrets/deep/x"},
		{"docs/link/../key", "secrets/key"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got, err := ws.Target(tc.path)
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(realDir, tc.want), got.Real)
			require.NoError(t, ws.WriteFile(tc.path, []byte("written")))
			assert.Equal(t, "written", readContent(t, got.Real))
		})
	}
	assert.Equal(t, "decoy", readContent(t, filepath.Join(dir, "docs", "key")))
	assert.ElementsMatch(t, []string{"key", "link"}, listDir(t, filepath.Join(dir, "docs")))
	assert.ElementsMatch(t, []string{"docs", "secrets"}, listDir(t, dir))
}

func TestTargetFollowsSymlinkBeforeDotDotIntoGit(t *testing.T) {
	dir := t.TempDir()
	realDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755))
	require.NoError(t, os.Symlink(".git/hooks", filepath.Join(dir, "x")))
	ws := newWorkspace(t, dir, nil)

	got, err := ws.Target("x/../config")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "config"), got.Path)
	assert.Equal(t, filepath.Join(realDir, ".git", "config"), got.Real)
}

func TestTargetAppliesPlainDotDot(t *testing.T) {
	dir, realDir := symlinkDotDotLayout(t)
	ws := newWorkspace(t, dir, nil)

	got, err := ws.Target("docs/../secrets/key")
	require.NoError(t, err)
	assert.Equal(t, policy.Target{Path: filepath.Join(dir, "secrets", "key"), Real: filepath.Join(realDir, "secrets", "key")}, got)
}

func TestTargetFollowsWorkspaceAfterItsSymlinkIsRepointed(t *testing.T) {
	parent := t.TempDir()
	realParent, err := filepath.EvalSymlinks(parent)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(parent, "data"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(parent, "other"), 0o755))
	link := filepath.Join(parent, "proj")
	require.NoError(t, os.Symlink("data", link))
	ws := newWorkspace(t, link, nil)
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink("other", link))

	path := filepath.Join(link, "secrets", "x")
	got, err := ws.Target(path)
	require.NoError(t, err)
	assert.Equal(t, path, got.Path)
	assert.Equal(t, filepath.Join(realParent, "data", "secrets", "x"), got.Real)
	require.NoError(t, ws.WriteFile(path, []byte("written")))
	assert.Equal(t, "written", readContent(t, got.Real))
	assert.Empty(t, listDir(t, filepath.Join(parent, "other")))
}

func TestPreparedWriteRejectsNewTraversal(t *testing.T) {
	for _, replacement := range []string{"directory", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			dir := t.TempDir()
			ws := newWorkspace(t, dir, nil)
			require.NoError(t, os.Mkdir(filepath.Join(dir, "denied"), 0o755))
			dest, err := ws.prepare("missing/deep/file")
			require.NoError(t, err)
			defer func() { dest.parent.Close() }()
			if replacement == "directory" {
				require.NoError(t, os.Mkdir(filepath.Join(dir, "missing"), 0o755))
			} else {
				require.NoError(t, os.Symlink("denied", filepath.Join(dir, "missing")))
			}
			require.ErrorIs(t, dest.write(ws, []byte("new")), ErrTargetChanged)
			assert.Empty(t, listDir(t, filepath.Join(dir, "denied")))
		})
	}
}

func TestPrepareWriteRejectsUnprotectedSymlinkAfterResolution(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "allowed", "file"), "allowed")
	writeFile(t, filepath.Join(dir, "denied", "file"), "denied")
	ws := newWorkspace(t, dir, nil)
	target, err := ws.Target("allowed/file")
	require.NoError(t, err)
	require.NoError(t, os.Rename(filepath.Join(dir, "allowed"), filepath.Join(dir, "pinned")))
	require.NoError(t, os.Symlink("denied", filepath.Join(dir, "allowed")))
	rel, err := filepath.Rel(ws.realDir, target.Real)
	require.NoError(t, err)
	dest, err := ws.prepareWrite(rel, "allowed/file")
	require.ErrorIs(t, err, ErrTargetChanged)
	assert.Nil(t, dest)
	assert.Equal(t, "denied", readContent(t, filepath.Join(dir, "denied", "file")))
}

func TestPreparedEditDetectsConflicts(t *testing.T) {
	for _, change := range []string{"unchanged", "same-size", "rename-over", "deletion", "symlink"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file")
			writeFile(t, path, "alpha")
			writeFile(t, filepath.Join(dir, "other"), "alpha")
			require.NoError(t, os.Chmod(path, 0o640))
			ws := newWorkspace(t, dir, nil)
			dest, err := ws.prepare("file")
			require.NoError(t, err)
			defer func() { dest.parent.Close() }()
			f, err := dest.open()
			require.NoError(t, err)
			defer f.Close()
			content, err := readForEdit(f, "file")
			require.NoError(t, err)
			dest.content = []byte(content)
			dest.original, err = f.Stat()
			require.NoError(t, err)
			switch change {
			case "same-size":
				require.NoError(t, os.WriteFile(path, []byte("bravo"), 0o600))
			case "rename-over":
				require.NoError(t, os.Rename(filepath.Join(dir, "other"), path))
			case "deletion":
				require.NoError(t, os.Remove(path))
			case "symlink":
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Symlink("other", path))
			}
			err = dest.write(ws, []byte("updated"))
			if change == "unchanged" {
				require.NoError(t, err)
				assert.Equal(t, "updated", readContent(t, path))
				info, err := os.Stat(path)
				require.NoError(t, err)
				assert.Equal(t, fs.FileMode(0o640), info.Mode().Perm())
			} else {
				require.ErrorIs(t, err, ErrEditConflict)
				if change == "same-size" {
					assert.Equal(t, "bravo", readContent(t, path))
				}
			}
			for _, name := range listDir(t, dir) {
				assert.NotContains(t, name, ".kopi-")
			}
		})
	}
}

func TestWriteFileLongBasename(t *testing.T) {
	dir := t.TempDir()
	name := strings.Repeat("a", 240)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("probe"), 0o600); errors.Is(err, syscall.ENAMETOOLONG) {
		t.Skip("filesystem does not support 240-byte basenames")
	} else {
		require.NoError(t, err)
	}
	require.NoError(t, os.Remove(path))
	ws := newWorkspace(t, dir, nil)
	require.NoError(t, ws.WriteFile(name, []byte("created")))
	assert.Equal(t, "created", readContent(t, path))
	require.NoError(t, os.Chmod(path, 0o640))
	require.NoError(t, ws.WriteFile(name, []byte("replaced")))
	assert.Equal(t, "replaced", readContent(t, path))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o640), info.Mode().Perm())
	assert.Equal(t, []string{name}, listDir(t, dir))
}

func TestWorkspaceShown(t *testing.T) {
	dir := t.TempDir()
	ws := newWorkspace(t, dir, nil)
	plans := NewPlans(filepath.Join(t.TempDir(), "plans"))
	require.NoError(t, plans.Select("s1"))
	withPlans := ws.WithPlans(plans)

	for _, tc := range []struct{ name, path, want string }{
		{"relative inside", "sub/a.go", "sub/a.go"},
		{"absolute inside", filepath.Join(dir, "sub", "a.go"), "sub/a.go"},
		{"absolute start directory", dir, "."},
		{"relative outside", "../x", "../x"},
		{"absolute outside", "/elsewhere/x", "/elsewhere/x"},
		{"home spelling is kept", "~/x", "~/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ws.shown(tc.path))
			assert.Equal(t, tc.want, withPlans.shown(tc.path))
		})
	}

	t.Run("plan file", func(t *testing.T) {
		assert.Equal(t, "plan", withPlans.shown(plans.Path()))
		assert.Equal(t, "plan", withPlans.shown(filepath.Join(filepath.Dir(plans.Path()), ".", filepath.Base(plans.Path()))))
		assert.Equal(t, plans.Path(), ws.shown(plans.Path()))
	})
	t.Run("another plan file", func(t *testing.T) {
		other := filepath.Join(filepath.Dir(plans.Path()), "s2.md")
		assert.Equal(t, other, withPlans.shown(other))
	})
}

// newPlanWorkspace returns a workspace over dir that reaches a plan file and a
// saved output folder, and the paths of both.
func newPlanWorkspace(t *testing.T, dir string) (ws *Workspace, plan, outputs string) {
	t.Helper()
	plans := NewPlans(filepath.Join(t.TempDir(), "plans"))
	require.NoError(t, plans.Select("s1"))
	saved, outputDir := newSavedOutputs(t)
	return newWorkspace(t, dir, nil).WithPlans(plans).WithOutputs(saved), plans.Path(), outputDir
}

func TestWorkspaceFileOutcome(t *testing.T) {
	dir := t.TempDir()
	ws, plan, outputs := newPlanWorkspace(t, dir)
	outcome := ws.fileOutcome("Read")
	args := func(path string) json.RawMessage {
		raw, err := json.Marshal(map[string]string{"file_path": path})
		require.NoError(t, err)
		return raw
	}

	for _, tc := range []struct {
		name, path string
		want       Action
	}{
		{"plan", plan, Action{Verb: "Read", One: "the plan"}},
		{"output file", filepath.Join(outputs, "bash-1.txt"), Action{Verb: "Read", One: "output file", Many: "output files"}},
		{"workspace file", "a.go", Action{}},
		{"workspace file named plan", "plan", Action{}},
		{"workspace file named like an output", "output bash-1.txt", Action{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done, action := outcome(args(tc.path), "")
			assert.Empty(t, done)
			assert.Equal(t, tc.want, action)
		})
	}
	_, action := outcome(json.RawMessage(`{`), "")
	assert.Equal(t, Action{}, action)
}

func TestWorkspaceFileSubject(t *testing.T) {
	dir := t.TempDir()
	ws, plan, outputs := newPlanWorkspace(t, dir)
	subject := func(path string) string {
		raw, err := json.Marshal(map[string]string{"file_path": path})
		require.NoError(t, err)
		return ws.fileSubject(raw, "")
	}

	assert.Equal(t, "a.go", subject("a.go"))
	assert.Equal(t, subject("a.go"), subject(filepath.Join(dir, "a.go")), "one file however it is written")
	assert.Equal(t, subject("a.go"), subject("./a.go"))
	assert.Equal(t, subject("a.go"), subject(dir+"/./a.go"))
	assert.Equal(t, "plan", subject(plan))
	assert.Equal(t, "output bash-1.txt", subject(filepath.Join(outputs, "bash-1.txt")))
	assert.Empty(t, ws.fileSubject(json.RawMessage(`{`), ""))
}

// addedDir adds a new directory to ws and returns it.
func addedDir(t *testing.T, ws *Workspace) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, ws.Add(dir, realPath(t, dir)))
	return dir
}

func TestAddedDirectoryIsReachedThroughItsView(t *testing.T) {
	ws := newWorkspace(t, t.TempDir(), nil)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	path := filepath.Join(dir, "a.txt")

	_, err := ws.in(path).Open(path)
	require.ErrorIs(t, err, ErrOutsideWorkspace)
	require.NoError(t, ws.Add(dir, realPath(t, dir)))

	assert.Equal(t, "hello", readAll(t, ws.in(path), path))
	assert.Equal(t, "hello", readAll(t, ws.in(path), filepath.Join(realPath(t, dir), "a.txt")))
	_, err = ws.Open(path)
	require.ErrorIs(t, err, ErrOutsideWorkspace)
	require.ErrorIs(t, ws.WriteFile(path, nil), ErrOutsideWorkspace)
	require.NoError(t, ws.in(path).WriteFile(filepath.Join(dir, "new", "b.txt"), []byte("b")))
	assert.Equal(t, "b", readContent(t, filepath.Join(dir, "new", "b.txt")))
	got, err := ws.in(path).Target(path)
	require.NoError(t, err)
	assert.Equal(t, policy.Target{Path: path, Real: filepath.Join(realPath(t, dir), "a.txt")}, got)
	assert.True(t, ws.in(path).extra)
	assert.False(t, ws.in("a.txt").extra)
}

func realPath(t *testing.T, dir string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return real
}

func TestAddedDirectoryRefusesProtectedStores(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	ws := newWorkspace(t, t.TempDir(), protectedStores(home, kopiHome))
	require.NoError(t, ws.Add(home, realPath(t, home)))

	for _, path := range []string{
		filepath.Join(home, ".ssh", "id_x"),
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".claude.json"),
		filepath.Join(kopiHome, "config.json"),
	} {
		t.Run(path, func(t *testing.T) {
			view := ws.in(path)
			f, err := view.Open(path)
			require.ErrorIs(t, err, ErrProtected)
			assert.Nil(t, f)
			require.ErrorIs(t, view.WriteFile(path, []byte("x")), ErrProtected)
		})
	}
	require.ErrorIs(t, ws.in(filepath.Join(home, ".netrc")).WriteFile(filepath.Join(home, ".netrc"), []byte("x")), ErrProtected)
	assert.Equal(t, "TOKEN=1", readAll(t, ws.in(filepath.Join(home, ".env")), filepath.Join(home, ".env")))
}

func TestAddedDirectoryPreparedWritePinsParent(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	require.NoError(t, os.Mkdir(filepath.Join(home, "ordinary"), 0o755))
	ws := newWorkspace(t, t.TempDir(), []string{kopiHome})
	require.NoError(t, ws.Add(home, realPath(t, home)))
	path := filepath.Join(home, "ordinary", "missing", "file")
	view := ws.in(path)
	dest, err := view.prepare(path)
	require.NoError(t, err)
	defer func() { dest.parent.Close() }()
	require.NoError(t, os.Rename(filepath.Join(home, "ordinary"), filepath.Join(home, "pinned")))
	require.NoError(t, os.Symlink("kopi-home", filepath.Join(home, "ordinary")))

	require.ErrorIs(t, dest.write(view, []byte("new")), ErrProtected)
	assert.Equal(t, []string{"missing"}, listDir(t, filepath.Join(home, "pinned")))
	assert.Empty(t, listDir(t, filepath.Join(home, "pinned", "missing")))
	assert.Equal(t, "{}", readContent(t, filepath.Join(kopiHome, "config.json")))
}

func TestAddedDirectoryTargetFollowsItsSymlinkAfterRepoint(t *testing.T) {
	parent := t.TempDir()
	realParent := realPath(t, parent)
	require.NoError(t, os.Mkdir(filepath.Join(parent, "data"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(parent, "other"), 0o755))
	link := filepath.Join(parent, "proj")
	require.NoError(t, os.Symlink("data", link))
	ws := newWorkspace(t, t.TempDir(), nil)
	require.NoError(t, ws.Add(link, realPath(t, link)))
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink("other", link))

	path := filepath.Join(link, "secrets", "x")
	view := ws.in(path)
	got, err := view.Target(path)
	require.NoError(t, err)
	assert.Equal(t, path, got.Path)
	assert.Equal(t, filepath.Join(realParent, "data", "secrets", "x"), got.Real)
	require.NoError(t, view.WriteFile(path, []byte("written")))
	assert.Equal(t, "written", readContent(t, got.Real))
	assert.Empty(t, listDir(t, filepath.Join(parent, "other")))
}

func TestAddedDirectoryTargetIsTheFileOpenReads(t *testing.T) {
	dir, realDir := symlinkDotDotLayout(t)
	ws := newWorkspace(t, t.TempDir(), nil)
	require.NoError(t, ws.Add(dir, realPath(t, dir)))
	path := filepath.Join(dir, "docs", "link") + "/../key"

	view := ws.in(path)
	got, err := view.Target(path)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(realDir, "secrets", "key"), got.Real)
	assert.Equal(t, "secret", readAll(t, view, path))
}

func TestCanAdd(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	start := filepath.Join(home, "start")
	require.NoError(t, os.MkdirAll(filepath.Join(start, "inside"), 0o755))
	ws := newWorkspace(t, start, protectedStores(home, kopiHome))
	other := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(other, "inner"), 0o755))
	require.NoError(t, ws.Add(other, realPath(t, other)))
	writeFile(t, filepath.Join(other, "file.txt"), "")

	for name, tc := range map[string]struct {
		dir  string
		want error
	}{
		"start directory":      {start, ErrInWorkspace},
		"inside start":         {filepath.Join(start, "inside"), ErrInWorkspace},
		"added directory":      {other, ErrInWorkspace},
		"inside added":         {filepath.Join(other, "inner"), ErrInWorkspace},
		"protected":            {filepath.Join(home, ".ssh"), ErrProtected},
		"Kopi home":            {kopiHome, ErrProtected},
		"regular file":         {filepath.Join(other, "file.txt"), ErrNotDirectory},
		"below a regular file": {filepath.Join(other, "file.txt", "x"), ErrNotDirectory},
		"missing":              {filepath.Join(home, "missing"), fs.ErrNotExist},
		"parent of start":      {home, nil},
		"unrelated directory":  {t.TempDir(), nil},
		"parent of added":      {filepath.Dir(other), nil},
	} {
		t.Run(name, func(t *testing.T) {
			err := ws.CanAdd(tc.dir)
			if tc.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.want)
		})
	}
	require.NoError(t, os.Mkdir(filepath.Join(kopiHome, "sub"), 0o755))
	require.ErrorIs(t, ws.CanAdd(filepath.Join(kopiHome, "sub")), ErrProtected)
	require.NoError(t, os.Symlink(filepath.Join(home, ".ssh"), filepath.Join(other, "link")))
	require.ErrorIs(t, ws.CanAdd(filepath.Join(other, "link")), ErrProtected)
}

func TestAddRefusesWhatCanAddRefuses(t *testing.T) {
	ws := newWorkspace(t, t.TempDir(), nil)
	dir := addedDir(t, ws)

	require.ErrorIs(t, ws.Add(dir, realPath(t, dir)), ErrInWorkspace)
	assert.Len(t, ws.more.dirs, 1)
}

func TestAddOfTheSameDirectoryRacesOnce(t *testing.T) {
	ws := newWorkspace(t, t.TempDir(), nil)
	dir := t.TempDir()
	real := realPath(t, dir)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range cap(errs) {
		wg.Go(func() { errs <- ws.Add(dir, real) })
	}
	wg.Wait()
	close(errs)

	added := 0
	for err := range errs {
		if err == nil {
			added++
			continue
		}
		require.ErrorIs(t, err, ErrInWorkspace)
	}
	assert.Equal(t, 1, added)
}

func TestAddedDirectorySpellings(t *testing.T) {
	ws := newWorkspace(t, t.TempDir(), nil)
	given := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(given, alias))
	writeFile(t, filepath.Join(given, "a.txt"), "hello")
	require.NoError(t, ws.Add(alias, realPath(t, given)))

	for _, path := range []string{filepath.Join(alias, "a.txt"), filepath.Join(realPath(t, given), "a.txt")} {
		assert.Equal(t, "hello", readAll(t, ws.in(path), path), path)
	}
}

func TestAddRefusesARealPathOfAnotherDirectory(t *testing.T) {
	ws := newWorkspace(t, t.TempDir(), nil)
	given, other := t.TempDir(), t.TempDir()

	require.Error(t, ws.Add(given, realPath(t, other)))
	assert.Empty(t, ws.more.dirs)
}

func TestAddRefusesARealPathReplacedBySymlink(t *testing.T) {
	parent := t.TempDir()
	a, b := filepath.Join(parent, "a"), filepath.Join(parent, "b")
	require.NoError(t, os.Mkdir(a, 0o755))
	writeFile(t, filepath.Join(b, "secret.txt"), "secret")
	canonical := realPath(t, a)
	require.NoError(t, os.Remove(a))
	require.NoError(t, os.Symlink(b, a))
	ws := newWorkspace(t, t.TempDir(), nil)

	require.Error(t, ws.Add(a, canonical))
	assert.Empty(t, ws.more.dirs)
	path := filepath.Join(canonical, "secret.txt")
	_, err := ws.Open(path)
	assert.ErrorIs(t, err, ErrOutsideWorkspace)
}

func TestAddedDirectoryIsReachedThroughBothCaseSpellings(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only darwin respells names as on disk")
	}
	parent := realPath(t, t.TempDir())
	onDisk := filepath.Join(parent, "Other")
	typed := filepath.Join(parent, "other")
	writeFile(t, filepath.Join(onDisk, "a.txt"), "hello")
	ws := newWorkspace(t, t.TempDir(), nil)
	require.NoError(t, ws.Add(typed, onDisk))

	for _, dir := range []string{typed, onDisk} {
		path := filepath.Join(dir, "a.txt")
		view := ws.in(path)
		assert.Equal(t, "hello", readAll(t, view, path), path)
		got, err := view.Target(path)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(onDisk, "a.txt"), got.Real)
		require.NoError(t, view.WriteFile(filepath.Join(dir, "b.txt"), []byte("b")), path)
		assert.Equal(t, "b", readContent(t, filepath.Join(onDisk, "b.txt")))
	}
	assert.Equal(t, filepath.Join(onDisk, "x"), ws.in(typed+"/x").named("x"))
	require.ErrorIs(t, ws.CanAdd(onDisk), ErrInWorkspace)
}

func TestNestedRootsPickTheStartDirectoryFirst(t *testing.T) {
	parent := t.TempDir()
	start := filepath.Join(parent, "b")
	require.NoError(t, os.Mkdir(start, 0o755))
	ws := newWorkspace(t, start, nil)
	require.NoError(t, ws.Add(parent, realPath(t, parent)))

	assert.Same(t, ws, ws.in(filepath.Join(start, "x")))
	assert.Same(t, ws, ws.in("x"))
	view := ws.in(filepath.Join(parent, "c", "x"))
	assert.True(t, view.extra)
	assert.Equal(t, realPath(t, parent), view.realDir)
}

func TestCopiesMadeBeforeTheAddReachTheAddedDirectory(t *testing.T) {
	base := newWorkspace(t, t.TempDir(), nil)
	plans := NewPlans(filepath.Join(t.TempDir(), "plans"))
	require.NoError(t, plans.Select("s1"))
	saved, outputs := newSavedOutputs(t)
	writeFile(t, filepath.Join(outputs, "o.txt"), "saved")
	copied := base.WithPlans(plans).WithOutputs(saved)
	dir := addedDir(t, base)
	path := filepath.Join(dir, "a.txt")
	writeFile(t, path, "hello")

	assert.Equal(t, "hello", readAll(t, copied.in(path), path))
	view := copied.in(path)
	assert.True(t, view.extra)
	require.NoError(t, view.WriteFile(plans.Path(), []byte("plan")))
	assert.Equal(t, "plan", readContent(t, plans.Path()))
	assert.False(t, copied.in(plans.Path()).extra)
	output := filepath.Join(outputs, "o.txt")
	dest, err := view.prepare(output)
	require.NoError(t, err)
	dest.parent.Close()
	assert.True(t, dest.output)
	require.ErrorIs(t, view.WriteFile(output, []byte("x")), ErrProtected)
}

func TestAddedDirectoryViewKeepsPlanAndOutputFiles(t *testing.T) {
	ws, plan, outputs := newPlanWorkspace(t, t.TempDir())
	home := t.TempDir()
	require.NoError(t, ws.Add(home, realPath(t, home)))
	view := ws.in(filepath.Join(home, "x"))

	assert.Same(t, view.plans, ws.plans)
	assert.Same(t, view.outputs, ws.outputs)
	assert.True(t, view.isPlan(plan))
	assert.True(t, view.isOutput(filepath.Join(outputs, "o.txt")))
}

func TestInSkipsAViewWhoseMatchClimbsOut(t *testing.T) {
	parent := realPath(t, t.TempDir())
	start := filepath.Join(parent, "s")
	require.NoError(t, os.Mkdir(start, 0o755))
	writeFile(t, filepath.Join(parent, "other", "f"), "hello")
	ws := newWorkspace(t, start, nil)
	require.NoError(t, ws.Add(parent, parent))
	path := filepath.Join(start, "..", "other", "f")

	view := ws.in(path)

	assert.True(t, view.extra)
	assert.Equal(t, "hello", readAll(t, view, path))
}
