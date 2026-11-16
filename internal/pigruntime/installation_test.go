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
