// Package skill models the skills Kopi offers: where project skills live, how
// a skill reads to the model, and the commands a user types to load one.
package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

type Skill struct {
	Name, Description, Body string
	// Dir is the directory holding the skill file.
	Dir                    string
	DisableModelInvocation bool
}

// Find lists the existing skill directories from dir up to the nearest
// ancestor holding a .git (a directory or a worktree's file), closest first;
// dir alone when no ancestor holds one. A result equal to exclude is left out.
// It reads no file contents.
func Find(dir, exclude string) []string {
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
	var found []string
	for _, d := range dirs {
		path := filepath.Join(d, ".agents", "skills")
		if path == exclude {
			continue
		}
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			found = append(found, path)
		}
	}
	return found
}

// Text is how a loaded skill reads to the model.
func Text(s Skill) string {
	return fmt.Sprintf(`<skill name="%s" directory="%s">`+"\n%s\n</skill>", s.Name, s.Dir, strings.TrimSpace(s.Body))
}

// Set is the skills a user can type as commands.
type Set struct {
	prefix string
	skills []Skill
}

// NewSet returns the typable skills. With a prefix every skill is typed as
// prefix:name; without one a skill whose name is in reserved is left out and
// its name returned, in input order.
func NewSet(prefix string, skills []Skill, reserved []string) (*Set, []string) {
	s := &Set{prefix: prefix}
	var dropped []string
	for _, sk := range skills {
		if prefix == "" && slices.Contains(reserved, sk.Name) {
			dropped = append(dropped, sk.Name)
			continue
		}
		s.skills = append(s.skills, sk)
	}
	slices.SortStableFunc(s.skills, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
	return s, dropped
}

// Skills returns the typable skills sorted by name.
func (s *Set) Skills() []Skill {
	if s == nil {
		return nil
	}
	return slices.Clone(s.skills)
}

// Command is what a user types to load sk, without the leading slash.
func (s *Set) Command(sk Skill) string {
	if s == nil || s.prefix == "" {
		return sk.Name
	}
	return s.prefix + ":" + sk.Name
}

// Lookup returns the skill a command, without its slash, names.
func (s *Set) Lookup(command string) (Skill, bool) {
	if s == nil {
		return Skill{}, false
	}
	for _, sk := range s.skills {
		if s.Command(sk) == command {
			return sk, true
		}
	}
	return Skill{}, false
}

// Token is a typed skill command: the rune offsets [Start, End) on logical
// line Line, split on "\n".
type Token struct {
	Line, Start, End int
	Skill            Skill
}

// Tokens returns every whole whitespace-delimited word of text that is a
// slash and a skill's command, in line then start order.
func (s *Set) Tokens(text string) []Token {
	if s == nil {
		return nil
	}
	var out []Token
	for line, l := range strings.Split(text, "\n") {
		runes := []rune(l)
		for i := 0; i < len(runes); {
			if unicode.IsSpace(runes[i]) {
				i++
				continue
			}
			start := i
			for i < len(runes) && !unicode.IsSpace(runes[i]) {
				i++
			}
			word := string(runes[start:i])
			if cmd, ok := strings.CutPrefix(word, "/"); ok {
				if sk, ok := s.Lookup(cmd); ok {
					out = append(out, Token{Line: line, Start: start, End: i, Skill: sk})
				}
			}
		}
	}
	return out
}

// Named returns the distinct skills Tokens finds, in first-seen order.
func (s *Set) Named(text string) []Skill {
	var out []Skill
	for _, tok := range s.Tokens(text) {
		if !slices.ContainsFunc(out, func(sk Skill) bool { return sk.Name == tok.Skill.Name }) {
			out = append(out, tok.Skill)
		}
	}
	return out
}
