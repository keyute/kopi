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
	c.InstructionFallbacks = []string{"CLAUDE.md"}
	c.Keys = DefaultKeys()
	c.StatusLine = DefaultStatusLine()
	c.StatusLine.Segments = map[string]StatusSegment{}
	c.AgentLine = DefaultAgentLine()
	c.Theme = DefaultTheme()
	c.Permissions = Permissions{Allow: []string{}, Ask: []string{}, Deny: []string{}}
	c.Sandbox = Sandbox{AllowWrite: []string{}, DenyRead: []string{}, AllowUnsandboxed: true}
	c.Skills = Skills{Prefix: "skill"}
	c.Classifier = Classifier{Instructions: []string{}}
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
	assert.Equal(t, DefaultAgentLine(), r.Config.AgentLine)
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

func TestResolveInstructionFallbacks(t *testing.T) {
	r, err := Resolve(Defaults())
	require.NoError(t, err)
	assert.Equal(t, []string{"CLAUDE.md"}, r.Config.InstructionFallbacks)

	global := globalLayer(t, `{"instructionFallbacks": ["GEMINI.md", ".cursorrules"]}`)
	r, err = Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, []string{"GEMINI.md", ".cursorrules"}, r.Config.InstructionFallbacks)

	global = globalLayer(t, `{"instructionFallbacks": []}`)
	r, err = Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Empty(t, r.Config.InstructionFallbacks)
	assert.Equal(t, global.Name, r.Origins["instructionFallbacks"])
}

func TestResolveRejectsInstructionFallbackNotAFileName(t *testing.T) {
	for _, name := range []string{"", ".", "..", "docs/AGENTS.md", "/AGENTS.md", "CLAUDE.md/", `docs\AGENTS.md`} {
		t.Run(name, func(t *testing.T) {
			global := globalLayer(t, fmt.Sprintf(`{"instructionFallbacks": ["CLAUDE.md", %q]}`, name))
			_, err := Resolve(Defaults(), global)
			require.ErrorIs(t, err, ErrInvalidValue)
			assert.Contains(t, err.Error(), global.Name)
			assert.Contains(t, err.Error(), "instructionFallbacks[1]")
		})
	}
}

func TestResolveSkillsPrefix(t *testing.T) {
	r, err := Resolve(Defaults())
	require.NoError(t, err)
	assert.Equal(t, "skill", r.Config.Skills.Prefix)

	for _, prefix := range []string{"", "s", "my-skill2"} {
		global := globalLayer(t, fmt.Sprintf(`{"skills": {"prefix": %q}}`, prefix))
		r, err = Resolve(Defaults(), global)
		require.NoError(t, err)
		assert.Equal(t, prefix, r.Config.Skills.Prefix)
		assert.Equal(t, global.Name, r.Origins["skills.prefix"])
	}
}

func TestResolveRejectsInvalidSkillsPrefix(t *testing.T) {
	for _, prefix := range []string{"Skill", "a b", "a:b", "a_b", "a/b"} {
		t.Run(prefix, func(t *testing.T) {
			global := globalLayer(t, fmt.Sprintf(`{"skills": {"prefix": %q}}`, prefix))
			_, err := Resolve(Defaults(), global)
			require.ErrorIs(t, err, ErrInvalidValue)
			assert.Contains(t, err.Error(), global.Name)
			assert.Contains(t, err.Error(), "skills.prefix")
		})
	}
}

func TestResolveRejectsUnknownSkillsKey(t *testing.T) {
	_, err := Resolve(Defaults(), globalLayer(t, `{"skills": {"x": 1}}`))
	require.ErrorIs(t, err, ErrUnknownKey)
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

func TestResolveRejectsInvalidSmallModelNamingLayer(t *testing.T) {
	_, err := Resolve(Defaults(), Map("flags", map[string]any{"smallModel": "noslash"}))
	require.ErrorIs(t, err, ErrInvalidModel)
	assert.Contains(t, err.Error(), "flags")
}

func TestResolveSmallModel(t *testing.T) {
	global := globalLayer(t, `{"smallModel": "anthropic/claude-haiku"}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, "anthropic/claude-haiku", r.Config.SmallModel)
	assert.Equal(t, global.Name, r.Origins["smallModel"])
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
	global := globalLayer(t, `{"statusline": {"left": ["$branch"]}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	want := DefaultStatusLine()
	want.Left = []string{"$branch"}
	want.Segments = map[string]StatusSegment{}
	assert.Equal(t, want, r.Config.StatusLine)
	assert.Equal(t, global.Name, r.Origins["statusline.left"])
	assert.Equal(t, "default", r.Origins["statusline.right"])
}

func TestResolveOverridesOneAgentLineSide(t *testing.T) {
	global := globalLayer(t, `{"agentline": {"left": ["$state"]}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	want := DefaultAgentLine()
	want.Left = []string{"$state"}
	assert.Equal(t, want, r.Config.AgentLine)
	assert.Equal(t, global.Name, r.Origins["agentline.left"])
	assert.Equal(t, "default", r.Origins["agentline.right"])
}

func TestResolveRejectsUnknownAgentSegmentNamingLayer(t *testing.T) {
	for _, side := range []string{"left", "right"} {
		global := globalLayer(t, `{"agentline": {"`+side+`": ["$agent", "$weather"]}}`)
		_, err := Resolve(Defaults(), global)
		require.ErrorIs(t, err, ErrInvalidValue)
		assert.Contains(t, err.Error(), global.Name)
		assert.Contains(t, err.Error(), `agentline.`+side+`: unknown segment "weather"`)
	}
}

func TestResolveAcceptsTheCountTokensAndChangesSegments(t *testing.T) {
	global := globalLayer(t, `{"statusline": {"left": ["$agents", "$shells", "$tokens", "$changes"]}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, []string{"$agents", "$shells", "$tokens", "$changes"}, r.Config.StatusLine.Left)
}

func TestResolveRejectsTheTasksSegment(t *testing.T) {
	_, err := Resolve(Defaults(), globalLayer(t, `{"statusline": {"right": ["$tasks"]}}`))
	require.ErrorIs(t, err, ErrInvalidValue)
	assert.Contains(t, err.Error(), `statusline.right: unknown segment "tasks"`)
}

func TestResolveRejectsInvalidEntries(t *testing.T) {
	for _, tc := range []struct{ name, entry, want string }{
		{"unknown segment", `$weather`, `unknown segment "weather"`},
		{"unknown segment in a group", `($model( $weather))`, `unknown segment "weather"`},
		{"unclosed group", `($model`, `"($model": unclosed "("`},
		{"unclosed nested group", `(($model)`, `"(($model)": unclosed "("`},
		{"stray close", `$model)`, `"$model)": ")" without "("`},
		{"bare dollar", `$model $`, `"$model $": "$" not followed by a segment name`},
		{"dollar before a space", `$ model`, `"$ model": "$" not followed by a segment name`},
		{"trailing backslash", `$model\\`, `"$model\\": trailing "\"`},
		{"escaped dollar names no segment", `\\$model`, `"\\$model" names no segment`},
		{"bare segment name", `model`, `"model" names no segment; write "$model"`},
		{"literal only", `at home`, `"at home" names no segment`},
		{"empty", ``, `"" names no segment`},
		{"empty group", `()`, `"()" names no segment`},
		{"group without a segment", `$model(at home)`, `"$model(at home)": a group names no segment`},
		{"empty group after a segment", `$model()`, `"$model()": a group names no segment`},
		{"nested group without a segment", `($model(at home))`, `"($model(at home))": a group names no segment`},
		{"control character", `$model\u001b[0m`, `"$model\x1b[0m": control character`},
	} {
		for _, bar := range []string{"statusline", "agentline"} {
			for _, side := range []string{"left", "right"} {
				t.Run(tc.name+" "+bar+" "+side, func(t *testing.T) {
					global := globalLayer(t, `{"`+bar+`": {"`+side+`": ["$model", "`+tc.entry+`"]}}`)
					_, err := Resolve(Defaults(), global)
					require.ErrorIs(t, err, ErrInvalidValue)
					assert.Contains(t, err.Error(), global.Name)
					assert.Contains(t, err.Error(), bar+"."+side+": "+tc.want)
				})
			}
		}
	}
}

func TestResolveRejectsAUserSegmentNameThatCannotBeReferenced(t *testing.T) {
	global := globalLayer(t, `{"statusline": {"segments": {"my-host": {"text": "w"}}}}`)
	_, err := Resolve(Defaults(), global)
	require.ErrorIs(t, err, ErrInvalidValue)
	assert.Contains(t, err.Error(), global.Name)
	assert.Contains(t, err.Error(), "statusline.segments.my-host: name must be letters, digits or _")
}

func TestResolveAcceptsTheEffortContextToolsAndCostSegments(t *testing.T) {
	global := globalLayer(t, `{"statusline": {"right": ["$effort", "$tools", "$cost"]}, "agentline": {"right": ["$effort", "$context", "$tools", "$cost"]}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, []string{"$effort", "$tools", "$cost"}, r.Config.StatusLine.Right)
	assert.Equal(t, []string{"$effort", "$context", "$tools", "$cost"}, r.Config.AgentLine.Right)
}

func TestResolveAcceptsNoticeOnEitherSide(t *testing.T) {
	global := globalLayer(t, `{"statusline": {"left": ["$notice", "$model"], "right": ["$mode", "$notice"]}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, []string{"$notice", "$model"}, r.Config.StatusLine.Left)
	assert.Equal(t, []string{"$mode", "$notice"}, r.Config.StatusLine.Right)
}

func TestEditPlanActionDefaultsToCtrlG(t *testing.T) {
	assert.Contains(t, KeyActions, "edit-plan")
	assert.Equal(t, []string{"ctrl+g"}, DefaultKeys()["edit-plan"])

	r, err := Resolve(Defaults(), globalLayer(t, `{"keys": {"edit-plan": ["ctrl+e"]}}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"ctrl+e"}, r.Config.Keys["edit-plan"])

	_, err = Resolve(Defaults(), globalLayer(t, `{"keys": {"edit-plan": []}}`))
	assert.ErrorIs(t, err, ErrInvalidValue)
}

func TestBackgroundActionAndTasksSegment(t *testing.T) {
	assert.Contains(t, KeyActions, "background")
	assert.Equal(t, []string{"ctrl+b"}, DefaultKeys()["background"])
	assert.Contains(t, KeyActions, "stop-agent")
	assert.Equal(t, []string{"x"}, DefaultKeys()["stop-agent"])
	assert.Equal(t, []string{"$notice", "$sandbox", "$shells", "$mode"}, DefaultStatusLine().Right)

	global := globalLayer(t, `{"keys": {"background": ["ctrl+g"]}, "statusline": {"left": ["$agents", "$shells"]}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, []string{"ctrl+g"}, r.Config.Keys["background"])
	assert.Equal(t, []string{"$agents", "$shells"}, r.Config.StatusLine.Left)

	_, err = Resolve(Defaults(), globalLayer(t, `{"keys": {"background": []}}`))
	require.ErrorIs(t, err, ErrInvalidValue)
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
		{"unknown segment", `{"statusline": {"right": ["$mode", "$weather"]}}`},
		{"rule list not a list", `{"permissions": {"allow": "Read"}}`},
		{"null rule list", `{"permissions": {"deny": null}}`},
		{"non-string rule", `{"permissions": {"ask": [5]}}`},
		{"permissions not an object", `{"permissions": ["Read"]}`},
		{"null permissions", `{"permissions": null}`},
		{"null sandbox enabled", `{"sandbox": {"enabled": null}}`},
		{"string sandbox enabled", `{"sandbox": {"enabled": "true"}}`},
		{"string sandbox network", `{"sandbox": {"network": "true"}}`},
		{"string sandbox allowUnsandboxed", `{"sandbox": {"allowUnsandboxed": "false"}}`},
		{"null sandbox list", `{"sandbox": {"allowWrite": null}}`},
		{"null sandbox", `{"sandbox": null}`},
		{"instruction fallbacks not a list", `{"instructionFallbacks": "CLAUDE.md"}`},
		{"null instruction fallbacks", `{"instructionFallbacks": null}`},
		{"non-string skills prefix", `{"skills": {"prefix": 5}}`},
		{"skills not an object", `{"skills": "x"}`},
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

func TestResolveThemeDefaultsAndNames(t *testing.T) {
	r, err := Resolve(Defaults(), globalLayer(t, `{"theme": {"dark": "my-dark"}}`))
	require.NoError(t, err)
	assert.Equal(t, Theme{Dark: "my-dark", Light: "catppuccin-latte"}, r.Config.Theme)

	for _, name := range []string{"", "a/b", `a\b`, ".hidden", ".."} {
		global := globalLayer(t, fmt.Sprintf(`{"theme": {"light": %q}}`, name))
		_, err := Resolve(Defaults(), global)
		require.ErrorIs(t, err, ErrInvalidValue, name)
		assert.Contains(t, err.Error(), global.Name)
		assert.Contains(t, err.Error(), "theme.light")
	}
}

func TestResolveSeparatorHonoursExplicitEmpty(t *testing.T) {
	r, err := Resolve(Defaults())
	require.NoError(t, err)
	assert.Equal(t, " · ", r.Config.StatusLine.Separator)

	r, err = Resolve(Defaults(), globalLayer(t, `{"statusline": {"separator": ""}}`))
	require.NoError(t, err)
	assert.Empty(t, r.Config.StatusLine.Separator)
	assert.Contains(t, r.Origins, "statusline.separator")
}

func TestResolveStatusSegments(t *testing.T) {
	global := globalLayer(t, `{"statusline": {
		"right": ["$k8s", "$host", "$mode"],
		"segments": {
			"k8s": {"command": "kubectl config current-context", "interval": "10s", "color": "dim"},
			"host": {"text": "work", "color": "#89B4FA"},
			"idx": {"text": "x", "color": "255"}
		}}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, StatusSegment{Command: "kubectl config current-context", Interval: "10s", Color: "dim"}, r.Config.StatusLine.Segments["k8s"])
	assert.Equal(t, StatusSegment{Text: "work", Color: "#89B4FA"}, r.Config.StatusLine.Segments["host"])
}

func TestResolveRejectsInvalidStatusSegment(t *testing.T) {
	for _, tc := range []struct{ name, seg string }{
		{"both", `"x": {"text": "a", "command": "b"}`},
		{"neither", `"x": {"color": "dim"}`},
		{"interval without command", `"x": {"text": "a", "interval": "5s"}`},
		{"bad interval", `"x": {"command": "a", "interval": "soon"}`},
		{"short interval", `"x": {"command": "a", "interval": "500ms"}`},
		{"bad colour", `"x": {"text": "a", "color": "blue"}`},
		{"short hex", `"x": {"text": "a", "color": "#12"}`},
		{"index out of range", `"x": {"text": "a", "color": "256"}`},
		{"signed index", `"x": {"text": "a", "color": "+5"}`},
		{"built-in name", `"model": {"text": "a"}`},
		{"newline in text", `"x": {"text": "a\nb"}`},
		{"escape in text", `"x": {"text": "\u001b[31ma"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global := globalLayer(t, `{"statusline": {"segments": {`+tc.seg+`}}}`)
			_, err := Resolve(Defaults(), global)
			require.ErrorIs(t, err, ErrInvalidValue)
			assert.Contains(t, err.Error(), global.Name)
			assert.Contains(t, err.Error(), "statusline.segments.")
		})
	}
}

func TestResolveRejectsControlCharacterInSeparator(t *testing.T) {
	for _, line := range []string{"statusline", "agentline"} {
		for _, sep := range []string{`\n`, `\t`, ` \u001b[0m `} {
			global := globalLayer(t, `{"`+line+`": {"separator": "`+sep+`"}}`)
			_, err := Resolve(Defaults(), global)
			require.ErrorIs(t, err, ErrInvalidValue)
			assert.Contains(t, err.Error(), global.Name)
			assert.Contains(t, err.Error(), line+".separator")
		}
	}
}

func TestResolveChecksSegmentNamesOnMergedConfig(t *testing.T) {
	defining := Map("defining", map[string]any{"statusline": map[string]any{"segments": map[string]any{"host": map[string]any{"text": "w"}}}})
	using := Map("using", map[string]any{"statusline": map[string]any{"left": []any{"$host"}}})
	r, err := Resolve(Defaults(), defining, using)
	require.NoError(t, err)
	assert.Equal(t, []string{"$host"}, r.Config.StatusLine.Left)

	_, err = Resolve(Defaults(), using)
	require.ErrorIs(t, err, ErrInvalidValue)
	assert.Contains(t, err.Error(), "using")
	assert.Contains(t, err.Error(), `statusline.left: unknown segment "host"`)
}

func TestColorValidators(t *testing.T) {
	for _, role := range ThemeRoles {
		assert.NoError(t, ValidColor(role))
		assert.Error(t, ValidLiteralColor(role))
	}
	for _, ok := range []string{"#fff", "#FfA0b1", "0", "7", "255"} {
		assert.NoError(t, ValidColor(ok), ok)
		assert.NoError(t, ValidLiteralColor(ok), ok)
	}
	for _, bad := range []string{"", "#", "#ff", "#ffff", "#ggg", "#1234567", "256", "-1", "+1", " 1", "1.0", "red"} {
		assert.Error(t, ValidColor(bad), bad)
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
	assert.Equal(t, Sandbox{AllowWrite: []string{"/var/y"}, DenyRead: []string{"~/.aws"}, Network: true, AllowUnsandboxed: true}, r.Config.Sandbox)
	assert.Equal(t, "flags", r.Origins["sandbox.allowWrite"])
	assert.Equal(t, global.Name, r.Origins["sandbox.denyRead"])
	assert.Equal(t, global.Name, r.Origins["sandbox.network"])
}

func TestSandboxAllowUnsandboxed(t *testing.T) {
	r, err := Resolve(Defaults())
	require.NoError(t, err)
	assert.True(t, r.Config.Sandbox.AllowUnsandboxed)

	global := globalLayer(t, `{"sandbox": {"allowUnsandboxed": false}}`)
	r, err = Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.False(t, r.Config.Sandbox.AllowUnsandboxed)
	assert.Equal(t, global.Name, r.Origins["sandbox.allowUnsandboxed"])
}

func TestResolveClassifier(t *testing.T) {
	global := globalLayer(t, `{"classifier": {"enabled": true, "model": "anthropic/claude-haiku", "instructions": ["deploys are fine", "no force push"]}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	assert.Equal(t, Classifier{Enabled: true, Model: "anthropic/claude-haiku", Instructions: []string{"deploys are fine", "no force push"}}, r.Config.Classifier)
	for _, key := range []string{"enabled", "model", "instructions"} {
		assert.Equal(t, global.Name, r.Origins["classifier."+key])
	}

	r, err = Resolve(Defaults())
	require.NoError(t, err)
	assert.Equal(t, Classifier{Instructions: []string{}}, r.Config.Classifier)
	assert.NotContains(t, r.Origins, "classifier.model")
}

func TestResolveRejectsInvalidClassifierModelNamingLayer(t *testing.T) {
	_, err := Resolve(Defaults(), Map("flags", map[string]any{"classifier": map[string]any{"model": "noslash"}}))
	require.ErrorIs(t, err, ErrInvalidModel)
	assert.Contains(t, err.Error(), "flags")
}

func TestResolveRejectsBlankClassifierInstructionNamingLayerAndIndex(t *testing.T) {
	for _, blank := range []string{"", "  \t"} {
		global := globalLayer(t, fmt.Sprintf(`{"classifier": {"instructions": ["ok", %q]}}`, blank))
		_, err := Resolve(Defaults(), global)
		require.ErrorIs(t, err, ErrInvalidValue)
		assert.Contains(t, err.Error(), global.Name)
		assert.Contains(t, err.Error(), "classifier.instructions[1]")
	}
}

func TestResolveRejectsBadClassifierShape(t *testing.T) {
	global := globalLayer(t, `{"classifier": {"enabled": "true"}}`)
	_, err := Resolve(Defaults(), global)
	require.ErrorIs(t, err, ErrInvalidValue)
	assert.Contains(t, err.Error(), global.Name)

	_, err = Resolve(Defaults(), globalLayer(t, `{"classifier": {"instructions": "be careful"}}`))
	require.ErrorIs(t, err, ErrInvalidValue)

	global = globalLayer(t, `{"classifier": {"bogus": true}}`)
	_, err = Resolve(Defaults(), global)
	require.ErrorIs(t, err, ErrUnknownKey)
	assert.Contains(t, err.Error(), global.Name)
}

func TestClassifierInstructionsReplaceAcrossLayers(t *testing.T) {
	global := globalLayer(t, `{"classifier": {"enabled": true, "instructions": ["a", "b"]}}`)
	r, err := Resolve(Defaults(), global, Map("flags", map[string]any{"classifier": map[string]any{"instructions": []any{"c"}}}))
	require.NoError(t, err)
	assert.Equal(t, Classifier{Enabled: true, Instructions: []string{"c"}}, r.Config.Classifier)
	assert.Equal(t, "flags", r.Origins["classifier.instructions"])
	assert.Equal(t, global.Name, r.Origins["classifier.enabled"])
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
	assert.Len(t, entries, 25+len(KeyActions))
	assert.IsIncreasing(t, entryKeys(entries))
	assert.Contains(t, entries, Entry{Key: "model", Value: "", Origin: "unset"})
	assert.Contains(t, entries, Entry{Key: "smallModel", Value: "", Origin: "unset"})
	assert.Contains(t, entries, Entry{Key: "version", Value: "1", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "mode", Value: "plan", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "instructionFallbacks", Value: `["CLAUDE.md"]`, Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "keys.submit", Value: `["enter"]`, Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "keys.newline", Value: `["shift+enter","alt+enter","ctrl+j"]`, Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "statusline.left", Value: `["$model","$context","$branch( $changes)"]`, Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "statusline.right", Value: `["$notice","$sandbox","$shells","$mode"]`, Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "permissions.allow", Value: "[]", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "permissions.ask", Value: "[]", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "permissions.deny", Value: "[]", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "sandbox.enabled", Value: "", Origin: "unset"})
	assert.Contains(t, entries, Entry{Key: "sandbox.allowWrite", Value: "[]", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "sandbox.denyRead", Value: "[]", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "sandbox.network", Value: "false", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "sandbox.allowUnsandboxed", Value: "true", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "skills.prefix", Value: "skill", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "classifier.enabled", Value: "false", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "classifier.model", Value: "", Origin: "unset"})
	assert.Contains(t, entries, Entry{Key: "classifier.instructions", Value: "[]", Origin: "default"})

	r, err = Resolve(Defaults(), Map("flags", map[string]any{
		"model":                "a/b",
		"smallModel":           "c/d",
		"mode":                 "execute",
		"instructionFallbacks": []any{},
		"statusline":           map[string]any{"right": []any{}},
		"permissions":          map[string]any{"deny": []any{"Read(.env)", "*(~/.ssh)"}},
		"sandbox":              map[string]any{"enabled": false, "denyRead": []any{"~/.aws"}, "network": true},
		"classifier":           map[string]any{"enabled": true, "model": "e/f"},
	}))
	require.NoError(t, err)
	entries = r.Entries()
	assert.Contains(t, entries, Entry{Key: "model", Value: "a/b", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "smallModel", Value: "c/d", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "mode", Value: "execute", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "instructionFallbacks", Value: "[]", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "statusline.right", Value: "[]", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "permissions.deny", Value: `["Read(.env)","*(~/.ssh)"]`, Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "permissions.allow", Value: "[]", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "sandbox.enabled", Value: "false", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "sandbox.denyRead", Value: `["~/.aws"]`, Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "sandbox.network", Value: "true", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "sandbox.allowWrite", Value: "[]", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "classifier.enabled", Value: "true", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "classifier.model", Value: "e/f", Origin: "flags"})
	assert.Contains(t, entries, Entry{Key: "classifier.instructions", Value: "[]", Origin: "default"})
}

func TestEntriesShowThemeSeparatorAndSegments(t *testing.T) {
	global := globalLayer(t, `{"theme": {"dark": "x"}, "statusline": {"segments": {
		"host": {"text": "work"},
		"k8s": {"command": "kubectl config current-context", "interval": "10s"}
	}}}`)
	r, err := Resolve(Defaults(), global)
	require.NoError(t, err)
	entries := r.Entries()
	assert.IsIncreasing(t, entryKeys(entries))
	assert.Contains(t, entries, Entry{Key: "theme.dark", Value: "x", Origin: global.Name})
	assert.Contains(t, entries, Entry{Key: "theme.light", Value: "catppuccin-latte", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "statusline.separator", Value: " · ", Origin: "default"})
	assert.Contains(t, entries, Entry{Key: "statusline.segments.host", Value: "text: work", Origin: global.Name})
	assert.Contains(t, entries, Entry{Key: "statusline.segments.k8s", Value: "command: kubectl config current-context every 10s", Origin: global.Name})
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
