package trust

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tempDir returns a canonical temporary directory; on macOS t.TempDir sits
// behind the /var -> /private/var symlink.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

func mkdir(t *testing.T, parts ...string) string {
	t.Helper()
	dir := filepath.Join(parts...)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	return dir
}

func TestOpenMissingFileIsEmpty(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "absent", "trust.json"))
	require.NoError(t, err)
	_, ok, err := s.Lookup(t.TempDir())
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestSetThenLookup(t *testing.T) {
	root := tempDir(t)
	s, err := Open(filepath.Join(root, "home", "trust.json"))
	require.NoError(t, err)
	project := mkdir(t, root, "project")

	require.NoError(t, s.Set(project, Untrusted))
	d, ok, err := s.Lookup(project)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, Untrusted, d)

	require.NoError(t, s.Set(project, Trusted))
	d, ok, err = s.Lookup(project)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, Trusted, d)
}

func TestLookupClosestDecisionApplies(t *testing.T) {
	root := tempDir(t)
	parent := mkdir(t, root, "parent")
	child := mkdir(t, parent, "child")
	inner := mkdir(t, child, "inner")
	sibling := mkdir(t, parent, "sibling")
	s, err := Open(filepath.Join(root, "trust.json"))
	require.NoError(t, err)
	require.NoError(t, s.Set(parent, Trusted))
	require.NoError(t, s.Set(child, Untrusted))

	for _, tc := range []struct {
		name string
		dir  string
		want Decision
	}{
		{"ancestor match", sibling, Trusted},
		{"closer decision wins", child, Untrusted},
		{"closer ancestor wins", inner, Untrusted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, ok, err := s.Lookup(tc.dir)
			require.NoError(t, err)
			assert.True(t, ok)
			assert.Equal(t, tc.want, d)
		})
	}

	_, ok, err := s.Lookup(root)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestSymlinkedPathResolvesToSameEntry(t *testing.T) {
	root := tempDir(t)
	project := mkdir(t, root, "project")
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(project, link))
	path := filepath.Join(root, "trust.json")
	s, err := Open(path)
	require.NoError(t, err)

	require.NoError(t, s.Set(link, Trusted))
	d, ok, err := s.Lookup(project)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, Trusted, d)

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{"version":1,"folders":{%q:"trusted"}}`, project), string(b))
}

func TestDecisionsPersistAcrossOpen(t *testing.T) {
	root := tempDir(t)
	home := filepath.Join(root, "home")
	path := filepath.Join(home, "trust.json")
	a := mkdir(t, root, "a")
	b := mkdir(t, root, "b")
	s, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s.Set(a, Trusted))
	require.NoError(t, s.Set(b, Untrusted))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	info, err = os.Stat(home)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file left behind")

	s, err = Open(path)
	require.NoError(t, err)
	for dir, want := range map[string]Decision{a: Trusted, b: Untrusted} {
		d, ok, err := s.Lookup(dir)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, want, d)
	}
}

func TestOpenRejectsInvalidFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    error
	}{
		{"bad version", `{"version":2,"folders":{}}`, ErrUnsupportedVersion},
		{"missing version", `{"folders":{}}`, ErrUnsupportedVersion},
		{"bad value", `{"version":1,"folders":{"/x":"maybe"}}`, ErrInvalidDecision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "trust.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			_, err := Open(path)
			require.ErrorIs(t, err, tc.want)
			assert.Contains(t, err.Error(), path)
		})
	}
}

func TestOpenRejectsMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,`), 0o600))
	_, err := Open(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
}

func TestSetKeepsDecisionSavedByAnotherStore(t *testing.T) {
	root := tempDir(t)
	a := mkdir(t, root, "a")
	b := mkdir(t, root, "b")
	path := filepath.Join(root, "trust.json")
	first, err := Open(path)
	require.NoError(t, err)
	second, err := Open(path)
	require.NoError(t, err)

	require.NoError(t, first.Set(a, Trusted))
	require.NoError(t, second.Set(b, Untrusted))

	d, ok, err := second.Lookup(a)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, Trusted, d)

	s, err := Open(path)
	require.NoError(t, err)
	for dir, want := range map[string]Decision{a: Trusted, b: Untrusted} {
		d, ok, err := s.Lookup(dir)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, want, d)
	}
}

func TestSetFailsWithoutWritingOnBadFile(t *testing.T) {
	root := tempDir(t)
	path := filepath.Join(root, "trust.json")
	s, err := Open(path)
	require.NoError(t, err)
	bad := `{"version":2,"folders":{}}`
	require.NoError(t, os.WriteFile(path, []byte(bad), 0o600))

	require.ErrorIs(t, s.Set(mkdir(t, root, "a"), Trusted), ErrUnsupportedVersion)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, bad, string(b))
}
