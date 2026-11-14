// Package history is the prompt history shared by every session in a
// workspace, persisted as JSON lines in one file for all workspaces.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type entry struct {
	Workspace string    `json:"workspace"`
	Text      string    `json:"text"`
	Time      time.Time `json:"time"`
}

// Store holds the newest entries of one workspace, oldest first. It is safe
// for concurrent use.
type Store struct {
	mu        sync.Mutex
	path      string
	workspace string
	limit     int
	texts     []string
}

// Open loads the last limit entries for workspace from path. A missing file is
// an empty store. On a read error the returned store is still usable, holding
// whatever was read before the failure.
func Open(path, workspace string, limit int) (*Store, error) {
	s := &Store{path: path, workspace: workspace, limit: limit}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("failed to open history %s: %w", path, err)
	}
	defer f.Close()

	// ReadBytes, not a Scanner: a pasted prompt can exceed any token limit.
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		var e entry
		if len(line) > 0 && json.Unmarshal(line, &e) == nil && e.Workspace == workspace {
			s.push(e.Text)
		}
		if errors.Is(err, io.EOF) {
			return s, nil
		}
		if err != nil {
			return s, fmt.Errorf("failed to read history %s: %w", path, err)
		}
	}
}

// Texts returns a copy of the entries, oldest first.
func (s *Store) Texts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}

// Add records text unless it repeats the newest entry. The entry stays in
// memory even when persisting it fails.
func (s *Store) Add(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.texts); n > 0 && s.texts[n-1] == text {
		return nil
	}
	s.push(text)

	line, err := json.Marshal(entry{Workspace: s.workspace, Text: text, Time: time.Now()})
	if err != nil {
		return fmt.Errorf("failed to encode history entry: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("failed to create history directory: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("failed to open history %s: %w", s.path, err)
	}
	// One write per entry so concurrent kopi processes appending to the same
	// file do not interleave within a line.
	_, err = f.Write(append(line, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("failed to append history %s: %w", s.path, err)
	}
	return nil
}

func (s *Store) push(text string) {
	s.texts = append(s.texts, text)
	if len(s.texts) > s.limit {
		s.texts = s.texts[len(s.texts)-s.limit:]
	}
}
