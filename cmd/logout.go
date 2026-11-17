package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/keyute/kopi/internal/config"
)

func logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout <provider>",
		Short: "Remove a model provider's stored credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := config.ResolvePaths()
			if err != nil {
				return err
			}
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to resolve working directory: %w", err)
			}
			rt, err := openRuntime(cmd.Context(), paths, cwd)
			if err != nil {
				return err
			}
			defer rt.Close()

			provider := args[0]
			if err := rt.Logout(cmd.Context(), provider); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Logged out of %s.\n", provider)
			return err
		},
	}
}
