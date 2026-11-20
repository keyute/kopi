package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

var searchable = []Tool{
	{Name: "mcp__gh__get_issue", Description: "Gets an issue.\nMore detail."},
	{Name: "mcp__gh__list_issues", Description: "Lists issues."},
	{Name: "mcp__docs__fetch"},
}

func noRank(string, []Tool, int) []string { return nil }

func noLoad(context.Context, []string) error { return nil }

func TestToolSearchDescribesItself(t *testing.T) {
	tool := NewToolSearch(searchable, noRank, noLoad)

	assert.Equal(t, "ToolSearch", tool.Name)
	assert.Equal(t, "ToolSearch", tool.Label)
	assert.Equal(t, "Loads deferred tools, such as the MCP server tools the system prompt lists by name, so that they can be called. "+
		"query is keywords for what you need, or exact tool names separated by spaces to load just those. "+
		"Returns the name and description of each tool it loaded; call them in your next response.", tool.Description)
	assert.Equal(t, []string{"query"}, tool.Parameters["required"])
	assert.Equal(t, policy.Read, tool.Capability)
	assert.False(t, tool.Ask)
	assert.Nil(t, tool.Target)
	assert.False(t, tool.Sandboxed)
	assert.False(t, tool.PlanSandboxed)
	assert.Equal(t, Action{Verb: "Ran", One: "tool search", Many: "tool searches"}, tool.Display.Action)
}

func TestToolSearchAdmitsBeforeLoading(t *testing.T) {
	var order []string
	tool := NewToolSearch(searchable,
		func(string, []Tool, int) []string {
			order = append(order, "rank")
			return []string{"mcp__docs__fetch"}
		},
		func(context.Context, []string) error {
			order = append(order, "load")
			return nil
		})

	_, err := tool.Run(t.Context(), json.RawMessage(`{"query":"docs"}`), func(target policy.Target) (policy.Mode, error) {
		order = append(order, "admit")
		assert.Equal(t, policy.Target{}, target)
		return policy.Plan, nil
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"admit", "rank", "load"}, order)
}

func TestToolSearchDoesNotLoadWithoutAdmission(t *testing.T) {
	loaded := false
	tool := NewToolSearch(searchable, noRank, func(context.Context, []string) error {
		loaded = true
		return nil
	})
	refused := errors.New("refused")

	_, err := tool.Run(t.Context(), json.RawMessage(`{"query":"mcp__docs__fetch"}`), func(policy.Target) (policy.Mode, error) { return policy.Execute, refused })

	require.ErrorIs(t, err, refused)
	assert.False(t, loaded)
}

func TestToolSearchLoadsRankedMatches(t *testing.T) {
	var loaded []string
	tool := NewToolSearch(searchable,
		func(query string, tools []Tool, limit int) []string {
			assert.Equal(t, "issue tracker", query)
			assert.Equal(t, searchable, tools)
			assert.Equal(t, 8, limit)
			return []string{"mcp__gh__get_issue", "mcp__docs__fetch"}
		},
		func(_ context.Context, names []string) error {
			loaded = names
			return nil
		})

	out, err := tool.Run(t.Context(), json.RawMessage(`{"query":"issue tracker"}`), allowTarget)

	require.NoError(t, err)
	assert.Equal(t, []string{"mcp__gh__get_issue", "mcp__docs__fetch"}, loaded)
	assert.Equal(t, "Loaded 2 tools; call them in your next response:\n- mcp__gh__get_issue: Gets an issue.\n- mcp__docs__fetch", out.Text)
	assert.Equal(t, "loaded 2 tools", tool.Display.Summary(out.Text))
	assert.Equal(t, out.Text, tool.Display.Body(out.Text))

	failed := errors.New("failed")
	failing := NewToolSearch(searchable, func(string, []Tool, int) []string { return []string{"mcp__docs__fetch"} },
		func(context.Context, []string) error { return failed })
	_, err = failing.Run(t.Context(), json.RawMessage(`{"query":"docs","limit":3}`), allowTarget)
	require.ErrorIs(t, err, failed)
	assert.Equal(t, "failed to load tools: failed", err.Error())
}

func TestToolSearchExactNamesLoadOnlyThose(t *testing.T) {
	var loaded []string
	tool := NewToolSearch(searchable,
		func(string, []Tool, int) []string {
			t.Error("rank called for exact names")
			return nil
		},
		func(_ context.Context, names []string) error {
			loaded = names
			return nil
		})

	out, err := tool.Run(t.Context(), json.RawMessage(`{"query":" mcp__gh__list_issues  mcp__docs__fetch mcp__gh__list_issues","limit":1}`), allowTarget)

	require.NoError(t, err)
	assert.Equal(t, []string{"mcp__gh__list_issues", "mcp__docs__fetch"}, loaded)
	assert.Equal(t, "Loaded 2 tools; call them in your next response:\n- mcp__gh__list_issues: Lists issues.\n- mcp__docs__fetch", out.Text)

	out, err = tool.Run(t.Context(), json.RawMessage(`{"query":"mcp__gh__get_issue"}`), allowTarget)
	require.NoError(t, err)
	assert.Equal(t, "Loaded 1 tool; call it in your next response:\n- mcp__gh__get_issue: Gets an issue.", out.Text)
	assert.Equal(t, "loaded 1 tool", tool.Display.Summary(out.Text))
}

func TestToolSearchReportsNoMatch(t *testing.T) {
	loaded := false
	tool := NewToolSearch(searchable, noRank, func(context.Context, []string) error {
		loaded = true
		return nil
	})

	out, err := tool.Run(t.Context(), json.RawMessage(`{"query":"mcp__gh__get_issue weather"}`), allowTarget)

	require.NoError(t, err)
	assert.Equal(t, `No deferred tool matches "mcp__gh__get_issue weather". Try other keywords, or exact names from the system prompt.`, out.Text)
	assert.Equal(t, "no match", tool.Display.Summary(out.Text))
	assert.Empty(t, tool.Display.Body(out.Text))
	assert.Equal(t, `Searched tools for "mcp__gh__get_issue weather"`, tool.Display.Done(json.RawMessage(`{"query":"mcp__gh__get_issue weather"}`)))
	assert.False(t, loaded)
}

func TestToolSearchRejectsBlankQuery(t *testing.T) {
	tool := NewToolSearch(searchable, noRank, noLoad)
	for _, args := range []string{`{}`, `{"query":"  "}`, `{"query":"docs","limit":0}`, `{"query":"docs","limit":21}`, `{"query":"docs","extra":1}`} {
		t.Run(args, func(t *testing.T) {
			admitted := false
			_, err := tool.Run(t.Context(), json.RawMessage(args), func(policy.Target) (policy.Mode, error) {
				admitted = true
				return policy.Execute, nil
			})
			require.ErrorIs(t, err, ErrInvalidArgs)
			assert.False(t, admitted)
		})
	}
}

func TestToolSearchDisplay(t *testing.T) {
	d := NewToolSearch(searchable, noRank, noLoad).Display

	for _, tc := range []struct{ name, args, want string }{
		{"no query", `{}`, "ToolSearch"},
		{"invalid", `{"query":`, "ToolSearch"},
		{"query", `{"query":"github issues"}`, "ToolSearch github issues"},
		{"multiline", `{"query":"a\nb"}`, "ToolSearch a …"},
		{"cut", `{"query":"` + strings.Repeat("x", 100) + `"}`, "ToolSearch " + strings.Repeat("x", 80-len("ToolSearch ")) + " …"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, d.Title(json.RawMessage(tc.args)))
		})
	}
}
