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
	dir, err = canonical(dir)
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
	dir, err = canonical(dir)
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

func canonical(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve folder %s: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("failed to resolve folder %s: %w", dir, err)
	}
	return resolved, nil
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
