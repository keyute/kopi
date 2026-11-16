package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testTools = []string{"Read", "Edit", "Write"}

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
	for _, s := range []string{"Read", "*", "Read(*.go)", "Edit(docs/**)", "Read(!.env.example)", "*(~/.ssh)", "Write(/etc/**)", "Read(a(1).txt)", "Bash"} {
		assert.NoError(t, ValidateRule(s), s)
	}
	for _, s := range []string{"", "Read(", "Read()", "Read(!)", "Read([)", "Read({a)", "Re ad", "Read(a))", "Read)", "(x)", "Read(x) "} {
		assert.ErrorIs(t, ValidateRule(s), ErrInvalidRule, s)
	}
}

func TestNewRulesRejectsUnknownToolNamingRuleAndSource(t *testing.T) {
	d := newDirs(t)
	_, err := NewRules(d.ws, d.home, testTools, Source{Name: "config", Deny: []string{"Bash"}})
	require.ErrorIs(t, err, ErrInvalidRule)
	assert.Contains(t, err.Error(), `config: rule "Bash": unknown tool "Bash"`)
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
	for _, tool := range testTools {
		eff, text, _ := r.decide(tool, env)
		assert.Equal(t, effectDeny, eff, tool)
		assert.Equal(t, "*(.env)", text)
	}
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
	for _, tool := range testTools {
		assert.True(t, all.Removes(tool), tool)
	}
	assert.False(t, d.rules(t, Source{Name: "config", Deny: []string{"*(.env)"}}).Removes("Read"))
}

func TestBuiltInAsksForEditsUnderGit(t *testing.T) {
	d := newDirs(t)
	r := d.rules(t, BuiltIn)
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
