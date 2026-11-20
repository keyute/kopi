package cmd

import (
	"github.com/spf13/cobra"

	"github.com/keyute/kopi/internal/sandbox"
)

func sandboxListenCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "sandbox-listen -- command [argument...]",
		Short:  "Hand Kopi the sandbox proxy listener, then run the command (run by the Linux sandbox, not by users)",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE:   func(_ *cobra.Command, args []string) error { return sandbox.ExecListening(args) },
	}
}
