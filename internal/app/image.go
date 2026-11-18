package app

import (
	"fmt"
	"io"
	"os"

	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/tool"
)

// loadImages reads every attachment or none: one bad path refuses them all.
func loadImages(paths []string) ([]pigruntime.Image, error) {
	var images []pigruntime.Image
	for _, path := range paths {
		img, err := loadImage(path)
		if err != nil {
			return nil, fmt.Errorf("failed to attach %s: %w", path, err)
		}
		images = append(images, img)
	}
	return images, nil
}

func loadImage(path string) (pigruntime.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return pigruntime.Image{}, fmt.Errorf("%w: %w", ErrBadImage, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return pigruntime.Image{}, fmt.Errorf("%w: %w", ErrBadImage, err)
	}
	if !info.Mode().IsRegular() {
		return pigruntime.Image{}, fmt.Errorf("%w: not a regular file", ErrBadImage)
	}
	if info.Size() > tool.MaxImageBytes {
		return pigruntime.Image{}, fmt.Errorf("%w: larger than %d MiB", ErrBadImage, tool.MaxImageBytes>>20)
	}
	// The limit also catches a file that grew since Stat.
	data, err := io.ReadAll(io.LimitReader(f, tool.MaxImageBytes+1))
	if err != nil {
		return pigruntime.Image{}, fmt.Errorf("%w: %w", ErrBadImage, err)
	}
	if len(data) > tool.MaxImageBytes {
		return pigruntime.Image{}, fmt.Errorf("%w: larger than %d MiB", ErrBadImage, tool.MaxImageBytes>>20)
	}
	mime, ok := tool.ImageType(data)
	if !ok {
		return pigruntime.Image{}, fmt.Errorf("%w: content is %s", ErrBadImage, mime)
	}
	return pigruntime.Image{Path: path, MimeType: mime, Data: data}, nil
}
