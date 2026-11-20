package tool

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/keyute/kopi/internal/policy"
)

const (
	// maxOutputFile bounds one saved output file, so a runaway command cannot
	// fill the disk.
	maxOutputFile = 64 << 20
	// outputMaxAge is how long Prune keeps a saved output file.
	outputMaxAge = 7 * 24 * time.Hour
)

// Outputs holds the root session's saved tool output, <dir>/<session id>/<name>.txt.
// A Workspace made with WithOutputs reaches those files, one at a time and read
// only, although they lie in Kopi's home, which the workspace otherwise refuses.
// A nil *Outputs saves nothing and names no file.
type Outputs struct {
	dir     string
	mu      sync.Mutex
	session string
}

func NewOutputs(dir string) *Outputs {
	return &Outputs{dir: filepath.Clean(dir)}
}

// Select makes the folder of sessionID the one new output goes to. The folder
// is created when output first needs it. An id that is not one plain name
// clears the selection.
func (o *Outputs) Select(sessionID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.session = ""
	if !plainName(sessionID) {
		return fmt.Errorf("session id %q is not usable in an output folder name", sessionID)
	}
	o.session = sessionID
	return nil
}

// sessionDir is the selected session's folder, or "" when none is selected. It
// reads the selection once, so one operation uses one folder.
func (o *Outputs) sessionDir() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.session == "" {
		return ""
	}
	return filepath.Join(o.dir, o.session)
}

// buffer returns an output buffer that saves to the folder selected now, so a
// straggler never lands in a later session's folder.
func (o *Outputs) buffer() *outputBuffer {
	return &outputBuffer{dir: o.sessionDir(), limit: maxOutputFile}
}

// holds reports whether abs names a file directly in the selected session's
// folder, and returns that folder.
func (o *Outputs) holds(abs string) (string, bool) {
	dir := o.sessionDir()
	clean := filepath.Clean(abs)
	return dir, dir != "" && filepath.Dir(clean) == dir
}

// target answers for abs when it is a saved output file. Real is built from the
// canonical folder and the leaf, which is not followed.
func (o *Outputs) target(abs string) (policy.Target, bool, error) {
	dir, ok := o.holds(abs)
	if !ok {
		return policy.Target{}, false, nil
	}
	clean := filepath.Clean(abs)
	canon, _, err := walk(string(filepath.Separator), dir, nil)
	if err != nil {
		return policy.Target{Path: clean}, true, err
	}
	return policy.Target{Path: clean, Real: filepath.Join(canon, filepath.Base(clean))}, true, nil
}

// prepare answers for abs when it is a saved output file; path is the caller's
// spelling, used in messages.
func (o *Outputs) prepare(abs, path string) (*writeDestination, bool, error) {
	t, ok, err := o.target(abs)
	if !ok || err != nil {
		return nil, ok, err
	}
	root, err := pinSession(filepath.Dir(t.Path), path, false)
	if err != nil {
		return nil, true, err
	}
	return &writeDestination{target: t, parent: root, rel: ".", path: path, leaf: filepath.Base(t.Path), output: true}, true, nil
}

// pinSession pins the session folder dir through its parent, the outputs
// folder, which must be a real directory as the plans folder must be; both
// are refused when they are symlinks. With create, a missing session folder is
// made first. path is the caller's spelling, used in messages.
func pinSession(dir, path string, create bool) (*os.Root, error) {
	outer, err := pinDir(filepath.Dir(dir), path)
	if err != nil {
		return nil, err
	}
	defer outer.Close()
	name := filepath.Base(dir)
	if create {
		if err := outer.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("failed to create %s: %w", path, err)
		}
	}
	before, err := outer.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
	}
	if !before.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrProtected, path)
	}
	root, err := outer.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(before, opened) {
		root.Close()
		return nil, fmt.Errorf("%w: %s", ErrTargetChanged, path)
	}
	return root, nil
}

// Prune removes saved output older than outputMaxAge, then the session folders
// that are left empty. A missing folder has nothing to prune; other failures
// are only logged.
func (o *Outputs) Prune(now time.Time) {
	// A symlinked folder is not Kopi's to prune.
	if info, err := os.Lstat(o.dir); err != nil || !info.IsDir() {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("failed to list saved outputs", "dir", o.dir, "err", err)
		}
		return
	}
	sessions, err := os.ReadDir(o.dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("failed to list saved outputs", "dir", o.dir, "err", err)
		}
		return
	}
	for _, s := range sessions {
		if !s.IsDir() {
			continue
		}
		dir := filepath.Join(o.dir, s.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			slog.Warn("failed to list saved outputs", "dir", dir, "err", err)
			continue
		}
		left := len(files)
		for _, e := range files {
			fi, err := e.Info()
			if err != nil || !e.Type().IsRegular() || now.Sub(fi.ModTime()) <= outputMaxAge {
				continue
			}
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				slog.Warn("failed to remove a saved output", "path", filepath.Join(dir, e.Name()), "err", err)
				continue
			}
			left--
		}
		if left == 0 {
			// Fails harmlessly if a file arrived since the listing.
			_ = os.Remove(dir)
		}
	}
}
