package pigruntime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallationIDIsCreatedOnceAndKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installation_id")

	id, err := installationID(path)
	require.NoError(t, err)
	_, err = uuid.Parse(id)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	again, err := installationID(path)
	require.NoError(t, err)
	assert.Equal(t, id, again)
}

func TestInstallationIDConcurrentFirstUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "installation_id")
	const callers = 32
	type result struct {
		id  string
		err error
	}
	ready := make(chan struct{}, callers)
	start := make(chan struct{})
	results := make(chan result, callers)
	for range callers {
		go func() {
			ready <- struct{}{}
			<-start
			id, err := installationID(path)
			results <- result{id: id, err: err}
		}()
	}
	for range callers {
		<-ready
	}
	close(start)

	var id string
	for range callers {
		result := <-results
		require.NoError(t, result.err)
		parsed, err := uuid.Parse(result.id)
		require.NoError(t, err)
		assert.Equal(t, parsed.String(), result.id)
		if id == "" {
			id = result.id
		}
		assert.Equal(t, id, result.id)
	}
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, id, string(content))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "temporary files are removed")
	assert.Equal(t, "installation_id", entries[0].Name())
}

func TestInstallationIDIsReturnedHyphenated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installation_id")
	require.NoError(t, os.WriteFile(path, []byte("urn:uuid:6BA7B810-9DAD-11D1-80B4-00C04FD430C8\n"), 0o600))

	id, err := installationID(path)

	require.NoError(t, err)
	assert.Equal(t, "6ba7b810-9dad-11d1-80b4-00c04fd430c8", id)
}

func TestInstallationIDRejectsInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installation_id")
	require.NoError(t, os.WriteFile(path, []byte("not-a-uuid"), 0o600))

	_, err := installationID(path)

	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "not-a-uuid", string(content), "an invalid file is reported, not regenerated")
}

func TestInstallationIDReadsExistingFromReadOnlyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	require.NoError(t, os.Mkdir(dir, 0o700))
	path := filepath.Join(dir, "installation_id")
	want := uuid.NewString()
	require.NoError(t, os.WriteFile(path, []byte(want), 0o600))
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	id, err := installationID(path)

	require.NoError(t, err)
	assert.Equal(t, want, id)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temporary file is created")
	assert.Equal(t, "installation_id", entries[0].Name())
}
