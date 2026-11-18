package cmd

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/pigruntime"
)

func TestLoginWithoutProviderListsProviders(t *testing.T) {
	e := newTestEnv(t)
	e.writeFile(t, filepath.Join(e.agentDir, "auth.json"), `{"mistral":{"type":"api_key","key":"`+syntheticKey+`"}}`)

	stdout, _, err := execute(t, "", "login")

	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`(?m)^  openai-codex +OpenAI \(ChatGPT Plus/Pro\) *$`), stdout)
	assert.Regexp(t, regexp.MustCompile(`(?m)^  openai +OpenAI \(ChatGPT subscription\), API key *$`), stdout)
	assert.Regexp(t, regexp.MustCompile(`(?m)^  mistral +API key +logged in$`), stdout)
	assert.Contains(t, stdout, "kopi login <provider>")
	assert.NotContains(t, stdout, syntheticKey)
}

func TestLoginStoresAPIKey(t *testing.T) {
	e := newTestEnv(t)

	stdout, _, err := execute(t, syntheticKey+"\n", "login", "mistral")

	require.NoError(t, err)
	assert.Contains(t, stdout, "Logged in to mistral.")
	assert.NotContains(t, stdout, syntheticKey)
	content, err := os.ReadFile(filepath.Join(e.agentDir, "auth.json"))
	require.NoError(t, err)
	assert.Contains(t, string(content), syntheticKey)
}

func TestLoginUnknownProviderFails(t *testing.T) {
	newTestEnv(t)

	_, _, err := execute(t, "", "login", "nosuchprovider")

	require.ErrorIs(t, err, pigruntime.ErrUnknownProvider)
}

func TestTerminalLoginAskReturnsWhenContextEnds(t *testing.T) {
	in, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	l := &terminalLogin{in: in, lines: bufio.NewReader(in), out: io.Discard}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := l.ask(ctx, pigruntime.LoginPrompt{Message: "Code:"})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestTerminalLoginCancelledAskHandsPendingReadToNextAsk(t *testing.T) {
	in, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	l := &terminalLogin{in: in, lines: bufio.NewReader(in), out: io.Discard}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := l.ask(ctx, pigruntime.LoginPrompt{Message: "Code:"})
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, l.pending, "the cancelled ask leaves its read in flight")

	done := make(chan readResult, 1)
	go func() {
		text, err := l.ask(t.Context(), pigruntime.LoginPrompt{Message: "Code:"})
		done <- readResult{text, err}
	}()
	_, err = io.WriteString(w, "code-123\n")
	require.NoError(t, err)
	res := <-done
	require.NoError(t, res.err)
	assert.Equal(t, "code-123", res.text)
	assert.Nil(t, l.pending)
}

func TestTerminalLoginPrintsPromptAndShowsText(t *testing.T) {
	var out strings.Builder
	l := &terminalLogin{in: strings.NewReader("a\nb\n"), lines: bufio.NewReader(strings.NewReader("a\nb\n")), out: &out}

	l.show("Visit https://login.example")
	_, err := l.ask(t.Context(), pigruntime.LoginPrompt{Message: "Code:", Placeholder: "paste"})
	require.NoError(t, err)
	_, err = l.ask(t.Context(), pigruntime.LoginPrompt{Message: "Key:", Secret: true})
	require.NoError(t, err)

	assert.Equal(t, "Visit https://login.example\nCode: [paste] Key: ", out.String())
}

func TestTerminalLoginSelectsOptionByNumberOrID(t *testing.T) {
	options := []pigruntime.LoginOption{{ID: "red", Label: "Red", Description: "warm"}, {ID: "blue", Label: "Blue"}}
	for answer, want := range map[string]string{"2": "blue", "red": "red", "green": "green", "3": "3"} {
		t.Run(answer, func(t *testing.T) {
			var out strings.Builder
			l := &terminalLogin{lines: bufio.NewReader(strings.NewReader(answer + "\n")), out: &out}

			got, err := l.ask(t.Context(), pigruntime.LoginPrompt{Message: "Team:", Options: options})

			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.Equal(t, "Team:\n  1) Red - warm\n  2) Blue\nChoice: ", out.String())
		})
	}
}
