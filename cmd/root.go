package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
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
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/headless"
	"github.com/keyute/kopi/internal/history"
	"github.com/keyute/kopi/internal/instruction"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/sandbox"
	"github.com/keyute/kopi/internal/tool"
	"github.com/keyute/kopi/internal/trust"
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
	root.AddCommand(mcpCmd())
	root.AddCommand(logoutCmd())
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
	sources := []policy.Source{
		{Name: origins["permissions.allow"], Allow: perms.Allow},
		{Name: origins["permissions.ask"], Ask: perms.Ask},
		{Name: origins["permissions.deny"], Deny: perms.Deny},
	}
	// ToolSearch is known to rules even when no server tools need it in the session.
	specs := tool.Specs(slices.Concat(tools, []tool.Tool{tool.NewToolSearch(nil, nil, nil)}))
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

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM)
	defer stop()

	var ask askTrust
	if fullscreen {
		ask = func(req tui.TrustRequest) (tui.TrustChoice, error) {
			askCtx, stopAsk := signal.NotifyContext(ctx, os.Interrupt)
			defer stopAsk()
			return tui.AskTrust(askCtx, cmd.InOrStdin(), cmd.OutOrStdout(), tui.Options{Keys: resolved.Config.Keys}, req)
		}
	}
	// The instruction walk runs on the path trust is decided on.
	instructionDir, err := trust.Canonical(cwd)
	if err != nil {
		return err
	}
	projectInstructions := instruction.Find(instructionDir, resolved.Config.InstructionFallbacks)
	set, err := resolveServers(resolved.Config.MCPServers, paths.TrustFile, cwd, userHome, projectInstructions, ask)
	if err != nil {
		return err
	}
	if set.undecided {
		skipped := "the MCP servers in its " + policy.ProjectMCPFile + " and its instruction files are"
		switch {
		case len(set.untrusted) == 0:
			skipped = "its instruction files are"
		case len(projectInstructions) == 0:
			skipped = "the MCP servers in its " + policy.ProjectMCPFile + " are"
		}
		slog.Warn("this folder is not trusted, so " + skipped + " skipped; run kopi here in a terminal to be asked whether to trust it")
	}
	instructions, instructionErrs := instruction.Load(paths.InstructionFile, []instruction.Root{{Dir: instructionDir, Trusted: set.trusted}}, resolved.Config.InstructionFallbacks)
	// From here until the frontend runs, SIGINT does not kill Kopi: stdio
	// servers run in their own process group, so the terminal's SIGINT does not
	// reach them, and the deferred Close must run. In print mode it ends startup
	// at the wait for servers or at Open; in fullscreen the wait runs behind the
	// interface and ends when the interface is quit (c.Close). ctx outlives
	// startup as the runtime's signal.
	startCtx, stopStart := signal.NotifyContext(ctx, os.Interrupt)
	defer stopStart()
	// interrupts is notified only once the frontend is about to run; stopping
	// it is deferred here so the handler outlives the servers' deferred Close.
	interrupts := make(chan os.Signal, 1)
	defer signal.Stop(interrupts)
	// workRunning is set when Close leaves work running; see below.
	var workRunning bool
	servers := pigruntime.StartServers(pigruntime.ServerOptions{Servers: set.servers, CWD: cwd, Version: kopiVersion()})
	// Closed even under running work: stdio servers would outlive the process,
	// and a call they are serving can fail as the process exits anyway.
	defer servers.Close()

	sb, planSB, unavailable, err := openSandbox(cmd.Context(), resolved.Config.Sandbox, cwd, userHome, paths.Home)
	if err != nil {
		return err
	}

	ws, err := tool.NewWorkspace(cwd, []string{paths.Home})
	if err != nil {
		return err
	}
	defer func() {
		if !workRunning {
			ws.Close()
		}
	}()
	tasks := tool.NewTasks()
	// Commands run in their own session, so they would otherwise outlive Kopi.
	defer tasks.StopAll()
	// rt opens only once the rules are checked, so a bad rule creates nothing;
	// WebFetch's calls come after that.
	var rt *pigruntime.Runtime
	smallModel := resolved.Config.SmallModel
	if smallModel == "" {
		smallModel = model
	}
	answer := func(ctx context.Context, system, prompt string) (string, error) {
		return rt.Answer(ctx, smallModel, system, prompt)
	}
	tools := []tool.Tool{
		tool.NewRead(ws), tool.NewWrite(ws), tool.NewEdit(ws), tool.NewBash(tool.NewShell(cwd, sb), tool.NewShell(cwd, planSB), tasks),
		tool.NewTaskOutput(tasks), tool.NewTaskStop(tasks),
		tool.NewWebFetch("Kopi/"+kopiVersion(), answer),
	}
	rules, err := permissionRules(cwd, userHome, tools, resolved.Config.Permissions, resolved.Origins, flagRules)
	if err != nil {
		return err
	}

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
		slog.Warn("sandbox is unavailable; Bash runs without it and needs approval or an allow rule, and plan mode refuses it", "err", unavailable)
	}
	rt, err = openRuntime(ctx, paths, cwd)
	if err != nil {
		return err
	}
	defer func() {
		if !workRunning {
			rt.Close()
		}
	}()
	if resolved.Config.SmallModel != "" {
		if err := rt.Check(startCtx, pigruntime.OpenOptions{Model: smallModel}); err != nil && (!fullscreen || !sameProviderNoCredential(err, model, smallModel)) {
			if errors.Is(err, pigruntime.ErrNoCredential) {
				providers, listErr := rt.Providers(ctx)
				if listErr != nil {
					err = errors.Join(err, listErr)
				} else {
					err = diagnose(err, smallModel, resolved.Origins["smallModel"], paths, app.OpenRequest{}, providers)
				}
			}
			return fmt.Errorf("failed to use \"smallModel\" in %s: %w", paths.ConfigFile, err)
		}
	}
	// Only the fullscreen interface has a human to answer approvals.
	cfg := app.Config{
		Model: model, Workspace: cwd, Tools: tools, JoinTimeout: joinTimeout, Rules: rules, Unattended: !fullscreen, Mode: mode, Files: ws,
		Errors: slices.Concat(set.errs, instructionErrs), Instructions: instructions, Tasks: tasks, Wake: fullscreen,
	}
	if len(set.servers) > 0 {
		cfg.Servers = func(ctx context.Context) ([]pigruntime.ServerStatus, error) { return waitServers(ctx, servers) }
	}
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
		// Created before Start so the TUI shows the events Start emits.
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
	if fullscreen {
		// The session opens behind the interface; prompts given meanwhile queue.
		err = c.Start(startCtx, req)
	} else {
		// The headless frontend shows no notices or errors; startup's all
		// arrive during Open.
		unsubscribe := c.Subscribe(func(ev event.Event) {
			switch ev := ev.(type) {
			case event.Notice:
				slog.Warn(printable(ev.Text, "    "))
			case event.Error:
				slog.Warn(printable(ev.Err.Error(), "    "))
			}
		})
		err = c.Open(startCtx, req)
		unsubscribe()
	}
	if err != nil {
		var providers []pigruntime.Provider
		var listErr error
		if errors.Is(err, pigruntime.ErrNoCredential) {
			providers, listErr = rt.Providers(ctx)
		}
		if listErr != nil {
			return errors.Join(err, listErr)
		}
		return diagnose(err, model, resolved.Origins["model"], paths, req, providers)
	}
	// SIGINT is a channel rather than part of the context because it must
	// keep firing: each one interrupts the running turn, and only one at
	// idle exits. It is registered before stopStart so SIGINT never reverts to
	// the default exit in between.
	signal.Notify(interrupts, os.Interrupt)
	stopStart()
	runErr := f.Run(ctx, interrupts)

	// Close must not inherit SIGTERM's cancellation; it bounds itself by
	// JoinTimeout.
	closeErr := c.Close(context.WithoutCancel(ctx))
	if errors.Is(closeErr, pigruntime.ErrJoinTimeout) {
		// Close left the session open under work that is still running, so
		// the runtime and workspace stay open too rather than being pulled
		// from under it; the process exits right after. The MCP servers are
		// closed regardless.
		workRunning = true
		closeErr = fmt.Errorf("running work did not stop before exit: %w", closeErr)
	}
	return errors.Join(runErr, closeErr)
}

// mcpServers is the MCP server set of a run in one folder, and whether the
// folder is trusted.
type mcpServers struct {
	// servers are the servers to start: the global ones and, in a trusted
	// folder, the project ones, which replace global ones of the same name.
	servers map[string]config.MCPServer
	// project names the servers in servers that the project declares.
	project map[string]bool
	// untrusted are the project's servers, skipped as the folder is not trusted.
	untrusted map[string]config.MCPServer
	// undecided means no trust decision applies and none was asked for.
	undecided bool
	// trusted means the folder is trusted, so its servers and instruction
	// files are used.
	trusted bool
	invalid map[string]error
	errs    []error
}

// askTrust asks the human whether to trust a folder; nil means no human can
// be asked.
type askTrust func(tui.TrustRequest) (tui.TrustChoice, error)

// resolveServers decides whether the folder is trusted and merges the
// project's servers over global ones when it is. The trust file is opened
// only when the project declares a server or has instruction files, which
// instructions lists; an undecided folder is asked about through ask and the
// answer saved, and without ask it is untrusted for this run.
func resolveServers(global map[string]config.MCPServer, trustFile, cwd, userHome string, instructions []string, ask askTrust) (mcpServers, error) {
	s := mcpServers{servers: maps.Clone(global), project: map[string]bool{}}
	if s.servers == nil {
		s.servers = map[string]config.MCPServer{}
	}
	project, invalid, err := config.ProjectServers(filepath.Join(cwd, policy.ProjectMCPFile))
	if err != nil {
		s.errs = append(s.errs, fmt.Errorf("skipped this folder's MCP servers: %w", err))
		project, invalid = nil, nil
	}
	for _, name := range slices.Sorted(maps.Keys(invalid)) {
		s.errs = append(s.errs, fmt.Errorf("MCP server %s: %w", name, invalid[name]))
		project[name] = config.MCPServer{}
	}
	if len(project) == 0 && len(instructions) == 0 {
		return s, nil
	}
	store, err := trust.Open(trustFile)
	if err != nil {
		return mcpServers{}, err
	}
	d, ok, err := store.Lookup(cwd)
	if err != nil {
		return mcpServers{}, err
	}
	switch {
	case ok:
	case ask == nil:
		s.undecided = true
	default:
		if d, err = decideTrust(store, cwd, userHome, project, invalid, instructions, ask); err != nil {
			return mcpServers{}, err
		}
	}
	if d != trust.Trusted {
		s.untrusted = project
		return s, nil
	}
	s.trusted = true
	s.invalid = invalid
	for name, p := range project {
		s.project[name] = true
		if invalid[name] != nil {
			delete(s.servers, name)
			continue
		}
		s.servers[name] = p
	}
	return s, nil
}

// waitServers waits for every server to settle; it fails when ctx ends first,
// so that startup stops rather than going on without the servers.
func waitServers(ctx context.Context, servers *pigruntime.Servers) ([]pigruntime.ServerStatus, error) {
	statuses := servers.Wait(ctx)
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("failed to start MCP servers: startup was interrupted: %w", err)
	}
	return statuses, nil
}

// decideTrust asks about cwd and saves the answer; a cancelled prompt saves
// nothing and returns no decision. The folder and parent are resolved first,
// so what is shown is what is saved, and the home check sees through
// symlinks and the case a name was typed in. An answer saved for the user's
// home or the root would cover almost every folder below it, so there either
// answer holds for this run only.
func decideTrust(store *trust.Store, cwd, userHome string, project map[string]config.MCPServer, invalid map[string]error, instructions []string, ask askTrust) (trust.Decision, error) {
	cwd, err := trust.Canonical(cwd)
	if err != nil {
		return 0, err
	}
	userHome, err = trust.Canonical(userHome)
	if err != nil {
		return 0, err
	}
	broad := cwd == userHome || filepath.Dir(cwd) == cwd
	req := tui.TrustRequest{Folder: cwd, Instructions: instructions}
	if parent := filepath.Dir(cwd); !broad && parent != filepath.Dir(parent) && parent != userHome {
		req.Parent = parent
	}
	for _, name := range slices.Sorted(maps.Keys(project)) {
		target := serverTarget(project[name])
		if invalid[name] != nil {
			target = "invalid, will not start"
		}
		req.Servers = append(req.Servers, name+": "+target)
	}
	choice, err := ask(req)
	if err != nil {
		return 0, err
	}
	dir, d := cwd, trust.Trusted
	switch choice {
	case tui.TrustFolder:
		if broad {
			return trust.Trusted, nil
		}
	case tui.TrustParent:
		dir = req.Parent
	case tui.TrustDeny:
		if broad {
			return trust.Untrusted, nil
		}
		d = trust.Untrusted
	default:
		return 0, nil
	}
	if err := store.Set(dir, d); err != nil {
		return 0, fmt.Errorf("failed to save the trust decision: %w", err)
	}
	return d, nil
}

// serverTarget is what a server runs or connects to, as configured: the
// command and its arguments, or the URL, then the names, never the values, of
// the environment variables and headers it sets.
func serverTarget(s config.MCPServer) string {
	target := s.URL
	if s.Command != "" {
		target = strings.Join(append([]string{s.Command}, s.Args...), " ")
	}
	if len(s.Env) > 0 {
		target += " (env: " + strings.Join(slices.Sorted(maps.Keys(s.Env)), ", ") + ")"
	}
	if len(s.Headers) > 0 {
		target += " (headers: " + strings.Join(slices.Sorted(maps.Keys(s.Headers)), ", ") + ")"
	}
	return target
}

// openSandbox returns the sandboxes Bash runs under in execute and in plan
// mode, both or neither; neither when sandbox.enabled is false. With enabled
// unset, a machine without a usable sandbox is not an error: the sandboxes
// are nil and unavailable says why.
func openSandbox(ctx context.Context, cfg config.Sandbox, cwd, userHome, kopiHome string) (sb, plan *sandbox.Sandbox, unavailable, err error) {
	if cfg.Enabled != nil && !*cfg.Enabled {
		return nil, nil, nil, nil
	}
	p := sandboxPolicy(cfg, cwd, userHome, kopiHome)
	sandboxes, err := sandbox.New(ctx, p, planPolicy(p, cwd))
	switch {
	case cfg.Enabled == nil && errors.Is(err, sandbox.ErrUnavailable):
		return nil, nil, err, nil
	case err != nil:
		return nil, nil, nil, fmt.Errorf("failed to start the sandbox: %w", err)
	}
	return sandboxes[0], sandboxes[1], nil, nil
}

// sandboxPolicy lets commands write only the workspace, temporary
// directories and the paths the user allowed, never read Kopi's home, and
// never write the git files that decide which config and hooks git runs, nor
// any .mcp.json or what the start directory's .mcp.json resolves to, either of
// which would run code outside the sandbox.
func sandboxPolicy(cfg config.Sandbox, cwd, userHome, kopiHome string) sandbox.Policy {
	expand := func(paths []string) []string {
		out := make([]string, 0, len(paths))
		for _, p := range paths {
			switch {
			case filepath.IsAbs(p):
			case p == "~":
				p = userHome
			case strings.HasPrefix(p, "~/"):
				p = userHome + string(filepath.Separator) + p[2:]
			default:
				p = cwd + string(filepath.Separator) + p
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
	// Unconditional: the sandbox also denies what a link resolves to, and a
	// linked directory is denied whole rather than left to be swapped for a file.
	denyWrite = append(denyWrite, filepath.Join(cwd, policy.ProjectMCPFile))
	write := []string{cwd, "/tmp", os.TempDir()}
	return sandbox.Policy{
		Write:          append(write, expand(cfg.AllowWrite)...),
		DenyRead:       append([]string{kopiHome}, expand(cfg.DenyRead)...),
		DenyWrite:      denyWrite,
		DenyWriteNames: []string{policy.ProjectMCPFile},
		Network:        cfg.Network,
	}
}

// planPolicy is p with the workspace denied rather than dropped from the write
// roots: a workspace under /tmp, the temporary directory or an allowWrite path
// would stay writable through that root.
func planPolicy(p sandbox.Policy, cwd string) sandbox.Policy {
	p.DenyWrite = slices.Concat(p.DenyWrite, []string{cwd})
	return p
}

// sameProviderNoCredential reports whether err is the missing credential of
// the provider smallModel shares with model; in fullscreen the first-run login
// of model supplies it.
func sameProviderNoCredential(err error, model, smallModel string) bool {
	provider, _, _ := strings.Cut(model, "/")
	smallProvider, _, _ := strings.Cut(smallModel, "/")
	return errors.Is(err, pigruntime.ErrNoCredential) && provider == smallProvider
}

// diagnose turns the open failures a user can fix into Kopi's own advice.
func diagnose(err error, model, origin string, paths config.Paths, req app.OpenRequest, providers []pigruntime.Provider) error {
	switch {
	case errors.Is(err, pigruntime.ErrNoCredential):
		id, _, _ := config.ParseModelRef(model)
		i := slices.IndexFunc(providers, func(p pigruntime.Provider) bool { return p.ID == id })
		switch {
		case i >= 0 && providers[i].APIKey:
			// PiG has no public accessor naming a provider's API-key variable.
			return fmt.Errorf("no credential for provider %q: run \"kopi login %s\" or set its API key in the environment", id, id)
		case i >= 0 && providers[i].SignIn != "":
			return fmt.Errorf("no credential for provider %q: run \"kopi login %s\"", id, id)
		}
		return fmt.Errorf("no credential for provider %q: supply its credential in the environment", id)
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
