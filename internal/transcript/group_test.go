package transcript

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/tool"
)

func TestGroupSentence(t *testing.T) {
	ran := tool.Action{Verb: "Ran", One: "tool", Many: "tools"}
	web := tool.Action{Verb: "Searched", One: "the web"}
	plan := tool.Action{Verb: "Edited", One: "the plan"}
	for _, tc := range []struct {
		name    string
		actions []tool.Action
		want    string
	}{
		{"single action", []tool.Action{readAction, readAction, readAction}, "Read 3 files"},
		{"mixed actions", []tool.Action{readAction, ran, readAction, ran, readAction}, "Read 3 files, ran 2 tools"},
		{"singular", []tool.Action{ran, readAction, ran}, "Ran 2 tools, read 1 file"},
		{"uncounted once", []tool.Action{web}, "Searched the web"},
		{"uncounted repeated", []tool.Action{web, web, web}, "Searched the web 3 times"},
		{"uncounted later", []tool.Action{readAction, readAction, plan}, "Read 2 files, edited the plan"},
		{"uncounted mixed", []tool.Action{plan, readAction, web, web, plan}, "Edited the plan 2 times, read 1 file, searched the web 2 times"},
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

func TestGroupSentenceCountsASubjectOnce(t *testing.T) {
	edited := tool.Action{Verb: "Edited", One: "file", Many: "files"}
	ran := tool.Action{Verb: "Ran", One: "command", Many: "commands"}
	plan := tool.Action{Verb: "Edited", One: "the plan"}
	g := Group{Calls: []Call{
		{Action: edited, Subject: "a.go"},
		{Action: edited, Subject: "b.go"},
		{Action: edited, Subject: "a.go"},
		{Action: readAction, Subject: "a.go"},
		{Action: readAction, Subject: "a.go"},
		{Action: ran},
		{Action: ran},
		{Action: plan, Subject: "plan"},
		{Action: plan, Subject: "plan"},
	}}
	assert.Equal(t, "Edited 2 files, read 1 file, ran 2 commands, edited the plan 2 times", g.Sentence())
}

func TestGroupSentenceCountsUnsandboxed(t *testing.T) {
	ran := tool.Action{Verb: "Ran", One: "command", Many: "commands"}
	g := Group{Calls: []Call{{Action: readAction}, {Action: ran}, {Action: ran, Unsandboxed: true}, {Action: ran}}}
	assert.Equal(t, "Read 1 file, ran 3 commands (1 unsandboxed)", g.Sentence())
}
