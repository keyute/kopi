package transcript

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/tool"
)

func TestGroupSentence(t *testing.T) {
	ran := tool.Action{Verb: "Ran", One: "tool", Many: "tools"}
	for _, tc := range []struct {
		name    string
		actions []tool.Action
		want    string
	}{
		{"single action", []tool.Action{readAction, readAction, readAction}, "Read 3 files"},
		{"mixed actions", []tool.Action{readAction, ran, readAction, ran, readAction}, "Read 3 files, ran 2 tools"},
		{"singular", []tool.Action{ran, readAction, ran}, "Ran 2 tools, read 1 file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var g Group
			for _, a := range tc.actions {
				g.Calls = append(g.Calls, Call{Action: a})
			}
			assert.Equal(t, tc.want, g.Sentence())
		})
	}
}
