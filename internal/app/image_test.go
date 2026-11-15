package app

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	return b.Bytes()
}

func pngFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shot.png")
	require.NoError(t, os.WriteFile(path, pngBytes(t), 0o600))
	return path
}

// badImages returns one path per way an attachment is refused.
func badImages(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	text := filepath.Join(dir, "notes.png")
	require.NoError(t, os.WriteFile(text, []byte("not an image\n"), 0o600))
	// A PNG header, then sparse bytes up to one past the limit.
	large := filepath.Join(dir, "large.png")
	require.NoError(t, os.WriteFile(large, pngBytes(t), 0o600))
	require.NoError(t, os.Truncate(large, maxImageBytes+1))
	sub := filepath.Join(dir, "dir.png")
	require.NoError(t, os.Mkdir(sub, 0o700))
	return map[string]string{
		"wrong type": text,
		"too large":  large,
		"missing":    filepath.Join(dir, "missing.png"),
		"directory":  sub,
	}
}

func TestLoadImagesRefusesBadFiles(t *testing.T) {
	good := pngFile(t)
	for name, path := range badImages(t) {
		t.Run(name, func(t *testing.T) {
			images, err := loadImages([]string{good, path})
			require.ErrorIs(t, err, ErrBadImage)
			assert.Contains(t, err.Error(), "failed to attach "+path+": ")
			assert.Nil(t, images)
		})
	}
}

func TestLoadImagesReadsImages(t *testing.T) {
	path := pngFile(t)
	gif := filepath.Join(t.TempDir(), "a.gif")
	require.NoError(t, os.WriteFile(gif, []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), 0o600))

	images, err := loadImages([]string{path, gif})
	require.NoError(t, err)
	require.Len(t, images, 2)
	assert.Equal(t, path, images[0].Path)
	assert.Equal(t, "image/png", images[0].MimeType)
	assert.Equal(t, pngBytes(t), images[0].Data)
	assert.Equal(t, gif, images[1].Path)
	assert.Equal(t, "image/gif", images[1].MimeType)

	images, err = loadImages(nil)
	require.NoError(t, err)
	assert.Nil(t, images)
}
