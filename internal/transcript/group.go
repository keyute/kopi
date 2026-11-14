package transcript

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/keyute/kopi/internal/tool"
)

// Sentence counts the calls per action in first-appearance order, e.g.
// "Read 3 files, ran 2 tools".
func (g Group) Sentence() string {
	var order []tool.Action
	counts := map[tool.Action]int{}
	for _, c := range g.Calls {
		if counts[c.Action] == 0 {
			order = append(order, c.Action)
		}
		counts[c.Action]++
	}
	clauses := make([]string, len(order))
	for i, a := range order {
		verb, noun := a.Verb, a.Many
		if i > 0 {
			verb = lowerFirst(verb)
		}
		if counts[a] == 1 {
			noun = a.One
		}
		clauses[i] = verb + " " + strconv.Itoa(counts[a]) + " " + noun
	}
	return strings.Join(clauses, ", ")
}

func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToLower(r)) + s[n:]
}
