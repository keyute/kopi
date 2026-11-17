package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

func TestMCPName(t *testing.T) {
	for _, tc := range []struct{ server, tool, want string }{
		{"context7", "resolve-library-id", "mcp__context7__resolve-library-id"},
		{"my.server", "get file", "mcp__my_server__get_file"},
		{"café", "a/b:c", "mcp__caf___a_b_c"},
	} {
		assert.Equal(t, tc.want, MCPName(tc.server, tc.tool))
	}
}

func noCall(context.Context, json.RawMessage) (string, error) { return "", nil }

func TestMCPDescribesItself(t *testing.T) {
	params := map[string]any{"type": "object"}
	tool := NewMCP(MCPTool{Server: "gh", Tool: "get_issue", Description: "Gets an issue.", Parameters: params}, noCall)

	assert.Equal(t, "mcp__gh__get_issue", tool.Name)
	assert.Equal(t, "gh/get_issue", tool.Label)
	assert.Equal(t, "Gets an issue.", tool.Description)
	assert.Equal(t, params, tool.Parameters)
	assert.Equal(t, policy.Exec, tool.Capability)
	assert.True(t, tool.Ask)
	assert.Nil(t, tool.Target)
	assert.False(t, tool.Sandboxed)
	assert.Equal(t, Action{Verb: "Called", One: "MCP tool", Many: "MCP tools"}, tool.Display.Action)
	assert.True(t, Specs([]Tool{tool})[tool.Name].Ask)

	readOnly := NewMCP(MCPTool{Server: "gh", Tool: "list", ReadOnly: true}, noCall)
	assert.Equal(t, policy.Read, readOnly.Capability)
	assert.Equal(t, "Tool list of MCP server gh.", readOnly.Description)
}

func TestMCPAdmitsBeforeCalling(t *testing.T) {
	var order []string
	args := json.RawMessage(`{"q":1}`)
	tool := NewMCP(MCPTool{Server: "s", Tool: "t"}, func(_ context.Context, got json.RawMessage) (string, error) {
		order = append(order, "call")
		assert.Equal(t, args, got)
		return "result", nil
	})

	out, err := tool.Run(t.Context(), args, func(target policy.Target) (policy.Mode, error) {
		order = append(order, "admit")
		assert.Equal(t, policy.Target{}, target)
		return policy.Execute, nil
	})

	require.NoError(t, err)
	assert.Equal(t, "result", out)
	assert.Equal(t, []string{"admit", "call"}, order)
}

func TestMCPDoesNotCallWithoutAdmission(t *testing.T) {
	called := false
	tool := NewMCP(MCPTool{Server: "s", Tool: "t"}, func(context.Context, json.RawMessage) (string, error) {
		called = true
		return "", nil
	})
	refused := errors.New("refused")

	_, err := tool.Run(t.Context(), json.RawMessage(`{}`), func(policy.Target) (policy.Mode, error) { return policy.Execute, refused })

	require.ErrorIs(t, err, refused)
	assert.False(t, called)
}

func TestMCPResult(t *testing.T) {
	result := func(text string, err error) Tool {
		return NewMCP(MCPTool{Server: "s", Tool: "t"}, func(context.Context, json.RawMessage) (string, error) { return text, err })
	}
	failed := errors.New("failed")
	for _, text := range []string{"", "short error"} {
		out, err := result(text, failed).Run(t.Context(), nil, allowTarget)
		require.ErrorIs(t, err, failed)
		want := failed.Error()
		if text != "" {
			want += ": " + text
		}
		assert.Equal(t, want, err.Error())
		assert.Empty(t, out)
	}

	for _, text := range []string{"", "short supplemental", strings.Repeat("界", 3*maxOutputPart) + "tail"} {
		t.Run(fmt.Sprintf("huge error supplemental bytes %d", len(text)), func(t *testing.T) {
			cause := errors.New("head" + strings.Repeat("界", 3*maxOutputPart) + "end")
			failed := &url.Error{Op: "call", URL: "synthetic", Err: cause}
			out, err := result(text, failed).Run(t.Context(), nil, allowTarget)
			require.ErrorIs(t, err, failed)
			require.ErrorIs(t, err, cause)
			var original *url.Error
			require.ErrorAs(t, err, &original)
			assert.Same(t, failed, original)
			assert.Empty(t, out)
			complete := failed.Error()
			if text != "" {
				complete += ": " + text
			}
			assert.Contains(t, err.Error(), fmt.Sprintf("%d bytes cut", len(complete)-2*maxOutputPart))
			assert.LessOrEqual(t, len(err.Error()), 2*maxOutputPart+100)
			assert.True(t, utf8.ValidString(err.Error()))
			assert.True(t, strings.HasPrefix(err.Error(), "call \"synthetic\": head"))
			assert.True(t, strings.HasSuffix(err.Error(), complete[len(complete)-3:]))
		})
	}

	out, err := result("", nil).Run(t.Context(), nil, allowTarget)
	require.NoError(t, err)
	assert.Equal(t, "no output", result("", nil).Display.Summary(out))

	long := "head" + strings.Repeat("x", 3*maxOutputPart) + "tail"
	out, err = result(long, nil).Run(t.Context(), nil, allowTarget)
	require.NoError(t, err)
	assert.Less(t, len(out), len(long))
	assert.True(t, strings.HasPrefix(out, "head"))
	assert.True(t, strings.HasSuffix(out, "tail"))
	assert.Contains(t, out, "bytes cut")

	failed = errors.New("failed")
	errorOut, err := result(long, failed).Run(t.Context(), nil, allowTarget)
	require.ErrorIs(t, err, failed)
	assert.Empty(t, errorOut)
	assert.True(t, strings.HasPrefix(err.Error(), "failed: head"))
	assert.True(t, strings.HasSuffix(err.Error(), "tail"))
	assert.Contains(t, err.Error(), fmt.Sprintf("%d bytes cut", len(failed.Error()+": "+long)-2*maxOutputPart))
	assert.LessOrEqual(t, len(err.Error()), 2*maxOutputPart+100)
}

func TestMCPDisplay(t *testing.T) {
	d := NewMCP(MCPTool{Server: "gh", Tool: "get_issue"}, noCall).Display

	for _, tc := range []struct{ name, args, want string }{
		{"no args", `{}`, "gh/get_issue"},
		{"invalid", `{"a":`, "gh/get_issue"},
		{"compact", "{\n  \"n\": 1,\n  \"s\": \"a\\nb\"\n}", `gh/get_issue {"n":1,"s":"a\nb"}`},
		{"cut", `{"s":"` + strings.Repeat("x", 100) + `"}`, `gh/get_issue {"s":"` + strings.Repeat("x", 80-len(`gh/get_issue {"s":"`)) + " …"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, d.Title(json.RawMessage(tc.args)))
		})
	}
	assert.Equal(t, "a …", NewMCP(MCPTool{Server: "a\nb", Tool: "t"}, noCall).Display.Title(json.RawMessage(`{}`)))

	assert.Equal(t, []PreviewLine{
		{Kind: Context, Text: "{"},
		{Kind: Context, Text: `  "n": 1`},
		{Kind: Context, Text: "}"},
	}, d.Preview(json.RawMessage(`{"n":1}`)))
}

func TestValidateMCPServerName(t *testing.T) {
	for _, name := range []string{"github", "github-other", "my.server", "my_server", "_github", "café-docs", "A1"} {
		assert.NoError(t, ValidateMCPServerName(name), name)
	}
	for _, name := range []string{"", "github__other", "github_", "github.", "github..other", "github._other", "café", ".."} {
		assert.ErrorIs(t, ValidateMCPServerName(name), ErrInvalidMCPServerName, name)
	}
}
