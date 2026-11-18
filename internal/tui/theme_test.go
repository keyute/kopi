package tui

import (
	"image/color"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/config"
)

func TestBuiltinThemesSetEveryRole(t *testing.T) {
	for name, build := range builtinThemes {
		t.Run(name, func(t *testing.T) {
			th := build()
			for role, slot := range th.roles() {
				require.NotNil(t, *slot, role)
				if _, none := (*slot).(lipgloss.NoColor); none {
					assert.True(t, role == "text" && strings.HasPrefix(name, "ansi-"), "%s is NoColor", role)
				}
			}
			assert.Contains(t, styles.Registry, th.codeTheme)
			assert.NotEmpty(t, th.codeFormatter)
		})
	}
}

func TestThemeRolesMatchConfig(t *testing.T) {
	var th theme
	assert.ElementsMatch(t, config.ThemeRoles, slices.Collect(maps.Keys(th.roles())))
}

func writeTheme(t *testing.T, dir, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o600))
}

func TestLoadThemes(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "mine", `{"base": "dracula", "colors": {"accent": "#abc", "dim": "244"}, "codeTheme": "monokai"}`)

	got, err := LoadThemes(dir, "mine", "gruvbox-light")
	require.NoError(t, err)

	want := builtinThemes["dracula"]()
	want.accent, want.dim, want.codeTheme = lipgloss.Color("#abc"), lipgloss.Color("244"), "monokai"
	assert.Equal(t, want, got.dark)
	assert.Equal(t, builtinThemes["gruvbox-light"](), got.light)
}

func TestLoadThemesErrors(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"role":    `{"base": "dracula", "colors": {"nope": "#fff"}}`,
		"colour":  `{"base": "dracula", "colors": {"accent": "red"}}`,
		"base":    `{"base": "nope"}`,
		"nobase":  `{}`,
		"style":   `{"base": "dracula", "codeTheme": "nope"}`,
		"unknown": `{"base": "dracula", "extra": 1}`,
	} {
		writeTheme(t, dir, name, body)
		_, err := LoadThemes(dir, name, "dracula")
		assert.ErrorContains(t, err, `failed to load theme "`+name+`"`, name)
	}

	_, err := LoadThemes(dir, "dracula", "missing")
	assert.ErrorContains(t, err, `failed to load theme "missing"`)
}

func TestLightBackgroundUsesTheLightTheme(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "mine", `{"base": "catppuccin-latte", "colors": {"userMessageBg": "#123456"}}`)
	themes, err := LoadThemes(dir, "dracula", "mine")
	require.NoError(t, err)
	opts := testOptions()
	opts.Themes = themes
	m, _ := newTestModelWith(t, 30, 20, opts)
	assert.Equal(t, themes.dark, m.th)

	m.Update(tea.BackgroundColorMsg{Color: color.White})

	assert.Equal(t, themes.light, m.th)
	assert.Equal(t, themes.light.userMessageBg, m.composer.ta.Styles().Focused.Text.GetBackground())
}
