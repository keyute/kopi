package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/lmittmann/tint"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/headless"
	"github.com/keyute/kopi/internal/history"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/sandbox"
	"github.com/keyute/kopi/internal/tool"
	"github.com/keyute/kopi/internal/tui"
)

const (
	joinTimeout = 5 * time.Second
	// historyLimit is how many prompts are kept for each workspace.
	historyLimit = 100
)

func Execute() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "kopi: %v\n", err)
		os.Exit(1)
	}
}

// newRoot builds the whole command tree; tests execute it in-process.
func newRoot() *cobra.Command {
	root := rootCmd()
	root.AddCommand(versionCmd())
	root.AddCommand(configCmd())
	root.AddCommand(loginCmd())
	return root
}

func rootCmd() *cobra.Command {
	var resumeID string
	var cont, printOnly bool
	var allowed, disallowed []string
	cmd := &cobra.Command{
		Use:           "kopi [prompt]",
		Short:         "Kopi coding agent",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return setupLogging(cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			flagRules := []policy.Source{
				{Name: "--allowed-tools", Allow: splitRules(allowed)},
				{Name: "--disallowed-tools", Deny: splitRules(disallowed)},
			}
			return run(cmd, args, printOnly, flagRules, app.OpenRequest{ResumeID: resumeID, Continue: cont})
		},
	}
	cmd.PersistentFlags().String("model", "", "model as provider/id (overrides the config file)")
	cmd.PersistentFlags().String("log-level", "warn", "log level: debug, info, warn or error")
	cmd.Flags().BoolVarP(&cont, "continue", "c", false, "continue the most recent session in this directory")
	cmd.Flags().StringVarP(&resumeID, "resume", "r", "", "resume the session with this id")
	cmd.Flags().BoolVarP(&printOnly, "print", "p", false, "print the response to one prompt and exit")
	cmd.PersistentFlags().String("mode", "", "mode sessions start in: plan or execute (overrides the config file)")
	cmd.Flags().StringArrayVar(&allowed, "allowed-tools", nil, "permission rules to allow, comma-separated or repeated, e.g. \"Edit(docs/**)\"")
	cmd.Flags().StringArrayVar(&disallowed, "disallowed-tools", nil, "permission rules to deny, comma-separated or repeated")
	cmd.MarkFlagsMutuallyExclusive("continue", "resume")
	return cmd
}

// splitRules splits each value on commas outside parentheses, which enclose
// path globs like Edit(src/{a,b}/**).
func splitRules(values []string) []string {
	var rules []string
	for _, v := range values {
		depth, start := 0, 0
		for i, r := range v {
			switch r {
			case '(':
				depth++
			case ')':
				depth--
			case ',':
				if depth == 0 {
					rules = append(rules, strings.TrimSpace(v[start:i]))
					start = i + 1
				}
			}
		}
		rules = append(rules, strings.TrimSpace(v[start:]))
	}
	return rules
}

// permissionRules builds the rules from each config permission list named by
// the layer that set it, then the flags.
func permissionRules(cwd, home string, tools []tool.Tool, perms config.Permissions, origins map[string]string, flagRules []policy.Source) (*policy.Rules, error) {
	specs := make(map[string]policy.ToolSpec, len(tools))
	for _, t := range tools {
		specs[t.Name] = policy.ToolSpec{Capability: t.Capability, Target: t.Target}
	}
	sources := []policy.Source{
		{Name: origins["permissions.allow"], Allow: perms.Allow},
		{Name: origins["permissions.ask"], Ask: perms.Ask},
		{Name: origins["permissions.deny"], Deny: perms.Deny},
	}
	rules, err := policy.NewRules(cwd, home, specs, append(sources, flagRules...)...)
	if err != nil {
		return nil, fmt.Errorf("failed to load permission rules: %w", err)
	}
	return rules, nil
}

func setupLogging(cmd *cobra.Command) error {
	level, err := logLevel(cmd)
	if err != nil {
		return err
	}
	stderr := cmd.ErrOrStderr()
	slog.SetDefault(slog.New(tint.NewTextHandler(stderr, &tint.Options{Level: level, NoColor: !isTerminal(stderr)})))
	return nil
}

func logLevel(cmd *cobra.Command) (slog.Level, error) {
	name, err := cmd.Flags().GetString("log-level")
	if err != nil {
		return 0, err
	}
	levels := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}
	level, ok := levels[name]
	if !ok {
		return 0, fmt.Errorf("invalid --log-level %q: want debug, info, warn or error", name)
	}
	return level, nil
}

// logToFile sends slog to path, which must not be stderr while the
// fullscreen UI owns the terminal. The returned func restores the previous
// logger and closes the file.
func logToFile(cmd *cobra.Command, path string) (func(), error) {
	level, err := logLevel(cmd)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(tint.NewTextHandler(f, &tint.Options{Level: level, NoColor: true})))
	return func() {
		slog.SetDefault(prev)
		_ = f.Close()
	}, nil
}

type frontend interface {
	Run(ctx context.Context, interrupts <-chan os.Signal) error
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// resolveConfig layers defaults, the global file and the flags the user set.
func resolveConfig(cmd *cobra.Command, paths config.Paths) (*config.Resolved, error) {
	global, err := config.GlobalFile(paths.ConfigFile)
	if err != nil {
		return nil, err
	}
	flags := map[string]any{}
	if cmd.Flags().Changed("model") {
		model, err := cmd.Flags().GetString("model")
		if err != nil {
			return nil, err
		}
		flags["model"] = model
	}
	if cmd.Flags().Changed("mode") {
		mode, err := cmd.Flags().GetString("mode")
		if err != nil {
			return nil, err
		}
		flags["mode"] = mode
	}
	return config.Resolve(config.Defaults(), global, config.Map("flags", flags))
}

func openRuntime(ctx context.Context, paths config.Paths, cwd string) (*pigruntime.Runtime, error) {
	return pigruntime.New(ctx, pigruntime.Options{Home: paths.Home, AgentDir: paths.AgentDir, CWD: cwd})
}

// readPrompt joins piped stdin and the argument, in that order.
func readPrompt(in io.Reader, args []string) (string, error) {
	var parts []string
	if !isTerminal(in) {
		b, err := io.ReadAll(in)
		if err != nil {
			return "", fmt.Errorf("failed to read stdin: %w", err)
		}
		parts = append(parts, string(b))
	}
	parts = append(parts, args...)
	var prompt []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			prompt = append(prompt, p)
		}
	}
	if len(prompt) == 0 {
		return "", errors.New("no prompt: pass one as an argument or pipe it on stdin")
	}
	return strings.Join(prompt, "\n\n"), nil
}

func run(cmd *cobra.Command, args []string, printOnly bool, flagRules []policy.Source, req app.OpenRequest) (err error) {
	paths, err := config.ResolvePaths()
	if err != nil {
		return err
	}
	resolved, err := resolveConfig(cmd, paths)
	if err != nil {
		return err
	}
	model := resolved.Config.Model
	if model == "" {
		return config.ErrNoModel
	}
	mode, err := policy.ParseMode(resolved.Config.Mode)
	if err != nil {
		return err
	}
	fullscreen := !printOnly && isTerminal(cmd.InOrStdin()) && isTerminal(cmd.OutOrStdout())
	var prompt string
	if fullscreen {
		if len(args) > 0 {
			return fmt.Errorf("unexpected argument %q: pass -p to print the response to a prompt", args[0])
		}
	} else if prompt, err = readPrompt(cmd.InOrStdin(), args); err != nil {
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

	sb, unavailable, err := openSandbox(cmd.Context(), resolved.Config.Sandbox, cwd, userHome, paths.Home)
	if err != nil {
		return err
	}

	// workRunning is set when Close leaves work running; see below.
	var workRunning bool
	ws, err := tool.NewWorkspace(cwd, []string{paths.Home})
	if err != nil {
		return err
	}
	defer func() {
		if !workRunning {
			ws.Close()
		}
	}()
	tools := []tool.Tool{tool.NewRead(ws), tool.NewWrite(ws), tool.NewEdit(ws), tool.NewBash(tool.NewShell(cwd, sb))}
	rules, err := permissionRules(cwd, userHome, tools, resolved.Config.Permissions, resolved.Origins, flagRules)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM)
	defer stop()

	if fullscreen {
		if err := os.MkdirAll(paths.Home, 0o700); err != nil {
			return fmt.Errorf("failed to create kopi home: %w", err)
		}
		restore, err := logToFile(cmd, paths.LogFile)
		if err != nil {
			return err
		}
		defer restore()
	}
	// Logged only now so that in fullscreen it goes to the log file.
	if unavailable != nil {
		slog.Warn("sandbox is unavailable; Bash runs without it and asks before each command", "err", unavailable)
	}

	rt, err := openRuntime(ctx, paths, cwd)
	if err != nil {
		return err
	}
	defer func() {
		if !workRunning {
			rt.Close()
		}
	}()
	// Only the fullscreen interface has a human to answer approvals.
	cfg := app.Config{Model: model, Workspace: cwd, Tools: tools, JoinTimeout: joinTimeout, Rules: rules, Unattended: !fullscreen, Mode: mode, Files: ws}
	// Only the fullscreen interface records prompts.
	if fullscreen {
		store, err := history.Open(paths.HistoryFile, cwd, historyLimit)
		if err != nil {
			slog.Warn("failed to open prompt history", "err", err)
		}
		cfg.History = store
	}
	c := app.NewController(rt, cfg)
	var f frontend
	if fullscreen {
		// Created before Open so the TUI shows the session event.
		f = tui.New(c, cmd.InOrStdin(), cmd.OutOrStdout(), tui.Options{
			Version:     kopiVersion(),
			Keys:        resolved.Config.Keys,
			StatusLeft:  resolved.Config.StatusLine.Left,
			StatusRight: resolved.Config.StatusLine.Right,
			ImageDir:    paths.ImageDir,
		})
	} else {
		f = headless.New(c, prompt, cmd.OutOrStdout())
	}
	// Only Open may be cancelled by SIGINT; ctx outlives it as the runtime's
	// signal.
	openCtx, stopOpen := signal.NotifyContext(ctx, os.Interrupt)
	if err := c.Open(openCtx, req); err != nil {
		stopOpen()
		return diagnose(err, model, resolved.Origins["model"], paths, req, rt.OAuthProviders())
	}
	// SIGINT is a channel rather than part of the context because it must
	// keep firing: each one interrupts the running turn, and only one at
	// idle exits. It is registered before stopOpen so SIGINT never reverts to
	// the default exit in between.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	stopOpen()
	runErr := f.Run(ctx, interrupts)

	// Close must not inherit SIGTERM's cancellation; it bounds itself by
	// JoinTimeout.
	closeErr := c.Close(context.WithoutCancel(ctx))
	if errors.Is(closeErr, pigruntime.ErrJoinTimeout) {
		// Close left the session open under work that is still running, so
		// the runtime and workspace stay open too rather than being pulled
		// from under it; the process exits right after.
		workRunning = true
		closeErr = fmt.Errorf("running work did not stop before exit: %w", closeErr)
	}
	return errors.Join(runErr, closeErr)
}

// openSandbox returns the sandbox Bash runs under, or nil when sandbox.enabled
// is false. With enabled unset, a machine without a usable sandbox is not an
// error: the sandbox is nil and unavailable says why.
func openSandbox(ctx context.Context, cfg config.Sandbox, cwd, userHome, kopiHome string) (sb *sandbox.Sandbox, unavailable, err error) {
	if cfg.Enabled != nil && !*cfg.Enabled {
		return nil, nil, nil
	}
	sb, err = sandbox.New(ctx, sandboxPolicy(cfg, cwd, userHome, kopiHome))
	switch {
	case cfg.Enabled == nil && errors.Is(err, sandbox.ErrUnavailable):
		return nil, err, nil
	case err != nil:
		return nil, nil, fmt.Errorf("failed to start the sandbox: %w", err)
	}
	return sb, nil, nil
}

// sandboxPolicy lets commands write only the workspace, temporary
// directories and the paths the user allowed, never read Kopi's home, and
// never write the git files that decide which config and hooks git runs, which
// would run code outside the sandbox.
func sandboxPolicy(cfg config.Sandbox, cwd, userHome, kopiHome string) sandbox.Policy {
	expand := func(paths []string) []string {
		out := make([]string, 0, len(paths))
		for _, p := range paths {
			switch {
			case filepath.IsAbs(p):
			case p == "~":
				p = userHome
			case strings.HasPrefix(p, "~/"):
				p = filepath.Join(userHome, p[2:])
			default:
				p = filepath.Join(cwd, p)
			}
			out = append(out, p)
		}
		return out
	}
	gitDir := filepath.Join(cwd, ".git")
	var denyWrite []string
	for _, name := range []string{"hooks", "config", "commondir", "config.worktree", "worktrees", "modules"} {
		denyWrite = append(denyWrite, filepath.Join(gitDir, name))
	}
	if _, err := os.Lstat(gitDir); errors.Is(err, os.ErrNotExist) {
		// With no .git, git takes a directory holding HEAD, objects and refs
		// for a git directory and runs its config.
		denyWrite = append(denyWrite, filepath.Join(cwd, "HEAD"))
	} else if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		// A .git file, as in a linked worktree or submodule, names the git directory.
		denyWrite = append(denyWrite, gitDir)
	}
	return sandbox.Policy{
		Write:     append([]string{cwd, "/tmp", os.TempDir()}, expand(cfg.AllowWrite)...),
		DenyRead:  append([]string{kopiHome}, expand(cfg.DenyRead)...),
		DenyWrite: denyWrite,
		Network:   cfg.Network,
	}
}

// diagnose turns the open failures a user can fix into Kopi's own advice.
func diagnose(err error, model, origin string, paths config.Paths, req app.OpenRequest, loginProviders []string) error {
	switch {
	case errors.Is(err, pigruntime.ErrNoCredential):
		provider, _, _ := config.ParseModelRef(model)
		if slices.Contains(loginProviders, provider) {
			return fmt.Errorf("no credential for provider %q: run \"kopi login %s\"", provider, provider)
		}
		// PiG has no public accessor naming a provider's API-key variable.
		return fmt.Errorf("no credential for provider %q: Kopi has no sign-in flow for it; supply its credential the way PiG reads it, such as an API key in the environment", provider)
	case errors.Is(err, pigruntime.ErrUnknownModel):
		where := "--model"
		if origin != "flags" {
			where = fmt.Sprintf("\"model\" in %s", paths.ConfigFile)
		}
		return fmt.Errorf("unknown model %q: check %s", model, where)
	case errors.Is(err, pigruntime.ErrSessionNotFound) && req.Continue:
		return errors.New("no session to continue in this directory: start without --continue")
	case errors.Is(err, pigruntime.ErrSessionNotFound):
		return fmt.Errorf("no session %q in this directory: start kopi and run /resume to list sessions", req.ResumeID)
	}
	return err
}
