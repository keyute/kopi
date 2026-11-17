package cmd

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/pigruntime"
)

func loginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login [provider]",
		Short: "Log in to a model provider, or list the providers that offer login",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := config.ResolvePaths()
			if err != nil {
				return err
			}
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to resolve working directory: %w", err)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			rt, err := openRuntime(ctx, paths, cwd)
			if err != nil {
				return err
			}
			defer rt.Close()

			out := cmd.OutOrStdout()
			if len(args) == 0 {
				providers, err := rt.Providers(ctx)
				if err != nil {
					return err
				}
				return listLogins(out, providers)
			}
			provider := args[0]
			if err := rt.Login(ctx, provider, cmd.InOrStdin(), out); err != nil {
				return err
			}
			_, err = fmt.Fprintf(out, "Logged in to %s. The credential is stored in %s.\n", provider, filepath.Join(paths.AgentDir, "auth.json"))
			return err
		},
	}
}

// listLogins prints one line per provider that offers a login: its id, its
// methods and whether Kopi holds a credential for it.
func listLogins(out io.Writer, providers []pigruntime.Provider) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "Providers that offer login:")
	for _, p := range providers {
		var methods []string
		if p.SignIn != "" {
			methods = append(methods, p.SignIn)
		}
		if p.APIKey {
			methods = append(methods, "API key")
		}
		if len(methods) == 0 {
			continue
		}
		status := ""
		if p.LoggedIn {
			status = "logged in"
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\n", p.ID, strings.Join(methods, ", "), status)
	}
	fmt.Fprintln(w, "\nRun \"kopi login <provider>\".")
	if err := w.Flush(); err != nil {
		return fmt.Errorf("failed to print providers: %w", err)
	}
	return nil
}
