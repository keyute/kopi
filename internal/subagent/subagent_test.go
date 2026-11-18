package subagent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var toolNames = []string{"Read", "Grep", "Bash"}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	zed := write(t, dir, "zed.md", "---\nname: zed\ndescription: last\n---\n")
	full := write(t, dir, "a.md", "---\nname: alpha\ndescription: first one\nmodel: openai/gpt-5\neffort: high\ntools: [Read, mcp__x__y]\nmode: plan\ninstructions: false\n---\n\n\nBe brief.\n\nTwice.\n\n")
	write(t, dir, "notes.txt", "ignored")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub.md"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o700))
	write(t, filepath.Join(dir, "nested"), "n.md", "garbage")

	defs, err := Load(dir, toolNames)
	require.NoError(t, err)
	assert.Equal(t, []Definition{
		{
			Name: "alpha", Description: "first one", Model: "openai/gpt-5", Effort: "high",
			Tools: []string{"Read", "mcp__x__y"}, Plan: true, Instructions: false,
			Prompt: "Be brief.\n\nTwice.", Path: full,
		},
		{Name: "zed", Description: "last", Instructions: true, Path: zed},
	}, defs)
}

func TestLoadMissingDir(t *testing.T) {
	defs, err := Load(filepath.Join(t.TempDir(), "none"), toolNames)
	require.NoError(t, err)
	assert.Empty(t, defs)
}

func TestLoadInvalid(t *testing.T) {
	tests := map[string]string{
		"no opening":     "name: a\n",
		"no closing":     "---\nname: a\ndescription: d\n",
		"unknown field":  "---\nname: a\ndescription: d\ncolor: red\n---\n",
		"bad yaml":       "---\nname: [\n---\n",
		"no name":        "---\ndescription: d\n---\n",
		"blank name":     "---\nname: \"  \"\ndescription: d\n---\n",
		"no description": "---\nname: a\n---\n",
		"multiline name": "---\nname: \"a\\nb\"\ndescription: d\n---\n",
		"control name":   "---\nname: \"a\\u0007\"\ndescription: d\n---\n",
		"line separator": "---\nname: a\ndescription: \"x\\u2028y\"\n---\n",
		"para separator": "---\nname: a\ndescription: \"x\\u2029y\"\n---\n",
		"bad mode":       "---\nname: a\ndescription: d\nmode: edit\n---\n",
		"unknown tool":   "---\nname: a\ndescription: d\ntools: [Nope]\n---\n",
		"duplicate tool": "---\nname: a\ndescription: d\ntools: [Read, Read]\n---\n",
		"empty tools":    "---\nname: a\ndescription: d\ntools: []\n---\n",
		"bad instr":      "---\nname: a\ndescription: d\ninstructions: maybe\n---\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := write(t, dir, "x.md", content)
			_, err := Load(dir, toolNames)
			require.ErrorIs(t, err, ErrInvalidDefinition)
			assert.ErrorContains(t, err, path)
		})
	}
}

func TestLoadDuplicateName(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "a.md", "---\nname: same\ndescription: d\n---\n")
	b := write(t, dir, "b.md", "---\nname: same\ndescription: d\n---\n")
	_, err := Load(dir, toolNames)
	require.ErrorIs(t, err, ErrDuplicateName)
	assert.ErrorContains(t, err, a)
	assert.ErrorContains(t, err, b)
}

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.md", "---\nname: a\ndescription: d\nmodel: \" p/m \"\nmode: \"\"\n---")
	defs, err := Load(dir, toolNames)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "p/m", defs[0].Model)
	assert.True(t, defs[0].Instructions)
	assert.False(t, defs[0].Plan)
	assert.Nil(t, defs[0].Tools)
	assert.Empty(t, defs[0].Prompt)
}

func TestLoadSymlink(t *testing.T) {
	dir, store := t.TempDir(), t.TempDir()
	target := write(t, store, "real.md", "---\nname: linked\ndescription: d\n---\nBody")
	link := filepath.Join(dir, "linked.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	require.NoError(t, os.Symlink(filepath.Join(store, "missing.md"), filepath.Join(dir, "dangling.md")))
	require.NoError(t, os.Symlink(store, filepath.Join(dir, "dir.md")))

	defs, err := Load(dir, toolNames)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "linked", defs[0].Name)
	assert.Equal(t, link, defs[0].Path)
}
