//go:build darwin || linux

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPListWithoutServersStripsControlsFromConfigPath(t *testing.T) {
	e := newTestEnv(t)
	home := filepath.Join(e.root, "kopi\x1b[31m\a\n\tpath")
	t.Setenv("KOPI_HOME", home)
	e.writeFile(t, filepath.Join(home, "config.json"), `{}`)

	stdout, _, err := execute(t, "", "mcp", "list")

	require.NoError(t, err)
	assert.Equal(t, "No MCP servers configured: declare them under mcpServers in "+filepath.Join(e.root, "kopi[31m  path", "config.json")+" or in .mcp.json in this folder.\n", stdout)
}

func TestMCPListStripsControlsFromUntrustedProjectPath(t *testing.T) {
	e := newTestEnv(t)
	workspace := filepath.Join(e.workspace, "project\x1b[31m\a\n\tpath")
	e.writeServers(t, filepath.Join(workspace, ".mcp.json"), map[string]any{"proj": map[string]any{"command": "must-not-start"}})
	t.Chdir(workspace)
	cwd, err := os.Getwd()
	require.NoError(t, err)

	stdout, _, err := execute(t, "", "mcp", "list")

	require.NoError(t, err)
	wantPath := filepath.Join(filepath.Dir(cwd), "project[31m  path", ".mcp.json")
	assert.Equal(t, "proj  project ("+wantPath+")\n  not trusted, not started\n", stdout)
}
