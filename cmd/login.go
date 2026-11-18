package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/muesli/cancelreader"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/keyute/kopi/internal/app"
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
			return loginTo(ctx, rt, cmd.InOrStdin(), out, args[0], filepath.Join(paths.AgentDir, "auth.json"))
		},
	}
}

// loginTo logs in to provider on the terminal and prints the sign-in notice.
func loginTo(ctx context.Context, rt *pigruntime.Runtime, in io.Reader, out io.Writer, provider, authFile string) error {
	t := &terminalLogin{in: in, lines: bufio.NewReader(in), out: out}
	notice, err := rt.Login(ctx, provider, pigruntime.LoginUI{Ask: t.ask, Show: t.show})
	if err != nil {
		return err
	}
	if notice != "" {
		if _, err := fmt.Fprintln(out, notice); err != nil {
			return fmt.Errorf("failed to print login notice: %w", err)
		}
	}
	_, err = fmt.Fprintf(out, "Logged in to %s. The credential is stored in %s.\n", provider, authFile)
	return err
}

// listLogins prints one line per provider that offers a login: its id, its
// methods and whether Kopi holds a credential for it.
func listLogins(out io.Writer, providers []pigruntime.Provider) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "Providers that offer login:")
	for _, p := range providers {
		methods := app.LoginMethods(p)
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

// terminalLogin answers a login's prompts on the terminal, or from piped input.
type terminalLogin struct {
	in    io.Reader
	lines *bufio.Reader
	out   io.Writer
	// pending is the read still in flight after its prompt's ctx ended; the
	// next prompt takes its answer instead of starting a concurrent read.
	// Prompts never overlap.
	pending chan readResult
}

type readResult struct {
	text string
	err  error
}

func (t *terminalLogin) show(text string) {
	fmt.Fprintln(t.out, text)
}

func (t *terminalLogin) ask(ctx context.Context, p pigruntime.LoginPrompt) (string, error) {
	switch {
	case len(p.Options) > 0:
		return t.selectOption(ctx, p)
	case p.Secret:
		return t.readSecret(ctx, p)
	}
	t.prompt(p)
	return t.read(ctx)
}

func (t *terminalLogin) prompt(p pigruntime.LoginPrompt) {
	if p.Placeholder != "" {
		fmt.Fprintf(t.out, "%s [%s] ", p.Message, p.Placeholder)
		return
	}
	fmt.Fprintf(t.out, "%s ", p.Message)
}

// selectOption maps a number or an exact ID to the option's ID; any other
// answer is returned for the login to refuse.
func (t *terminalLogin) selectOption(ctx context.Context, p pigruntime.LoginPrompt) (string, error) {
	fmt.Fprintln(t.out, p.Message)
	for i, o := range p.Options {
		if o.Description != "" {
			fmt.Fprintf(t.out, "  %d) %s - %s\n", i+1, o.Label, o.Description)
			continue
		}
		fmt.Fprintf(t.out, "  %d) %s\n", i+1, o.Label)
	}
	fmt.Fprint(t.out, "Choice: ")
	answer, err := t.read(ctx)
	if err != nil {
		return "", err
	}
	if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(p.Options) {
		return p.Options[n-1].ID, nil
	}
	return answer, nil
}

// read runs readLine off the prompt's goroutine so a cancelled ctx (the OAuth
// callback server winning against a pasted code) returns at once.
func (t *terminalLogin) read(ctx context.Context) (string, error) {
	if t.pending == nil {
		ch := make(chan readResult, 1)
		t.pending = ch
		go func() {
			text, err := t.readLine()
			ch <- readResult{text: text, err: err}
		}()
	}
	select {
	case res := <-t.pending:
		t.pending = nil
		return res.text, res.err
	case <-ctx.Done():
		return "", context.Cause(ctx)
	}
}

func (t *terminalLogin) readLine() (string, error) {
	line, err := t.lines.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", fmt.Errorf("failed to read login input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// readSecret reads without echo on a terminal, and returns before ctx's end
// only once the terminal is restored. Piped input is read as a plain line.
func (t *terminalLogin) readSecret(ctx context.Context, p pigruntime.LoginPrompt) (string, error) {
	f, ok := t.in.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		t.prompt(p)
		return t.read(ctx)
	}
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	reader, err := cancelreader.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("failed to create login input reader: %w", err)
	}
	defer func() { _ = reader.Close() }()
	state, err := term.MakeRaw(int(f.Fd()))
	if err != nil {
		return "", fmt.Errorf("failed to set terminal state: %w", err)
	}
	defer func() { _ = term.Restore(int(f.Fd()), state) }()
	terminal := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{reader, t.out}, "")
	prompt := p.Message
	if p.Placeholder != "" {
		prompt += " [" + p.Placeholder + "]"
	}
	done := make(chan readResult, 1)
	go func() {
		text, err := terminal.ReadPassword(prompt + " ")
		done <- readResult{text: text, err: err}
	}()
	select {
	case res := <-done:
		// In raw mode the editor reports Ctrl-C, and Ctrl-D on an empty line, as EOF.
		if errors.Is(res.err, io.EOF) {
			// The terminal is still raw, so end the prompt line explicitly.
			fmt.Fprint(t.out, "\r\n")
			return "", context.Canceled
		}
		if res.err != nil {
			return "", fmt.Errorf("failed to read login input: %w", res.err)
		}
		return res.text, nil
	case <-ctx.Done():
		// The editor must stop writing before the caller regains the terminal.
		reader.Cancel()
		<-done
		return "", context.Cause(ctx)
	}
}
