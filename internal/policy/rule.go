package policy

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Source is one origin of rules. A `!` exception only exempts calls from the
// list it is in, so one source can never loosen another's rules.
type Source struct {
	Name             string
	Allow, Ask, Deny []string
}

var BuiltIn = Source{Name: "built-in", Ask: []string{"Edit(.git)", "Write(.git)"}}

var ruleName = regexp.MustCompile(`^(\*|[A-Za-z_][A-Za-z0-9_]*)$`)

type rule struct {
	text   string
	name   string
	except bool
	// patterns are slash-separated absolute globs; nil for a bare rule, which
	// matches every call of its tool.
	patterns []string
}

type source struct {
	name             string
	allow, ask, deny []rule
}

type Rules struct {
	sources []source
	// fold matches case-insensitively, as the file systems of darwin and
	// windows do by default.
	fold bool
}

type effect int

const (
	effectNone effect = iota
	effectAllow
	effectAsk
	effectDeny
)

// ValidateRule checks only the syntax of s, not that its tool exists.
func ValidateRule(s string) error {
	_, _, _, err := parseRule(s)
	return err
}

func parseRule(s string) (name, spec string, except bool, err error) {
	name, rest, hasSpec := strings.Cut(s, "(")
	if !ruleName.MatchString(name) {
		return "", "", false, fmt.Errorf("rule %q: the name must be a tool name or *: %w", s, ErrInvalidRule)
	}
	if !hasSpec {
		return name, "", false, nil
	}
	spec, ok := strings.CutSuffix(rest, ")")
	if !ok || !balanced(spec) {
		return "", "", false, fmt.Errorf("rule %q: unbalanced parentheses: %w", s, ErrInvalidRule)
	}
	spec, except = strings.CutPrefix(spec, "!")
	if spec == "" {
		return "", "", false, fmt.Errorf("rule %q: empty path: %w", s, ErrInvalidRule)
	}
	if !doublestar.ValidatePattern(spec) {
		return "", "", false, fmt.Errorf("rule %q: invalid glob: %w", s, ErrInvalidRule)
	}
	return name, spec, except, nil
}

func balanced(s string) bool {
	depth := 0
	for _, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// NewRules parses every source's rules; a rule name must be * or one of tools.
// Workspace and home must be absolute.
func NewRules(workspace, home string, tools []string, sources ...Source) (*Rules, error) {
	for _, d := range []struct{ name, dir string }{{"workspace", workspace}, {"home", home}} {
		if _, err := filepath.EvalSymlinks(d.dir); err != nil {
			return nil, fmt.Errorf("failed to resolve %s: %w", d.name, err)
		}
	}
	ws, hm := escape(filepath.ToSlash(filepath.Clean(workspace))), escape(filepath.ToSlash(filepath.Clean(home)))
	r := &Rules{fold: runtime.GOOS == "darwin" || runtime.GOOS == "windows"}
	for _, src := range sources {
		s := source{name: src.Name}
		for _, l := range []struct {
			in  []string
			out *[]rule
		}{{src.Allow, &s.allow}, {src.Ask, &s.ask}, {src.Deny, &s.deny}} {
			for _, text := range l.in {
				name, spec, except, err := parseRule(text)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", src.Name, err)
				}
				if name != "*" && !slices.Contains(tools, name) {
					return nil, fmt.Errorf("%s: rule %q: unknown tool %q: %w", src.Name, text, name, ErrInvalidRule)
				}
				ru := rule{text: text, name: name, except: except}
				if spec != "" {
					ru.patterns = forms(expand(spec, ws, hm))
				}
				*l.out = append(*l.out, ru)
			}
		}
		r.sources = append(r.sources, s)
	}
	return r, nil
}

func escape(s string) string {
	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune(`*?[]{}\`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// expand makes spec an absolute glob; ws and home are already escaped.
func expand(spec, ws, home string) string {
	switch {
	case strings.HasPrefix(spec, "/"):
		return path.Clean(spec)
	case spec == "~" || strings.HasPrefix(spec, "~/"):
		return path.Join(home, strings.TrimPrefix(spec, "~"))
	case !strings.Contains(strings.TrimRight(spec, "/"), "/"):
		// As in .gitignore, a name with at most a trailing slash matches at any depth.
		return path.Join(ws, "**", spec)
	default:
		return path.Join(ws, spec)
	}
}

// forms returns pat and, if its literal leading directory resolves elsewhere
// through symlinks, the same glob under the resolved directory, so a call
// naming either side of a link meets the rule. Links are resolved once, here:
// one created or changed later is not seen.
func forms(pat string) []string {
	dir, tail := splitLiteral(pat)
	resolved := filepath.ToSlash(resolve(filepath.FromSlash(dir)))
	if resolved == dir {
		return []string{pat}
	}
	return []string{pat, path.Join(escape(resolved), tail)}
}

// splitLiteral splits pat at the last slash before its first unescaped glob
// metacharacter into the directory, unescaped, and the rest, still escaped. A
// pattern with no glob is literal throughout, so dir is all of it.
func splitLiteral(pat string) (dir, tail string) {
	var lit strings.Builder
	cut, litCut := 0, 0
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		switch {
		case c == '\\' && i+1 < len(pat):
			i++
			lit.WriteByte(pat[i])
		case strings.IndexByte("*?[{", c) >= 0:
			dir = lit.String()[:litCut]
			if dir == "" {
				dir = "/"
			}
			return dir, pat[cut:]
		default:
			if c == '/' {
				cut, litCut = i, lit.Len()
			}
			lit.WriteByte(c)
		}
	}
	return lit.String(), ""
}

// resolve resolves the symlinks of p's deepest ancestor that can be resolved
// and appends the rest of p unchanged.
func resolve(p string) string {
	var rest []string
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{r}, rest...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
}

// Removes reports whether a bare deny takes tool away from the model entirely.
// An exception for tool in the same list exempts some calls, so it keeps the
// tool available.
func (r *Rules) Removes(tool string) bool {
	if r == nil {
		return false
	}
	for _, s := range r.sources {
		bare, exempt := false, false
		for _, ru := range s.deny {
			if ru.name != "*" && ru.name != tool {
				continue
			}
			bare = bare || (!ru.except && ru.patterns == nil)
			exempt = exempt || ru.except
		}
		if bare && !exempt {
			return true
		}
	}
	return false
}

// decide returns the strongest effect any source's rules have on the call,
// with the matching rule's text and source. t is nil for a call on no path.
func (r *Rules) decide(tool string, t *Target) (eff effect, text, src string) {
	for _, l := range []struct {
		eff        effect
		tightening bool
		rules      func(source) []rule
	}{
		{effectDeny, true, func(s source) []rule { return s.deny }},
		{effectAsk, true, func(s source) []rule { return s.ask }},
		{effectAllow, false, func(s source) []rule { return s.allow }},
	} {
		for _, s := range r.sources {
			if text, ok := r.listMatches(l.rules(s), tool, t, l.tightening); ok {
				return l.eff, text, s.name
			}
		}
	}
	return effectNone, "", ""
}

// listMatches reports whether some positive rule in rules matches and no
// exception does. An exception loosens what its positive rules tighten and
// vice versa.
func (r *Rules) listMatches(rules []rule, tool string, t *Target, tightening bool) (string, bool) {
	text, found := "", false
	for _, ru := range rules {
		if ru.name != "*" && ru.name != tool {
			continue
		}
		if ru.except {
			if r.matches(ru, t, !tightening) {
				return "", false
			}
			continue
		}
		if !found && r.matches(ru, t, tightening) {
			text, found = ru.text, true
		}
	}
	return text, found
}

// matches requires either the lexical or the resolved path to match when the
// match tightens, and both when it loosens, so a symlink cannot be used to
// slip past a rule.
func (r *Rules) matches(ru rule, t *Target, tightening bool) bool {
	if ru.patterns == nil {
		return true
	}
	if t == nil {
		return false
	}
	lexical, resolved := r.matchPath(ru, t.Path), r.matchPath(ru, t.Real)
	if tightening {
		return lexical || resolved
	}
	return lexical && resolved
}

// matchPath tests p and each of its ancestors, so a pattern naming a
// directory covers everything in it.
func (r *Rules) matchPath(ru rule, p string) bool {
	if p == "" {
		return false
	}
	p = filepath.ToSlash(filepath.Clean(p))
	if r.fold {
		p = strings.ToLower(p)
	}
	for {
		for _, pat := range ru.patterns {
			if r.fold {
				pat = strings.ToLower(pat)
			}
			if doublestar.MatchUnvalidated(pat, p) {
				return true
			}
		}
		parent := path.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}
