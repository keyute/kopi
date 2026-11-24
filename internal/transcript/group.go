package transcript

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/keyute/kopi/internal/tool"
)

// Sentence counts the calls per action in first-appearance order, e.g.
// "Read 3 files, ran 2 tools". Calls of a counted action on one subject count
// once: seven edits of two files read "edited 2 files".
func (g Group) Sentence() string {
	type thing struct {
		action  tool.Action
		subject string
	}
	var order []tool.Action
	counts := map[tool.Action]int{}
	unsandboxed := map[tool.Action]int{}
	seen := map[thing]bool{}
	for _, c := range g.Calls {
		if counts[c.Action] == 0 {
			order = append(order, c.Action)
		}
		if c.Unsandboxed {
			unsandboxed[c.Action]++
		}
		if t := (thing{c.Action, c.Subject}); c.Subject != "" && c.Action.Many != "" {
			if seen[t] {
				continue
			}
			seen[t] = true
		}
		counts[c.Action]++
	}
	clauses := make([]string, len(order))
	for i, a := range order {
		verb := a.Verb
		if i > 0 {
			verb = lowerFirst(verb)
		}
		switch {
		case a.Many == "" && counts[a] == 1:
			clauses[i] = verb + " " + a.One
		case a.Many == "":
			clauses[i] = verb + " " + a.One + " " + strconv.Itoa(counts[a]) + " times"
		case counts[a] == 1:
			clauses[i] = verb + " 1 " + a.One
		default:
			clauses[i] = verb + " " + strconv.Itoa(counts[a]) + " " + a.Many
		}
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
