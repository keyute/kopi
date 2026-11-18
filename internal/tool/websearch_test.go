package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

type searchCall struct{ system, query string }

// recordSearches returns res and err and records each call.
func recordSearches(res SearchResult, err error) (func(context.Context, string, string) (SearchResult, error), *[]searchCall) {
	var calls []searchCall
	return func(_ context.Context, system, query string) (SearchResult, error) {
		calls = append(calls, searchCall{system, query})
		return res, err
	}, &calls
}

func TestWebSearchDescribesItself(t *testing.T) {
	search, _ := recordSearches(SearchResult{}, nil)
	tool := NewWebSearch(search)

	assert.Equal(t, "WebSearch", tool.Name)
	assert.Equal(t, "WebSearch", tool.Label)
	assert.Equal(t, []string{"query"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	assert.Contains(t, tool.Parameters["properties"], "query")
	assert.Equal(t, policy.Read, tool.Capability)
	assert.True(t, tool.Ask)
	assert.Nil(t, tool.Target)
	assert.False(t, tool.Sandboxed)
	assert.False(t, tool.PlanSandboxed)
	assert.Equal(t, Action{Verb: "Ran", One: "web search", Many: "web searches"}, tool.Display.Action)
}

func TestWebSearchRejectsInvalidArgs(t *testing.T) {
	search, calls := recordSearches(SearchResult{}, nil)
	tool := NewWebSearch(search)

	for _, raw := range []string{`{}`, `{"query":" "}`, `{"query":"q","extra":1}`} {
		_, err := tool.Run(t.Context(), json.RawMessage(raw), admitRead)

		require.ErrorIs(t, err, ErrInvalidArgs, raw)
	}
	assert.Empty(t, *calls)
}

func TestWebSearchSearchesNothingWithoutAdmission(t *testing.T) {
	search, calls := recordSearches(SearchResult{}, nil)
	tool := NewWebSearch(search)
	refused := errors.New("refused")

	_, err := tool.Run(t.Context(), json.RawMessage(`{"query":"q"}`), func(target policy.Target) (policy.Mode, error) {
		assert.Equal(t, policy.Target{}, target)
		return policy.Plan, refused
	})

	require.ErrorIs(t, err, refused)
	assert.Empty(t, *calls)
}

func TestWebSearchReturnsAnswerAndSources(t *testing.T) {
	search, calls := recordSearches(SearchResult{
		Answer: "Go 1.27 is out.",
		Sources: []Source{
			{Title: "Go\n  1.27 notes", URL: "https://go.dev/doc/go1.27"},
			{URL: "https://example.com/news"},
		},
	}, nil)
	tool := NewWebSearch(search)

	out, err := tool.Run(t.Context(), json.RawMessage(`{"query":"latest Go release"}`), admitRead)

	require.NoError(t, err)
	assert.Equal(t, "Go 1.27 is out.\n\nSources:\n- [Go 1.27 notes](https://go.dev/doc/go1.27)\n- https://example.com/news", out)
	assert.Equal(t, []searchCall{{webSearchSystemPrompt, "latest Go release"}}, *calls)
}

func TestWebSearchOmitsEmptySources(t *testing.T) {
	search, _ := recordSearches(SearchResult{Answer: "No idea."}, nil)
	tool := NewWebSearch(search)

	out, err := tool.Run(t.Context(), json.RawMessage(`{"query":"q"}`), admitRead)

	require.NoError(t, err)
	assert.Equal(t, "No idea.", out)
}

func TestWebSearchCutsSourcesToTwenty(t *testing.T) {
	var sources []Source
	for i := range 25 {
		sources = append(sources, Source{URL: fmt.Sprintf("https://example.com/%d", i)})
	}
	search, _ := recordSearches(SearchResult{Answer: "a", Sources: sources}, nil)
	tool := NewWebSearch(search)

	out, err := tool.Run(t.Context(), json.RawMessage(`{"query":"q"}`), admitRead)

	require.NoError(t, err)
	assert.Contains(t, out, "- https://example.com/19")
	assert.NotContains(t, out, "https://example.com/20")
	assert.Equal(t, "20 sources", tool.Display.Summary(out))
}

func TestWebSearchWrapsSearchError(t *testing.T) {
	failure := errors.New("provider said no")
	search, _ := recordSearches(SearchResult{}, failure)
	tool := NewWebSearch(search)

	_, err := tool.Run(t.Context(), json.RawMessage(`{"query":"q"}`), admitRead)

	require.ErrorIs(t, err, failure)
	assert.Contains(t, err.Error(), "failed to search the web")
}

func TestWebSearchDisplay(t *testing.T) {
	search, _ := recordSearches(SearchResult{}, nil)
	d := NewWebSearch(search).Display

	assert.Equal(t, "WebSearch go release", d.Title(json.RawMessage(`{"query":"go release"}`)))
	assert.Equal(t, "WebSearch", d.Title(json.RawMessage(`{}`)))
	assert.Equal(t, "no sources", d.Summary("answer"))
	assert.Equal(t, "1 source", d.Summary("answer\n\nSources:\n- https://a.example"))
	assert.Equal(t, "2 sources", d.Summary("answer\n\nSources:\n- https://a.example\n- https://b.example"))
}

func TestWebSearchDisplayPreview(t *testing.T) {
	search, _ := recordSearches(SearchResult{}, nil)
	d := NewWebSearch(search).Display
	preview := func(query string) []PreviewLine {
		raw, err := json.Marshal(map[string]string{"query": query})
		require.NoError(t, err)
		return d.Preview(raw)
	}
	long := strings.Repeat("q", maxBashTitleRunes+1)

	assert.Empty(t, preview("go release"))
	assert.Equal(t, []PreviewLine{{Kind: Context, Text: long}}, preview(long))
	assert.Equal(t, []PreviewLine{{Kind: Context, Text: "go"}, {Kind: Context, Text: "release"}}, preview("go\nrelease"))
}
