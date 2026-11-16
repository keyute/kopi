package sandbox

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRejectsRelativePath(t *testing.T) {
	for _, p := range []Policy{{Write: []string{"rel"}}, {DenyRead: []string{"a/b"}}, {DenyWrite: []string{""}}} {
		_, err := New(t.Context(), p)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrUnavailable)
		assert.Contains(t, err.Error(), "is not absolute")
	}
}

func TestNewUnavailableOffDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin has a sandbox")
	}
	_, err := New(t.Context(), Policy{Write: []string{t.TempDir()}})
	require.ErrorIs(t, err, ErrUnavailable)
	assert.Contains(t, err.Error(), runtime.GOOS)
}

func TestWrapDoesNotAliasPrefix(t *testing.T) {
	s := &Sandbox{prefix: seatbeltPrefix(Policy{Write: []string{t.TempDir()}})}
	want := append([]string(nil), s.prefix...)

	a := s.Wrap([]string{"a"})
	b := s.Wrap([]string{"b"})
	assert.Equal(t, append(append([]string(nil), want...), "a"), a)
	assert.Equal(t, append(append([]string(nil), want...), "b"), b)

	a[0] = "x"
	empty := s.Wrap(nil)
	empty[0] = "z"
	assert.Equal(t, want, s.prefix)
	assert.Equal(t, want[0], b[0])
}
