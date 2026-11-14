package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ws = "/work/a"

func line(t *testing.T, workspace, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"workspace": workspace, "text": text, "time": "2026-01-02T03:04:05Z"})
	require.NoError(t, err)
	return string(b) + "\n"
}

func writeFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600))
}

func TestOpenMissingFileIsEmpty(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "history.jsonl"), ws, 10)
	require.NoError(t, err)
	assert.Empty(t, s.Texts())
}

func TestAddReopenRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home", "history.jsonl")
	s, err := Open(path, ws, 10)
	require.NoError(t, err)
	for _, text := range []string{"one", "two", "three"} {
		require.NoError(t, s.Add(text))
	}
	assert.Equal(t, []string{"one", "two", "three"}, s.Texts())

	reopened, err := Open(path, ws, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"one", "two", "three"}, reopened.Texts())
}

func TestOpenFiltersOtherWorkspaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	writeFile(t, path, line(t, ws, "mine"), line(t, "/work/b", "theirs"), line(t, ws, "also mine"))

	s, err := Open(path, ws, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"mine", "also mine"}, s.Texts())
}

func TestLimitKeepsNewest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	s, err := Open(path, ws, 3)
	require.NoError(t, err)
	for i := range 6 {
		require.NoError(t, s.Add(fmt.Sprint(i)))
	}
	assert.Equal(t, []string{"3", "4", "5"}, s.Texts())

	reopened, err := Open(path, ws, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"4", "5"}, reopened.Texts())
}

func TestAddDropsConsecutiveDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	s, err := Open(path, ws, 10)
	require.NoError(t, err)
	require.NoError(t, s.Add("same"))
	require.NoError(t, s.Add("same"))
	assert.Equal(t, []string{"same"}, s.Texts())

	reopened, err := Open(path, ws, 10)
	require.NoError(t, err)
	require.NoError(t, reopened.Add("same"))
	require.NoError(t, reopened.Add("other"))
	require.NoError(t, reopened.Add("same"))
	assert.Equal(t, []string{"same", "other", "same"}, reopened.Texts())

	final, err := Open(path, ws, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"same", "other", "same"}, final.Texts())
}

func TestOpenSkipsMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	writeFile(t, path, line(t, ws, "before"), "{not json\n", line(t, ws, "after"))

	s, err := Open(path, ws, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"before", "after"}, s.Texts())
}

func TestLongLineRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	long := strings.Repeat("x", 200*1024)
	s, err := Open(path, ws, 10)
	require.NoError(t, err)
	require.NoError(t, s.Add(long))
	require.NoError(t, s.Add("short"))

	reopened, err := Open(path, ws, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{long, "short"}, reopened.Texts())
}

func TestAddCreatesPrivateFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	path := filepath.Join(dir, "history.jsonl")
	s, err := Open(path, ws, 10)
	require.NoError(t, err)
	require.NoError(t, s.Add("secret"))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	info, err = os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestAddWriteFailureKeepsEntry(t *testing.T) {
	path := t.TempDir()
	s, err := Open(path, ws, 10)
	require.Error(t, err)
	require.Error(t, s.Add("kept"))
	assert.Equal(t, []string{"kept"}, s.Texts())
}
