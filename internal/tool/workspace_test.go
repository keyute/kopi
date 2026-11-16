package tool

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
