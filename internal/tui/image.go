package tui

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// imageMaxAge is how long a saved clipboard image is kept; drafts and queued
// messages that refer to it are long gone by then.
const imageMaxAge = 24 * time.Hour

var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true}

// unescape undoes a shell's backslash escaping, as terminals apply to a
// dropped file's path.
var unescape = regexp.MustCompile(`\\(.)`)

// imagePath reports whether s is one absolute path to an image file, as a
// terminal pastes a dropped file: plain, quoted, backslash-escaped or as a
// file:// URL.
func imagePath(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, "\n") {
		return "", false
	}
	if rest, ok := strings.CutPrefix(s, "file://"); ok {
		p, err := url.PathUnescape(rest)
		if err != nil {
			return "", false
		}
		s = p
	}
	if n := len(s); n >= 2 && (s[0] == '"' || s[0] == '\'') && s[n-1] == s[0] {
		s = s[1 : n-1]
	} else {
		s = unescape.ReplaceAllString(s, "$1")
	}
	if rest, ok := strings.CutPrefix(s, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		s = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(s) || !imageExts[strings.ToLower(filepath.Ext(s))] {
		return "", false
	}
	if fi, err := os.Stat(s); err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return s, true
}

// saveImage writes PNG bytes to a new file in dir.
func saveImage(dir string, png []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("failed to create the image directory: %w", err)
	}
	path := filepath.Join(dir, strconv.FormatInt(time.Now().UnixNano(), 10)+".png")
	if err := os.WriteFile(path, png, 0o600); err != nil {
		return "", fmt.Errorf("failed to save the clipboard image: %w", err)
	}
	return path, nil
}

// pruneImages removes files in dir older than imageMaxAge. A missing dir has
// nothing to prune; other failures are only logged.
func pruneImages(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("failed to list saved images", "dir", dir, "err", err)
		}
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil || now.Sub(fi.ModTime()) <= imageMaxAge {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			slog.Warn("failed to remove a saved image", "path", filepath.Join(dir, e.Name()), "err", err)
		}
	}
}
