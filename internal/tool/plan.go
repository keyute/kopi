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
	if !plainName(sessionID) {
		return fmt.Errorf("session id %q is not usable in a plan file name", sessionID)
	}
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", p.dir, err)
	}
	p.name = sessionID + ".md"
	return nil
}

// plainName reports whether id is one plain, non-hidden file name.
func plainName(id string) bool {
	return id != "" && filepath.IsLocal(id) && filepath.Base(id) == id && !strings.HasPrefix(id, ".")
}

// Path is the absolute plan file path, or "" when none is selected. It reads
// the selected file once, so one operation uses one name.
func (p *Plans) Path() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.name == "" {
		return ""
	}
	return filepath.Join(p.dir, p.name)
}

// Read returns the plan's text; a missing file wraps fs.ErrNotExist.
func (p *Plans) Read() (string, error) {
	path := p.Path()
	if path == "" {
		return "", fmt.Errorf("failed to read the plan file: %w", fs.ErrNotExist)
	}
	dest, err := p.destination(path, path)
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

// planTarget builds the target from the canonical plans directory and the leaf,
// not the caller's spelling. The leaf is not followed: Write replaces a
// symlink there rather than writing through it. PlanFile is set even when
// resolving fails, so the gate can tell the plan file from other paths.
func (p *Plans) planTarget(path string) (policy.Target, error) {
	dir, _, err := walk(string(filepath.Separator), p.dir, nil)
	if err != nil {
		return policy.Target{Path: path, PlanFile: true}, err
	}
	return policy.Target{Path: path, Real: filepath.Join(dir, filepath.Base(path)), PlanFile: true}, nil
}

// target answers for abs when it is the plan file.
func (p *Plans) target(abs string) (policy.Target, bool, error) {
	path := p.Path()
	if path == "" || filepath.Clean(abs) != path {
		return policy.Target{}, false, nil
	}
	t, err := p.planTarget(path)
	return t, true, err
}

// prepare answers for abs when it is the plan file; path is the caller's
// spelling, used in messages.
func (p *Plans) prepare(abs, path string) (*writeDestination, bool, error) {
	t, ok, err := p.target(abs)
	if !ok || err != nil {
		return nil, ok, err
	}
	dest, err := p.destination(path, t.Path)
	if err != nil {
		return nil, true, err
	}
	dest.target = t
	return dest, true, nil
}

// destination pins the plans directory with pinDir. plan is the canonical plan
// file path, whose leaf is the one pinned.
func (p *Plans) destination(path, plan string) (*writeDestination, error) {
	root, err := pinDir(p.dir, path)
	if err != nil {
		return nil, err
	}
	return &writeDestination{parent: root, rel: ".", path: path, leaf: filepath.Base(plan), plan: true}, nil
}

// pinDir opens dir, which must be a real directory and not a symlink, with the
// same check, open and verify as prepareWrite.
func pinDir(dir, path string) (*os.Root, error) {
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
	return root, nil
}
