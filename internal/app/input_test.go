package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestQueryAt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		text      string
		line, col int
		want      Query
		ok        bool
	}{
		{name: "slash", text: "/", col: 1, want: Query{Kind: KindCommand, End: 1}, ok: true},
		{name: "partial name", text: "/re", col: 3, want: Query{Kind: KindCommand, End: 3, Text: "re"}, ok: true},
		{name: "mid name", text: "/resume x", col: 3, want: Query{Kind: KindCommand, End: 7, Text: "re"}, ok: true},
		{name: "argument", text: "/resume a", col: 9, want: Query{Kind: KindArgument, Command: "resume", Start: 8, End: 9, Text: "a"}, ok: true},
		{name: "empty argument", text: "/resume ", col: 8, want: Query{Kind: KindArgument, Command: "resume", Start: 8, End: 8}, ok: true},
		{name: "indented", text: "  /pl", col: 5, want: Query{Kind: KindCommand, Start: 2, End: 5, Text: "pl"}, ok: true},
		{name: "escaped slash", text: "//x", col: 3},
		{name: "command on later line", text: "hi\n/pl", line: 1, col: 3},
		{name: "after blank lines", text: "\n /pl", line: 1, col: 4, want: Query{Kind: KindCommand, Line: 1, Start: 1, End: 4, Text: "pl"}, ok: true},
		{name: "bare at", text: "@", col: 1, want: Query{Kind: KindReference, End: 1}, ok: true},
		{name: "mid reference", text: "see @src/main.go", col: 11, want: Query{Kind: KindReference, Start: 4, End: 16, Text: "src/ma"}, ok: true},
		{name: "at inside word", text: "a@b", col: 3},
		{name: "escaped space", text: `@my\ di`, col: 7, want: Query{Kind: KindReference, End: 7, Text: "my di"}, ok: true},
		{name: "reference in argument", text: "/resume @x", col: 10, want: Query{Kind: KindArgument, Command: "resume", Start: 8, End: 10, Text: "@x"}, ok: true},
		{name: "before slash", text: "  /plan", col: 2},
		{name: "argument of plan", text: "/plan x", col: 7, want: Query{Kind: KindArgument, Command: "plan", Start: 6, End: 7, Text: "x"}, ok: true},
		{name: "reference on second line", text: "hi\nsee @a", line: 1, col: 6, want: Query{Kind: KindReference, Line: 1, Start: 4, End: 6, Text: "a"}, ok: true},
		{name: "line after command", text: "/plan\n@a", line: 1, col: 2},
		{name: "prose", text: "hello", col: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, ok := QueryAt(tc.text, tc.line, tc.col)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, q)
		})
	}
}

func TestSpans(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []Span
	}{
		{text: "/plan x", want: []Span{{Start: 0, End: 5, Kind: KindCommand}}},
		{text: "  /plan", want: []Span{{Start: 2, End: 7, Kind: KindCommand}}},
		{text: "/nope"},
		{text: "x /plan"},
		{text: "/plan\nx"},
		{text: "@a and @b", want: []Span{{Start: 0, End: 2, Kind: KindReference}, {Start: 7, End: 9, Kind: KindReference}}},
		{text: "/resume @x", want: []Span{{Start: 0, End: 7, Kind: KindCommand}}},
		{text: "/nope @x"},
		{text: "hi\nsee @a\\ b", want: []Span{{Line: 1, Start: 4, End: 9, Kind: KindReference}}},
		{text: "a @ b"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			assert.Equal(t, tc.want, Spans(tc.text))
		})
	}
}

func TestMatch(t *testing.T) {
	cs := []Candidate{{Value: "planner"}, {Value: "plan"}, {Value: "execute"}}
	assert.Equal(t, cs, Match(Query{Kind: KindCommand}, cs))
	assert.Equal(t, []Candidate{{Value: "plan"}, {Value: "planner"}}, Match(Query{Kind: KindCommand, Text: "plan"}, cs))
	assert.Equal(t, []Candidate{{Value: "plan"}, {Value: "planner"}}, Match(Query{Kind: KindCommand, Text: "PLAN"}, cs))

	files := []Candidate{{Value: "docs/main-notes.md"}, {Value: "src/domain/"}, {Value: "cmd/main.go"}, {Value: "src/main/"}}
	assert.Equal(t, []Candidate{{Value: "src/main/"}, {Value: "cmd/main.go"}, {Value: "docs/main-notes.md"}, {Value: "src/domain/"}},
		Match(Query{Kind: KindReference, Text: "Main"}, files))

	dirs := []Candidate{{Value: "src/"}, {Value: "src/a.go"}}
	assert.Equal(t, []Candidate{{Value: "src/a.go"}}, Match(Query{Kind: KindReference, Text: "src/"}, dirs))

	sessions := []Candidate{
		{Value: "0a1b2c3d", Description: "2026-11-01 10:00:00  fix the parser"},
		{Value: "4e5f6a7b", Description: "2026-11-02 11:00:00  add tests"},
	}
	assert.Equal(t, sessions[1:], Match(Query{Kind: KindArgument, Text: "tests"}, sessions))
	assert.Equal(t, sessions[:1], Match(Query{Kind: KindArgument, Text: "parser"}, sessions))
	assert.Equal(t, sessions[1:], Match(Query{Kind: KindArgument, Text: "4e5f"}, sessions))
	assert.Empty(t, Match(Query{Kind: KindCommand, Text: "tests"}, sessions))
}

func TestReference(t *testing.T) {
	assert.Equal(t, "@src/main.go", Reference("src/main.go"))
	assert.Equal(t, `@my\ dir/a\ b.txt`, Reference("my dir/a b.txt"))
}
