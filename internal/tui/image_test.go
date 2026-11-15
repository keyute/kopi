package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeImage creates an image file named name in dir and returns its path.
func writeImage(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("png"), 0o600))
	return path
}

func TestImagePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "my pics")
	require.NoError(t, os.Mkdir(dir, 0o700))
	png := writeImage(t, dir, "a b.png")
	upper := writeImage(t, dir, "c.JPG")
	txt := writeImage(t, dir, "notes.txt")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "d.png"), 0o700))

	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain", png, png},
		{"trailing newline", png + "\n", png},
		{"double quoted", `"` + png + `"`, png},
		{"single quoted", "'" + png + "'", png},
		{"backslash escaped", strings.ReplaceAll(png, " ", `\ `), png},
		{"file url", "file://" + strings.ReplaceAll(png, " ", "%20"), png},
		{"home", "~/my pics/a b.png", png},
		{"upper-case extension", upper, upper},
		{"wrong extension", txt, ""},
		{"missing file", filepath.Join(dir, "gone.png"), ""},
		{"directory", filepath.Join(dir, "d.png"), ""},
		{"relative", "my pics/a b.png", ""},
		{"two lines", png + "\n" + upper, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := imagePath(tc.in)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want != "", ok)
		})
	}
}

func TestPruneImagesRemovesOnlyOldFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := writeImage(t, dir, "old.png")
	require.NoError(t, os.Chtimes(old, now.Add(-25*time.Hour), now.Add(-25*time.Hour)))
	fresh := writeImage(t, dir, "fresh.png")
	require.NoError(t, os.Chtimes(fresh, now.Add(-23*time.Hour), now.Add(-23*time.Hour)))

	pruneImages(dir, now)

	assert.NoFileExists(t, old)
	assert.FileExists(t, fresh)
}

func TestSaveImageWritesPrivateFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")

	path, err := saveImage(dir, []byte("png"))

	require.NoError(t, err)
	assert.Equal(t, dir, filepath.Dir(path))
	assert.True(t, strings.HasSuffix(path, ".png"), path)
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	di, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), di.Mode().Perm())
}
