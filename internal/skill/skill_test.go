package skill

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		require.NoError(t, os.MkdirAll(p, 0o755))
	}
}

func TestFindWithoutGitCoversDirAlone(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b")
	mkdirs(t, filepath.Join(dir, ".agents", "skills"), filepath.Join(root, ".agents", "skills"))

	assert.Equal(t, []string{filepath.Join(dir, ".agents", "skills")}, Find(dir, ""))
}

func TestFindWalksToGitDirectoryClosestFirst(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b")
	mkdirs(t, filepath.Join(root, ".git"), filepath.Join(root, ".agents", "skills"), filepath.Join(dir, ".agents", "skills"))
	mkdirs(t, filepath.Join(filepath.Dir(root), ".agents", "skills-not"))

	assert.Equal(t, []string{
		filepath.Join(dir, ".agents", "skills"),
		filepath.Join(root, ".agents", "skills"),
	}, Find(dir, ""))
}

func TestFindStopsAtGitFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "wt", "sub")
	mkdirs(t, dir, filepath.Join(root, ".agents", "skills"), filepath.Join(root, "wt", ".agents", "skills"))
	require.NoError(t, os.WriteFile(filepath.Join(root, "wt", ".git"), []byte("gitdir: x"), 0o644))

	assert.Equal(t, []string{filepath.Join(root, "wt", ".agents", "skills")}, Find(dir, ""))
}

func TestFindReturnsOnlyExistingDirectories(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a")
	mkdirs(t, filepath.Join(root, ".git"), filepath.Join(dir, ".agents"))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".agents"), nil, 0o644))

	assert.Empty(t, Find(dir, ""))
}

func TestFindFollowsSymlinkToDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "elsewhere")
	mkdirs(t, target, filepath.Join(root, ".agents"))
	link := filepath.Join(root, ".agents", "skills")
	require.NoError(t, os.Symlink(target, link))

	assert.Equal(t, []string{link}, Find(root, ""))
}

func TestFindHonoursExclude(t *testing.T) {
	root := t.TempDir()
	skills := filepath.Join(root, ".agents", "skills")
	mkdirs(t, skills)

	assert.Empty(t, Find(root, skills))
	assert.Equal(t, []string{skills}, Find(root, filepath.Join(root, "other")))
}

func TestText(t *testing.T) {
	sk := Skill{Name: "deploy", Dir: "/s/deploy", Body: "\n\n  Do it.\nTwice.\n\n"}

	assert.Equal(t, "<skill name=\"deploy\" directory=\"/s/deploy\">\nDo it.\nTwice.\n</skill>", Text(sk))
}

func TestNewSetWithPrefix(t *testing.T) {
	set, dropped := NewSet("skill", []Skill{{Name: "b"}, {Name: "a"}}, []string{"a"})

	assert.Empty(t, dropped)
	require.Len(t, set.Skills(), 2)
	assert.Equal(t, "a", set.Skills()[0].Name)
	assert.Equal(t, "skill:a", set.Command(set.Skills()[0]))
	got, ok := set.Lookup("skill:b")
	assert.True(t, ok)
	assert.Equal(t, "b", got.Name)
	_, ok = set.Lookup("b")
	assert.False(t, ok)
}

func TestNewSetWithoutPrefixDropsReserved(t *testing.T) {
	set, dropped := NewSet("", []Skill{{Name: "z"}, {Name: "help"}, {Name: "a"}, {Name: "clear"}}, []string{"clear", "help"})

	assert.Equal(t, []string{"help", "clear"}, dropped)
	require.Len(t, set.Skills(), 2)
	assert.Equal(t, "a", set.Command(set.Skills()[0]))
	_, ok := set.Lookup("help")
	assert.False(t, ok)
	_, ok = set.Lookup("z")
	assert.True(t, ok)
}

func TestTokens(t *testing.T) {
	foo, bar := Skill{Name: "foo"}, Skill{Name: "bar"}
	set, _ := NewSet("skill", []Skill{foo, bar}, nil)

	tests := []struct {
		name string
		text string
		want []Token
	}{
		{"start", "/skill:foo go", []Token{{0, 0, 10, foo}}},
		{"mid-line", "use /skill:foo now", []Token{{0, 4, 14, foo}}},
		{"later line", "hi\nthere /skill:bar", []Token{{1, 6, 16, bar}}},
		{"after tab", "a\t/skill:foo", []Token{{0, 2, 12, foo}}},
		{"next to CR", "/skill:foo\r\n/skill:bar", []Token{{0, 0, 10, foo}, {1, 0, 10, bar}}},
		{"full-width space", "a　/skill:foo", []Token{{0, 2, 12, foo}}},
		{"wide runes count once", "日本 /skill:foo", []Token{{0, 3, 13, foo}}},
		{"trailing dot", "/skill:foo.", nil},
		{"double slash", "//skill:foo", nil},
		{"no slash", "skill:foo", nil},
		{"unknown", "/skill:nope", nil},
		{"two on a line in order", "/skill:bar /skill:foo", []Token{{0, 0, 10, bar}, {0, 11, 21, foo}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, set.Tokens(tt.text))
		})
	}
}

func TestNamedDedupesInFirstSeenOrder(t *testing.T) {
	foo, bar := Skill{Name: "foo"}, Skill{Name: "bar"}
	set, _ := NewSet("", []Skill{foo, bar}, nil)

	assert.Equal(t, []Skill{bar, foo}, set.Named("/bar /foo\n/bar /foo"))
	assert.Empty(t, set.Named("nothing"))
}

func TestNilSet(t *testing.T) {
	var set *Set

	assert.Empty(t, set.Skills())
	assert.Empty(t, set.Tokens("/x"))
	assert.Empty(t, set.Named("/x"))
	_, ok := set.Lookup("x")
	assert.False(t, ok)
	assert.Equal(t, "x", set.Command(Skill{Name: "x"}))
}
