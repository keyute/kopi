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
	unsandboxed := map[tool.Action]int{}
	for _, c := range g.Calls {
		if counts[c.Action] == 0 {
			order = append(order, c.Action)
		}
		counts[c.Action]++
		if c.Unsandboxed {
			unsandboxed[c.Action]++
		}
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
		if n := unsandboxed[a]; n > 0 {
			clauses[i] += " (" + strconv.Itoa(n) + " unsandboxed)"
		}
	}
	return strings.Join(clauses, ", ")
}

func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToLower(r)) + s[n:]
}
