package pigruntime

import (
	"context"
	"fmt"
	"os"

	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"

	"github.com/keyute/kopi/internal/skill"
)

// Skills loads the skills under dirs, which exist and are directories; the
// others are skipped. A skill of the same name in an earlier directory wins,
// so the order is precedence. The loader's own diagnostics are reported except
// name collisions, which overrides intend.
//
// PiG does the loading through a loader that is never attached to a session,
// which keeps coding.NoResources. Every input of the loader is explicit, as a
// nil one turns on ambient discovery of ~/.pig, ~/.agents and the project. The
// project is forced untrusted because in-memory settings default to a trusted
// one, and a trusted project makes PiG walk the project's directories.
func (r *Runtime) Skills(dirs []string) ([]skill.Skill, []error) {
	var existing []string
	for _, d := range dirs {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			existing = append(existing, d)
		}
	}
	if len(existing) == 0 {
		return nil, nil
	}
	loader := coding.NewDefaultResourceLoader(skillLoaderOptions(r.svcs.CWD(), r.svcs.AgentDir(), existing))
	defer loader.Close()
	if err := loader.Reload(); err != nil {
		return nil, []error{fmt.Errorf("failed to load skills: %w", err)}
	}
	res := loader.GetSkills()
	var errs []error
	for _, d := range res.Diagnostics {
		if d.Type != extension.DiagnosticCollision {
			errs = append(errs, fmt.Errorf("skill %s: %s", d.Path, d.Message))
		}
	}
	var skills []skill.Skill
	for _, s := range res.Skills {
		skills = append(skills, skill.Skill{
			Name:                   s.Name,
			Description:            s.Description,
			Body:                   s.Body,
			Dir:                    s.BaseDir,
			DisableModelInvocation: s.DisableModelInvocation,
		})
	}
	return skills, errs
}

// skillLoaderOptions states every input of the skill loader.
func skillLoaderOptions(cwd, agentDir string, dirs []string) coding.DefaultResourceLoaderOptions {
	sm := coding.NewInMemorySettingsManager(coding.Settings{})
	sm.SetProjectTrusted(false)
	empty := ""
	return coding.DefaultResourceLoaderOptions{
		CWD:                  cwd,
		AgentDir:             agentDir,
		SettingsManager:      sm,
		NoSkills:             true,
		AdditionalSkillPaths: dirs,
		NoPromptTemplates:    true,
		NoThemes:             true,
		NoContextFiles:       true,
		NoExtensions:         true,
		LoadExtensions: func(context.Context, coding.ExtensionLoadRequest) (coding.LoadExtensionsResult, error) {
			return coding.LoadExtensionsResult{}, nil
		},
		SystemPrompt:       &empty,
		AppendSystemPrompt: []string{},
	}
}
