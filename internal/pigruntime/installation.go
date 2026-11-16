package pigruntime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// installationID returns the installation's UUID stored at path, creating it
// on first use. It is returned hyphenated, the only form PiG's Sign in with
// ChatGPT accepts as a device ID.
func installationID(path string) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return "", fmt.Errorf("failed to create installation id %s: %w", path, err)
	}
	defer os.Remove(f.Name())

	id := uuid.NewString()
	_, err = f.WriteString(id)
	if err := errors.Join(err, f.Close()); err != nil {
		return "", fmt.Errorf("failed to write installation id %s: %w", path, err)
	}
	// Publish only the complete file, without replacing another caller's ID.
	err = os.Link(f.Name(), path)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("failed to create installation id %s: %w", path, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read installation id %s: %w", path, err)
	}
	parsed, err := uuid.Parse(strings.TrimSpace(string(b)))
	if err != nil {
		return "", fmt.Errorf("failed to read installation id %s: %w", path, err)
	}
	return parsed.String(), nil
}
