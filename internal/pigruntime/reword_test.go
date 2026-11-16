package pigruntime

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRewordNamesKopi(t *testing.T) {
	for pig, kopi := range map[string]string{
		"github-copilot: not logged in (run `pig login github-copilot`)":              "github-copilot: not logged in (run `kopi login github-copilot`)",
		"Meta session expired (status 401). Run `/login meta` to sign in again.":      "Meta session expired (status 401). Run `kopi login meta` to sign in again.",
		"Port 1455 is in use, probably by an unfinished login in another pi session.": "Port 1455 is in use, probably by an unfinished login in another Kopi session.",
		"provider is not configured: openai":                                          "provider is not configured: openai",
	} {
		assert.Equal(t, kopi, reword(errors.New(pig)).Error())
	}
}

func TestRewordKeepsWrappedError(t *testing.T) {
	err := reword(fmt.Errorf("run `pig login github-copilot`: %w", ErrNoCredential))

	assert.ErrorIs(t, err, ErrNoCredential)
	assert.Equal(t, "run `kopi login github-copilot`: no credential", err.Error())
}
