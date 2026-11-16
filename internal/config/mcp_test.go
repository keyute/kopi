package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func projectFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestProjectServersMissingFile(t *testing.T) {
	servers, invalid, err := ProjectServers(filepath.Join(t.TempDir(), ".mcp.json"))
	require.NoError(t, err)
	assert.Empty(t, invalid)
	assert.Nil(t, servers)
}

func TestProjectServersReadsEntriesLeniently(t *testing.T) {
	path := projectFile(t, `{
		"mcpServers": {
			"local": {"command": "npx", "args": ["-y", "pkg"], "env": {"A": "b"}, "disabled": false},
			"remote": {"type": "http", "url": "https://example.test/mcp", "headers": {"X": "y"}, "oauth": {"clientId": "c"}, "timeout": 5},
			"legacy": {"type": "sse", "url": "https://example.test/sse"},
			"broken": {"command": "srv", "url": "https://example.test"}
		},
		"other": true
	}`)
	servers, invalid, err := ProjectServers(path)
	require.NoError(t, err)
	assert.Empty(t, invalid)
	assert.Equal(t, map[string]MCPServer{
		"local":  {Command: "npx", Args: []string{"-y", "pkg"}, Env: map[string]string{"A": "b"}},
		"remote": {Type: "http", URL: "https://example.test/mcp", Headers: map[string]string{"X": "y"}},
		"legacy": {Type: "sse", URL: "https://example.test/sse"},
		"broken": {Command: "srv", URL: "https://example.test"},
	}, servers)
}

func TestProjectServersRejectsMalformedFile(t *testing.T) {
	for _, tc := range []struct {
		name, content string
	}{
		{"malformed", `{"mcpServers": {`},
		{"not an object", `["srv"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := projectFile(t, tc.content)
			_, invalid, err := ProjectServers(path)
			assert.Empty(t, invalid)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "failed to parse")
			assert.Contains(t, err.Error(), path)
		})
	}
}

func TestProjectServersDecodesEntriesIndependently(t *testing.T) {
	path := projectFile(t, `{"mcpServers":{
		"healthy":{"command":"srv"},
		"bad-command":{"command":42},
		"bad-args":{"args":"not-an-array"}
	}}`)

	servers, invalid, err := ProjectServers(path)

	require.NoError(t, err)
	assert.Equal(t, map[string]MCPServer{"healthy": {Command: "srv"}}, servers)
	require.Len(t, invalid, 2)
	assert.ErrorContains(t, invalid["bad-command"], "failed to decode MCP server")
	assert.ErrorContains(t, invalid["bad-args"], "failed to decode MCP server")
}

func lookupIn(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

func TestExpand(t *testing.T) {
	s := MCPServer{
		Command: "${BIN}",
		Args:    []string{"--dir=${DIR:-/srv}", "$BIN", "${EMPTY:-fallback}", "${BIN"},
		Env:     map[string]string{"TOKEN": "${TOKEN}"},
		URL:     "https://${HOST}/mcp",
		Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"},
	}
	got, err := s.Expand(lookupIn(map[string]string{"BIN": "srv", "EMPTY": "", "TOKEN": "t0k", "HOST": "example.test"}))
	require.NoError(t, err)
	assert.Equal(t, MCPServer{
		Command: "srv",
		Args:    []string{"--dir=/srv", "$BIN", "fallback", "${BIN"},
		Env:     map[string]string{"TOKEN": "t0k"},
		URL:     "https://example.test/mcp",
		Headers: map[string]string{"Authorization": "Bearer t0k"},
	}, got)

	assert.Equal(t, "${BIN}", s.Command)
	assert.Equal(t, []string{"--dir=${DIR:-/srv}", "$BIN", "${EMPTY:-fallback}", "${BIN"}, s.Args)
	assert.Equal(t, map[string]string{"TOKEN": "${TOKEN}"}, s.Env)
	assert.Equal(t, map[string]string{"Authorization": "Bearer ${TOKEN}"}, s.Headers)
}

func TestExpandSetVariableOverridesDefault(t *testing.T) {
	got, err := MCPServer{URL: "${URL:-https://default.test}"}.Expand(lookupIn(map[string]string{"URL": "https://set.test"}))
	require.NoError(t, err)
	assert.Equal(t, "https://set.test", got.URL)
}

func TestExpandUnsetVariableFails(t *testing.T) {
	_, err := MCPServer{Command: "srv", Env: map[string]string{"TOKEN": "${MISSING_TOKEN}"}}.Expand(lookupIn(nil))
	require.ErrorIs(t, err, ErrUnsetVariable)
	assert.Contains(t, err.Error(), "MISSING_TOKEN")
}
