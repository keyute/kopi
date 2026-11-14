package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	c.Keys = DefaultKeys()
	c.StatusLine = DefaultStatusLine()
	return c
}

func TestResolveDefaultsOnly(t *testing.T) {
	r, err := Resolve(Defaults())
	require.NoError(t, err)
	assert.Equal(t, withDefaults(Config{Version: 1}), r.Config)
	assert.Equal(t, "default", r.Origins["version"])
	assert.Equal(t, "default", r.Origins["keys.submit"])
	assert.Equal(t, "default", r.Origins["statusline.left"])
	assert.NotContains(t, r.Origins, "model")
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

func TestEntriesSortedWithUnsetKeysAndOrigins(t *testing.T) {
	r, err := Resolve(Defaults())
	require.NoError(t, err)
	entries := r.Entries()
	assert.Len(t, entries, 4+len(KeyActions))
	assert.IsIncreasing(t, entryKeys(entries))
	assert.Contains(t, entries, Entry{Key: "model", Value: "", Origin: "unset"})
	assert.Contains(t, entries, Entry{Key: "version", Value: "1", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "keys.submit", Value: `["enter"]`, Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "keys.newline", Value: `["shift+enter","alt+enter","ctrl+j"]`, Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "statusline.left", Value: `["model","context","branch"]`, Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "statusline.right", Value: `["notice","mode"]`, Origin: "default"})

	r, err = Resolve(Defaults(), Map("flags", map[string]any{"model": "a/b", "statusline": map[string]any{"right": []any{}}}))
	require.NoError(t, err)
	entries = r.Entries()
	assert.Contains(t, entries, Entry{Key: "model", Value: "a/b", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "statusline.right", Value: "[]", Origin: "flags"})
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
