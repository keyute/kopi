package pigruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/skill"
)

func writeSkill(t *testing.T, dir, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644))
}

func skillFile(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\nBody of " + name + "\n"
}

func skillNames(skills []skill.Skill) []string {
	names := []string{}
	for _, s := range skills {
		names = append(names, s.Name)
	}
	return names
}

func TestSkillsIgnoresAmbientLocations(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	writeSkill(t, filepath.Join(sb.home, ".agents", "skills", "h"), skillFile("h", "trap"))
	writeSkill(t, filepath.Join(sb.ws, ".pig", "skills", "p"), skillFile("p", "trap"))
	writeSkill(t, filepath.Join(sb.ws, ".agents", "skills", "a"), skillFile("a", "trap"))
	writeSkill(t, filepath.Join(sb.agentDir, "skills", "g"), skillFile("g", "trap"))
	dir := filepath.Join(t.TempDir(), "skills")
	writeSkill(t, filepath.Join(dir, "x"), skillFile("x", "wanted"))

	skills, errs := r.Skills([]string{dir})

	assert.Empty(t, errs)
	assert.Equal(t, []string{"x"}, skillNames(skills))
}

func TestSkillLoaderOptionsStateEveryInput(t *testing.T) {
	dirs := []string{"b", "a"}

	o := skillLoaderOptions("/cwd", "/agent", dirs)

	require.NotNil(t, o.SettingsManager)
	assert.False(t, o.SettingsManager.IsProjectTrusted())
	assert.True(t, o.NoSkills)
	assert.True(t, o.NoPromptTemplates)
	assert.True(t, o.NoThemes)
	assert.True(t, o.NoContextFiles)
	assert.True(t, o.NoExtensions)
	assert.NotNil(t, o.LoadExtensions)
	require.NotNil(t, o.SystemPrompt)
	assert.Equal(t, "", *o.SystemPrompt)
	assert.NotNil(t, o.AppendSystemPrompt)
	assert.Empty(t, o.AppendSystemPrompt)
	assert.Equal(t, dirs, o.AdditionalSkillPaths)
	assert.Equal(t, "/cwd", o.CWD)
	assert.Equal(t, "/agent", o.AgentDir)
}

func TestSkillsFirstDirectoryWinsWithoutError(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	root := t.TempDir()
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	writeSkill(t, filepath.Join(first, "same"), skillFile("same", "from first"))
	writeSkill(t, filepath.Join(second, "same"), skillFile("same", "from second"))

	skills, errs := r.Skills([]string{first, second})

	assert.Empty(t, errs)
	require.Len(t, skills, 1)
	assert.Equal(t, "from first", skills[0].Description)
}

func TestSkillsSkipsMissingDirectories(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, "x"), skillFile("x", "d"))
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))

	skills, errs := r.Skills([]string{filepath.Join(dir, "missing"), file, dir})
	assert.Empty(t, errs)
	assert.Equal(t, []string{"x"}, skillNames(skills))

	skills, errs = r.Skills(nil)
	assert.Empty(t, skills)
	assert.Empty(t, errs)

	skills, errs = r.Skills([]string{filepath.Join(dir, "missing")})
	assert.Empty(t, skills)
	assert.Empty(t, errs)
}

func TestSkillsReportsMalformedAndIncomplete(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, "bad"), "---\nname: [unclosed\ndescription: d\n---\nbody\n")
	writeSkill(t, filepath.Join(dir, "nodesc"), "---\nname: nodesc\n---\nbody\n")

	skills, errs := r.Skills([]string{dir})

	assert.Empty(t, skills)
	var text string
	for _, err := range errs {
		text += err.Error() + "\n"
	}
	assert.Contains(t, text, filepath.Join(dir, "bad", "SKILL.md"))
}

func TestSkillsMapsFields(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, "defaulted"), "---\ndescription: from dir\ndisable-model-invocation: true\n---\nDo it.\n")

	skills, errs := r.Skills([]string{dir})

	assert.Empty(t, errs)
	require.Len(t, skills, 1)
	assert.Equal(t, "defaulted", skills[0].Name)
	assert.Equal(t, "from dir", skills[0].Description)
	assert.Equal(t, "Do it.", strings.TrimSpace(skills[0].Body))
	assert.True(t, skills[0].DisableModelInvocation)
	assert.Equal(t, filepath.Join(dir, "defaulted"), skills[0].Dir)
}
