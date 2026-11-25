// Package trust stores whether the human trusts a folder, which gates whether
// anything the folder supplies, such as its .mcp.json servers, is used. The
// trust file is the only place these decisions are kept.
package trust

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/gofrs/flock"
)

const version = 1

type Decision int

const (
	Trusted Decision = iota + 1
	Untrusted
)

var names = map[Decision]string{Trusted: "trusted", Untrusted: "untrusted"}

type file struct {
	Version int               `json:"version"`
	Folders map[string]string `json:"folders"`
}

// Store holds decisions keyed by canonical folder path.
type Store struct {
	path    string
	folders map[string]Decision
}

// Open loads the trust file at path; a missing file is an empty store.
func Open(path string) (*Store, error) {
	s := &Store{path: path, folders: map[string]Decision{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read trust file %s: %w", path, err)
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("failed to parse trust file %s: %w", path, err)
	}
	if f.Version != version {
		return nil, fmt.Errorf("%w: %d in %s, want %d", ErrUnsupportedVersion, f.Version, path, version)
	}
	for dir, name := range f.Folders {
		d, ok := parse(name)
		if !ok {
			return nil, fmt.Errorf("%w: %q for %s in %s", ErrInvalidDecision, name, dir, path)
		}
		s.folders[dir] = d
	}
	return s, nil
}

func parse(name string) (Decision, bool) {
	for d, n := range names {
		if n == name {
			return d, true
		}
	}
	return 0, false
}

// Lookup returns the decision saved for dir or, failing that, for its closest
// ancestor that has one.
func (s *Store) Lookup(dir string) (d Decision, ok bool, err error) {
	dir, err = Canonical(dir)
	if err != nil {
		return 0, false, err
	}
	for {
		if d, ok := s.folders[dir]; ok {
			return d, true, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0, false, nil
		}
		dir = parent
	}
}

// Set records the decision for dir and rewrites the trust file. It applies the
// change to the file as it is now, so a decision another process saved since
// Open is kept.
func (s *Store) Set(dir string, d Decision) (err error) {
	dir, err = Canonical(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("failed to create trust file directory: %w", err)
	}
	// The sidecar stays in place because write replaces the data inode.
	lock := flock.New(s.path + ".lock")
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("failed to lock trust file %s: %w", s.path, err)
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil && err == nil {
			err = fmt.Errorf("failed to unlock trust file %s: %w", s.path, unlockErr)
		}
	}()
	fresh, err := Open(s.path)
	if err != nil {
		return err
	}
	fresh.folders[dir] = d
	if err := fresh.write(); err != nil {
		return err
	}
	s.folders = fresh.folders
	return nil
}

// Canonical is dir absolute, cleaned and with symlinks resolved, as decisions
// are keyed. On darwin each name is also spelled as on disk: APFS is usually
// case-insensitive and the working directory keeps the case it was typed in.
func Canonical(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve folder %s: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("failed to resolve folder %s: %w", dir, err)
	}
	if runtime.GOOS == "darwin" {
		return onDiskNames(resolved), nil
	}
	return resolved, nil
}

// Broad reports whether dir is the user's home or a filesystem root: an answer
// saved for it would cover almost every folder below, so it holds for this run
// only. Both are put through Canonical first, so the home is recognised through
// a symlink or in another case, and either must exist.
func Broad(dir, userHome string) (bool, error) {
	dir, err := Canonical(dir)
	if err != nil {
		return false, err
	}
	userHome, err = Canonical(userHome)
	if err != nil {
		return false, err
	}
	return dir == userHome || filepath.Dir(dir) == dir, nil
}

// onDiskNames rebuilds path, which has no symlinks, from the names its
// parents list for it. A name that cannot be checked, or that no entry of its
// parent matches, stays as given.
func onDiskNames(path string) string {
	out := string(filepath.Separator)
	for _, name := range strings.Split(path, string(filepath.Separator)) {
		if name == "" {
			continue
		}
		parent := out
		out = filepath.Join(parent, name)
		entries, err := os.ReadDir(parent)
		if err != nil || slices.ContainsFunc(entries, func(e fs.DirEntry) bool { return e.Name() == name }) {
			continue
		}
		info, err := os.Lstat(out)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if ei, err := e.Info(); err == nil && os.SameFile(info, ei) {
				out = filepath.Join(parent, e.Name())
				break
			}
		}
	}
	return out
}

// write replaces the file through a rename so a reader never sees it half
// written.
func (s *Store) write() error {
	f := file{Version: version, Folders: map[string]string{}}
	for dir, d := range s.folders {
		f.Folders[dir] = names[d]
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode trust file: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".trust-*.json") // created 0600
	if err != nil {
		return fmt.Errorf("failed to create trust file: %w", err)
	}
	_, err = tmp.Write(append(b, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), s.path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("failed to write trust file %s: %w", s.path, err)
	}
	return nil
}
