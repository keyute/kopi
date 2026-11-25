package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testTools = map[string]ToolSpec{
	"Read":  {Capability: Read, Target: pathTarget},
	"Edit":  {Capability: Write, Target: pathTarget},
	"Write": {Capability: Write, Target: pathTarget},
}

type dirs struct{ ws, home string }

func newDirs(t *testing.T) dirs {
	t.Helper()
	return dirs{ws: t.TempDir(), home: t.TempDir()}
}

func (d dirs) rules(t *testing.T, sources ...Source) *Rules {
	t.Helper()
	r, err := NewRules(d.ws, d.home, testTools, sources...)
	require.NoError(t, err)
	r.fold = false
	return r
}

// at is a call on p with no symlink in the way.
func at(p string) *Target { return &Target{Path: p, Real: p} }

func TestValidateRule(t *testing.T) {
	for _, s := range []string{"Read", "*", "Read(*.go)", "Edit(docs/**)", "Read(!.env.example)", "*(~/.ssh)", "Write(/etc/**)", "Read(a(1).txt)", "Bash", "Re-ad", "mcp__context7__resolve-library-id", "mcp__context7__*", "mcp__github__get_*", "mcp__*"} {
		assert.NoError(t, ValidateRule(s), s)
	}
	for _, s := range []string{"", "Read(", "Read()", "Read(!)", "Read([)", "Read({a)", "Re ad", "Read(a))", "Read)", "(x)", "Read(x) ", "-Read", "Read*", "*Read", "mcp__x*y", "mcp__x__**", "mcp__x__y(src/**)", "mcp__x__*(!a)"} {
		assert.ErrorIs(t, ValidateRule(s), ErrInvalidRule, s)
	}
}

func TestNewRulesRejectsUnknownToolNamingRuleAndSource(t *testing.T) {
	d := newDirs(t)
	_, err := NewRules(d.ws, d.home, testTools, Source{Name: "config", Deny: []string{"Bash"}})
	require.ErrorIs(t, err, ErrInvalidRule)
	assert.Contains(t, err.Error(), `config: rule "Bash": unknown tool "Bash"`)
}

func TestNewRulesRejectsPathRuleForPathlessTool(t *testing.T) {
	d := newDirs(t)
	tools := map[string]ToolSpec{"Read": {Capability: Read, Target: pathTarget}, "Exec": {Capability: Exec}}
	for _, text := range []string{"Exec(ls)", "Exec(!x)"} {
		_, err := NewRules(d.ws, d.home, tools, Source{Name: "config", Ask: []string{text}})
		require.ErrorIs(t, err, ErrInvalidRule, text)
		assert.Contains(t, err.Error(), `config: rule "`+text+`": tool "Exec" takes no path`)
	}
	_, err := NewRules(d.ws, d.home, tools, Source{Name: "config", Allow: []string{"Exec"}, Deny: []string{"*(x)"}})
	assert.NoError(t, err)
}

func TestNewRulesTakesMCPNamesWithoutKnowingTheTool(t *testing.T) {
	d := newDirs(t)
	_, err := NewRules(d.ws, d.home, testTools, Source{
		Name:  "config",
		Allow: []string{"mcp__context7__resolve-library-id"},
		Ask:   []string{"mcp__github__get_*"},
		Deny:  []string{"mcp__*"},
	})
	assert.NoError(t, err)
}

func TestNewRulesRejectsMCPNamesThatNameNoTool(t *testing.T) {
	d := newDirs(t)
	for _, text := range []string{"mcp__github", "mcp__", "mcp__github__", "mcp____tool", "mcp__git*", "mcp___*", "mcp____*", "mcp__a_*"} {
		for _, src := range []Source{
			{Name: "config", Allow: []string{text}},
			{Name: "config", Ask: []string{text}},
			{Name: "config", Deny: []string{text}},
		} {
			_, err := NewRules(d.ws, d.home, testTools, src)
			require.ErrorIs(t, err, ErrInvalidRule, text)
			assert.Contains(t, err.Error(), `config: rule "`+text+`": name one MCP tool as mcp__server__tool or all of a server's as mcp__server__*`)
		}
	}
	for _, text := range []string{"mcp__*", "mcp__github__*", "mcp__github__get_*", "mcp__github__get_issue", "mcp__a__b__c"} {
		assert.NoError(t, ValidateRule(text), text)
	}
}

func TestNewRulesRejectsPathRuleForMCPTool(t *testing.T) {
	d := newDirs(t)
	_, err := NewRules(d.ws, d.home, testTools, Source{Name: "config", Allow: []string{"mcp__x__y(src/**)"}})
	require.ErrorIs(t, err, ErrInvalidRule)
	assert.Contains(t, err.Error(), `config: rule "mcp__x__y(src/**)": an MCP tool takes no path`)
}

func TestNewRulesRejectsMalformedRuleNamingSource(t *testing.T) {
	d := newDirs(t)
	_, err := NewRules(d.ws, d.home, testTools, Source{Name: "--allowed-tools", Allow: []string{"Read("}})
	require.ErrorIs(t, err, ErrInvalidRule)
	assert.Contains(t, err.Error(), `--allowed-tools: rule "Read("`)
}

func TestPrecedenceIsDenyAskAllowRegardlessOfOrder(t *testing.T) {
	d := newDirs(t)
	secret := filepath.Join(d.ws, "secret")
	allow := Source{Name: "a", Allow: []string{"Read"}}
	ask := Source{Name: "b", Ask: []string{"Read(secret)"}}
	deny := Source{Name: "c", Deny: []string{"Read(secret)"}}
	for _, order := range [][]Source{{allow, ask, deny}, {deny, ask, allow}, {ask, allow, deny}} {
		eff, text, src := d.rules(t, order...).decide("Read", at(secret))
		assert.Equal(t, effectDeny, eff)
		assert.Equal(t, "Read(secret)", text)
		assert.Equal(t, "c", src)
	}
	for _, order := range [][]Source{{allow, ask}, {ask, allow}} {
		eff, _, src := d.rules(t, order...).decide("Read", at(secret))
		assert.Equal(t, effectAsk, eff)
		assert.Equal(t, "b", src)
	}
	eff, _, _ := d.rules(t, allow, ask).decide("Read", at(filepath.Join(d.ws, "other")))
	assert.Equal(t, effectAllow, eff)
	eff, _, _ = d.rules(t).decide("Read", at(secret))
	assert.Equal(t, effectNone, eff)
}

func TestPathForms(t *testing.T) {
	d := newDirs(t)
	cases := []struct {
		name, rule  string
		match, miss []string
	}{
		{"absolute", "Read(/etc/passwd)", []string{"/etc/passwd"}, []string{filepath.Join(d.ws, "etc/passwd")}},
		{"home", "Read(~/.ssh)", []string{filepath.Join(d.home, ".ssh/id_rsa")}, []string{filepath.Join(d.ws, ".ssh/id_rsa")}},
		{"relative with slash", "Read(docs/*.md)", []string{filepath.Join(d.ws, "docs/a.md")}, []string{filepath.Join(d.ws, "sub/docs/a.md"), filepath.Join(d.ws, "docs/a.txt")}},
		{"dot relative", "Read(./notes.txt)", []string{filepath.Join(d.ws, "notes.txt")}, []string{filepath.Join(d.ws, "a/notes.txt")}},
		{"trailing slash at any depth", "Read(secrets/)", []string{filepath.Join(d.ws, "secrets/k"), filepath.Join(d.ws, "a/secrets/k")}, []string{filepath.Join(d.ws, "secretsx/k"), filepath.Join(d.home, "secrets/k")}},
		{"bare name at any depth", "Read(.env)", []string{filepath.Join(d.ws, ".env"), filepath.Join(d.ws, "a/b/.env")}, []string{filepath.Join(d.home, ".env"), filepath.Join(d.ws, ".envrc")}},
		{"bare glob at any depth", "Read(*.pem)", []string{filepath.Join(d.ws, "k.pem"), filepath.Join(d.ws, "a/k.pem")}, []string{filepath.Join(d.ws, "k.pem.txt")}},
		{"directory covers children", "Read(src/gen)", []string{filepath.Join(d.ws, "src/gen"), filepath.Join(d.ws, "src/gen/x/y.go")}, []string{filepath.Join(d.ws, "src/general.go")}},
		{"double star", "Read(docs/**)", []string{filepath.Join(d.ws, "docs/a/b.md")}, []string{filepath.Join(d.ws, "doc/a.md")}},
		{"double slash cleaned", "Read(//etc/hosts)", []string{"/etc/hosts"}, []string{filepath.Join(d.ws, "etc/hosts")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := d.rules(t, Source{Name: "config", Deny: []string{tc.rule}})
			for _, p := range tc.match {
				eff, _, _ := r.decide("Read", at(p))
				assert.Equal(t, effectDeny, eff, p)
			}
			for _, p := range tc.miss {
				eff, _, _ := r.decide("Read", at(p))
				assert.Equal(t, effectNone, eff, p)
			}
		})
	}
}

func TestSpecRuleNeedsTargetAndBareRuleDoesNot(t *testing.T) {
	d := newDirs(t)
	eff, _, _ := d.rules(t, Source{Name: "config", Deny: []string{"Read(.env)"}}).decide("Read", nil)
	assert.Equal(t, effectNone, eff)
	eff, _, _ = d.rules(t, Source{Name: "config", Deny: []string{"Read"}}).decide("Read", nil)
	assert.Equal(t, effectDeny, eff)
}

func TestRuleGovernsOnlyItsTool(t *testing.T) {
	d := newDirs(t)
	env := at(filepath.Join(d.ws, ".env"))
	r := d.rules(t, Source{Name: "config", Deny: []string{"Read(.env)"}})
	for tool, want := range map[string]effect{"Read": effectDeny, "Edit": effectNone, "Write": effectNone} {
		eff, _, _ := r.decide(tool, env)
		assert.Equal(t, want, eff, tool)
	}
	r = d.rules(t, Source{Name: "config", Deny: []string{"*(.env)"}})
	for tool := range testTools {
		eff, text, _ := r.decide(tool, env)
		assert.Equal(t, effectDeny, eff, tool)
		assert.Equal(t, "*(.env)", text)
	}
}

func TestMCPRuleNames(t *testing.T) {
	d := newDirs(t)
	for _, tc := range []struct {
		rule        string
		match, miss []string
	}{
		{"mcp__c7__resolve-library-id", []string{"mcp__c7__resolve-library-id"}, []string{"mcp__c7__resolve-library-idx", "mcp__c7__query"}},
		{"mcp__c7__*", []string{"mcp__c7__resolve-library-id", "mcp__c7__query"}, []string{"mcp__c77__query", "mcp__gh__get_issue", "Read"}},
		{"mcp__gh__get_*", []string{"mcp__gh__get_issue"}, []string{"mcp__gh__list_issues"}},
		{"mcp__gh___*", []string{"mcp__gh___x"}, []string{"mcp__gh__x"}},
		{"mcp__*", []string{"mcp__c7__query", "mcp__gh__get_issue"}, []string{"Read", "Edit"}},
		{"*", []string{"mcp__c7__query", "Read"}, nil},
	} {
		t.Run(tc.rule, func(t *testing.T) {
			r := d.rules(t, Source{Name: "config", Deny: []string{tc.rule}})
			for _, tool := range tc.match {
				eff, text, _ := r.decide(tool, nil)
				assert.Equal(t, effectDeny, eff, tool)
				assert.Equal(t, tc.rule, text, tool)
				assert.True(t, r.Removes(tool), tool)
			}
			for _, tool := range tc.miss {
				eff, _, _ := r.decide(tool, nil)
				assert.Equal(t, effectNone, eff, tool)
				assert.False(t, r.Removes(tool), tool)
			}
		})
	}
}

func TestMCPPrefixRulesKeepPrecedence(t *testing.T) {
	d := newDirs(t)
	r := d.rules(t,
		Source{Name: "a", Allow: []string{"mcp__gh__get_issue"}},
		Source{Name: "b", Ask: []string{"mcp__gh__*"}},
	)
	eff, text, src := r.decide("mcp__gh__get_issue", nil)
	assert.Equal(t, effectAsk, eff)
	assert.Equal(t, "mcp__gh__*", text)
	assert.Equal(t, "b", src)
}

func TestExceptionsExemptFromTheirListInAnyOrder(t *testing.T) {
	d := newDirs(t)
	env, example := at(filepath.Join(d.ws, ".env")), at(filepath.Join(d.ws, ".env.example"))
	pair := [][]string{{"Read(.env*)", "Read(!.env.example)"}, {"Read(!.env.example)", "Read(.env*)"}}
	for _, rules := range pair {
		for _, tc := range []struct {
			src  Source
			want effect
		}{
			{Source{Name: "config", Deny: rules}, effectDeny},
			{Source{Name: "config", Ask: rules}, effectAsk},
			{Source{Name: "config", Allow: rules}, effectAllow},
		} {
			r := d.rules(t, tc.src)
			eff, _, _ := r.decide("Read", env)
			assert.Equal(t, tc.want, eff)
			eff, _, _ = r.decide("Read", example)
			assert.Equal(t, effectNone, eff)
		}
	}
}

func TestExceptionDoesNotReachAnotherSource(t *testing.T) {
	d := newDirs(t)
	example := at(filepath.Join(d.ws, ".env.example"))
	r := d.rules(t,
		Source{Name: "config", Deny: []string{"Read(.env*)"}},
		Source{Name: "--disallowed-tools", Deny: []string{"Read(!.env.example)"}},
		Source{Name: "--allowed-tools", Allow: []string{"Read(!.env.example)"}, Ask: []string{"Read(!.env.example)"}},
	)
	eff, _, src := r.decide("Read", example)
	assert.Equal(t, effectDeny, eff)
	assert.Equal(t, "config", src)
}

func TestSymlinks(t *testing.T) {
	d := newDirs(t)
	ws := func(p string) string { return filepath.Join(d.ws, p) }
	deny := Source{Name: "config", Deny: []string{"Read(secret)", "Read(.env*)", "Read(!.env.example)"}}
	allow := Source{Name: "config", Allow: []string{"Edit(docs/**)", "Edit(!docs/private/**)"}}
	r := d.rules(t, deny, allow)
	cases := []struct {
		name, tool string
		target     Target
		want       effect
	}{
		{"deny via resolved path", "Read", Target{Path: ws("link"), Real: ws("secret")}, effectDeny},
		{"deny via written path", "Read", Target{Path: ws("secret"), Real: ws("other")}, effectDeny},
		{"deny exception needs both", "Read", Target{Path: ws(".env.example"), Real: ws(".env")}, effectDeny},
		{"deny exception with both", "Read", Target{Path: ws(".env.example"), Real: ws("x/.env.example")}, effectNone},
		{"allow needs both", "Edit", Target{Path: ws("docs/link"), Real: ws("etc/passwd")}, effectNone},
		{"allow with both", "Edit", Target{Path: ws("docs/link"), Real: ws("docs/a.md")}, effectAllow},
		{"allow exception via either", "Edit", Target{Path: ws("docs/link"), Real: ws("docs/private/x")}, effectNone},
	}
	for _, tc := range cases {
		eff, _, _ := r.decide(tc.tool, &tc.target)
		assert.Equal(t, tc.want, eff, tc.name)
	}
}

func TestCaseFolding(t *testing.T) {
	d := newDirs(t)
	r := d.rules(t, Source{Name: "config", Deny: []string{"Read(Secret.TXT)"}})
	lower := at(filepath.Join(d.ws, "secret.txt"))
	eff, _, _ := r.decide("Read", lower)
	assert.Equal(t, effectNone, eff)
	r.fold = true
	eff, _, _ = r.decide("Read", lower)
	assert.Equal(t, effectDeny, eff)
}

func TestWorkspaceMatchesGivenAndResolvedForms(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	link := filepath.Join(t.TempDir(), "ws")
	require.NoError(t, os.Symlink(dir, link))
	d := dirs{ws: link, home: t.TempDir()}
	r := d.rules(t, Source{Name: "config", Allow: []string{"Edit(docs/**)"}}, Source{Name: "other", Deny: []string{"Read(.env)"}})

	eff, _, _ := r.decide("Edit", &Target{Path: filepath.Join(link, "docs/a.md"), Real: filepath.Join(resolved, "docs/a.md")})
	assert.Equal(t, effectAllow, eff)
	eff, _, _ = r.decide("Read", at(filepath.Join(resolved, ".env")))
	assert.Equal(t, effectDeny, eff)
}

func TestRemovesOnlyForBareDeny(t *testing.T) {
	d := newDirs(t)
	r := d.rules(t,
		Source{Name: "config", Deny: []string{"Read", "Edit(.env)"}, Ask: []string{"Write"}},
		Source{Name: "flags", Allow: []string{"Edit"}},
	)
	assert.True(t, r.Removes("Read"))
	assert.False(t, r.Removes("Edit"))
	assert.False(t, r.Removes("Write"))

	all := d.rules(t, Source{Name: "config", Deny: []string{"*"}})
	for tool := range testTools {
		assert.True(t, all.Removes(tool), tool)
	}
	assert.False(t, d.rules(t, Source{Name: "config", Deny: []string{"*(.env)"}}).Removes("Read"))
}

func TestBuiltInAsksForEditsUnderGit(t *testing.T) {
	d := newDirs(t)
	r := d.rules(t)
	for _, tool := range []string{"Edit", "Write"} {
		for _, p := range []string{".git/config", "sub/repo/.git/HEAD"} {
			eff, text, src := r.decide(tool, at(filepath.Join(d.ws, p)))
			assert.Equal(t, effectAsk, eff, tool, p)
			assert.Equal(t, tool+"(.git)", text)
			assert.Equal(t, "built-in", src)
		}
		eff, _, _ := r.decide(tool, at(filepath.Join(d.ws, "src/a.go")))
		assert.Equal(t, effectNone, eff)
	}
	eff, _, _ := r.decide("Read", at(filepath.Join(d.ws, ".git/config")))
	assert.Equal(t, effectNone, eff)
}

func TestBuiltInGitComponentsAndAliases(t *testing.T) {
	d := newDirs(t)
	r := d.rules(t, Source{Name: "config", Allow: []string{"*"}})
	for _, tool := range []string{"Edit", "Write"} {
		for _, tc := range []struct {
			name   string
			target *Target
			want   effect
		}{
			{"git itself", at("/repo/.git"), effectAsk},
			{"ancestor", at("/repo/.git/hooks/pre-commit"), effectAsk},
			{"drive prefix", at("C:/repo/.git/hooks/pre-commit"), effectAsk},
			{"resolved alias", &Target{Path: "/repo/link/pre-commit", Real: "/repo/.git/hooks/pre-commit"}, effectAsk},
			{"lexical alias", &Target{Path: "/repo/.git/hooks/pre-commit", Real: "/repo/hooks/pre-commit"}, effectAsk},
			{"normalized", at("/repo/sub/../.git/hooks/pre-commit"), effectAsk},
			{"normalized away", at("/repo/.git/../src/a.go"), effectAllow},
			{"not a component", at("/repo/not.git/hooks/pre-commit"), effectAllow},
			{"suffix", at("/repo/.gitignore"), effectAllow},
			{"ordinary", at("/repo/src/a.go"), effectAllow},
			{"no target", nil, effectAllow},
		} {
			t.Run(tool+"/"+tc.name, func(t *testing.T) {
				eff, text, src := r.decide(tool, tc.target)
				assert.Equal(t, tc.want, eff)
				if tc.want == effectAsk {
					assert.Equal(t, tool+"(.git)", text)
					assert.Equal(t, "built-in", src)
				}
			})
		}
		upper := at("C:/repo/.GIT/hooks/pre-commit")
		r.fold = false
		eff, _, _ := r.decide(tool, upper)
		assert.Equal(t, effectAllow, eff)
		r.fold = true
		eff, _, _ = r.decide(tool, upper)
		assert.Equal(t, effectAsk, eff)
		r.fold = false
	}
}

func TestBuiltInGitAskPrecedenceInsideGitWorkspace(t *testing.T) {
	for _, sub := range []string{".git", ".git/hooks"} {
		t.Run(sub, func(t *testing.T) {
			d := newDirs(t)
			d.ws = filepath.Join(d.ws, sub)
			require.NoError(t, os.MkdirAll(d.ws, 0o755))
			for _, tool := range []string{"Edit", "Write"} {
				target := at(filepath.Join(d.ws, "pre-commit"))
				config := Source{Name: "config", Allow: []string{tool}, Ask: []string{tool, tool + "(!pre-commit)"}}
				r := d.rules(t, config)
				eff, text, src := r.decide(tool, target)
				assert.Equal(t, effectAsk, eff)
				assert.Equal(t, tool+"(.git)", text)
				assert.Equal(t, "built-in", src)

				config.Ask = []string{tool}
				eff, _, src = d.rules(t, config).decide(tool, target)
				assert.Equal(t, effectAsk, eff)
				assert.Equal(t, "built-in", src)

				deny := Source{Name: "deny", Deny: []string{tool + "(pre-commit)"}}
				eff, text, src = d.rules(t, config, deny).decide(tool, target)
				assert.Equal(t, effectDeny, eff)
				assert.Equal(t, tool+"(pre-commit)", text)
				assert.Equal(t, "deny", src)
			}
		})
	}
}

func TestBuiltInAsksForEditsOfMCPFile(t *testing.T) {
	d := newDirs(t)
	r := d.rules(t, Source{Name: "config", Allow: []string{"*"}})
	ws := func(p string) string { return filepath.Join(d.ws, p) }
	for _, tool := range []string{"Edit", "Write"} {
		for _, tc := range []struct {
			name   string
			target *Target
			want   effect
		}{
			{"root", at(ws(".mcp.json")), effectAsk},
			{"nested", at(ws("sub/.mcp.json")), effectAsk},
			{"outside workspace", at("/repo/.mcp.json"), effectAsk},
			{"resolved alias", &Target{Path: ws("x"), Real: ws(".mcp.json")}, effectAsk},
			{"lexical alias", &Target{Path: ws(".mcp.json"), Real: ws("servers.json")}, effectAsk},
			{"normalized", at(d.ws + "/sub/../.mcp.json"), effectAsk},
			{"prefix", at(ws("not.mcp.json")), effectAllow},
			{"suffix", at(ws(".mcp.json.bak")), effectAllow},
			{"no target", nil, effectAllow},
		} {
			t.Run(tool+"/"+tc.name, func(t *testing.T) {
				eff, text, src := r.decide(tool, tc.target)
				assert.Equal(t, tc.want, eff)
				if tc.want == effectAsk {
					assert.Equal(t, tool+"(.mcp.json)", text)
					assert.Equal(t, "built-in", src)
				}
			})
		}
		for _, name := range []string{".MCP.JSON", ".mcp.jſon"} {
			target := at(ws(name))
			r.fold = false
			eff, _, _ := r.decide(tool, target)
			assert.Equal(t, effectAllow, eff, name)
			r.fold = true
			eff, _, _ = r.decide(tool, target)
			assert.Equal(t, effectAsk, eff, name)
			r.fold = false
		}
	}
	eff, _, _ := r.decide("Read", at(ws(".mcp.json")))
	assert.Equal(t, effectAllow, eff)
}

func TestBuiltInAsksForEditsOfLinkedMCPFile(t *testing.T) {
	d := newDirs(t)
	require.NoError(t, os.WriteFile(filepath.Join(d.ws, "servers.json"), []byte("{}"), 0o600))
	require.NoError(t, os.Symlink("servers.json", filepath.Join(d.ws, ".mcp.json")))
	r := d.rules(t, Source{Name: "config", Allow: []string{"*"}})
	resolved, err := filepath.EvalSymlinks(d.ws)
	require.NoError(t, err)
	rws := func(p string) string { return filepath.Join(resolved, p) }
	for _, tool := range []string{"Edit", "Write"} {
		for _, tc := range []struct {
			name   string
			target *Target
			want   effect
		}{
			{"target", at(rws("servers.json")), effectAsk},
			{"resolved alias", &Target{Path: filepath.Join(d.ws, "x"), Real: rws("servers.json")}, effectAsk},
			{"other", at(rws("other.json")), effectAllow},
		} {
			t.Run(tool+"/"+tc.name, func(t *testing.T) {
				eff, text, src := r.decide(tool, tc.target)
				assert.Equal(t, tc.want, eff)
				if tc.want == effectAsk {
					assert.Equal(t, tool+"(.mcp.json)", text)
					assert.Equal(t, "built-in", src)
				}
			})
		}
		target := at(rws("SERVERS.JSON"))
		r.fold = false
		eff, _, _ := r.decide(tool, target)
		assert.Equal(t, effectAllow, eff)
		r.fold = true
		eff, _, _ = r.decide(tool, target)
		assert.Equal(t, effectAsk, eff)
		r.fold = false
	}
}

func TestBuiltInMCPFileAskPrecedence(t *testing.T) {
	d := newDirs(t)
	target := at(filepath.Join(d.ws, ".mcp.json"))
	for _, tool := range []string{"Edit", "Write"} {
		config := Source{Name: "config", Allow: []string{tool}, Ask: []string{tool, tool + "(!.mcp.json)"}}
		eff, text, src := d.rules(t, config).decide(tool, target)
		assert.Equal(t, effectAsk, eff)
		assert.Equal(t, tool+"(.mcp.json)", text)
		assert.Equal(t, "built-in", src)

		deny := Source{Name: "deny", Deny: []string{tool + "(.mcp.json)"}}
		eff, text, src = d.rules(t, config, deny).decide(tool, target)
		assert.Equal(t, effectDeny, eff)
		assert.Equal(t, tool+"(.mcp.json)", text)
		assert.Equal(t, "deny", src)
	}
}

// linkedDir returns a link to a new directory and the directory's resolved path.
func linkedDir(t *testing.T) (link, resolved string) {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	link = filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.Symlink(dir, link))
	return link, resolved
}

func TestAbsoluteSpecUnderSymlinkedParent(t *testing.T) {
	link, resolved := linkedDir(t)
	d := dirs{ws: link, home: t.TempDir()}
	rule := "Read(" + filepath.ToSlash(filepath.Join(link, "secret")) + ")"

	deny := d.rules(t, Source{Name: "config", Deny: []string{rule}})
	for _, p := range []string{filepath.Join(link, "secret"), filepath.Join(resolved, "secret")} {
		eff, _, _ := deny.decide("Read", at(p))
		assert.Equal(t, effectDeny, eff, p)
	}

	allow := d.rules(t, Source{Name: "config", Allow: []string{rule}})
	eff, _, _ := allow.decide("Read", &Target{Path: filepath.Join(link, "secret"), Real: filepath.Join(resolved, "secret")})
	assert.Equal(t, effectAllow, eff)
	eff, _, _ = allow.decide("Read", &Target{Path: filepath.Join(link, "secret"), Real: filepath.Join(resolved, "other")})
	assert.Equal(t, effectNone, eff)
}

func TestRelativeSpecWithSymlinkedLeadingDirectory(t *testing.T) {
	d := newDirs(t)
	ws, err := filepath.EvalSymlinks(d.ws)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(d.ws, "realdocs"), 0o755))
	require.NoError(t, os.Symlink("realdocs", filepath.Join(d.ws, "docs")))

	deny := d.rules(t, Source{Name: "config", Deny: []string{"Read(docs/secret)"}})
	eff, _, _ := deny.decide("Read", &Target{Path: filepath.Join(d.ws, "realdocs/secret"), Real: filepath.Join(ws, "realdocs/secret")})
	assert.Equal(t, effectDeny, eff)

	allow := d.rules(t, Source{Name: "config", Allow: []string{"Edit(docs/**)"}})
	eff, _, _ = allow.decide("Edit", &Target{Path: filepath.Join(d.ws, "docs/a.md"), Real: filepath.Join(ws, "realdocs/a.md")})
	assert.Equal(t, effectAllow, eff)
	eff, _, _ = allow.decide("Edit", &Target{Path: filepath.Join(d.ws, "docs/a.md"), Real: filepath.Join(ws, "other/a.md")})
	assert.Equal(t, effectNone, eff)
}

func TestSymlinkBeforeParentComponent(t *testing.T) {
	for _, form := range []string{"absolute", "relative", "home"} {
		for _, leaf := range []string{"secret", "missing", "missing/", "missing//", "missing//child///", "secret/*.txt", "missing/*.txt"} {
			t.Run(form+"/"+leaf, func(t *testing.T) {
				d := newDirs(t)
				base := d.ws
				if form == "home" {
					base = d.home
				}
				require.NoError(t, os.MkdirAll(filepath.Join(base, "private/sub"), 0o755))
				require.NoError(t, os.Mkdir(filepath.Join(base, "private/secret"), 0o755))
				require.NoError(t, os.Symlink("private/sub", filepath.Join(base, "link")))
				spec := "./link/../" + leaf
				switch form {
				case "absolute":
					spec = escape(filepath.ToSlash(base)) + "/link/../" + leaf
				case "home":
					spec = "~/link/../" + leaf
				}
				name := leaf
				if leaf == "secret/*.txt" {
					name = "secret/file.txt"
				}
				if leaf == "missing/*.txt" {
					name = "missing/file.txt"
				}
				realBase, err := filepath.EvalSymlinks(base)
				require.NoError(t, err)
				intended := at(filepath.Join(realBase, "private", name))
				unintended := at(filepath.Join(realBase, name))
				text := "Read(" + spec + ")"
				for _, tc := range []struct {
					source Source
					want   effect
				}{
					{Source{Name: "deny", Deny: []string{text}}, effectDeny},
					{Source{Name: "allow", Allow: []string{text}}, effectAllow},
				} {
					r := d.rules(t, tc.source)
					eff, _, _ := r.decide("Read", intended)
					assert.Equal(t, tc.want, eff, tc.source.Name+" intended")
					eff, _, _ = r.decide("Read", unintended)
					assert.Equal(t, effectNone, eff, tc.source.Name+" unintended")
					eff, _, _ = r.decide("Read", &Target{Path: unintended.Path, Real: intended.Real})
					want := effectNone
					if tc.want == effectDeny {
						want = effectDeny
					}
					assert.Equal(t, want, eff, tc.source.Name+" mixed target")
				}
			})
		}
	}
}

func TestMissingLeafUnderSymlinkWithTrailingSlash(t *testing.T) {
	for _, form := range []string{"absolute", "relative", "home"} {
		for _, leaf := range []string{"missing/", "missing//", "missing//child///"} {
			t.Run(form+"/"+leaf, func(t *testing.T) {
				d := newDirs(t)
				base := d.ws
				if form == "home" {
					base = d.home
				}
				require.NoError(t, os.Mkdir(filepath.Join(base, "realdocs"), 0o755))
				require.NoError(t, os.Symlink("realdocs", filepath.Join(base, "docs")))
				spec := "./docs/" + leaf
				switch form {
				case "absolute":
					spec = escape(filepath.ToSlash(base)) + "/docs/" + leaf
				case "home":
					spec = "~/docs/" + leaf
				}
				realBase, err := filepath.EvalSymlinks(base)
				require.NoError(t, err)
				lexical := filepath.Join(base, "docs", leaf)
				resolved := filepath.Join(realBase, "realdocs", leaf)
				other := filepath.Join(realBase, "other", leaf)
				text := "Read(" + spec + ")"
				for _, tc := range []struct {
					source Source
					want   effect
				}{
					{Source{Name: "deny", Deny: []string{text}}, effectDeny},
					{Source{Name: "allow", Allow: []string{text}}, effectAllow},
				} {
					r := d.rules(t, tc.source)
					for _, target := range []*Target{at(lexical), at(resolved), {Path: lexical, Real: resolved}} {
						eff, _, _ := r.decide("Read", target)
						assert.Equal(t, tc.want, eff, tc.source.Name+" intended")
					}
					eff, _, _ := r.decide("Read", at(other))
					assert.Equal(t, effectNone, eff, tc.source.Name+" unintended")
					eff, _, _ = r.decide("Read", &Target{Path: lexical, Real: other})
					want := effectNone
					if tc.want == effectDeny {
						want = effectDeny
					}
					assert.Equal(t, want, eff, tc.source.Name+" mixed target")
				}
			})
		}
	}
}

func TestWorkspaceWithGlobMetacharacters(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "p[1]{x}*")
	require.NoError(t, os.Mkdir(ws, 0o755))
	resolved, err := filepath.EvalSymlinks(ws)
	require.NoError(t, err)
	r := dirs{ws: ws, home: t.TempDir()}.rules(t, Source{Name: "config", Deny: []string{"Read(docs/*.md)"}})
	for _, p := range []string{filepath.Join(ws, "docs/a.md"), filepath.Join(resolved, "docs/a.md")} {
		eff, _, _ := r.decide("Read", at(p))
		assert.Equal(t, effectDeny, eff, p)
	}
	eff, _, _ := r.decide("Read", at(filepath.Join(filepath.Dir(ws), "p1x/docs/a.md")))
	assert.Equal(t, effectNone, eff)
}

func TestSplitLiteral(t *testing.T) {
	for _, tc := range []struct{ pat, dir, tail string }{
		{"/a/b", "/a/b", ""},
		{"/a/b/*.go", "/a/b", "/*.go"},
		{`/a/b\*c/d\\e/**`, `/a/b*c/d\e`, "/**"},
		{`/a/x\[1\]{y,z}`, "/a", `/x\[1\]{y,z}`},
		{"/**", "/", "/**"},
	} {
		dir, tail := splitLiteral(tc.pat)
		assert.Equal(t, tc.dir, dir, tc.pat)
		assert.Equal(t, tc.tail, tail, tc.pat)
	}
}

func TestRemovesOnNilRules(t *testing.T) {
	var r *Rules
	assert.False(t, r.Removes("Read"))
}

func TestExceptionWithTrailingSlashAppliesAtAnyDepth(t *testing.T) {
	d := newDirs(t)
	r := d.rules(t, Source{Name: "config", Deny: []string{"Read(*.key)", "Read(!fixtures/)"}})
	eff, _, _ := r.decide("Read", at(filepath.Join(d.ws, "a/fixtures/test.key")))
	assert.Equal(t, effectNone, eff)
	eff, _, _ = r.decide("Read", at(filepath.Join(d.ws, "a/real.key")))
	assert.Equal(t, effectDeny, eff)
}

func TestRemovesKeepsToolWithExceptionInSameList(t *testing.T) {
	d := newDirs(t)
	assert.False(t, d.rules(t, Source{Name: "config", Deny: []string{"Edit", "Edit(!docs/**)"}}).Removes("Edit"))

	r := d.rules(t, Source{Name: "config", Deny: []string{"*", "Read(!docs/**)"}})
	assert.False(t, r.Removes("Read"))
	assert.True(t, r.Removes("Edit"))
	assert.True(t, r.Removes("Write"))

	assert.False(t, d.rules(t, Source{Name: "config", Deny: []string{"Edit", "*(!docs/**)"}}).Removes("Edit"))
	other := d.rules(t, Source{Name: "config", Deny: []string{"Edit"}}, Source{Name: "flags", Deny: []string{"Edit(!docs/**)"}})
	assert.True(t, other.Removes("Edit"))
}

func TestHidesUsesPathRulesOfTheToolOrStar(t *testing.T) {
	d := newDirs(t)
	root := *at(d.ws)
	key, env := *at(filepath.Join(d.ws, "secrets/k")), *at(filepath.Join(d.ws, ".env"))
	r := d.rules(t, Source{Name: "config", Deny: []string{"Read(secrets/**)", "*(.env*)"}})
	assert.True(t, r.Hides("Read", root, key))
	assert.True(t, r.Hides("Read", root, env))
	assert.False(t, r.Hides("Read", root, *at(filepath.Join(d.ws, "main.go"))))

	assert.False(t, d.rules(t, Source{Name: "config", Deny: []string{"Edit(secrets/**)"}}).Hides("Read", root, key))
	assert.True(t, d.rules(t, Source{Name: "config", Ask: []string{"Read(secrets/**)"}}).Hides("Read", root, key))
	assert.False(t, d.rules(t, Source{Name: "config", Allow: []string{"Read(secrets/**)"}}).Hides("Read", root, key))
}

func TestHidesIgnoresBareRules(t *testing.T) {
	d := newDirs(t)
	file := *at(filepath.Join(d.ws, "a.go"))
	for _, ask := range []string{"*", "Read"} {
		assert.False(t, d.rules(t, Source{Name: "config", Ask: []string{ask}}).Hides("Read", *at(d.ws), file), ask)
	}
}

func TestHidesHonoursExceptions(t *testing.T) {
	d := newDirs(t)
	r := d.rules(t, Source{Name: "config", Deny: []string{"Read(secrets/**)", "Read(!secrets/public/**)"}})
	assert.False(t, r.Hides("Read", *at(d.ws), *at(filepath.Join(d.ws, "secrets/public/a"))))
	assert.True(t, r.Hides("Read", *at(d.ws), *at(filepath.Join(d.ws, "secrets/b"))))
}

func TestHidesExceptionsRequireBothPaths(t *testing.T) {
	d := newDirs(t)
	root := *at(d.ws)
	public := filepath.Join(d.ws, "secrets/public/a")
	private := filepath.Join(d.ws, "secrets/private/a")
	rules := []string{"Read(secrets/**)", "Read(!secrets/public/**)"}
	for _, source := range []Source{
		{Name: "deny", Deny: rules},
		{Name: "ask", Ask: rules},
	} {
		t.Run(source.Name, func(t *testing.T) {
			r := d.rules(t, source)
			for _, tc := range []struct {
				name   string
				target Target
				want   bool
			}{
				{"lexical exception only", Target{Path: public, Real: private}, true},
				{"resolved exception only", Target{Path: private, Real: public}, true},
				{"both paths excepted", *at(public), false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					assert.Equal(t, tc.want, r.Hides("Read", root, tc.target))
				})
			}
		})
	}
}

func TestHidesExceptionDoesNotReachAnotherSource(t *testing.T) {
	d := newDirs(t)
	root := *at(d.ws)
	public := *at(filepath.Join(d.ws, "secrets/public/a"))
	rules := []string{"Read(secrets/**)", "Read(!secrets/public/**)"}
	for _, excepted := range []Source{
		{Name: "deny exception", Deny: rules},
		{Name: "ask exception", Ask: rules},
	} {
		t.Run(excepted.Name, func(t *testing.T) {
			assert.False(t, d.rules(t, excepted).Hides("Read", root, public))
			for _, other := range []Source{
				{Name: "other deny", Deny: []string{"Read(secrets/**)"}},
				{Name: "other ask", Ask: []string{"Read(secrets/**)"}},
			} {
				t.Run(other.Name, func(t *testing.T) {
					assert.True(t, d.rules(t, excepted, other).Hides("Read", root, public))
					assert.True(t, d.rules(t, other, excepted).Hides("Read", root, public))
				})
			}
		})
	}
}

func TestHidesSkipsRuleThatMatchedTheRoot(t *testing.T) {
	d := newDirs(t)
	src := *at(filepath.Join(d.ws, "src"))
	r := d.rules(t, Source{Name: "config", Ask: []string{"Read(src/**)"}})
	file := *at(filepath.Join(d.ws, "src/a.go"))
	assert.False(t, r.Hides("Read", src, file))
	assert.True(t, r.Hides("Read", *at(d.ws), file))

	r = d.rules(t, Source{Name: "config", Ask: []string{"Read(src)", "*(.env*)"}})
	assert.False(t, r.Hides("Read", src, file))
	assert.True(t, r.Hides("Read", src, *at(filepath.Join(d.ws, "src/.env"))))
}

func TestHidesAppliesRuleMatchingRootWhenExceptionAdmittedIt(t *testing.T) {
	d := newDirs(t)
	pub := *at(filepath.Join(d.ws, "src/pub"))
	r := d.rules(t, Source{Name: "config", Deny: []string{"Read(src/**)", "Read(!src/pub)"}})
	link := Target{Path: filepath.Join(d.ws, "src/pub/l"), Real: filepath.Join(d.ws, "src/private")}
	assert.True(t, r.Hides("Read", pub, link))
}

func TestHidesAcrossSources(t *testing.T) {
	d := newDirs(t)
	src := *at(filepath.Join(d.ws, "src"))
	env := *at(filepath.Join(d.ws, "src/.env"))
	approved := Source{Name: "config", Ask: []string{"Read(src)"}}
	for _, other := range []Source{
		{Name: "flags", Deny: []string{"*(.env*)"}},
		{Name: "flags", Ask: []string{"*(.env*)"}},
	} {
		r := d.rules(t, approved, other)
		assert.True(t, r.Hides("Read", src, env))
		assert.False(t, r.Hides("Read", src, *at(filepath.Join(d.ws, "src/a.go"))))
	}
}

func TestHidesWithNilRules(t *testing.T) {
	var r *Rules
	assert.False(t, r.Hides("Read", Target{}, Target{}))
}

// addedDir is a new resolved directory, as SetDirs takes it.
func addedDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

func TestSetDirsExtendsRelativeAndBareRulesOnly(t *testing.T) {
	d := newDirs(t)
	extra := addedDir(t)
	r := d.rules(t,
		Source{Name: "config", Deny: []string{"Read(.env)", "Read(/etc/passwd)", "Read(~/.ssh)"}, Allow: []string{"Edit(docs/**)"}})
	deny := []string{filepath.Join(extra, ".env"), filepath.Join(extra, "a/.env")}
	allow := filepath.Join(extra, "docs/a.md")

	for _, p := range deny {
		eff, _, _ := r.decide("Read", at(p))
		assert.Equal(t, effectNone, eff, p)
	}
	eff, _, _ := r.decide("Edit", at(allow))
	assert.Equal(t, effectNone, eff)

	r.SetDirs([]string{extra})

	for _, p := range deny {
		eff, _, _ := r.decide("Read", at(p))
		assert.Equal(t, effectDeny, eff, p)
	}
	eff, _, _ = r.decide("Edit", at(allow))
	assert.Equal(t, effectAllow, eff)
	eff, _, _ = r.decide("Edit", at(filepath.Join(extra, "src/a.md")))
	assert.Equal(t, effectNone, eff)
	for _, p := range []string{filepath.Join(extra, "etc/passwd"), filepath.Join(extra, ".ssh/id_rsa")} {
		eff, _, _ := r.decide("Read", at(p))
		assert.Equal(t, effectNone, eff, p)
	}
	eff, _, _ = r.decide("Read", at("/etc/passwd"))
	assert.Equal(t, effectDeny, eff)
	eff, _, _ = r.decide("Read", at(filepath.Join(d.home, ".ssh/id_rsa")))
	assert.Equal(t, effectDeny, eff)
}

func TestSetDirsAppliesRulesThroughASymlinkedSpelling(t *testing.T) {
	d := newDirs(t)
	real := filepath.Join(addedDir(t), "proj")
	require.NoError(t, os.Mkdir(real, 0o755))
	link := filepath.Join(addedDir(t), "link")
	require.NoError(t, os.Symlink(real, link))
	r := d.rules(t, Source{Name: "config", Allow: []string{"Edit(src/**)"}})

	viaLink := &Target{Path: filepath.Join(link, "src/a.go"), Real: filepath.Join(real, "src/a.go")}
	viaReal := at(filepath.Join(real, "src/a.go"))
	r.SetDirs([]string{link})
	for _, tg := range []*Target{viaLink, viaReal} {
		eff, _, _ := r.decide("Edit", tg)
		assert.Equal(t, effectAllow, eff, tg.Path)
	}
	r.SetDirs([]string{real})
	eff, _, _ := r.decide("Edit", viaReal)
	assert.Equal(t, effectAllow, eff)
	r.SetDirs([]string{link, real})
	for _, tg := range []*Target{viaLink, viaReal, at(filepath.Join(link, "src/a.go"))} {
		eff, _, _ := r.decide("Edit", tg)
		assert.Equal(t, effectAllow, eff, tg.Path)
	}
	eff, _, _ = r.decide("Edit", at(filepath.Join(real, "other/a.go")))
	assert.Equal(t, effectNone, eff)
}

func TestSetDirsResolvesSymlinkBeforeParent(t *testing.T) {
	d := newDirs(t)
	base := addedDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(base, "actual/sub"), 0o755))
	require.NoError(t, os.Symlink("actual/sub", filepath.Join(base, "link")))
	require.NoError(t, os.WriteFile(filepath.Join(base, "actual/servers.json"), []byte("{}"), 0o600))
	require.NoError(t, os.Symlink("servers.json", filepath.Join(base, "actual", ProjectMCPFile)))
	given := base + "/link/.."
	r := d.rules(t, Source{Name: "config", Deny: []string{"Read(private/**)"}, Allow: []string{"Write"}})
	r.SetDirs([]string{given})

	for _, tc := range []struct {
		tool, path string
		want       effect
	}{
		{"Read", filepath.Join(base, "actual/private/file"), effectDeny},
		{"Read", filepath.Join(base, "private/file"), effectNone},
		{"Write", filepath.Join(base, "actual/servers.json"), effectAsk},
		{"Write", filepath.Join(base, "servers.json"), effectAllow},
	} {
		eff, _, _ := r.decide(tc.tool, at(tc.path))
		assert.Equal(t, tc.want, eff, tc.path)
	}
}

func TestSetDirsKeepsExceptionsInAddedDirectory(t *testing.T) {
	d := newDirs(t)
	extra := addedDir(t)
	r := d.rules(t, Source{Name: "config", Deny: []string{"Read(.env*)", "Read(!.env.example)"}}, Source{Name: "other", Ask: []string{"Read(.env.example)"}})
	r.SetDirs([]string{extra})

	eff, _, _ := r.decide("Read", at(filepath.Join(extra, ".env")))
	assert.Equal(t, effectDeny, eff)
	eff, _, src := r.decide("Read", at(filepath.Join(extra, ".env.example")))
	assert.Equal(t, effectAsk, eff)
	assert.Equal(t, "other", src)
	assert.True(t, r.Hides("Read", *at(extra), *at(filepath.Join(extra, ".env"))))
}

func TestSetDirsReplacesWhatAnEarlierCallSet(t *testing.T) {
	d := newDirs(t)
	first, second := addedDir(t), addedDir(t)
	r := d.rules(t, Source{Name: "config", Deny: []string{"Read(.env)"}})
	inFirst, inSecond := at(filepath.Join(first, ".env")), at(filepath.Join(second, ".env"))

	r.SetDirs([]string{first})
	eff, _, _ := r.decide("Read", inFirst)
	assert.Equal(t, effectDeny, eff)

	r.SetDirs([]string{second})
	eff, _, _ = r.decide("Read", inFirst)
	assert.Equal(t, effectNone, eff)
	eff, _, _ = r.decide("Read", inSecond)
	assert.Equal(t, effectDeny, eff)

	r.SetDirs([]string{first, second})
	eff, _, _ = r.decide("Read", inFirst)
	assert.Equal(t, effectDeny, eff)

	r.SetDirs(nil)
	for _, p := range []*Target{inFirst, inSecond} {
		eff, _, _ := r.decide("Read", p)
		assert.Equal(t, effectNone, eff)
	}
	eff, _, _ = r.decide("Read", at(filepath.Join(d.ws, ".env")))
	assert.Equal(t, effectDeny, eff)
}

func TestSetDirsPreservesResolutionsOnAdditionAndRollback(t *testing.T) {
	d := newDirs(t)
	first, second := addedDir(t), addedDir(t)
	for _, name := range []string{"old", "new"} {
		require.NoError(t, os.Mkdir(filepath.Join(first, name), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(first, name+".json"), []byte("{}"), 0o600))
	}
	private := filepath.Join(first, "private")
	mcp := filepath.Join(first, ProjectMCPFile)
	require.NoError(t, os.Symlink("old", private))
	require.NoError(t, os.Symlink("old.json", mcp))
	r := d.rules(t, Source{Name: "config", Deny: []string{"Read(private/**)"}, Ask: []string{"Write(private/**)"}, Allow: []string{"Edit(private/**)", "Write"}})
	r.SetDirs([]string{first})
	require.NoError(t, os.Remove(private))
	require.NoError(t, os.Symlink("new", private))
	require.NoError(t, os.Remove(mcp))
	require.NoError(t, os.Symlink("new.json", mcp))

	for _, roots := range [][]string{{first, second}, {first}} {
		r.SetDirs(roots)
		for _, tc := range []struct {
			tool string
			path string
			want effect
		}{
			{"Read", filepath.Join(first, "old/file"), effectDeny},
			{"Read", filepath.Join(first, "new/file"), effectNone},
			{"Write", filepath.Join(first, "old/file"), effectAsk},
			{"Edit", filepath.Join(first, "old/file"), effectAllow},
			{"Edit", filepath.Join(first, "new/file"), effectNone},
			{"Write", filepath.Join(first, "old.json"), effectAsk},
			{"Write", filepath.Join(first, "new.json"), effectAllow},
		} {
			eff, _, _ := r.decide(tc.tool, at(tc.path))
			assert.Equal(t, tc.want, eff, "%s %s with %v", tc.tool, tc.path, roots)
		}
	}
}

func TestSetDirsGuardsAddedDirectoryMCPFile(t *testing.T) {
	d := newDirs(t)
	extra := addedDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(extra, "servers.json"), []byte("{}"), 0o600))
	require.NoError(t, os.Symlink("servers.json", filepath.Join(extra, ".mcp.json")))
	require.NoError(t, os.WriteFile(filepath.Join(d.ws, "ws.json"), []byte("{}"), 0o600))
	require.NoError(t, os.Symlink("ws.json", filepath.Join(d.ws, ".mcp.json")))
	r := d.rules(t, Source{Name: "config", Allow: []string{"*"}})
	r.SetDirs([]string{extra})

	for _, tool := range []string{"Edit", "Write"} {
		eff, _, _ := r.decide(tool, at(Resolve(filepath.Join(d.ws, "ws.json"))))
		assert.Equal(t, effectAsk, eff, tool+" workspace target")
		for _, name := range []string{".mcp.json", "servers.json"} {
			eff, text, src := r.decide(tool, at(filepath.Join(extra, name)))
			assert.Equal(t, effectAsk, eff, tool+" "+name)
			assert.Equal(t, tool+"("+ProjectMCPFile+")", text)
			assert.Equal(t, "built-in", src)
		}
		eff, _, _ = r.decide(tool, at(filepath.Join(extra, "other.json")))
		assert.Equal(t, effectAllow, eff)
	}

	r.SetDirs(nil)
	eff, _, _ := r.decide("Edit", at(filepath.Join(extra, "servers.json")))
	assert.Equal(t, effectAllow, eff)
}

func TestSetDirsMatchesGlobMetacharactersLiterally(t *testing.T) {
	d := newDirs(t)
	extra := filepath.Join(addedDir(t), "p[1]*")
	require.NoError(t, os.Mkdir(extra, 0o755))
	r := d.rules(t, Source{Name: "config", Deny: []string{"Read(docs/*.md)"}})
	r.SetDirs([]string{extra})

	eff, _, _ := r.decide("Read", at(filepath.Join(extra, "docs/a.md")))
	assert.Equal(t, effectDeny, eff)
	eff, _, _ = r.decide("Read", at(filepath.Join(filepath.Dir(extra), "p1/docs/a.md")))
	assert.Equal(t, effectNone, eff)
}

func TestSetDirsOnNilRules(t *testing.T) {
	var r *Rules
	assert.NotPanics(t, func() { r.SetDirs([]string{"/x"}) })
}

func TestSetDirsWhileReading(t *testing.T) {
	d := newDirs(t)
	extra := addedDir(t)
	r := d.rules(t, Source{Name: "config", Deny: []string{"Read(.env)"}})
	target := at(filepath.Join(extra, ".env"))
	started, done := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(done)
		close(started)
		for range 100 {
			r.decide("Read", target)
			r.Hides("Read", *at(extra), *target)
			r.Removes("Read")
		}
	}()
	<-started
	for range 100 {
		r.SetDirs([]string{extra})
		r.SetDirs(nil)
	}
	<-done
}
