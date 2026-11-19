package tool

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/keyute/kopi/internal/policy"
)

// Plans holds the root session's plan file, <dir>/<session id>.md. A Workspace
// made with WithPlans reaches that one file although it lies in Kopi's home,
// which the workspace otherwise refuses. A nil *Plans names no file.
type Plans struct {
	dir  string
	mu   sync.Mutex
	name string
}

func NewPlans(dir string) *Plans {
	return &Plans{dir: filepath.Clean(dir)}
}

// Select names the plan file of sessionID and creates the directory. An id that
// is not one plain name clears the file.
func (p *Plans) Select(sessionID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.name = ""
	if sessionID == "" || !filepath.IsLocal(sessionID) || filepath.Base(sessionID) != sessionID || strings.HasPrefix(sessionID, ".") {
		return fmt.Errorf("session id %q is not usable in a plan file name", sessionID)
	}
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", p.dir, err)
	}
	p.name = sessionID + ".md"
	return nil
}

// Path is the absolute plan file path, or "" when none is selected.
func (p *Plans) Path() string {
	path, _ := p.current()
	return path
}

// current reads the selected file once, so one operation uses one name.
func (p *Plans) current() (path, name string) {
	if p == nil {
		return "", ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.name == "" {
		return "", ""
	}
	return filepath.Join(p.dir, p.name), p.name
}

// Read returns the plan's text; a missing file wraps fs.ErrNotExist.
func (p *Plans) Read() (string, error) {
	path, name := p.current()
	if path == "" {
		return "", fmt.Errorf("failed to read the plan file: %w", fs.ErrNotExist)
	}
	dest, err := p.destination(path, name)
	if err != nil {
		return "", err
	}
	defer dest.parent.Close()
	f, err := dest.open()
	if err != nil {
		return "", err
	}
	defer f.Close()
	return readForEdit(f, path)
}

// matches reports whether abs, cleaned, is exactly the plan file, and returns
// the path and name it matched.
func (p *Plans) matches(abs string) (path, name string, ok bool) {
	path, name = p.current()
	return path, name, path != "" && filepath.Clean(abs) == path
}

// planTarget builds the target from the canonical path, not the caller's
// spelling.
func planTarget(path string) (policy.Target, error) {
	real, _, err := walk(string(filepath.Separator), path, nil)
	if err != nil {
		return policy.Target{}, err
	}
	return policy.Target{Path: path, Real: real, PlanFile: true}, nil
}

// target answers for abs when it is the plan file.
func (p *Plans) target(abs string) (policy.Target, bool, error) {
	path, _, ok := p.matches(abs)
	if !ok {
		return policy.Target{}, false, nil
	}
	t, err := planTarget(path)
	return t, true, err
}

// prepare answers for abs when it is the plan file; path is the caller's
// spelling, used in messages.
func (p *Plans) prepare(abs, path string) (*writeDestination, bool, error) {
	canonical, name, ok := p.matches(abs)
	if !ok {
		return nil, false, nil
	}
	target, err := planTarget(canonical)
	if err != nil {
		return nil, true, err
	}
	dest, err := p.destination(path, name)
	if err != nil {
		return nil, true, err
	}
	dest.target = target
	return dest, true, nil
}

// destination pins the plans directory, which must be a real directory and not
// a symlink, with the same check, open and verify as prepareWrite. name is the
// leaf the caller matched.
func (p *Plans) destination(path, name string) (*writeDestination, error) {
	dir := p.dir
	before, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
	}
	if !before.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrProtected, path)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(before, opened) {
		root.Close()
		return nil, fmt.Errorf("%w: %s", ErrTargetChanged, path)
	}
	return &writeDestination{parent: root, rel: ".", path: path, leaf: name, plan: true}, nil
}
