// Package sandbox confines commands with the operating system's sandbox.
package sandbox

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
)

// Policy is what a sandboxed command may do. Paths are absolute.
type Policy struct {
	Write     []string // roots commands may write under
	DenyRead  []string // paths commands may neither read nor write
	DenyWrite []string // paths commands may read but not write
	Network   bool
}

// Sandbox wraps commands so they run under one Policy.
type Sandbox struct{ prefix []string }

// New checks that the platform sandbox works and builds it for p. It returns
// ErrUnavailable when this platform has no sandbox or it cannot be applied.
func New(ctx context.Context, p Policy) (*Sandbox, error) {
	for _, list := range [][]string{p.Write, p.DenyRead, p.DenyWrite} {
		for _, path := range list {
			if !filepath.IsAbs(path) {
				return nil, fmt.Errorf("sandbox path %q is not absolute", path)
			}
		}
	}
	switch runtime.GOOS {
	case "darwin":
		if err := probe(ctx); err != nil {
			return nil, err
		}
		return &Sandbox{prefix: seatbeltPrefix(p)}, nil
	default:
		return nil, fmt.Errorf("%w on %s", ErrUnavailable, runtime.GOOS)
	}
}

// Wrap returns the argv that runs argv inside the sandbox.
func (s *Sandbox) Wrap(argv []string) []string {
	return slices.Concat(s.prefix, argv)
}
