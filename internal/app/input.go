package app

import (
	"cmp"
	"maps"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sahilm/fuzzy"

	"github.com/keyute/kopi/internal/skill"
)

type Kind int

const (
	KindCommand Kind = iota + 1
	KindArgument
	KindReference
)

// Span is runes [Start, End) of logical line Line of the input.
type Span struct {
	Line, Start, End int
	Kind             Kind
}

// Spans returns the input's highlighted runs in line and rune-offset order.
// A control command is highlighted alone; any other input gets its @ references,
// the skill commands skills types and, when ultracode is set, the keyword that
// turns it on, unless it opens with an unknown command.
func Spans(text string, skills *skill.Set, ultracode bool) []Span {
	if line, col, ok := commandAt(text); ok {
		name, _, _ := parse(text)
		if isControl(name) {
			return []Span{{Line: line, Start: col, End: col + 1 + utf8.RuneCountInString(name), Kind: KindCommand}}
		}
		if _, ok := skills.Lookup(leadingWord(text)); !ok {
			return nil
		}
	}
	return promptSpans(text, skills, ultracode)
}

// promptSpans returns the @ references, the skill commands skills types and,
// when ultracode is set, the keyword, of text taken as a prompt.
func promptSpans(text string, skills *skill.Set, ultracode bool) []Span {
	var spans []Span
	for i, l := range strings.Split(text, "\n") {
		for _, r := range references([]rune(l)) {
			if r[1]-r[0] > 1 {
				spans = append(spans, Span{Line: i, Start: r[0], End: r[1], Kind: KindReference})
			}
		}
	}
	refs := len(spans)
	for _, tok := range skills.Tokens(text) {
		if !slices.ContainsFunc(spans[:refs], func(r Span) bool { return r.Line == tok.Line && r.Start <= tok.Start && tok.Start < r.End }) {
			spans = append(spans, Span{Line: tok.Line, Start: tok.Start, End: tok.End, Kind: KindCommand})
		}
	}
	if ultracode {
		spans = append(spans, keywordSpans(text, skills)...)
	}
	slices.SortStableFunc(spans, func(a, b Span) int { return cmp.Or(cmp.Compare(a.Line, b.Line), cmp.Compare(a.Start, b.Start)) })
	return spans
}

// keyword is the word that makes the Workflow tool callable.
const keyword = "ultracode"

// keywordSpans returns the runs of the whole word keyword, ignoring case, outside
// @ references and skill commands.
func keywordSpans(text string, skills *skill.Set) []Span {
	var spans []Span
	tokens := skills.Tokens(text)
	for i, l := range strings.Split(text, "\n") {
		line := []rune(l)
		refs := references(line)
		word := func(j int) bool {
			return j >= 0 && j < len(line) && (unicode.IsLetter(line[j]) || unicode.IsDigit(line[j]) || line[j] == '_')
		}
		for start := 0; start+len(keyword) <= len(line); start++ {
			end := start + len(keyword)
			if !strings.EqualFold(string(line[start:end]), keyword) || word(start-1) || word(end) {
				continue
			}
			inToken := slices.ContainsFunc(tokens, func(t skill.Token) bool { return t.Line == i && t.Start <= start && start < t.End })
			if !inToken && !slices.ContainsFunc(refs, func(r [2]int) bool { return r[0] <= start && start < r[1] }) {
				spans = append(spans, Span{Line: i, Start: start, End: end, Kind: KindCommand})
			}
		}
	}
	return spans
}

// Query is the token at the cursor that completion replaces: runes [Start, End) of
// line Line, sigil included. Text is the part typed before the cursor, without the
// sigil and unescaped. Command names the command whose argument a KindArgument is.
type Query struct {
	Kind             Kind
	Command          string
	Line, Start, End int
	Text             string
}

// QueryAt returns the query at rune col of logical line line, if any.
func QueryAt(text string, line, col int) (Query, bool) {
	l := []rune(strings.Split(text, "\n")[line])
	if cl, c, ok := commandAt(text); ok {
		cmdLine := []rune(strings.Split(text, "\n")[cl])
		ne := c + 1
		for ne < len(cmdLine) && cmdLine[ne] != ' ' {
			ne++
		}
		// Only a control command owns the rest of the input; after a skill or
		// an unknown name the input is a prompt.
		if isControl(string(cmdLine[c+1 : ne])) {
			if line != cl || col <= c {
				return Query{}, false
			}
			if col <= ne {
				return Query{Kind: KindCommand, Line: line, Start: c, End: ne, Text: string(l[c+1 : col])}, true
			}
			start := ne
			for start < len(l) && unicode.IsSpace(l[start]) {
				start++
			}
			start = min(col, start)
			end := max(col, len([]rune(strings.TrimRightFunc(string(l), unicode.IsSpace))))
			return Query{Kind: KindArgument, Command: string(l[c+1 : ne]), Line: line, Start: start, End: end, Text: string(l[start:col])}, true
		}
		if line == cl && c < col && col <= ne {
			return Query{Kind: KindCommand, Line: line, Start: c, End: ne, Text: string(l[c+1 : col])}, true
		}
	}
	for _, r := range references(l) {
		if r[0] < col && col <= r[1] {
			return Query{Kind: KindReference, Line: line, Start: r[0], End: r[1], Text: unescape(string(l[r[0]+1 : col]))}, true
		}
	}
	return Query{}, false
}

// Candidate is one completion: Value is inserted, Label is shown in its place
// when set, and Description is shown beside it.
type Candidate struct{ Value, Label, Description string }

// Shown is what the menu displays for c.
func (c Candidate) Shown() string { return cmp.Or(c.Label, c.Value) }

// Match is the candidates matching q.Text on what they show, best first; all of
// them in order when q.Text is empty. An argument also matches on its
// description, ranked by the better of the two.
// For a reference it leaves out the directory q.Text names exactly.
func Match(q Query, cs []Candidate) []Candidate {
	if q.Text == "" {
		return cs
	}
	values := make([]string, 0, len(cs))
	for _, c := range cs {
		values = append(values, c.Shown())
	}
	matches := fuzzy.Find(q.Text, values)
	if q.Kind == KindArgument {
		descs := make([]string, 0, len(cs))
		for _, c := range cs {
			descs = append(descs, c.Description)
		}
		// Matched apart from the value: as one string the matcher would latch
		// onto scattered characters of an ID.
		best := map[int]fuzzy.Match{}
		for _, m := range append(matches, fuzzy.Find(q.Text, descs)...) {
			if b, ok := best[m.Index]; !ok || m.Score > b.Score {
				best[m.Index] = m
			}
		}
		matches = slices.SortedFunc(maps.Values(best), func(a, b fuzzy.Match) int {
			return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.Index, b.Index))
		})
	}
	out := make([]Candidate, 0, len(matches))
	for _, m := range matches {
		// A directory already typed in full lists its entries, not itself.
		if q.Kind == KindReference && strings.HasSuffix(q.Text, "/") && cs[m.Index].Value == q.Text {
			continue
		}
		out = append(out, cs[m.Index])
	}
	if q.Kind == KindReference {
		slices.SortStableFunc(out, func(a, b Candidate) int {
			return cmp.Compare(basenameTier(q.Text, a.Value), basenameTier(q.Text, b.Value))
		})
	}
	return out
}

// basenameTier ranks v's basename against text: 0 equal, 1 prefixed, 2 neither,
// all ignoring case.
func basenameTier(text, v string) int {
	base := path.Base(strings.TrimSuffix(v, "/"))
	switch {
	case strings.EqualFold(base, text):
		return 0
	case strings.HasPrefix(strings.ToLower(base), strings.ToLower(text)):
		return 1
	}
	return 2
}

// Reference is the @ token naming path.
func Reference(path string) string {
	return "@" + strings.ReplaceAll(path, " ", `\ `)
}

func unescape(s string) string {
	return strings.ReplaceAll(s, `\ `, " ")
}

// commandAt returns the position of the input's first non-space rune when the
// input is a command; that is where parse finds the "/".
func commandAt(text string) (line, col int, ok bool) {
	if _, _, ok := parse(text); !ok {
		return 0, 0, false
	}
	for i, l := range strings.Split(text, "\n") {
		if j := slices.IndexFunc([]rune(l), func(r rune) bool { return !unicode.IsSpace(r) }); j >= 0 {
			return i, j, true
		}
	}
	return 0, 0, false
}

// references returns the [start, end) rune ranges of line's @ tokens: each
// starts at an "@" at the line start or after whitespace and runs to the first
// whitespace not escaped by a backslash.
func references(line []rune) [][2]int {
	var out [][2]int
	for i := 0; i < len(line); i++ {
		if line[i] != '@' || i > 0 && !unicode.IsSpace(line[i-1]) {
			continue
		}
		j := i + 1
		for j < len(line) && (!unicode.IsSpace(line[j]) || line[j-1] == '\\' && line[j] == ' ') {
			j++
		}
		out = append(out, [2]int{i, j})
		i = j
	}
	return out
}
