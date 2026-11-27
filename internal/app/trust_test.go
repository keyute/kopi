package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/trust"
)

type trustEnv struct {
	root, home, kopiHome, workspace string
}

// newTrustEnv is a home, a Kopi home and a workspace in one temp directory.
func newTrustEnv(t *testing.T) trustEnv {
	t.Helper()
	root := t.TempDir()
	e := trustEnv{
		root:      root,
		home:      filepath.Join(root, "home"),
		kopiHome:  filepath.Join(root, "kopi"),
		workspace: filepath.Join(root, "ws"),
	}
	for _, dir := range []string{e.home, e.kopiHome, e.workspace} {
		require.NoError(t, os.Mkdir(dir, 0o755))
	}
	return e
}

func (e trustEnv) writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// trustFolders is the folders object of the trust file, nil when there is none.
func trustFolders(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var f struct{ Folders map[string]string }
	require.NoError(t, json.Unmarshal(b, &f))
	return f.Folders
}

func canonical(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return p
}

func noAsk(t *testing.T) AskTrust {
	return func(TrustRequest) (TrustChoice, error) {
		t.Error("asked whether to trust the folder")
		return TrustCancelled, nil
	}
}

// askOnce records the request and cancels, which saves nothing.
func askOnce(asked *TrustRequest) AskTrust {
	return func(req TrustRequest) (TrustChoice, error) {
		*asked = req
		return TrustCancelled, nil
	}
}

const projectMCP = `{"mcpServers":{"shared":{"command":"npx","args":["-y","docs-mcp"],"env":{"TOKEN":"${TOKEN}"}},"remote":{"type":"http","url":"https://example.com/mcp"}},"other":true}`

var globalServers = map[string]config.MCPServer{
	"shared": {URL: "https://global.example.com/mcp"},
	"solo":   {Command: "solo-mcp"},
}

func TestCovers(t *testing.T) {
	home := t.TempDir()
	t.Run("servers and skills", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"b":{"command":"run","args":["x","y"]},"a":{"url":"https://example.com/mcp"},"bad":"nope"}}`), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".agents", "skills"), 0o700))

		servers, skills := covers(dir, home)

		assert.Equal(t, []string{"a: https://example.com/mcp", "b: run x y", "bad: invalid, will not start"}, servers)
		assert.Equal(t, []string{filepath.Join(dir, ".agents", "skills")}, skills)
	})
	t.Run("neither", func(t *testing.T) {
		servers, skills := covers(t.TempDir(), home)

		assert.Empty(t, servers)
		assert.Empty(t, skills)
	})
	t.Run("unreadable .mcp.json", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("{"), 0o600))

		servers, _ := covers(dir, home)

		assert.Empty(t, servers)
	})
}

func TestResolveServersWithoutProjectFile(t *testing.T) {
	e := newTrustEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")

	set, err := ResolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, noAsk(t))

	require.NoError(t, err)
	assert.Equal(t, globalServers, set.Servers)
	assert.Empty(t, set.Project)
	assert.Empty(t, set.Untrusted)
	assert.Empty(t, set.Errs)
	assert.NoFileExists(t, trustFile)

	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), `{"mcpServers":{}}`)
	_, err = ResolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, noAsk(t))
	require.NoError(t, err)
	assert.NoFileExists(t, trustFile, "a file declaring no servers needs no decision")
}

func TestResolveServersTrustedProjectReplacesGlobal(t *testing.T) {
	e := newTrustEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), projectMCP)
	b, err := json.Marshal(map[string]any{"version": 1, "folders": map[string]string{canonical(t, e.root): "trusted"}})
	require.NoError(t, err)
	e.writeFile(t, trustFile, string(b))

	set, err := ResolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, noAsk(t))

	require.NoError(t, err)
	assert.Equal(t, map[string]config.MCPServer{
		"shared": {Command: "npx", Args: []string{"-y", "docs-mcp"}, Env: map[string]string{"TOKEN": "${TOKEN}"}},
		"remote": {Type: "http", URL: "https://example.com/mcp"},
		"solo":   {Command: "solo-mcp"},
	}, set.Servers)
	assert.Equal(t, map[string]bool{"shared": true, "remote": true}, set.Project)
	assert.Empty(t, set.Untrusted)
	assert.False(t, set.Undecided)
}

func TestResolveServersUntrustedSkipsProject(t *testing.T) {
	e := newTrustEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), projectMCP)
	b, err := json.Marshal(map[string]any{"version": 1, "folders": map[string]string{canonical(t, e.workspace): "untrusted"}})
	require.NoError(t, err)
	e.writeFile(t, trustFile, string(b))

	set, err := ResolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, noAsk(t))

	require.NoError(t, err)
	assert.Equal(t, globalServers, set.Servers)
	assert.Empty(t, set.Project)
	assert.Len(t, set.Untrusted, 2)
	assert.False(t, set.Undecided)
	assert.Empty(t, set.Errs)
}

func TestResolveServersUndecidedWithoutAsk(t *testing.T) {
	e := newTrustEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), projectMCP)

	set, err := ResolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, nil)

	require.NoError(t, err)
	assert.Equal(t, globalServers, set.Servers)
	assert.Len(t, set.Untrusted, 2)
	assert.True(t, set.Undecided)
	assert.NoFileExists(t, trustFile)
}

func TestResolveServersAsksAndSaves(t *testing.T) {
	for _, tt := range []struct {
		name    string
		choice  TrustChoice
		trusted bool
		saved   func(e trustEnv) map[string]string
	}{
		{"folder", TrustFolder, true, func(e trustEnv) map[string]string { return map[string]string{canonical(t, e.workspace): "trusted"} }},
		{"parent", TrustParent, true, func(e trustEnv) map[string]string { return map[string]string{canonical(t, e.root): "trusted"} }},
		{"deny", TrustDeny, false, func(e trustEnv) map[string]string { return map[string]string{canonical(t, e.workspace): "untrusted"} }},
		{"cancel", TrustCancelled, false, func(trustEnv) map[string]string { return nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTrustEnv(t)
			trustFile := filepath.Join(e.kopiHome, "trust.json")
			e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), projectMCP)
			var asked []TrustRequest
			ask := func(req TrustRequest) (TrustChoice, error) {
				asked = append(asked, req)
				return tt.choice, nil
			}

			set, err := ResolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, ask)

			require.NoError(t, err)
			assert.Equal(t, []TrustRequest{{
				Folder:  canonical(t, e.workspace),
				Parent:  canonical(t, e.root),
				Servers: []string{"remote: https://example.com/mcp", "shared: npx -y docs-mcp (env: TOKEN)"},
			}}, asked)
			assert.Equal(t, tt.saved(e), trustFolders(t, trustFile))
			assert.Equal(t, tt.trusted, set.Project["shared"])
			assert.Equal(t, !tt.trusted, len(set.Untrusted) == 2)
			assert.False(t, set.Undecided)
		})
	}
}

func TestDecideTrustOffersNoParentAtHomeOrRoot(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	proj := filepath.Join(home, "proj")
	require.NoError(t, os.MkdirAll(proj, 0o755))
	homeLink := filepath.Join(root, "home-link")
	require.NoError(t, os.Symlink(home, homeLink))
	for _, tt := range []struct{ name, cwd, home string }{
		{"home", proj, home},
		{"at home", home, home},
		{"at home through a symlink", homeLink, home},
		{"home with a trailing slash", proj, home + string(filepath.Separator)},
		{"home through a symlink", proj, homeLink},
		{"working directory through a symlinked home", filepath.Join(homeLink, "proj"), home},
		{"root", "/", home},
		{"directly under root", "/usr", home},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var asked TrustRequest
			_, err := decideTrust(nil, tt.cwd, tt.home, map[string]config.MCPServer{"a": {Command: "a"}}, nil, nil, nil, askOnce(&asked))
			require.NoError(t, err)
			assert.Equal(t, canonical(t, tt.cwd), asked.Folder)
			assert.Empty(t, asked.Parent)
		})
	}
}

func TestDecideTrustTreatsHomeTypedInAnotherCaseAsHome(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("case-insensitive folders are checked on darwin")
	}
	home := filepath.Join(canonical(t, t.TempDir()), "home")
	require.NoError(t, os.Mkdir(home, 0o755))
	upper := filepath.Join(filepath.Dir(home), "HOME")
	if _, err := os.Stat(upper); err != nil {
		t.Skip("the temporary directory is on a case-sensitive volume")
	}
	trustFile := filepath.Join(t.TempDir(), "trust.json")
	store, err := trust.Open(trustFile)
	require.NoError(t, err)

	d, err := decideTrust(store, upper, home, map[string]config.MCPServer{"a": {Command: "a"}}, nil, nil, nil, func(TrustRequest) (TrustChoice, error) { return TrustFolder, nil })

	require.NoError(t, err)
	assert.Equal(t, trust.Trusted, d)
	assert.Nil(t, trustFolders(t, trustFile), "an answer at home holds for this run only")
}

func TestServerTargetNamesEnvAndHeadersWithoutValues(t *testing.T) {
	for _, tt := range []struct {
		name   string
		server config.MCPServer
		want   string
	}{
		{"command", config.MCPServer{Command: "npx", Args: []string{"-y", "pkg"}}, "npx -y pkg"},
		{"env", config.MCPServer{Command: "npx", Args: []string{"-y", "pkg"}, Env: map[string]string{"TOKEN": "secret", "NODE_OPTIONS": "--require x"}}, "npx -y pkg (env: NODE_OPTIONS, TOKEN)"},
		{"headers", config.MCPServer{URL: "https://x", Headers: map[string]string{"X-Api-Key": "secret", "Authorization": "Bearer secret"}}, "https://x (headers: Authorization, X-Api-Key)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, serverTarget(tt.server))
		})
	}
}

func TestDecideTrustAtHomeOrRootTrustsForThisRunOnly(t *testing.T) {
	home := canonical(t, t.TempDir())
	for _, tt := range []struct {
		name, cwd string
		choice    TrustChoice
		want      trust.Decision
		saved     map[string]string
	}{
		{"home trusted", home, TrustFolder, trust.Trusted, nil},
		{"root trusted", "/", TrustFolder, trust.Trusted, nil},
		{"home denied", home, TrustDeny, trust.Untrusted, nil},
		{"root denied", "/", TrustDeny, trust.Untrusted, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			trustFile := filepath.Join(t.TempDir(), "trust.json")
			store, err := trust.Open(trustFile)
			require.NoError(t, err)

			d, err := decideTrust(store, tt.cwd, home, map[string]config.MCPServer{"a": {Command: "a"}}, nil, nil, nil, func(TrustRequest) (TrustChoice, error) { return tt.choice, nil })

			require.NoError(t, err)
			assert.Equal(t, tt.want, d)
			assert.Equal(t, tt.saved, trustFolders(t, trustFile))
		})
	}
}

func TestDecideTrustResolvesASymlinkedWorkingDirectory(t *testing.T) {
	e := newTrustEnv(t)
	real := filepath.Join(e.root, "real", "proj")
	require.NoError(t, os.MkdirAll(real, 0o755))
	link := filepath.Join(e.root, "links", "proj")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
	require.NoError(t, os.Symlink(real, link))
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	store, err := trust.Open(trustFile)
	require.NoError(t, err)
	var asked TrustRequest
	ask := func(req TrustRequest) (TrustChoice, error) {
		asked = req
		return TrustParent, nil
	}

	d, err := decideTrust(store, link, e.home, map[string]config.MCPServer{"a": {Command: "a"}}, nil, nil, nil, ask)

	require.NoError(t, err)
	assert.Equal(t, trust.Trusted, d)
	assert.Equal(t, canonical(t, real), asked.Folder)
	assert.Equal(t, canonical(t, filepath.Dir(real)), asked.Parent, "the parent of the real folder, not of the link")
	assert.Equal(t, map[string]string{asked.Parent: "trusted"}, trustFolders(t, trustFile), "exactly the path shown is saved")
	again, err := trust.Open(trustFile)
	require.NoError(t, err)
	got, ok, err := again.Lookup(link)
	require.NoError(t, err)
	assert.True(t, ok, "the saved entry covers the folder on the next run")
	assert.Equal(t, trust.Trusted, got)
}

func TestDecideTrustFailsOnAnUnresolvableHome(t *testing.T) {
	_, err := decideTrust(nil, t.TempDir(), filepath.Join(t.TempDir(), "missing"), map[string]config.MCPServer{"a": {Command: "a"}}, nil, nil, nil, noAsk(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to resolve")
}

func TestResolveServersInvalidDeclarations(t *testing.T) {
	for _, decision := range []string{"trusted", "untrusted", "undecided", "ask"} {
		t.Run(decision, func(t *testing.T) {
			e := newTrustEnv(t)
			trustFile := filepath.Join(e.kopiHome, "trust.json")
			e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), `{"mcpServers":{"shared":{"command":42},"bad":{"args":false},"healthy":{"command":"healthy-mcp"}}}`)
			if decision == "trusted" || decision == "untrusted" {
				b, err := json.Marshal(map[string]any{"version": 1, "folders": map[string]string{canonical(t, e.workspace): decision}})
				require.NoError(t, err)
				e.writeFile(t, trustFile, string(b))
			}
			var ask AskTrust
			if decision == "ask" {
				ask = func(req TrustRequest) (TrustChoice, error) {
					assert.Equal(t, []string{"bad: invalid, will not start", "healthy: healthy-mcp", "shared: invalid, will not start"}, req.Servers)
					return TrustCancelled, nil
				}
			}

			set, err := ResolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, ask)

			require.NoError(t, err)
			require.Len(t, set.Errs, 2)
			assert.ErrorContains(t, set.Errs[0], "MCP server bad: failed to decode")
			assert.ErrorContains(t, set.Errs[1], "MCP server shared: failed to decode")
			if decision == "trusted" {
				assert.Equal(t, map[string]config.MCPServer{"solo": {Command: "solo-mcp"}, "healthy": {Command: "healthy-mcp"}}, set.Servers)
				assert.Len(t, set.Invalid, 2)
				assert.True(t, set.Project["shared"])
			} else {
				assert.Equal(t, globalServers, set.Servers)
				assert.Len(t, set.Untrusted, 3)
				assert.Empty(t, set.Invalid)
				assert.Equal(t, decision == "undecided", set.Undecided)
			}
		})
	}
}

func TestResolveServersMalformedProjectFileIsANotice(t *testing.T) {
	e := newTrustEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), `{"mcpServers":`)

	set, err := ResolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, noAsk(t))

	require.NoError(t, err)
	assert.Equal(t, globalServers, set.Servers)
	require.Len(t, set.Errs, 1)
	assert.ErrorContains(t, set.Errs[0], "skipped this folder's MCP servers: failed to parse MCP config "+filepath.Join(e.workspace, ".mcp.json"))
	assert.NoFileExists(t, trustFile)
}

func TestResolveServersAsksForInstructionFiles(t *testing.T) {
	e := newTrustEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	files := []string{filepath.Join(e.workspace, "AGENTS.md")}
	var asked []TrustRequest
	ask := func(req TrustRequest) (TrustChoice, error) {
		asked = append(asked, req)
		return TrustFolder, nil
	}

	set, err := ResolveServers(globalServers, trustFile, e.workspace, e.home, files, nil, ask)

	require.NoError(t, err)
	assert.Equal(t, []TrustRequest{{Folder: canonical(t, e.workspace), Parent: canonical(t, e.root), Instructions: files}}, asked)
	assert.True(t, set.Trusted)
	assert.Equal(t, globalServers, set.Servers)
	assert.Equal(t, map[string]string{canonical(t, e.workspace): "trusted"}, trustFolders(t, trustFile))
}

func TestResolveServersInstructionFilesUndecidedWithoutAsk(t *testing.T) {
	e := newTrustEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")

	set, err := ResolveServers(globalServers, trustFile, e.workspace, e.home, []string{filepath.Join(e.workspace, "AGENTS.md")}, nil, nil)

	require.NoError(t, err)
	assert.True(t, set.Undecided)
	assert.False(t, set.Trusted)
	assert.Empty(t, set.Untrusted)
	assert.NoFileExists(t, trustFile)
}

func TestResolveServersMalformedProjectFileStillAsksForInstructionFiles(t *testing.T) {
	e := newTrustEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), `{"mcpServers":`)
	files := []string{filepath.Join(e.workspace, "AGENTS.md")}
	var asked TrustRequest
	ask := func(req TrustRequest) (TrustChoice, error) {
		asked = req
		return TrustFolder, nil
	}

	set, err := ResolveServers(globalServers, trustFile, e.workspace, e.home, files, nil, ask)

	require.NoError(t, err)
	assert.Equal(t, files, asked.Instructions)
	assert.Empty(t, asked.Servers)
	assert.True(t, set.Trusted)
	assert.Equal(t, globalServers, set.Servers)
	assert.Empty(t, set.Project)
	require.Len(t, set.Errs, 1)
	assert.ErrorContains(t, set.Errs[0], "skipped this folder's MCP servers: failed to parse MCP config")
}

func TestResolveServersWithoutServersOrInstructionsLeavesTheTrustFileAlone(t *testing.T) {
	e := newTrustEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	e.writeFile(t, trustFile, `{"version":1,"folders":{"/x":"maybe"}}`)

	set, err := ResolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, noAsk(t))

	require.NoError(t, err, "an invalid trust file is not opened")
	assert.False(t, set.Trusted)
	assert.False(t, set.Undecided)
}

func TestResolveServersInvalidTrustFileIsFatal(t *testing.T) {
	e := newTrustEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), projectMCP)
	e.writeFile(t, trustFile, `{"version":1,"folders":{"/x":"maybe"}}`)

	_, err := ResolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, noAsk(t))

	require.Error(t, err)
	assert.Contains(t, err.Error(), trustFile)
}

func TestResolveServersAsksForSkillFolders(t *testing.T) {
	e := newTrustEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	folders := []string{filepath.Join(e.workspace, ".agents", "skills")}
	var asked []TrustRequest
	ask := func(req TrustRequest) (TrustChoice, error) {
		asked = append(asked, req)
		return TrustFolder, nil
	}

	set, err := ResolveServers(globalServers, trustFile, e.workspace, e.home, nil, folders, ask)

	require.NoError(t, err)
	assert.Equal(t, []TrustRequest{{Folder: canonical(t, e.workspace), Parent: canonical(t, e.root), Skills: folders}}, asked)
	assert.True(t, set.Trusted)
	assert.Equal(t, map[string]string{canonical(t, e.workspace): "trusted"}, trustFolders(t, trustFile))
}

func TestResolveServersSkillFoldersUndecidedWithoutAsk(t *testing.T) {
	e := newTrustEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")

	set, err := ResolveServers(globalServers, trustFile, e.workspace, e.home, nil, []string{filepath.Join(e.workspace, ".agents", "skills")}, nil)

	require.NoError(t, err)
	assert.True(t, set.Undecided)
	assert.False(t, set.Trusted)
	assert.NoFileExists(t, trustFile)
}
