package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSandboxListenIsHiddenAndTakesTheCommandAfterDashes(t *testing.T) {
	root := newRoot()
	found, _, err := root.Find([]string{"sandbox-listen"})
	require.NoError(t, err)
	require.Equal(t, "sandbox-listen", found.Name())
	assert.True(t, found.Hidden)

	// RunE would act on this process's fd 5 and port 3128.
	var got []string
	found.RunE = func(_ *cobra.Command, args []string) error {
		got = args
		return nil
	}
	root.SetArgs([]string{"sandbox-listen", "--", "bash", "-c", "echo $0", "--flag"})
	require.NoError(t, root.Execute())
	assert.Equal(t, []string{"bash", "-c", "echo $0", "--flag"}, got)
}

func TestSandboxListenNeedsACommand(t *testing.T) {
	root := newRoot()
	found, _, err := root.Find([]string{"sandbox-listen"})
	require.NoError(t, err)
	found.RunE = func(*cobra.Command, []string) error { t.Fatal("ran"); return nil }
	root.SetArgs([]string{"sandbox-listen", "--"})
	assert.Error(t, root.Execute())
}
