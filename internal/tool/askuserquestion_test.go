package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

const questionsArgs = `{"questions":[
	{"question":"Which database?","header":"Database","options":[{"label":"PostgreSQL","description":"Relational"},{"label":"SQLite","description":"Embedded"}]},
	{"question":"Which features?","header":"Features","multiSelect":true,"options":[{"label":"Caching","description":""},{"label":"Search","description":""},{"label":"Queues","description":""}]},
	{"question":"Deploy target?","header":"Target","options":[{"label":"Prod","description":""},{"label":"Staging","description":""}]}
]}`

func answering(answers []Answer, err error) Ask {
	return func(context.Context, []Question) ([]Answer, error) { return answers, err }
}

func TestAskUserQuestionDescribesItself(t *testing.T) {
	tool := NewAskUserQuestion(nil)

	assert.Equal(t, "AskUserQuestion", tool.Name)
	assert.Equal(t, "AskUserQuestion", tool.Label)
	assert.Equal(t, "Asks the user one to four questions and waits for the answers. Each question offers two to four options, and the user can always type an answer of their own instead, so an option labelled \"Other\" is refused. The user may add a note to an option they choose, or dismiss the questions without answering; the result says which.", tool.Description)
	assert.Equal(t, []string{"questions"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	assert.Equal(t, policy.Read, tool.Capability)
	assert.True(t, tool.Exclusive)
	assert.False(t, tool.Ask)
	assert.Nil(t, tool.Target)
	assert.Zero(t, tool.Display.Action)
	assert.True(t, tool.Display.Solo)
	assert.Empty(t, tool.Display.Body("The user answered:\n\"Which database?\" = \"PostgreSQL\""))
}

func TestAskUserQuestionRefusesInvalidArgs(t *testing.T) {
	q := func(mutate string) string {
		return `{"questions":[{"question":"Q?","header":"H","options":[` + mutate + `]}]}`
	}
	opt := func(label string) string { return `{"label":"` + label + `","description":"d"}` }
	tests := []struct{ name, args, want string }{
		{"no questions", `{"questions":[]}`, "one to four questions"},
		{"five questions", `{"questions":[` + `{},{},{},{},{}` + `]}`, "one to four questions"},
		{"unknown field", `{"questions":[],"x":1}`, "unknown field"},
		{"blank question", `{"questions":[{"question":" ","header":"H","options":[` + opt("a") + `,` + opt("b") + `]}]}`, "question 1: question is blank"},
		{"blank header", `{"questions":[{"question":"Q","header":"","options":[` + opt("a") + `,` + opt("b") + `]}]}`, "question 1: header is blank"},
		{"header line break", `{"questions":[{"question":"Q","header":"a\nb","options":[` + opt("a") + `,` + opt("b") + `]}]}`, "header must not contain"},
		{"one option", q(opt("a")), "two to four options"},
		{"five options", q(opt("a") + "," + opt("b") + "," + opt("c") + "," + opt("d") + "," + opt("e")), "two to four options"},
		{"blank label", q(opt("a") + "," + opt(" ")), "option 2: label is blank"},
		{"label tab", q(opt("a") + "," + opt(`b\tc`)), "label must not contain"},
		{"label Other", q(opt("a") + "," + opt(" other ")), "is reserved: the user can always type their own answer"},
		{"duplicate label", q(opt("a") + "," + opt("A ")), "repeats another option"},
		{"preview when multi", `{"questions":[{"question":"Q","header":"H","multiSelect":true,"options":[{"label":"a","description":"","preview":"p"},` + opt("b") + `]}]}`, "single-select questions only"},
		{"control in question", `{"questions":[{"question":"Q\u001b[2J","header":"H","options":[` + opt("a") + `,` + opt("b") + `]}]}`, "control character"},
		{"C1 in description", q(opt("a") + `,{"label":"b","description":"x\u009by"}`), "control character"},
		{"DEL in label", q(opt("a") + "," + opt(`b\u007f`)), "control character"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			tool := NewAskUserQuestion(func(context.Context, []Question) ([]Answer, error) {
				called = true
				return nil, nil
			})

			_, err := tool.Run(t.Context(), json.RawMessage(tc.args), allowTarget)

			require.ErrorIs(t, err, ErrInvalidArgs)
			assert.Contains(t, err.Error(), tc.want)
			assert.False(t, called)
		})
	}
}

func TestAskUserQuestionFormatsAnswers(t *testing.T) {
	tool := NewAskUserQuestion(answering([]Answer{
		{Picks: []Pick{{Option: 0, Note: " pin to 16 "}}},
		{Picks: []Pick{{Option: 0}, {Option: 1, Note: "  "}}, Custom: " rate limiting "},
		{Custom: "staging\nonly"},
	}, nil))

	out, err := tool.Run(t.Context(), json.RawMessage(questionsArgs), allowTarget)

	require.NoError(t, err)
	assert.Equal(t, "The user answered:\n"+
		`"Which database?" = "PostgreSQL" with note "pin to 16"`+"\n"+
		`"Which features?" = "Caching", "Search", typed "rate limiting"`+"\n"+
		`"Deploy target?" = typed "staging\nonly"`, out.Text)
}

func TestAskUserQuestionReportsDismissal(t *testing.T) {
	tool := NewAskUserQuestion(answering(nil, nil))

	out, err := tool.Run(t.Context(), json.RawMessage(questionsArgs), allowTarget)

	require.NoError(t, err)
	assert.Equal(t, "The user dismissed the questions without answering.", out.Text)
}

func TestAskUserQuestionStopsWhenNotAdmitted(t *testing.T) {
	denied := errors.New("denied")
	called := false
	tool := NewAskUserQuestion(func(context.Context, []Question) ([]Answer, error) {
		called = true
		return nil, nil
	})

	_, err := tool.Run(t.Context(), json.RawMessage(questionsArgs), func(policy.Target) (policy.Mode, error) { return 0, denied })

	assert.ErrorIs(t, err, denied)
	assert.False(t, called)
}

func TestAskUserQuestionPassesAskErrorThrough(t *testing.T) {
	failed := errors.New("no screen")
	tool := NewAskUserQuestion(answering(nil, failed))

	_, err := tool.Run(t.Context(), json.RawMessage(questionsArgs), allowTarget)

	assert.Equal(t, failed, err)
}

func TestAskUserQuestionRefusesMismatchedAnswers(t *testing.T) {
	one := Answer{Picks: []Pick{{Option: 0}}}
	multi := Answer{Picks: []Pick{{Option: 0}}}
	tests := []struct {
		name    string
		answers []Answer
	}{
		{"too few", []Answer{one}},
		{"out of range", []Answer{{Picks: []Pick{{Option: 2}}}, multi, one}},
		{"negative", []Answer{{Picks: []Pick{{Option: -1}}}, multi, one}},
		{"not ascending", []Answer{one, {Picks: []Pick{{Option: 1}, {Option: 0}}}, one}},
		{"repeated", []Answer{one, {Picks: []Pick{{Option: 1}, {Option: 1}}}, one}},
		{"single with two picks", []Answer{{Picks: []Pick{{Option: 0}, {Option: 1}}}, multi, one}},
		{"single with pick and custom", []Answer{{Picks: []Pick{{Option: 0}}, Custom: "x"}, multi, one}},
		{"single with nothing", []Answer{{Custom: " "}, multi, one}},
		{"multi with nothing", []Answer{one, {}, one}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewAskUserQuestion(answering(tc.answers, nil))

			_, err := tool.Run(t.Context(), json.RawMessage(questionsArgs), allowTarget)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "failed to read the answers: ")
			assert.NotErrorIs(t, err, ErrInvalidArgs)
		})
	}
}

func TestAskUserQuestionTitle(t *testing.T) {
	title := NewAskUserQuestion(nil).Display.Title

	done := NewAskUserQuestion(nil).Display.Done

	assert.Equal(t, "AskUserQuestion", title(json.RawMessage(questionsArgs)))
	assert.Equal(t, "Asked about Database, Features, Target", done(json.RawMessage(questionsArgs)))
	assert.Equal(t, "Asked", done(json.RawMessage(`{`)))
	assert.Equal(t, "Asked", done(json.RawMessage(`{}`)))
}

func TestAskUserQuestionRefused(t *testing.T) {
	refused := NewAskUserQuestion(nil).Display.Refused

	assert.True(t, refused(questionsDismissed))
	assert.False(t, refused("The user answered:\n"+`"Which database?" = "PostgreSQL"`))
}

func TestAskUserQuestionSummary(t *testing.T) {
	summary := NewAskUserQuestion(nil).Display.Summary

	assert.Equal(t, "dismissed", summary("The user dismissed the questions without answering."))
	assert.Equal(t, `"PostgreSQL" with note "pin to 16" · "Caching", typed "x"`, summary("The user answered:\n"+
		`"Which database?" = "PostgreSQL" with note "pin to 16"`+"\n"+
		`"Which features?" = "Caching", typed "x"`))
}
