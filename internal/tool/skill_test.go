package tool

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/skill"
)

func admitAll(policy.Target) (policy.Mode, error) { return policy.Plan, nil }

func skillDir(t *testing.T) skill.Skill {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ref.md"), []byte("one\ntwo\nthree\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	return skill.Skill{Name: "deploy", Description: "Deploys.", Body: "\nDo it.\n", Dir: dir}
}

func runSkill(t *testing.T, sk skill.Skill, args string) (string, error) {
	t.Helper()
	res, err := NewSkill([]skill.Skill{sk}, "").Run(t.Context(), json.RawMessage(args), admitAll)
	return res.Text, err
}

func TestSkillReturnsInstructions(t *testing.T) {
	sk := skillDir(t)

	out, err := runSkill(t, sk, `{"name":"deploy"}`)

	require.NoError(t, err)
	assert.Equal(t, skill.Text(sk), out)
}

func TestSkillReadsBundledFile(t *testing.T) {
	sk := skillDir(t)

	out, err := runSkill(t, sk, `{"name":"deploy","file":"ref.md"}`)
	require.NoError(t, err)
	assert.Equal(t, "     1\tone\n     2\ttwo\n     3\tthree\n", out)

	out, err = runSkill(t, sk, `{"name":"deploy","file":"ref.md","offset":2,"limit":1}`)
	require.NoError(t, err)
	assert.Equal(t, "     2\ttwo\n[Showing lines 2-2. More lines remain; use offset=3 to continue.]\n", out)
}

func TestSkillRefusesFilesOutsideItsDirectory(t *testing.T) {
	sk := skillDir(t)
	outside := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(sk.Dir, "link")))

	for name, file := range map[string]string{
		"parent":    "../x",
		"absolute":  outside,
		"symlink":   "link",
		"directory": "sub",
		"missing":   "nope",
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]string{"name": "deploy", "file": file})
			require.NoError(t, err)
			out, err := runSkill(t, sk, string(raw))
			require.Error(t, err)
			assert.NotContains(t, out, "secret")
		})
	}
	_, err := runSkill(t, sk, `{"name":"deploy","file":"../x"}`)
	assert.ErrorIs(t, err, ErrInvalidArgs)
	_, err = runSkill(t, sk, `{"name":"deploy","file":"sub"}`)
	assert.ErrorIs(t, err, ErrNotRegular)
}

func TestSkillRefusesFilesAfterDirectoryIsReplaced(t *testing.T) {
	sk := skillDir(t)
	tool := NewSkill([]skill.Skill{sk}, "")
	require.NoError(t, os.RemoveAll(sk.Dir))
	require.NoError(t, os.MkdirAll(sk.Dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sk.Dir, "ref.md"), []byte("secret"), 0o644))

	out, err := tool.Run(t.Context(), json.RawMessage(`{"name":"deploy","file":"ref.md"}`), admitAll)
	assert.ErrorIs(t, err, ErrSkillChanged)
	assert.NotContains(t, out.Text, "secret")

	out, err = tool.Run(t.Context(), json.RawMessage(`{"name":"deploy"}`), admitAll)
	require.NoError(t, err)
	assert.Equal(t, skill.Text(sk), out.Text)
}

func TestSkillRefusesFilesOfADirectoryMissingAtStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "late")
	tool := NewSkill([]skill.Skill{{Name: "late", Dir: dir}}, "")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ref.md"), []byte("secret"), 0o644))

	out, err := tool.Run(t.Context(), json.RawMessage(`{"name":"late","file":"ref.md"}`), admitAll)

	assert.ErrorIs(t, err, ErrSkillChanged)
	assert.NotContains(t, out.Text, "secret")
}

func TestSkillRejectsBadArguments(t *testing.T) {
	sk := skillDir(t)
	for _, args := range []string{
		`{}`,
		`{"name":"deploy","offset":1}`,
		`{"name":"deploy","limit":1}`,
		`{"name":"deploy","file":"ref.md","offset":0}`,
		`{"name":"deploy","file":"ref.md","limit":0}`,
		`{"name":"deploy","extra":1}`,
	} {
		_, err := runSkill(t, sk, args)
		assert.ErrorIs(t, err, ErrInvalidArgs, args)
	}
}

func TestSkillUnknownName(t *testing.T) {
	_, err := runSkill(t, skillDir(t), `{"name":"nope"}`)

	assert.ErrorIs(t, err, ErrUnknownSkill)
	assert.ErrorContains(t, err, "nope")
}

func TestSkillAdmitsBeforeReading(t *testing.T) {
	sk := skill.Skill{Name: "gone", Dir: filepath.Join(t.TempDir(), "missing")}
	denied := errors.New("denied")
	tool := NewSkill([]skill.Skill{sk}, "")
	raw := json.RawMessage(`{"name":"gone","file":"x"}`)
	want, err := tool.Target(raw)
	require.NoError(t, err)

	_, err = tool.Run(t.Context(), raw, func(target policy.Target) (policy.Mode, error) {
		assert.Equal(t, want, target)
		return policy.Plan, denied
	})

	assert.ErrorIs(t, err, denied)
}

func TestSkillTarget(t *testing.T) {
	sk := skillDir(t)
	real := filepath.Join(t.TempDir(), "real")
	require.NoError(t, os.MkdirAll(real, 0o755))
	linked := skill.Skill{Name: "linked", Dir: filepath.Join(t.TempDir(), "link")}
	require.NoError(t, os.Symlink(real, linked.Dir))
	realResolved, err := filepath.EvalSymlinks(real)
	require.NoError(t, err)
	tool := NewSkill([]skill.Skill{sk, linked}, "")

	dir, err := tool.Target(json.RawMessage(`{"name":"deploy"}`))
	require.NoError(t, err)
	assert.Equal(t, sk.Dir, dir.Path)

	file, err := tool.Target(json.RawMessage(`{"name":"deploy","file":"sub/../ref.md"}`))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(sk.Dir, "ref.md"), file.Path)

	through, err := tool.Target(json.RawMessage(`{"name":"linked","file":"ref.md"}`))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(linked.Dir, "ref.md"), through.Path)
	assert.Equal(t, filepath.Join(realResolved, "ref.md"), through.Real)

	_, err = tool.Target(json.RawMessage(`{"name":"nope"}`))
	assert.ErrorIs(t, err, ErrUnknownSkill)
	_, err = tool.Target(json.RawMessage(`{"name":"deploy","file":"../x"}`))
	assert.ErrorIs(t, err, ErrInvalidArgs)
	_, err = tool.Target(json.RawMessage(`{"name":"deploy","offset":"x"}`))
	require.NoError(t, err)
}

func TestSkillRunAdmitsTheTargetItReports(t *testing.T) {
	sk := skillDir(t)
	tool := NewSkill([]skill.Skill{sk}, "")
	for _, args := range []string{`{"name":"deploy"}`, `{"name":"deploy","file":"ref.md"}`} {
		want, err := tool.Target(json.RawMessage(args))
		require.NoError(t, err)
		var got policy.Target

		_, err = tool.Run(t.Context(), json.RawMessage(args), func(target policy.Target) (policy.Mode, error) {
			got = target
			return policy.Plan, nil
		})

		require.NoError(t, err)
		assert.Equal(t, want, got, args)
	}
}

func TestSkillRefusesSymlinkRetargetedAfterAdmission(t *testing.T) {
	for _, tc := range []struct {
		name        string
		initial     string
		replacement string
		file        string
	}{
		{"file", "allowed/ref.md", "replacement/ref.md", "link"},
		{"parent directory", "allowed", "replacement", "link/ref.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sk := skillDir(t)
			for dir, content := range map[string]string{
				"allowed":     "allowed content",
				"replacement": "replacement content",
			} {
				require.NoError(t, os.Mkdir(filepath.Join(sk.Dir, dir), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(sk.Dir, dir, "ref.md"), []byte(content), 0o644))
			}
			link := filepath.Join(sk.Dir, "link")
			require.NoError(t, os.Symlink(tc.initial, link))
			tool := NewSkill([]skill.Skill{sk}, "")
			raw, err := json.Marshal(map[string]string{"name": sk.Name, "file": tc.file})
			require.NoError(t, err)
			want, err := tool.Target(raw)
			require.NoError(t, err)
			real, err := filepath.EvalSymlinks(filepath.Join(sk.Dir, "allowed", "ref.md"))
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(sk.Dir, tc.file), want.Path)
			assert.Equal(t, real, want.Real)
			var admitted bool

			out, err := tool.Run(t.Context(), raw, func(target policy.Target) (policy.Mode, error) {
				admitted = true
				assert.Equal(t, want, target)
				require.NoError(t, os.Remove(link))
				require.NoError(t, os.Symlink(tc.replacement, link))
				return policy.Plan, nil
			})

			assert.True(t, admitted)
			assert.ErrorIs(t, err, ErrSkillChanged)
			assert.NotContains(t, out.Text, "replacement content")
		})
	}
}

func TestValidateSkillTarget(t *testing.T) {
	for _, name := range []string{"unchanged", "different file", "parent symlink", "nested parent symlink", "leaf symlink"} {
		t.Run(name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			for _, parent := range []string{"allowed", "denied"} {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, parent, "nested"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, parent, "nested", "ref.md"), []byte(parent), 0o644))
			}
			target := filepath.Join(dir, "allowed", "nested", "ref.md")
			denied := filepath.Join(dir, "denied", "nested", "ref.md")
			openPath := denied
			if name == "unchanged" {
				openPath = target
			}
			f, err := os.Open(openPath)
			require.NoError(t, err)
			defer f.Close()
			opened, err := f.Stat()
			require.NoError(t, err)

			switch name {
			case "parent symlink":
				parent := filepath.Join(dir, "allowed")
				require.NoError(t, os.Rename(parent, parent+"-old"))
				require.NoError(t, os.Symlink("denied", parent))
			case "nested parent symlink":
				parent := filepath.Dir(target)
				require.NoError(t, os.Rename(parent, parent+"-old"))
				require.NoError(t, os.Symlink(filepath.Dir(denied), parent))
			case "leaf symlink":
				require.NoError(t, os.Remove(target))
				require.NoError(t, os.Symlink(denied, target))
			}
			if name == "parent symlink" || name == "nested parent symlink" {
				resolved, err := os.Lstat(target)
				require.NoError(t, err)
				require.True(t, os.SameFile(opened, resolved))
			}

			err = validateSkillTarget(target, opened)

			if name == "unchanged" {
				require.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrSkillChanged)
			}
		})
	}
}

func TestSkillDescriptionListsInvocableSkillsSorted(t *testing.T) {
	desc := NewSkill([]skill.Skill{
		{Name: "zeta", Description: "Last."},
		{Name: "hidden", Description: "Not listed.", DisableModelInvocation: true},
		{Name: "alpha", Description: "First line.\nSecond line."},
	}, "").Description

	assert.Contains(t, desc, "Skills:\nalpha: First line. Second line.\nzeta: Last.")
	assert.NotContains(t, desc, "hidden")
}

func TestSkillDescriptionWithoutList(t *testing.T) {
	none := NewSkill(nil, "").Description
	allDisabled := NewSkill([]skill.Skill{{Name: "a", Description: "d", DisableModelInvocation: true}}, "").Description

	assert.Equal(t, none, allDisabled)
	assert.NotContains(t, none, "Skills:")
}

func TestSkillServesDisabledSkills(t *testing.T) {
	sk := skillDir(t)
	sk.DisableModelInvocation = true

	out, err := runSkill(t, sk, `{"name":"deploy"}`)

	require.NoError(t, err)
	assert.Equal(t, skill.Text(sk), out)
}

func TestSkillSpec(t *testing.T) {
	tool := NewSkill(nil, "")
	spec := Specs([]Tool{tool})["Skill"]

	assert.Equal(t, "Skill", tool.Label)
	assert.Equal(t, policy.Read, spec.Capability)
	assert.NotNil(t, spec.Target)
	assert.False(t, spec.Ask)
}

func TestSkillDisplay(t *testing.T) {
	d := NewSkill(nil, "").Display

	assert.Equal(t, "Skill", d.Title(json.RawMessage(`{}`)))
	assert.Equal(t, "Skill deploy", d.Title(json.RawMessage(`{"name":"deploy"}`)))
	assert.Equal(t, "Skill deploy ref.md", d.Title(json.RawMessage(`{"name":"deploy","file":"ref.md"}`)))
	assert.Equal(t, "Loaded skill deploy", d.Done(json.RawMessage(`{"name":"deploy"}`)))
	assert.Equal(t, "Read skill deploy ref.md", d.Done(json.RawMessage(`{"name":"deploy","file":"ref.md"}`)))
	_, action := d.Outcome(json.RawMessage(`{"name":"deploy"}`), "")
	assert.Equal(t, Action{}, action)
	_, action = d.Outcome(json.RawMessage(`{"name":"deploy","file":"ref.md"}`), "")
	assert.Equal(t, Action{Verb: "Read", One: "skill file", Many: "skill files"}, action)
	assert.Equal(t, "loaded", d.Summary(skill.Text(skill.Skill{Name: "a"})))
	assert.Equal(t, "2 lines", d.Summary("     1\ta\n     2\tb\n"))
	assert.Equal(t, "1  a\n2  b\n", d.Body("     1\ta\n     2\tb\n"))
	text := skill.Text(skill.Skill{Name: "a"})
	assert.Equal(t, text, d.Body(text))
}

// skillHome builds a Kopi home holding a credential, a config and one skill,
// and returns its path.
func skillHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "kopi")
	for file, content := range map[string]string{
		"agent/auth.json":   "secret-auth",
		"config.json":       "secret-config",
		"skills/s/SKILL.md": "---\nname: s\n---\n",
		"skills/s/ref.md":   "ref",
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(home, file)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(home, file), []byte(content), 0o644))
	}
	return home
}

func runSkillAt(t *testing.T, home, dir, file string) (string, error) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"name": "s", "file": file})
	require.NoError(t, err)
	res, err := NewSkill([]skill.Skill{{Name: "s", Dir: dir}}, home).Run(t.Context(), raw, admitAll)
	return res.Text, err
}

func TestSkillReadsFileOfSkillInsideHome(t *testing.T) {
	home := skillHome(t)

	out, err := runSkillAt(t, home, filepath.Join(home, "skills", "s"), "ref.md")

	require.NoError(t, err)
	assert.Contains(t, out, "ref")
}

func TestSkillRefusesHomeOutsideSkills(t *testing.T) {
	home := skillHome(t)
	for name, tc := range map[string]struct{ dir, file string }{
		"agent":    {filepath.Join(home, "agent"), "auth.json"},
		"home":     {home, "config.json"},
		"homeDeep": {home, "agent/auth.json"},
		"ancestor": {filepath.Dir(home), "kopi/agent/auth.json"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := runSkillAt(t, home, tc.dir, tc.file)

			assert.ErrorIs(t, err, ErrProtected)
			assert.NotContains(t, out, "secret")
		})
	}
}

func TestSkillRefusesFilesAfterHomeEntryLinksToSkillDirectory(t *testing.T) {
	home := skillHome(t)
	sk := skillDir(t)
	tool := NewSkill([]skill.Skill{sk}, home)
	raw := json.RawMessage(`{"name":"deploy","file":"ref.md"}`)

	out, err := tool.Run(t.Context(), raw, admitAll)
	require.NoError(t, err)
	assert.Equal(t, "     1\tone\n     2\ttwo\n     3\tthree\n", out.Text)

	require.NoError(t, os.Symlink(sk.Dir, filepath.Join(home, "linked")))
	out, err = tool.Run(t.Context(), raw, admitAll)

	assert.ErrorIs(t, err, ErrProtected)
	assert.Empty(t, out.Text)
}

func TestSkillReadsSkillsAndUnrelatedFilesFromDirectoryHoldingHome(t *testing.T) {
	home := skillHome(t)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(home), "beside.md"), []byte("beside"), 0o644))

	out, err := runSkillAt(t, home, home, "skills/s/ref.md")
	require.NoError(t, err)
	assert.Contains(t, out, "ref")

	out, err = runSkillAt(t, home, filepath.Dir(home), "beside.md")
	require.NoError(t, err)
	assert.Contains(t, out, "beside")
}

func TestSkillReadsWhenHomeDoesNotExist(t *testing.T) {
	sk := skillDir(t)

	out, err := NewSkill([]skill.Skill{sk}, filepath.Join(t.TempDir(), "missing")).Run(t.Context(), json.RawMessage(`{"name":"deploy","file":"ref.md"}`), admitAll)

	require.NoError(t, err)
	assert.Contains(t, out.Text, "one")
}

func TestSkillMissingFileInOrdinaryDirectoryIsNotFound(t *testing.T) {
	_, err := runSkill(t, skillDir(t), `{"name":"deploy","file":"nope"}`)

	assert.ErrorIs(t, err, fs.ErrNotExist)
	assert.NotErrorIs(t, err, ErrProtected)
}

func TestSkillRefusesMissingAndExistingHomeFilesAlike(t *testing.T) {
	home := skillHome(t)
	tl := NewSkill([]skill.Skill{{Name: "s", Dir: home}}, home)
	for _, file := range []string{"agent/nope.json", "agent/auth.json", "agent/auth.json/x", "agent/nope/x", "agent/", "agent/nope/"} {
		raw, err := json.Marshal(map[string]string{"name": "s", "file": file})
		require.NoError(t, err)

		_, err = tl.Target(raw)
		require.NoError(t, err, file)
		out, err := tl.Run(t.Context(), raw, admitAll)

		assert.ErrorIs(t, err, ErrProtected, file)
		assert.NotContains(t, out.Text, "secret")
	}
}
