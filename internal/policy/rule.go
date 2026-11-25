package policy

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
)

// Source is one origin of rules. A `!` exception only exempts calls from the
// list it is in, so one source can never loosen another's rules.
type Source struct {
	Name             string
	Allow, Ask, Deny []string
}

// MCPPrefix starts the name of every tool from an MCP server.
const MCPPrefix = "mcp__"

// ProjectMCPFile is the file in which a folder declares MCP servers; Kopi
// starts a trusted folder's servers outside the sandbox. Loading follows a
// symlink, as every peer does, so protection covers the name in every
// directory and whatever the start directory's .mcp.json resolves to at
// startup, with no file-or-directory check: skipping a linked directory let
// Bash swap it for a file. Deliberately not chased: another link on the way,
// a link broken at startup, an existing hard link, and other folders' link
// targets. Codex and Claude Code's sandbox stop at the resolved target too
// (anthropics/sandbox-runtime#790); reopen only with new evidence.
const ProjectMCPFile = ".mcp.json"

// ruleName also admits a trailing * for parseRule to refuse on non-MCP names.
var ruleName = regexp.MustCompile(`^(\*|[A-Za-z_][A-Za-z0-9_-]*\*?)$`)

type rule struct {
	text   string
	name   string
	except bool
	// patterns are slash-separated absolute globs; nil for a bare rule, which
	// matches every call of its tool.
	patterns []string
	// spec is the path as written, which SetDirs extends to added directories;
	// extra holds those patterns. Only extra changes after NewRules.
	spec  string
	extra []string
}

type source struct {
	name             string
	allow, ask, deny []rule
}

type Rules struct {
	// mu guards each rule's extra and mcpFiles, the two things SetDirs replaces.
	mu      sync.RWMutex
	sources []source
	// fold matches case-insensitively, as the file systems of darwin and
	// windows do by default.
	fold bool
	// mcpFiles are the files the .mcp.json of the workspace, then of each added
	// directory, resolves to when the rules or the directories are set.
	mcpFiles []string
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
	mcp := strings.HasPrefix(name, MCPPrefix)
	if name != "*" && strings.HasSuffix(name, "*") && !mcp {
		return "", "", false, fmt.Errorf("rule %q: only a name starting with %s may end in *: %w", s, MCPPrefix, ErrInvalidRule)
	}
	if mcp {
		suffix, star := strings.CutSuffix(strings.TrimPrefix(name, MCPPrefix), "*")
		server, tool, cut := strings.Cut(suffix, "__")
		// A prefix that ends inside the server name, such as mcp__git*, would
		// also cover other servers' tools. Past the first __ it cannot, as a
		// server name never contains __ or ends in _.
		if valid := star && suffix == "" || cut && server != "" && (star || tool != ""); !valid {
			return "", "", false, fmt.Errorf("rule %q: name one MCP tool as mcp__server__tool or all of a server's as mcp__server__*: %w", s, ErrInvalidRule)
		}
	}
	if !hasSpec {
		return name, "", false, nil
	}
	if mcp {
		return "", "", false, fmt.Errorf("rule %q: an MCP tool takes no path: %w", s, ErrInvalidRule)
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

// NewRules parses every source's rules; a rule name must be *, one of tools or
// start with MCPPrefix, whose server may be absent in this run, and a rule with
// a path must name * or a tool that acts on a path. Workspace and home must be
// absolute.
func NewRules(workspace, home string, tools map[string]ToolSpec, sources ...Source) (*Rules, error) {
	for _, d := range []struct{ name, dir string }{{"workspace", workspace}, {"home", home}} {
		if _, err := filepath.EvalSymlinks(d.dir); err != nil {
			return nil, fmt.Errorf("failed to resolve %s: %w", d.name, err)
		}
	}
	ws, hm := escape(filepath.ToSlash(filepath.Clean(workspace))), escape(filepath.ToSlash(filepath.Clean(home)))
	r := &Rules{
		fold:     runtime.GOOS == "darwin" || runtime.GOOS == "windows",
		mcpFiles: []string{Resolve(filepath.Join(workspace, ProjectMCPFile))},
	}
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
				if name != "*" && !strings.HasPrefix(name, MCPPrefix) {
					ts, ok := tools[name]
					if !ok {
						return nil, fmt.Errorf("%s: rule %q: unknown tool %q: %w", src.Name, text, name, ErrInvalidRule)
					}
					if ts.Target == nil && spec != "" {
						return nil, fmt.Errorf("%s: rule %q: tool %q takes no path: %w", src.Name, text, name, ErrInvalidRule)
					}
				}
				ru := rule{text: text, name: name, except: except}
				if spec != "" {
					ru.patterns, ru.spec = forms(expand(spec, ws, hm)), spec
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
	if rest, ok := underWorkspace(spec); ok {
		return ws + rest
	}
	if strings.HasPrefix(spec, "/") {
		return spec
	}
	return home + strings.TrimPrefix(spec, "~")
}

// underWorkspace reports whether spec is relative to the workspace and, if so,
// what follows the workspace's path.
func underWorkspace(spec string) (rest string, ok bool) {
	switch {
	case strings.HasPrefix(spec, "/"), spec == "~", strings.HasPrefix(spec, "~/"):
		return "", false
	case !strings.Contains(strings.TrimRight(spec, "/"), "/"):
		// As in .gitignore, a name with at most a trailing slash matches at any depth.
		return "/**/" + spec, true
	default:
		return "/" + spec, true
	}
}

// eachRule calls f on every rule in order.
func (r *Rules) eachRule(f func(*rule)) {
	for i := range r.sources {
		s := &r.sources[i]
		for _, rules := range [][]rule{s.allow, s.ask, s.deny} {
			for j := range rules {
				f(&rules[j])
			}
		}
	}
}

// SetDirs makes relative and bare-name rule paths apply in each of dirs as in
// the workspace, and guards what each one's .mcp.json resolves to. It replaces
// what an earlier call set; nil removes it. dirs are absolute, as the human
// spelled them (a path through a symlink keeps both its lexical and resolved
// forms, as the workspace does), and exist.
func (r *Rules) SetDirs(dirs []string) {
	if r == nil {
		return
	}
	escaped := make([]string, len(dirs))
	mcp := make([]string, len(dirs))
	for i, dir := range dirs {
		escaped[i] = escape(filepath.ToSlash(filepath.Clean(dir)))
		mcp[i] = Resolve(filepath.Join(dir, ProjectMCPFile))
	}
	// Forms stat the file system, so they are built before the lock is taken;
	// spec and patterns never change, so reading them needs none.
	var extras [][]string
	r.eachRule(func(ru *rule) {
		var extra []string
		if rest, ok := underWorkspace(ru.spec); ok && ru.patterns != nil {
			for _, dir := range escaped {
				extra = append(extra, forms(dir+rest)...)
			}
		}
		extras = append(extras, extra)
	})

	r.mu.Lock()
	defer r.mu.Unlock()
	i := 0
	r.eachRule(func(ru *rule) {
		ru.extra = extras[i]
		i++
	})
	r.mcpFiles = append(r.mcpFiles[:1], mcp...)
}

// forms resolves the literal prefix before cleaning it. A cleaned lexical
// alias is kept only if it still names the same target: cleaning link/.. can
// otherwise introduce a rule for an unrelated path. Links are resolved once,
// here; one created or changed later is not seen.
func forms(pat string) []string {
	dir, tail := splitLiteral(pat)
	resolved := filepath.ToSlash(Resolve(filepath.FromSlash(dir)))
	realPattern := path.Join(escape(resolved), tail)
	cleanDir := path.Clean(dir)
	if filepath.ToSlash(Resolve(filepath.FromSlash(cleanDir))) != resolved {
		return []string{realPattern}
	}
	lexical := path.Clean(pat)
	if lexical == realPattern {
		return []string{lexical}
	}
	return []string{lexical, realPattern}
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

// Resolve resolves the symlinks of p's deepest ancestor that can be resolved
// and appends the rest of p unchanged, so p need not exist.
func Resolve(p string) string {
	var rest []string
	for {
		for len(p) > len(filepath.VolumeName(p))+1 && os.IsPathSeparator(p[len(p)-1]) {
			p = p[:len(p)-1]
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{r}, rest...)...)
		}
		// filepath.Dir would clean away a symlink followed by .. before
		// the next attempt to resolve the remaining prefix.
		parent, base := filepath.Split(p)
		if parent == p || parent == "" {
			return p
		}
		rest = append([]string{base}, rest...)
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.sources {
		bare, exempt := false, false
		for _, ru := range s.deny {
			if !nameMatches(ru.name, tool) {
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, l := range []struct {
		eff        effect
		tightening bool
		rules      func(source) []rule
	}{
		{effectDeny, true, func(s source) []rule { return s.deny }},
		{effectAsk, true, func(s source) []rule { return s.ask }},
		{effectAllow, false, func(s source) []rule { return s.allow }},
	} {
		if l.eff == effectAsk && (tool == "Edit" || tool == "Write") && t != nil {
			if r.underGit(t.Path) || r.underGit(t.Real) {
				return effectAsk, tool + "(.git)", "built-in"
			}
			if r.isMCPFile(t.Path) || r.isMCPFile(t.Real) {
				return effectAsk, tool + "(" + ProjectMCPFile + ")", "built-in"
			}
		}
		for _, s := range r.sources {
			if text, ok := r.listMatches(l.rules(s), tool, t, l.tightening); ok {
				return l.eff, text, s.name
			}
		}
	}
	return effectNone, "", ""
}

// Hides reports whether a search by tool rooted at root leaves t out: a
// deny or ask rule with a path matches t, no exception in its list does, and
// that rule does not match root, where the gate already judged the call,
// unless an exception in its list admitted root.
func (r *Rules) Hides(tool string, root, t Target) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.sources {
		for _, rules := range [][]rule{s.deny, s.ask} {
			if r.listHides(rules, tool, root, t) {
				return true
			}
		}
	}
	return false
}

func (r *Rules) listHides(rules []rule, tool string, root, t Target) bool {
	_, rootJudged := r.listMatches(rules, tool, &root, true)
	hit := false
	for _, ru := range rules {
		if !nameMatches(ru.name, tool) {
			continue
		}
		if ru.except {
			if r.matches(ru, &t, false) {
				return false
			}
			continue
		}
		if ru.patterns != nil && r.matches(ru, &t, true) && (!rootJudged || !r.matches(ru, &root, true)) {
			hit = true
		}
	}
	return hit
}

func (r *Rules) underGit(p string) bool {
	p = filepath.ToSlash(filepath.Clean(p))
	if r.fold {
		p = strings.ToLower(p)
	}
	return slices.Contains(strings.Split(p, "/"), ".git")
}

func (r *Rules) isMCPFile(p string) bool {
	p = filepath.Clean(p)
	if slices.ContainsFunc(r.mcpFiles, func(f string) bool { return p == f || r.fold && strings.EqualFold(p, f) }) {
		return true
	}
	base := path.Base(filepath.ToSlash(p))
	return base == ProjectMCPFile || r.fold && strings.EqualFold(base, ProjectMCPFile)
}

// listMatches reports whether some positive rule in rules matches and no
// exception does. An exception loosens what its positive rules tighten and
// vice versa.
func (r *Rules) listMatches(rules []rule, tool string, t *Target, tightening bool) (string, bool) {
	text, found := "", false
	for _, ru := range rules {
		if !nameMatches(ru.name, tool) {
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

// nameMatches reports whether a rule's name covers tool: the same name, or a
// name ending in * as a prefix of tool, which makes a lone * cover every tool.
func nameMatches(name, tool string) bool {
	prefix, star := strings.CutSuffix(name, "*")
	return name == tool || (star && strings.HasPrefix(tool, prefix))
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
		for _, pats := range [2][]string{ru.patterns, ru.extra} {
			for _, pat := range pats {
				if r.fold {
					pat = strings.ToLower(pat)
				}
				if doublestar.MatchUnvalidated(pat, p) {
					return true
				}
			}
		}
		parent := path.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}
