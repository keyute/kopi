package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

func globalLayer(t *testing.T, content string) Layer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	l, err := GlobalFile(path)
	require.NoError(t, err)
	return l
}

// withDefaults returns c with the default UI blocks filled in.
func withDefaults(c Config) Config {
	c.Mode = "plan"
	c.Keys = DefaultKeys()
	c.StatusLine = DefaultStatusLine()
	c.Permissions = Permissions{Allow: []string{}, Ask: []string{}, Deny: []string{}}
	c.Sandbox = Sandbox{AllowWrite: []string{}, DenyRead: []string{}}
	c.MCPServers = map[string]MCPServer{}
	return c
}

func TestResolveDefaultsOnly(t *testing.T) {
	r, err := Resolve(Defaults())
	require.NoError(t, err)
	assert.Equal(t, withDefaults(Config{Version: 1}), r.Config)
	assert.Equal(t, "default", r.Origins["version"])
	assert.Equal(t, "default", r.Origins["keys.submit"])
	assert.Equal(t, "default", r.Origins["statusline.left"])
	assert.Equal(t, "default", r.Origins["mode"])
	assert.NotContains(t, r.Origins, "model")
}

func TestResolveModeFromGlobalFile(t *testing.T) {
	global := globalLayer(t, `{"mode": "execute"}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, "execute", r.Config.Mode)
	assert.Equal(t, global.Name, r.Origins["mode"])
}

func TestResolveRejectsInvalidModeNamingLayer(t *testing.T) {
	for _, mode := range []string{"auto", "", "Plan"} {
		t.Run(mode, func(t *testing.T) {
			global := globalLayer(t, fmt.Sprintf(`{"mode": %q}`, mode))
			_, err := Resolve(Defaults(), global)
			require.ErrorIs(t, err, ErrInvalidValue)
			assert.Contains(t, err.Error(), global.Name)
			assert.Contains(t, err.Error(), "want plan or execute")
		})
	}
}

func TestResolveLaterLayersOverrideEarlier(t *testing.T) {
	global := globalLayer(t, `{"version": 1, "model": "openai-codex/gpt-x"}`)
	r, err := Resolve(Defaults(), global, Map("flags", map[string]any{"model": "anthropic/claude"}))
	require.NoError(t, err)
	assert.Equal(t, withDefaults(Config{Version: 1, Model: "anthropic/claude"}), r.Config)
	assert.Equal(t, global.Name, r.Origins["version"])
	assert.Equal(t, "flags", r.Origins["model"])
}

func TestResolveLayerWithoutKeyKeepsEarlierOrigin(t *testing.T) {
	global := globalLayer(t, `{"model": "openai-codex/gpt-x"}`)
	r, err := Resolve(Defaults(), global, Map("flags", nil))
	require.NoError(t, err)
	assert.Equal(t, withDefaults(Config{Version: 1, Model: "openai-codex/gpt-x"}), r.Config)
	assert.Equal(t, "default", r.Origins["version"])
	assert.Equal(t, global.Name, r.Origins["model"])
}

func TestResolveMissingGlobalFileYieldsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "config.json")
	global, err := GlobalFile(path)
	require.NoError(t, err)
	assert.Contains(t, global.Name, path)

	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, withDefaults(Config{Version: 1}), r.Config)
	assert.Equal(t, "default", r.Origins["version"])
	assert.NotContains(t, r.Origins, "model")
}

func TestResolveRejectsUnknownKeyNamingLayer(t *testing.T) {
	global := globalLayer(t, `{"version": 1, "modle": "a/b"}`)
	_, err := Resolve(Defaults(), global)
	require.ErrorIs(t, err, ErrUnknownKey)
	assert.Contains(t, err.Error(), global.Name)
	assert.Contains(t, err.Error(), "modle")
}

func TestResolveRejectsWrongTypeNamingLayer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"string version", `{"version": "1"}`},
		{"numeric model", `{"model": 5}`},
		{"fractional version", `{"version": 1.9}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global := globalLayer(t, tc.content)
			_, err := Resolve(Defaults(), global)
			require.ErrorIs(t, err, ErrInvalidValue)
			assert.NotErrorIs(t, err, ErrUnknownKey)
			assert.Contains(t, err.Error(), global.Name)
		})
	}
}

func TestResolveRejectsMalformedJSONNamingLayer(t *testing.T) {
	global := globalLayer(t, `{"version": 1,`)
	_, err := Resolve(Defaults(), global)
	require.ErrorIs(t, err, ErrInvalidValue)
	assert.Contains(t, err.Error(), global.Name)
}

func TestResolveRejectsUnsupportedVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int
	}{
		{"newer", 2},
		{"explicit zero", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(Defaults(), Map("flags", map[string]any{"version": tc.version}))
			require.ErrorIs(t, err, ErrUnsupportedVersion)
			assert.Contains(t, err.Error(), "flags")
		})
	}
}

func TestResolveRejectsInvalidModelNamingLayer(t *testing.T) {
	_, err := Resolve(Defaults(), Map("flags", map[string]any{"model": "noslash"}))
	require.ErrorIs(t, err, ErrInvalidModel)
	assert.Contains(t, err.Error(), "flags")
}

func TestResolveOverridesOneActionKeepingOthers(t *testing.T) {
	global := globalLayer(t, `{"keys": {"submit": ["ctrl+s"]}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	want := DefaultKeys()
	want["submit"] = []string{"ctrl+s"}
	assert.Equal(t, want, r.Config.Keys)
	assert.Equal(t, global.Name, r.Origins["keys.submit"])
	assert.Equal(t, "default", r.Origins["keys.newline"])
}

func TestResolveOverridesOneStatusLineSide(t *testing.T) {
	global := globalLayer(t, `{"statusline": {"left": ["branch"]}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, StatusLine{Left: []string{"branch"}, Right: DefaultStatusLine().Right}, r.Config.StatusLine)
	assert.Equal(t, global.Name, r.Origins["statusline.left"])
	assert.Equal(t, "default", r.Origins["statusline.right"])
}

func TestResolveAcceptsNoticeOnEitherSide(t *testing.T) {
	global := globalLayer(t, `{"statusline": {"left": ["notice", "model"], "right": ["mode", "notice"]}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, StatusLine{Left: []string{"notice", "model"}, Right: []string{"mode", "notice"}}, r.Config.StatusLine)
}

func TestResolveRejectsUnknownActionNamingLayer(t *testing.T) {
	global := globalLayer(t, `{"keys": {"sumbit": ["enter"]}}`)
	_, err := Resolve(Defaults(), global)
	require.ErrorIs(t, err, ErrUnknownKey)
	assert.Contains(t, err.Error(), global.Name)
	assert.Contains(t, err.Error(), "sumbit")
}

func TestResolveRejectsUnknownPermissionsKey(t *testing.T) {
	global := globalLayer(t, `{"permissions": {"defaultMode": "plan"}}`)
	_, err := Resolve(Defaults(), global)
	require.ErrorIs(t, err, ErrUnknownKey)
	assert.Contains(t, err.Error(), global.Name)
	assert.Contains(t, err.Error(), "defaultMode")
}

func TestResolveRejectsUnknownStatusLineKey(t *testing.T) {
	global := globalLayer(t, `{"statusline": {"middle": []}}`)
	_, err := Resolve(Defaults(), global)
	require.ErrorIs(t, err, ErrUnknownKey)
	assert.Contains(t, err.Error(), global.Name)
	assert.Contains(t, err.Error(), "middle")
}

func TestResolveRejectsInvalidUIValueNamingLayer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"empty key list", `{"keys": {"submit": []}}`},
		{"empty key", `{"keys": {"submit": [""]}}`},
		{"blank key", `{"keys": {"submit": ["enter", " "]}}`},
		{"non-string key", `{"keys": {"submit": [5]}}`},
		{"key not a list", `{"keys": {"submit": "enter"}}`},
		{"null key list", `{"keys": {"submit": null}}`},
		{"keys not an object", `{"keys": "x"}`},
		{"null keys", `{"keys": null}`},
		{"side not a list", `{"statusline": {"left": "model"}}`},
		{"statusline not an object", `{"statusline": ["model"]}`},
		{"unknown segment", `{"statusline": {"right": ["mode", "weather"]}}`},
		{"rule list not a list", `{"permissions": {"allow": "Read"}}`},
		{"null rule list", `{"permissions": {"deny": null}}`},
		{"non-string rule", `{"permissions": {"ask": [5]}}`},
		{"permissions not an object", `{"permissions": ["Read"]}`},
		{"null permissions", `{"permissions": null}`},
		{"null sandbox enabled", `{"sandbox": {"enabled": null}}`},
		{"string sandbox enabled", `{"sandbox": {"enabled": "true"}}`},
		{"string sandbox network", `{"sandbox": {"network": "true"}}`},
		{"null sandbox list", `{"sandbox": {"allowWrite": null}}`},
		{"null sandbox", `{"sandbox": null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global := globalLayer(t, tc.content)
			_, err := Resolve(Defaults(), global)
			require.ErrorIs(t, err, ErrInvalidValue)
			assert.NotErrorIs(t, err, ErrUnknownKey)
			assert.Contains(t, err.Error(), global.Name)
		})
	}
}

func TestResolveAcceptsPermissionRules(t *testing.T) {
	global := globalLayer(t, `{"permissions": {"allow": ["Read", "Edit(docs/**)"], "ask": ["Write(!docs/x)"], "deny": ["Read(.env)", "*(~/.ssh)"]}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, Permissions{
		Allow: []string{"Read", "Edit(docs/**)"},
		Ask:   []string{"Write(!docs/x)"},
		Deny:  []string{"Read(.env)", "*(~/.ssh)"},
	}, r.Config.Permissions)
	assert.Equal(t, global.Name, r.Origins["permissions.deny"])
}

func TestResolveRejectsInvalidRuleNamingLayerAndIndex(t *testing.T) {
	global := globalLayer(t, `{"permissions": {"deny": ["Read(.env)", "Edit", "Read(x"]}}`)
	_, err := Resolve(Defaults(), global)
	require.ErrorIs(t, err, ErrInvalidValue)
	require.ErrorIs(t, err, policy.ErrInvalidRule)
	assert.Contains(t, err.Error(), global.Name)
	assert.Contains(t, err.Error(), "permissions.deny[2]")
}

func TestPermissionListsReplaceAcrossLayers(t *testing.T) {
	global := globalLayer(t, `{"permissions": {"allow": ["Read", "Edit"], "deny": ["Read(.env)"]}}`)
	r, err := Resolve(Defaults(), global, Map("flags", map[string]any{"permissions": map[string]any{"allow": []any{"Write"}}}))
	require.NoError(t, err)
	assert.Equal(t, Permissions{Allow: []string{"Write"}, Ask: []string{}, Deny: []string{"Read(.env)"}}, r.Config.Permissions)
	assert.Equal(t, "flags", r.Origins["permissions.allow"])
	assert.Equal(t, global.Name, r.Origins["permissions.deny"])
	assert.Equal(t, "default", r.Origins["permissions.ask"])
}

func TestResolveSandboxEnabled(t *testing.T) {
	r, err := Resolve(Defaults())
	require.NoError(t, err)
	assert.Nil(t, r.Config.Sandbox.Enabled)
	assert.NotContains(t, r.Origins, "sandbox.enabled")

	for _, enabled := range []bool{true, false} {
		global := globalLayer(t, fmt.Sprintf(`{"sandbox": {"enabled": %t}}`, enabled))
		r, err := Resolve(Defaults(), global)
		require.NoError(t, err)
		require.NotNil(t, r.Config.Sandbox.Enabled)
		assert.Equal(t, enabled, *r.Config.Sandbox.Enabled)
		assert.Equal(t, global.Name, r.Origins["sandbox.enabled"])
	}
}

func TestResolveRejectsEmptySandboxPathNamingLayerAndIndex(t *testing.T) {
	for _, key := range []string{"allowWrite", "denyRead"} {
		t.Run(key, func(t *testing.T) {
			global := globalLayer(t, fmt.Sprintf(`{"sandbox": {%q: ["~/.cache", ""]}}`, key))
			_, err := Resolve(Defaults(), global)
			require.ErrorIs(t, err, ErrInvalidValue)
			assert.Contains(t, err.Error(), global.Name)
			assert.Contains(t, err.Error(), "sandbox."+key+"[1]")
		})
	}
}

func TestSandboxListsReplaceAcrossLayers(t *testing.T) {
	global := globalLayer(t, `{"sandbox": {"allowWrite": ["~/.cache", "/tmp/x"], "denyRead": ["~/.aws"], "network": true}}`)
	r, err := Resolve(Defaults(), global, Map("flags", map[string]any{"sandbox": map[string]any{"allowWrite": []any{"/var/y"}}}))
	require.NoError(t, err)
	assert.Equal(t, Sandbox{AllowWrite: []string{"/var/y"}, DenyRead: []string{"~/.aws"}, Network: true}, r.Config.Sandbox)
	assert.Equal(t, "flags", r.Origins["sandbox.allowWrite"])
	assert.Equal(t, global.Name, r.Origins["sandbox.denyRead"])
	assert.Equal(t, global.Name, r.Origins["sandbox.network"])
}

func TestResolveMCPServers(t *testing.T) {
	global := globalLayer(t, `{"mcpServers": {
		"local": {"command": "npx", "args": ["-y", "pkg"], "env": {"TOKEN": "${TOKEN}"}},
		"typed": {"type": "stdio", "command": "srv"},
		"remote": {"type": "http", "url": "https://example.test/mcp", "headers": {"Authorization": "Bearer x"}},
		"untyped": {"url": "https://example.test/other"}
	}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, map[string]MCPServer{
		"local":   {Command: "npx", Args: []string{"-y", "pkg"}, Env: map[string]string{"TOKEN": "${TOKEN}"}},
		"typed":   {Type: "stdio", Command: "srv"},
		"remote":  {Type: "http", URL: "https://example.test/mcp", Headers: map[string]string{"Authorization": "Bearer x"}},
		"untyped": {URL: "https://example.test/other"},
	}, r.Config.MCPServers)
}

func TestResolveRejectsInvalidMCPServerNamingLayerAndServer(t *testing.T) {
	for _, tc := range []struct {
		name, entry, want string
	}{
		{"neither", `{"args": ["x"]}`, "want command or url"},
		{"both", `{"command": "srv", "url": "https://example.test"}`, "not both"},
		{"stdio with url", `{"type": "stdio", "url": "https://example.test"}`, "stdio needs command"},
		{"http with command", `{"type": "http", "command": "srv"}`, "http needs url"},
		{"unsupported type", `{"type": "sse", "url": "https://example.test"}`, `unsupported transport type "sse"`},
		{"args with url", `{"url": "https://example.test", "args": ["x"]}`, "args and env need command"},
		{"env with url", `{"url": "https://example.test", "env": {"A": "b"}}`, "args and env need command"},
		{"headers with command", `{"command": "srv", "headers": {"A": "b"}}`, "headers need url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global := globalLayer(t, `{"mcpServers": {"srv": `+tc.entry+`}}`)
			_, err := Resolve(Defaults(), global)
			require.ErrorIs(t, err, ErrInvalidValue)
			assert.Contains(t, err.Error(), global.Name)
			assert.Contains(t, err.Error(), "mcpServers.srv")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestResolveRejectsEmptyMCPServerName(t *testing.T) {
	global := globalLayer(t, `{"mcpServers": {"": {"command": "srv"}}}`)
	_, err := Resolve(Defaults(), global)
	require.ErrorIs(t, err, ErrInvalidValue)
	assert.Contains(t, err.Error(), global.Name)
	assert.Contains(t, err.Error(), "empty server name")
}

func TestResolveRejectsUnknownMCPServerKey(t *testing.T) {
	global := globalLayer(t, `{"mcpServers": {"srv": {"command": "srv", "timeout": 5}}}`)
	_, err := Resolve(Defaults(), global)
	require.ErrorIs(t, err, ErrUnknownKey)
	assert.Contains(t, err.Error(), global.Name)
	assert.Contains(t, err.Error(), "timeout")
}

func TestResolveMisShapedMCPSecretsAreNotEchoed(t *testing.T) {
	for _, tc := range []struct {
		name, content, path string
	}{
		{"headers not an object", `{"mcpServers": {"gh": {"type": "http", "url": "https://x", "headers": "Authorization: Bearer ghp_SECRET"}}}`, "mcpServers[gh].headers"},
		{"env value a number", `{"mcpServers": {"gh": {"command": "srv", "env": {"TOKEN": 424242}}}}`, "mcpServers[gh].env[TOKEN]"},
		{"env value a list", `{"mcpServers": {"gh": {"command": "srv", "env": {"TOKEN": ["ghp_SECRET"]}}}}`, "mcpServers[gh].env[TOKEN]"},
		{"header value an object", `{"mcpServers": {"gh": {"url": "https://x", "headers": {"Authorization": {"v": "ghp_SECRET"}}}}}`, "mcpServers[gh].headers[Authorization]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global := globalLayer(t, tc.content)
			_, err := Resolve(Defaults(), global)
			require.ErrorIs(t, err, ErrInvalidValue)
			assert.Contains(t, err.Error(), tc.path)
			assert.NotContains(t, err.Error(), "ghp_SECRET")
			assert.NotContains(t, err.Error(), "424242")
		})
	}
}

func TestResolveKeepsDotsInMCPNames(t *testing.T) {
	global := globalLayer(t, `{"mcpServers": {
		"a.b": {"command": "srv", "env": {"X.Y": "1"}},
		"c": {"url": "https://example.test", "headers": {"H.I": "2"}}
	}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, map[string]MCPServer{
		"a.b": {Command: "srv", Env: map[string]string{"X.Y": "1"}},
		"c":   {URL: "https://example.test", Headers: map[string]string{"H.I": "2"}},
	}, r.Config.MCPServers)
	assert.Contains(t, r.Entries(), Entry{Key: "mcpServers.a.b", Value: "stdio: srv", Origin: global.Name})
}

func TestEntriesShowMCPServersWithoutSecrets(t *testing.T) {
	global := globalLayer(t, `{"mcpServers": {
		"local": {"command": "npx", "args": ["-y", "pkg"], "env": {"TOKEN": "secret-env"}},
		"remote": {"type": "http", "url": "https://example.test/mcp", "headers": {"Authorization": "secret-header"}}
	}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	entries := r.Entries()
	assert.IsIncreasing(t, entryKeys(entries))
	assert.Contains(t, entries, Entry{Key: "mcpServers.local", Value: "stdio: npx -y pkg", Origin: global.Name})
	assert.Contains(t, entries, Entry{Key: "mcpServers.remote", Value: "http: https://example.test/mcp", Origin: global.Name})
	for _, e := range entries {
		assert.NotContains(t, e.Value, "secret")
		assert.NotContains(t, e.Key, "TOKEN")
		assert.NotContains(t, e.Key, "Authorization")
	}
}

func TestEntriesSortedWithUnsetKeysAndOrigins(t *testing.T) {
	r, err := Resolve(Defaults())
	require.NoError(t, err)
	entries := r.Entries()
	assert.Len(t, entries, 12+len(KeyActions))
	assert.IsIncreasing(t, entryKeys(entries))
	assert.Contains(t, entries, Entry{Key: "model", Value: "", Origin: "unset"})
	assert.Contains(t, entries, Entry{Key: "version", Value: "1", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "mode", Value: "plan", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "keys.submit", Value: `["enter"]`, Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "keys.newline", Value: `["shift+enter","alt+enter","ctrl+j"]`, Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "statusline.left", Value: `["model","context","branch"]`, Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "statusline.right", Value: `["notice","mode"]`, Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "permissions.allow", Value: "[]", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "permissions.ask", Value: "[]", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "permissions.deny", Value: "[]", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "sandbox.enabled", Value: "", Origin: "unset"})
	assert.Contains(t, entries, Entry{Key: "sandbox.allowWrite", Value: "[]", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "sandbox.denyRead", Value: "[]", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "sandbox.network", Value: "false", Origin: "default"})

	r, err = Resolve(Defaults(), Map("flags", map[string]any{
		"model":       "a/b",
		"mode":        "execute",
		"statusline":  map[string]any{"right": []any{}},
		"permissions": map[string]any{"deny": []any{"Read(.env)", "*(~/.ssh)"}},
		"sandbox":     map[string]any{"enabled": false, "denyRead": []any{"~/.aws"}, "network": true},
	}))
	require.NoError(t, err)
	entries = r.Entries()
	assert.Contains(t, entries, Entry{Key: "model", Value: "a/b", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "mode", Value: "execute", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "statusline.right", Value: "[]", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "permissions.deny", Value: `["Read(.env)","*(~/.ssh)"]`, Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "permissions.allow", Value: "[]", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "sandbox.enabled", Value: "false", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "sandbox.denyRead", Value: `["~/.aws"]`, Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "sandbox.network", Value: "true", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "sandbox.allowWrite", Value: "[]", Origin: "default"})
}

func entryKeys(entries []Entry) []string {
	keys := make([]string, len(entries))
	for i, e := range entries {
		keys[i] = e.Key
	}
	return keys
}

func TestParseModelRef(t *testing.T) {
	for _, tc := range []struct {
		in, provider, id string
		wantErr          bool
	}{
		{in: "openai-codex/gpt-x", provider: "openai-codex", id: "gpt-x"},
		{in: "a/b/c", provider: "a", id: "b/c"},
		{in: "noslash", wantErr: true},
		{in: "/id", wantErr: true},
		{in: "provider/", wantErr: true},
		{in: "", wantErr: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			provider, id, err := ParseModelRef(tc.in)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalidModel)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.provider, provider)
			assert.Equal(t, tc.id, id)
		})
	}
}

func TestResolveMCPServerNameContract(t *testing.T) {
	for _, name := range []string{"github__other", "github_", "github.", "github..other", "github._other"} {
		global := globalLayer(t, fmt.Sprintf(`{"mcpServers": {%q: {"command": "fake"}}}`, name))
		_, err := Resolve(Defaults(), global)
		require.ErrorIs(t, err, ErrInvalidValue, name)
		assert.Contains(t, err.Error(), global.Name)
	}
	for _, name := range []string{"github", "github-other", "my.server", "my_server", "_github", "café-docs"} {
		global := globalLayer(t, fmt.Sprintf(`{"mcpServers": {%q: {"command": "fake"}}}`, name))
		_, err := Resolve(Defaults(), global)
		require.NoError(t, err, name)
	}
}
