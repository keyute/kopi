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

// rules are applied in order; an earlier rule wins an overlap.
var rules = []func(text string) []Span{commandSpans, referenceSpans}

// Spans returns the input's highlighted runs, sorted; an earlier rule wins an overlap.
func Spans(text string) []Span {
	var kept []Span
	for _, rule := range rules {
		for _, s := range rule(text) {
			overlaps := slices.ContainsFunc(kept, func(k Span) bool {
				return k.Line == s.Line && k.Start < s.End && s.Start < k.End
			})
			if !overlaps {
				kept = append(kept, s)
			}
		}
	}
	slices.SortFunc(kept, func(a, b Span) int {
		return cmp.Or(cmp.Compare(a.Line, b.Line), cmp.Compare(a.Start, b.Start))
	})
	return kept
}

func commandSpans(text string) []Span {
	line, col, ok := commandAt(text)
	if !ok {
		return nil
	}
	name, _, _ := parse(text)
	if !slices.ContainsFunc(commandTable(), func(c command) bool { return c.Name == name }) {
		return nil
	}
	return []Span{{Line: line, Start: col, End: col + 1 + utf8.RuneCountInString(name), Kind: KindCommand}}
}

func referenceSpans(text string) []Span {
	if _, _, ok := parse(text); ok {
		return nil
	}
	var spans []Span
	for i, l := range strings.Split(text, "\n") {
		for _, r := range references([]rune(l)) {
			if r[1]-r[0] > 1 {
				spans = append(spans, Span{Line: i, Start: r[0], End: r[1], Kind: KindReference})
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
		if line != cl || col <= c {
			return Query{}, false
		}
		ne := c + 1
		for ne < len(l) && l[ne] != ' ' {
			ne++
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
	for _, r := range references(l) {
		if r[0] < col && col <= r[1] {
			return Query{Kind: KindReference, Line: line, Start: r[0], End: r[1], Text: unescape(string(l[r[0]+1 : col]))}, true
		}
	}
	return Query{}, false
}

// Candidate is one completion: Value is inserted, Description is shown beside it.
type Candidate struct{ Value, Description string }

// Match is the candidates matching q.Text, best first; all of them in order when q.Text is empty.
// An argument also matches on its description, ranked by the better of the two.
// For a reference it leaves out the directory q.Text names exactly.
func Match(q Query, cs []Candidate) []Candidate {
	if q.Text == "" {
		return cs
	}
	values := make([]string, 0, len(cs))
	for _, c := range cs {
		values = append(values, c.Value)
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
		for j < len(line) && (!unicode.IsSpace(line[j]) || line[j-1] == '\\') {
			j++
		}
		out = append(out, [2]int{i, j})
		i = j
	}
	return out
}
