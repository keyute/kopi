package policy

import (
	"fmt"
	"io/fs"
	"os"
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

// MCPPrefix starts the name of every tool from an MCP server.
const MCPPrefix = "mcp__"

// ProjectMCPFile is the file in which a folder declares MCP servers; Kopi
// starts a trusted folder's servers outside the sandbox.
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
	// mcpLink is the workspace's ProjectMCPFile resolved at start, which an
	// edit reaches under another name when that file is a symlink.
	mcpLink string
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
		fold:    runtime.GOOS == "darwin" || runtime.GOOS == "windows",
		mcpLink: Resolve(filepath.Join(workspace, ProjectMCPFile)),
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
		return spec
	case spec == "~" || strings.HasPrefix(spec, "~/"):
		return home + strings.TrimPrefix(spec, "~")
	case !strings.Contains(strings.TrimRight(spec, "/"), "/"):
		// As in .gitignore, a name with at most a trailing slash matches at any depth.
		return ws + "/**/" + spec
	default:
		return ws + "/" + spec
	}
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

// Resolve is the path ResolveLinks resolves p to.
func Resolve(p string) string {
	resolved, _ := ResolveLinks(p)
	return resolved
}

// maxLinks is how many symlinks ResolveLinks follows, as darwin's MAXSYMLINKS.
const maxLinks = 32

// ResolveLinks resolves p name by name as the kernel does when creating it,
// following every symlink, dangling ones too, so p need not exist: from the
// first name that does not exist the rest of p is appended unchanged. links
// are, in order, the symlinks passed and the names the walk backs over with
// .., including a missing name a later .. would back over: the paths a
// command could replace to change where p resolves. At the link past
// maxLinks it stops and returns that link's path, so a cycle still ends.
//
// Adapted from walkSymlinks in Go's path/filepath (BSD-3-Clause).
func ResolveLinks(p string) (resolved string, links []string) {
	sep := string(os.PathSeparator)
	volLen := len(filepath.VolumeName(p))
	if volLen < len(p) && os.IsPathSeparator(p[volLen]) {
		volLen++
	}
	vol := p[:volLen]
	dest := vol
	// lastSep is the index of the last separator in dest after the volume,
	// or -1.
	lastSep := func() int {
		for r := len(dest) - 1; r >= volLen; r-- {
			if os.IsPathSeparator(dest[r]) {
				return r
			}
		}
		return -1
	}
	var end int
	for start := volLen; start < len(p); start = end {
		for start < len(p) && os.IsPathSeparator(p[start]) {
			start++
		}
		end = start
		for end < len(p) && !os.IsPathSeparator(p[end]) {
			end++
		}
		switch p[start:end] {
		case "", ".":
			continue
		case "..":
			// Back up over the last resolved name, keeping a leading .. that
			// cannot be backed over.
			if r := lastSep(); r < 0 || dest[r+1:] == ".." {
				if len(dest) > volLen {
					dest += sep
				}
				dest += ".."
			} else {
				links = append(links, dest)
				dest = dest[:r]
			}
			continue
		}
		prev := dest
		if len(dest) > len(filepath.VolumeName(dest)) && !os.IsPathSeparator(dest[len(dest)-1]) {
			dest += sep
		}
		dest += p[start:end]
		fi, err := os.Lstat(dest)
		if err != nil {
			// Join backs over the rest's .. lexically, where the kernel would
			// walk through dest.
			if slices.Contains(strings.Split(filepath.ToSlash(p[end:]), "/"), "..") {
				links = append(links, dest)
			}
			return filepath.Join(prev, p[start:]), links
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		links = append(links, dest)
		link, err := os.Readlink(dest)
		if err != nil || len(links) > maxLinks {
			return filepath.Join(dest, p[end:]), links
		}
		p = link + p[end:]
		if v := len(filepath.VolumeName(link)); v > 0 {
			if v < len(link) && os.IsPathSeparator(link[v]) {
				v++
			}
			vol, volLen = link[:v], v
			dest, end = vol, v
		} else if len(link) > 0 && os.IsPathSeparator(link[0]) {
			vol, volLen = link[:1], 1
			dest, end = vol, 1
		} else {
			// A relative target replaces the link's name in dest.
			if r := lastSep(); r < 0 {
				dest = vol
			} else {
				dest = dest[:r]
			}
			end = 0
		}
	}
	return filepath.Clean(dest), links
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

func (r *Rules) underGit(p string) bool {
	p = filepath.ToSlash(filepath.Clean(p))
	if r.fold {
		p = strings.ToLower(p)
	}
	return slices.Contains(strings.Split(p, "/"), ".git")
}

func (r *Rules) isMCPFile(p string) bool {
	p = filepath.ToSlash(filepath.Clean(p))
	same := func(a, b string) bool { return a == b || r.fold && strings.EqualFold(a, b) }
	return same(path.Base(p), ProjectMCPFile) || same(p, filepath.ToSlash(r.mcpLink))
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
