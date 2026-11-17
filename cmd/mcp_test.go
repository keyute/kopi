package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest/mcpfake"
)

const stdioHelperEnv = "KOPI_MCP_STDIO_HELPER"

// TestMCPStdioHelper is not a test: the stdio tests run the test binary as a
// server through it.
func TestMCPStdioHelper(t *testing.T) {
	if os.Getenv(stdioHelperEnv) != "serve" {
		t.Skip("helper process for the stdio tests")
	}
	fake := &mcpfake.Server{Tools: []map[string]any{
		{"name": "search", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}},
		{"name": "write", "inputSchema": map[string]any{"type": "object"}},
		{"name": strings.Repeat("x", 60), "inputSchema": map[string]any{"type": "object"}},
	}, Reply: func(name string, _ json.RawMessage) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "synthetic MCP result from " + name}}}
	}}
	if err := fake.ServeStdio(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// writeServers writes servers as the mcpServers of path.
func (e testEnv) writeServers(t *testing.T, path string, servers map[string]any) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"mcpServers": servers})
	require.NoError(t, err)
	e.writeFile(t, path, string(b))
}

func TestMCPListWithoutServers(t *testing.T) {
	e := newTestEnv(t)
	before := pigruntimetest.Snapshot(t, e.root)

	stdout, _, err := execute(t, "", "mcp", "list")

	require.NoError(t, err)
	assert.Equal(t, "No MCP servers configured: declare them under mcpServers in "+filepath.Join(e.kopiHome, "config.json")+" or in .mcp.json in this folder.\n", stdout)
	assert.Equal(t, before, pigruntimetest.Snapshot(t, e.root))
}

func TestServerBlockStripsControlsFromWhere(t *testing.T) {
	for _, where := range []string{"global (/config\x1b[31m\a\n\tpath)", "project (/config\x1b[31m\a\n\tpath)"} {
		t.Run(strings.Split(where, " ")[0], func(t *testing.T) {
			got := serverBlock(pigruntime.ServerStatus{Name: "fake"}, where)

			assert.Equal(t, "fake  "+strings.Split(where, " ")[0]+" (/config[31m  path)\n  connected, 0 tools\n", got)
		})
	}
}

func TestMCPListShowsAFailedServer(t *testing.T) {
	e := newTestEnv(t)
	configFile := filepath.Join(e.kopiHome, "config.json")
	e.writeServers(t, configFile, map[string]any{"broken": map[string]any{"command": "kopi-no-such-command-7f3a", "env": map[string]string{"TOKEN": "secret-env-5d1e"}}})

	stdout, _, err := execute(t, "", "mcp", "list")

	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(stdout, "broken  global ("+configFile+")\n  failed: "), stdout)
	assert.Contains(t, stdout, "kopi-no-such-command-7f3a")
	assert.NotContains(t, stdout, "secret-env-5d1e")
}

func TestMCPListShowsToolsOfAStdioServer(t *testing.T) {
	e := newTestEnv(t)
	configFile := filepath.Join(e.kopiHome, "config.json")
	e.writeServers(t, configFile, map[string]any{"fake": map[string]any{
		"command": os.Args[0],
		"args":    []string{"-test.run=^TestMCPStdioHelper$"},
		"env":     map[string]string{stdioHelperEnv: "serve"},
	}})

	stdout, _, err := execute(t, "", "mcp", "list")

	require.NoError(t, err)
	assert.Equal(t, "fake  global ("+configFile+")\n"+
		"  connected, 2 tools\n"+
		"    mcp__fake__search  read-only\n"+
		"    mcp__fake__write   execute\n"+
		"  skipped "+strings.Repeat("x", 60)+": name longer than 64 characters\n", stdout)
}

func TestMCPListInAnUndecidedFolder(t *testing.T) {
	e := newTestEnv(t)
	configFile := filepath.Join(e.kopiHome, "config.json")
	e.writeServers(t, configFile, map[string]any{"broken": map[string]any{"command": "kopi-no-such-command-7f3a"}})
	projectFile := filepath.Join(e.workspace, ".mcp.json")
	e.writeServers(t, projectFile, map[string]any{"proj": map[string]any{"command": "kopi-no-such-command-91b0"}})

	stdout, stderr, err := execute(t, "", "mcp", "list")

	require.NoError(t, err)
	blocks := strings.Split(stdout, "\n\n")
	require.Len(t, blocks, 2, stdout)
	assert.True(t, strings.HasPrefix(blocks[0], "broken  global ("+configFile+")\n  failed: "), blocks[0])
	assert.Equal(t, "proj  project ("+projectFile+")\n  not trusted, not started\n", blocks[1])
	assert.NotContains(t, stdout+stderr, "kopi-no-such-command-91b0", "the project's server is not started")
	assert.NoFileExists(t, filepath.Join(e.kopiHome, "trust.json"))
}

func TestMCPListShowsATrustedProjectServerOnce(t *testing.T) {
	e := newTestEnv(t)
	e.writeServers(t, filepath.Join(e.kopiHome, "config.json"), map[string]any{"shared": map[string]any{"url": "https://global.example.com/mcp"}})
	projectFile := filepath.Join(e.workspace, ".mcp.json")
	e.writeServers(t, projectFile, map[string]any{"shared": map[string]any{"command": "kopi-no-such-command-91b0"}})
	b, err := json.Marshal(map[string]any{"version": 1, "folders": map[string]string{canonical(t, e.workspace): "trusted"}})
	require.NoError(t, err)
	e.writeFile(t, filepath.Join(e.kopiHome, "trust.json"), string(b))

	stdout, _, err := execute(t, "", "mcp", "list")

	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(stdout, "shared  "), stdout)
	assert.True(t, strings.HasPrefix(stdout, "shared  project ("+projectFile+")\n  failed: "), stdout)
	assert.Contains(t, stdout, "kopi-no-such-command-91b0")
}

func TestMCPListStripsControlsFromUntrustedText(t *testing.T) {
	e := newTestEnv(t)
	configFile := filepath.Join(e.kopiHome, "config.json")
	e.writeServers(t, configFile, map[string]any{"broken": map[string]any{"command": "kopi-no-such-\x1b[2J-command"}})
	projectFile := filepath.Join(e.workspace, ".mcp.json")
	e.writeServers(t, projectFile, map[string]any{"evil\x1b]52;c;c2VjcmV0\a\nfake": map[string]any{"command": "x"}})

	stdout, _, err := execute(t, "", "mcp", "list")

	require.NoError(t, err)
	assert.NotContains(t, stdout, "\x1b")
	assert.NotContains(t, stdout, "\a")
	assert.Contains(t, stdout, "kopi-no-such-[2J-command", "the error is printed without ESC")
	blocks := strings.Split(stdout, "\n\n")
	require.Len(t, blocks, 2, stdout)
	assert.Equal(t, "evil]52;c;c2VjcmV0 fake  project ("+projectFile+")\n  not trusted, not started\n", blocks[1])
}

func TestMCPListInvalidProjectDeclarations(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprint(trusted), func(t *testing.T) {
			e := newTestEnv(t)
			e.writeServers(t, filepath.Join(e.kopiHome, "config.json"), map[string]any{"shared": map[string]any{"command": "global-must-not-start"}})
			projectFile := filepath.Join(e.workspace, ".mcp.json")
			e.writeServers(t, projectFile, map[string]any{
				"shared":         map[string]any{"command": 42},
				"evil\x1b[31m\a": map[string]any{"args": false},
				"healthy": map[string]any{
					"command": os.Args[0],
					"args":    []string{"-test.run=^TestMCPStdioHelper$"},
					"env":     map[string]string{stdioHelperEnv: "serve"},
				},
			})
			if trusted {
				b, err := json.Marshal(map[string]any{"version": 1, "folders": map[string]string{canonical(t, e.workspace): "trusted"}})
				require.NoError(t, err)
				e.writeFile(t, filepath.Join(e.kopiHome, "trust.json"), string(b))
			}

			stdout, stderr, err := execute(t, "", "mcp", "list")

			require.NoError(t, err)
			assert.NotContains(t, stdout+stderr, "\x1b")
			assert.NotContains(t, stdout+stderr, "\a")
			assert.Equal(t, 2, strings.Count(stderr, "failed to decode MCP server"))
			assert.Contains(t, stdout, "evil[31m  project ("+projectFile+")")
			if trusted {
				assert.NotContains(t, stdout+stderr, "global-must-not-start")
				assert.Equal(t, 2, strings.Count(stdout, "failed: failed to decode MCP server"))
				assert.Contains(t, stdout, "connected, 2 tools")
				assert.Equal(t, 1, strings.Count(stdout, "shared  "))
			} else {
				assert.Equal(t, 3, strings.Count(stdout, "not trusted, not started"))
				assert.NotContains(t, stdout, "connected")
			}
		})
	}
}

func TestPrintable(t *testing.T) {
	assert.Equal(t, "a b c", printable("a\nb\r\nc", ""))
	assert.Equal(t, "a\n    b\n    [31mc", printable("a\nb\r\n\x1b[31mc", "    "))
	assert.Equal(t, "xy", printable("x\x00\x7f\u009by", ""))
}

func TestMCPListStripsControlsFromNotices(t *testing.T) {
	e := newTestEnv(t)
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), `{"mcpServers":{"evil\u001b[31mRED\u001b[0m":{"command":5}}}`)

	_, stderr, err := execute(t, "", "mcp", "list")

	require.NoError(t, err)
	assert.Contains(t, stderr, "failed to decode MCP server")
	assert.Contains(t, stderr, "evil[31mRED[0m")
	assert.NotContains(t, stderr, "\x1b")
}
