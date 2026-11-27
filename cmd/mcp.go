package cmd

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/policy"
)

func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Inspect MCP servers",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(mcpListCmd())
	return cmd
}

// mcpListCmd never asks whether to trust the folder: an undecided folder's
// servers are listed as not trusted and not started.
func mcpListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Start the MCP servers this folder would use and list their tools",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolvePaths()
			if err != nil {
				return err
			}
			resolved, _, err := resolveConfig(cmd, paths)
			if err != nil {
				return err
			}
			userHome, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("failed to resolve home directory: %w", err)
			}
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to resolve working directory: %w", err)
			}
			set, err := app.ResolveServers(resolved.Config.MCPServers, paths.TrustFile, cwd, userHome, nil, nil, nil)
			if err != nil {
				return err
			}
			for _, err := range set.Errs {
				slog.Warn(printable(err.Error(), "    "))
			}
			out := cmd.OutOrStdout()
			if len(set.Servers) == 0 && len(set.Untrusted) == 0 && len(set.Invalid) == 0 {
				// The warning already says why this folder's servers are missing.
				if len(set.Errs) > 0 {
					return nil
				}
				_, err := fmt.Fprintf(out, "No MCP servers configured: declare them under mcpServers in %s or in %s in this folder.\n", printable(paths.ConfigFile, ""), policy.ProjectMCPFile)
				return err
			}
			// Deferred before Close so the handler outlives it: a signal while
			// Close runs must not kill Kopi and orphan the servers.
			waitCtx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			servers := pigruntime.StartServers(pigruntime.ServerOptions{Servers: set.Servers, CWD: cwd, Version: kopiVersion()})
			defer servers.Close()
			statuses, err := waitServers(waitCtx, servers)
			if err != nil {
				return err
			}
			projectFile := filepath.Join(cwd, policy.ProjectMCPFile)
			blocks := map[string][]string{}
			for _, st := range statuses {
				where := "global (" + paths.ConfigFile + ")"
				if set.Project[st.Name] {
					where = "project (" + projectFile + ")"
				}
				blocks[st.Name] = append(blocks[st.Name], serverBlock(st, where))
			}
			for name, err := range set.Invalid {
				blocks[name] = append(blocks[name], serverBlock(pigruntime.ServerStatus{Name: name, Err: err}, "project ("+projectFile+")"))
			}
			for _, name := range slices.Sorted(maps.Keys(set.Untrusted)) {
				blocks[name] = append(blocks[name], fmt.Sprintf("%s  project (%s)\n  not trusted, not started\n", printable(name, ""), printable(projectFile, "")))
			}
			var all []string
			for _, name := range slices.Sorted(maps.Keys(blocks)) {
				all = append(all, blocks[name]...)
			}
			_, err = fmt.Fprint(out, strings.Join(all, "\n"))
			return err
		},
	}
}

// serverBlock is a started server's name, where it is declared, its state,
// its tools and the tools it skipped.
func serverBlock(st pigruntime.ServerStatus, where string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", printable(st.Name, ""), printable(where, ""))
	if st.Err != nil {
		fmt.Fprintf(&b, "  failed: %s\n", printable(st.Err.Error(), "    "))
		return b.String()
	}
	count := fmt.Sprintf("%d tools", len(st.Tools))
	if len(st.Tools) == 1 {
		count = "1 tool"
	}
	fmt.Fprintf(&b, "  connected, %s\n", count)
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, t := range st.Tools {
		kind := "execute"
		if t.Capability == policy.Read {
			kind = "read-only"
		}
		fmt.Fprintf(tw, "    %s\t%s\n", printable(t.Name, ""), kind)
	}
	_ = tw.Flush()
	for _, s := range st.Skipped {
		fmt.Fprintf(&b, "  skipped %s\n", printable(s, ""))
	}
	return b.String()
}

// printable makes text from outside Kopi, such as a .mcp.json the human has
// not trusted, safe to print: control characters, ESC among them, are
// dropped. Line breaks become spaces, or with indent start an indented line.
func printable(s, indent string) string {
	var b strings.Builder
	for _, r := range strings.ReplaceAll(s, "\r\n", "\n") {
		switch {
		case r == '\n' && indent != "":
			b.WriteString("\n" + indent)
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
