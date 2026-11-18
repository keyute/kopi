// Package instruction finds and reads the instruction files Kopi adds to the
// model's instructions.
package instruction

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const FileName = "AGENTS.md"

// Root is a folder whose project files are loaded only when it is trusted.
type Root struct {
	Dir     string
	Trusted bool
}

type File struct{ Path, Text string }

// Group holds the files found from one directory: Kopi's home (the global
// file's directory) or a root's Dir.
type Group struct {
	Dir    string
	Global bool
	Files  []File
}

// Find lists dir's project files, outermost first; it reads none of them. The
// search runs from the nearest directory at or above dir holding a .git (a
// directory or a worktree's file) down to dir, or covers dir alone when there
// is none. Each directory contributes its AGENTS.md, else its first fallback
// that is a regular file.
func Find(dir string, fallbacks []string) []string {
	var dirs []string
	for d := dir; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			break
		}
		if filepath.Dir(d) == d {
			dirs = []string{dir}
			break
		}
	}
	slices.Reverse(dirs)
	var files []string
	for _, d := range dirs {
		for _, name := range append([]string{FileName}, fallbacks...) {
			path := filepath.Join(d, name)
			if isRegular(path) {
				files = append(files, path)
				break
			}
		}
	}
	return files
}

// Load returns the global file's group, then one group per trusted root, in
// order. A file that cannot be read is left out and reported; none is fatal.
// A path is loaded at most once, in the first group that lists it.
func Load(globalFile string, roots []Root, fallbacks []string) ([]Group, []error) {
	var groups []Group
	var errs []error
	loaded := map[string]bool{}
	add := func(dir string, global bool, paths []string) {
		g := Group{Dir: dir, Global: global}
		for _, path := range paths {
			if loaded[path] {
				continue
			}
			loaded[path] = true
			b, err := os.ReadFile(path)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to read instruction file %s: %w", path, err))
				continue
			}
			if text := strings.TrimSpace(string(b)); text != "" {
				g.Files = append(g.Files, File{Path: path, Text: text})
			}
		}
		if len(g.Files) > 0 {
			groups = append(groups, g)
		}
	}
	var global []string
	if isRegular(globalFile) {
		global = []string{globalFile}
	}
	add(filepath.Dir(globalFile), true, global)
	for _, r := range roots {
		if r.Trusted {
			add(r.Dir, false, Find(r.Dir, fallbacks))
		}
	}
	return groups, errs
}

func isRegular(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
