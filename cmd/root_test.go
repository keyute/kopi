package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/headless"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/sandbox"
	"github.com/keyute/kopi/internal/tool"
)

const (
	// syntheticKey is the mock provider's API key; it must never be printed.
	syntheticKey = "sk-kopi-synthetic-3f9a"
	// waitLimit only guards against a hung test; no test relies on it elapsing.
	waitLimit = 30 * time.Second
)

type testEnv struct {
	root, home, kopiHome, agentDir, workspace string
}

// newTestEnv points HOME and KOPI_HOME at empty temp dirs, runs in a temp
// workspace with an empty PATH, and restores the PiG variables the commands
// set process-wide.
func newTestEnv(t *testing.T) testEnv {
	t.Helper()
	root := t.TempDir()
	e := testEnv{
		root:      root,
		home:      filepath.Join(root, "home"),
		kopiHome:  filepath.Join(root, "kopi"),
		workspace: filepath.Join(root, "ws"),
	}
	e.agentDir = filepath.Join(e.kopiHome, "agent")
	for _, dir := range []string{e.home, e.kopiHome, e.workspace} {
		require.NoError(t, os.Mkdir(dir, 0o755))
	}
	t.Setenv("HOME", e.home)
	t.Setenv("KOPI_HOME", e.kopiHome)
	t.Setenv("PATH", "")
	pigruntimetest.RestoreEnv(t)
	t.Chdir(e.workspace)
	return e
}

func (e testEnv) writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// execute runs the command tree in-process with stdin as input.
func execute(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRoot()
	var out, errOut bytes.Buffer
	root.SetArgs(args)
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&out)
	root.SetErr(&errOut)
	err = root.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

func TestHelpVersionAndConfigCreateNothing(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"version"}, {"config"}} {
		t.Run(args[0], func(t *testing.T) {
			e := newTestEnv(t)
			before := pigruntimetest.Snapshot(t, e.root)

			stdout, _, err := execute(t, "", args...)

			require.NoError(t, err)
			assert.NotEmpty(t, stdout)
			assert.Equal(t, before, pigruntimetest.Snapshot(t, e.root))
			assert.NoDirExists(t, e.agentDir)
		})
	}
}

func TestRootWithoutModelFails(t *testing.T) {
	newTestEnv(t)

	_, _, err := execute(t, "")

	require.ErrorIs(t, err, config.ErrNoModel)
	assert.Contains(t, err.Error(), "--model")
}

func TestRootRejectsContinueWithResume(t *testing.T) {
	newTestEnv(t)

	_, _, err := execute(t, "", "--model", "a/b", "--continue", "--resume", "x")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "continue")
	assert.Contains(t, err.Error(), "resume")
}

func TestRootRejectsInvalidLogLevel(t *testing.T) {
	newTestEnv(t)

	_, _, err := execute(t, "", "--log-level", "loud", "config")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--log-level")
}

func TestRootRejectsInvalidMode(t *testing.T) {
	e := newTestEnv(t)

	_, _, err := execute(t, "", "--model", mockModel, "--mode", "auto", "-p", "hi")

	require.ErrorIs(t, err, config.ErrInvalidValue)
	assert.Contains(t, err.Error(), "flags")
	assert.Contains(t, err.Error(), "want plan or execute")
	assert.NoDirExists(t, e.agentDir)
}

func TestSplitRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		want   []string
	}{
		{
			name:   "repeated values and embedded commas",
			values: []string{"Read, Edit(src/{a,b}/**)", "Write(x,y)", "*(.env*)"},
			want:   []string{"Read", "Edit(src/{a,b}/**)", "Write(x,y)", "*(.env*)"},
		},
		{
			name:   "opening brace in character class",
			values: []string{"Read(file[{]),Edit"},
			want:   []string{"Read(file[{])", "Edit"},
		},
		{
			name:   "closing brace in character class",
			values: []string{"Read(file[}]),Edit"},
			want:   []string{"Read(file[}])", "Edit"},
		},
		{
			name:   "escaped braces",
			values: []string{`Read(file\{),Edit`, `Read(file\}),Write`},
			want:   []string{`Read(file\{)`, "Edit", `Read(file\})`, "Write"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, splitRules(tc.values))
		})
	}
}

func TestRootRejectsInvalidRuleInFlag(t *testing.T) {
	for _, flag := range []string{"--allowed-tools", "--disallowed-tools"} {
		t.Run(flag, func(t *testing.T) {
			e := newTestEnv(t)

			_, _, err := execute(t, "", "--model", mockModel, flag, "Read,Nope(ls)", "-p", "hi")

			require.Error(t, err)
			assert.Contains(t, err.Error(), flag+": ")
			assert.Contains(t, err.Error(), `unknown tool "Nope"`)
			assert.NoDirExists(t, e.agentDir)
		})
	}
}

func TestRootRejectsInvalidRuleInConfig(t *testing.T) {
	e := newTestEnv(t)
	configFile := filepath.Join(e.kopiHome, "config.json")
	e.writeFile(t, configFile, `{"permissions":{"deny":["Nope"]}}`)

	_, _, err := execute(t, "", "--model", mockModel, "-p", "hi")

	require.Error(t, err)
	assert.Contains(t, err.Error(), configFile)
	assert.Contains(t, err.Error(), `unknown tool "Nope"`)
	assert.NoDirExists(t, e.agentDir)
}

func TestRootRejectsPathRuleForBash(t *testing.T) {
	e := newTestEnv(t)

	_, _, err := execute(t, "", "--model", mockModel, "--allowed-tools", "Bash(ls)", "-p", "hi")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "takes no path")
	assert.NoDirExists(t, e.agentDir)
}

// builtinModel names one of PiG's builtin models for provider.
func builtinModel(t *testing.T, provider string) string {
	t.Helper()
	models := ai.GetBuiltinModels(provider)
	require.NotEmpty(t, models)
	return provider + "/" + models[0].ID
}

// openai-codex is OAuth-only, so with an empty home it has no credential;
// Open fails at the credential check, before any provider request.
func TestRootWithoutCredentialSuggestsLogin(t *testing.T) {
	newTestEnv(t)

	_, _, err := execute(t, "", "--model", builtinModel(t, "openai-codex"), "-p", "hi")

	require.Error(t, err)
	assert.Equal(t, `no credential for provider "openai-codex": run "kopi login openai-codex"`, err.Error())
}

func TestRootWithoutCredentialForProviderWithoutLogin(t *testing.T) {
	newTestEnv(t)
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	for _, provider := range []string{"mistral", "openai"} {
		_, _, err := execute(t, "", "--model", builtinModel(t, provider), "-p", "hi")

		require.Error(t, err)
		assert.Contains(t, err.Error(), fmt.Sprintf("no credential for provider %q", provider))
		assert.Contains(t, err.Error(), "API key in the environment")
		assert.NotContains(t, err.Error(), "login")
	}
}

func TestRootUnknownModelNamesWhereItIsSet(t *testing.T) {
	e := newTestEnv(t)

	_, _, err := execute(t, "", "--model", "nosuch/model-1", "-p", "hi")
	require.Error(t, err)
	assert.Equal(t, `unknown model "nosuch/model-1": check --model`, err.Error())

	e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), `{"model":"nosuch/model-1"}`)
	_, _, err = execute(t, "", "-p", "hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Join(e.kopiHome, "config.json"))
}

func TestRootReportsMissingSession(t *testing.T) {
	e := newTestEnv(t)
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "unused") })
	e.writeMockModels(t, server.URL)

	_, _, err := execute(t, "", "--model", mockModel, "--resume", "no-such-id", "-p", "hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no session "no-such-id"`)

	_, _, err = execute(t, "", "--model", mockModel, "--continue", "-p", "hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no session to continue")
}

const mockModel = "kopi-mock/mock-1"

// writeMockModels configures a custom OpenAI-compatible provider in PiG's
// models.json under Kopi's agent dir. The apiKey is a literal: PiG treats a
// value without "$" or a leading "!" as the key itself.
func (e testEnv) writeMockModels(t *testing.T, baseURL string) {
	t.Helper()
	models := map[string]any{"providers": map[string]any{"kopi-mock": map[string]any{
		"baseUrl": baseURL + "/v1",
		"api":     "openai-completions",
		"apiKey":  syntheticKey,
		"models":  []any{map[string]any{"id": "mock-1", "name": "Kopi mock"}},
	}}}
	b, err := json.Marshal(models)
	require.NoError(t, err)
	e.writeFile(t, filepath.Join(e.agentDir, "models.json"), string(b))
}

type mockRequest struct {
	path, authorization string
	body                map[string]any
}

// newMockProvider serves chat completions with respond and records each
// request.
func newMockProvider(t *testing.T, respond func(http.ResponseWriter)) *mockServer {
	t.Helper()
	m := &mockServer{requests: make(chan mockRequest, 16)}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}
		m.requests <- mockRequest{path: r.URL.Path, authorization: r.Header.Get("Authorization"), body: body}
		respond(w)
	}))
	t.Cleanup(m.Close)
	return m
}

type mockServer struct {
	*httptest.Server
	requests chan mockRequest
}

func (m *mockServer) next(t *testing.T) mockRequest {
	t.Helper()
	select {
	case req := <-m.requests:
		return req
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for a provider request")
		return mockRequest{}
	}
}

// writeChunk writes one OpenAI chat-completions streaming chunk.
func writeChunk(w http.ResponseWriter, delta map[string]any, finish any) {
	b, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1, "model": "mock-1",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	fmt.Fprintf(w, "data: %s\n\n", b)
}

// streamText answers in the OpenAI chat-completions streaming format.
func streamText(w http.ResponseWriter, parts ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	writeChunk(w, map[string]any{"role": "assistant", "content": ""}, nil)
	for _, p := range parts {
		writeChunk(w, map[string]any{"content": p}, nil)
	}
	writeChunk(w, map[string]any{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// streamReads answers with one Read tool call per path.
func streamReads(w http.ResponseWriter, paths ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	writeChunk(w, map[string]any{"role": "assistant", "content": ""}, nil)
	for i, p := range paths {
		args, _ := json.Marshal(map[string]any{"file_path": p})
		writeChunk(w, map[string]any{"tool_calls": []any{map[string]any{
			"index": i, "id": fmt.Sprintf("call_%d", i), "type": "function",
			"function": map[string]any{"name": "Read", "arguments": string(args)},
		}}}, nil)
	}
	writeChunk(w, map[string]any{}, "tool_calls")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func TestRootStreamsTurnFromMockServer(t *testing.T) {
	e := newTestEnv(t)
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "Hello from ", "the mock server.") })
	e.writeMockModels(t, server.URL)

	stdout, stderr, err := execute(t, "", "--model", mockModel, "-p", "say hello")

	require.NoError(t, err)
	assert.Equal(t, "Hello from the mock server.\n", stdout)
	req := server.next(t)
	assert.Equal(t, "/v1/chat/completions", req.path)
	assert.Equal(t, "Bearer "+syntheticKey, req.authorization)
	var tools []string
	for _, tl := range req.body["tools"].([]any) {
		tools = append(tools, tl.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	assert.Equal(t, []string{"Read", "Write", "Edit", "Bash"}, tools)
	messages := req.body["messages"].([]any)
	first := messages[0].(map[string]any)
	assert.Contains(t, []string{"system", "developer"}, first["role"])
	assert.Contains(t, first["content"], "You are Kopi, a coding agent working in")
	assert.NotContains(t, stdout+stderr, syntheticKey)
	assert.NoFileExists(t, filepath.Join(e.kopiHome, "history.jsonl"), "headless runs record no prompt history")
}

func TestRootReportsProviderFailure(t *testing.T) {
	e := newTestEnv(t)
	server := newMockProvider(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"mock rejected the request","type":"invalid_request_error"}}`)
	})
	e.writeMockModels(t, server.URL)

	stdout, stderr, err := execute(t, "", "--model", mockModel, "-p", "say hello")

	require.ErrorIs(t, err, headless.ErrFailed)
	assert.Contains(t, err.Error(), "mock rejected the request")
	server.next(t)
	assert.Empty(t, stdout)
	assert.NotContains(t, stdout+stderr+err.Error(), syntheticKey)
}

// The workspace spans KOPI_HOME, so only the protected list, not the workspace
// boundary, can refuse this read.
func TestRootReadRefusesProtectedPaths(t *testing.T) {
	e := newTestEnv(t)
	t.Chdir(e.root)
	secret := filepath.Join(e.kopiHome, "secret.txt")
	e.writeFile(t, secret, "synthetic kopi secret")
	var calls atomic.Int32
	server := newMockProvider(t, func(w http.ResponseWriter) {
		if calls.Add(1) == 1 {
			streamReads(w, secret)
			return
		}
		streamText(w, "done")
	})
	e.writeMockModels(t, server.URL)

	_, _, err := execute(t, "", "--model", mockModel, "-p", "read them")

	require.NoError(t, err)
	server.next(t)
	var results []string
	for _, m := range server.next(t).body["messages"].([]any) {
		if msg := m.(map[string]any); msg["role"] == "tool" {
			results = append(results, fmt.Sprint(msg["content"]))
		}
	}
	require.Len(t, results, 1)
	assert.Contains(t, results[0], tool.ErrProtected.Error())
	assert.NotContains(t, results[0], "synthetic")
}

// lastUserText is the text of the last user message in a chat-completions
// request body, whose content is a list of parts.
func lastUserText(body map[string]any) string {
	var text string
	for _, m := range body["messages"].([]any) {
		if msg := m.(map[string]any); msg["role"] == "user" {
			text = ""
			for _, part := range msg["content"].([]any) {
				text += fmt.Sprint(part.(map[string]any)["text"])
			}
		}
	}
	return text
}

func TestRootPromptFromStdinAndArgument(t *testing.T) {
	for _, tc := range []struct {
		name, stdin string
		args        []string
		want        string
	}{
		{"stdin", "  piped document\n", nil, "piped document"},
		{"stdin and argument", "piped document\n", []string{"-p", "summarise it"}, "piped document\n\nsummarise it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "ok") })
			e.writeMockModels(t, server.URL)

			stdout, _, err := execute(t, tc.stdin, append([]string{"--model", mockModel}, tc.args...)...)

			require.NoError(t, err)
			assert.Equal(t, "ok\n", stdout)
			assert.Equal(t, tc.want, lastUserText(server.next(t).body))
		})
	}
}

func TestRootWithoutPromptFails(t *testing.T) {
	for _, stdin := range []string{"", " \n"} {
		for _, args := range [][]string{nil, {"-p"}} {
			t.Run(fmt.Sprintf("%q %v", stdin, args), func(t *testing.T) {
				e := newTestEnv(t)
				before := pigruntimetest.Snapshot(t, e.root)

				_, _, err := execute(t, stdin, append([]string{"--model", mockModel}, args...)...)

				require.Error(t, err)
				assert.Contains(t, err.Error(), "no prompt")
				assert.Equal(t, before, pigruntimetest.Snapshot(t, e.root))
				assert.NoDirExists(t, e.agentDir)
			})
		}
	}
}

// streamEdit answers with one Edit call replacing "alpha" with "beta" in path.
func streamEdit(w http.ResponseWriter, path string) {
	w.Header().Set("Content-Type", "text/event-stream")
	writeChunk(w, map[string]any{"role": "assistant", "content": ""}, nil)
	args, _ := json.Marshal(map[string]any{"file_path": path, "old_string": "alpha", "new_string": "beta"})
	writeChunk(w, map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": "call_0", "type": "function",
		"function": map[string]any{"name": "Edit", "arguments": string(args)},
	}}}, nil)
	writeChunk(w, map[string]any{}, "tool_calls")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// printEdit runs kopi -p with flags while the model edits path, a file under
// the workspace holding "alpha\n", and returns the tool result the model got
// back and the file's content afterwards.
func printEdit(t *testing.T, path string, flags ...string) (result, content string) {
	t.Helper()
	return printEditIn(t, newTestEnv(t), path, flags...)
}

func printEditIn(t *testing.T, e testEnv, path string, flags ...string) (result, content string) {
	t.Helper()
	file := filepath.Join(e.workspace, path)
	e.writeFile(t, file, "alpha\n")
	var calls atomic.Int32
	server := newMockProvider(t, func(w http.ResponseWriter) {
		if calls.Add(1) == 1 {
			streamEdit(w, path)
			return
		}
		streamText(w, "done")
	})
	e.writeMockModels(t, server.URL)

	done := make(chan error, 1)
	go func() {
		_, _, err := execute(t, "", append([]string{"--model", mockModel, "-p", "edit it"}, flags...)...)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for the print run to finish")
	}

	server.next(t)
	var results []string
	for _, m := range server.next(t).body["messages"].([]any) {
		if msg := m.(map[string]any); msg["role"] == "tool" {
			results = append(results, fmt.Sprint(msg["content"]))
		}
	}
	require.Len(t, results, 1)
	b, err := os.ReadFile(file)
	require.NoError(t, err)
	return results[0], string(b)
}

func TestRootPrintExecuteWithAllowRuleEdits(t *testing.T) {
	_, content := printEdit(t, "docs/a.md", "--mode", "execute", "--allowed-tools", "Edit(docs/**)")

	assert.Equal(t, "beta\n", content)
}

func TestRootPrintExecuteWithoutAllowRuleDenies(t *testing.T) {
	result, content := printEdit(t, "docs/a.md", "--mode", "execute")

	assert.Contains(t, result, "needs approval")
	assert.Contains(t, result, "--allowed-tools")
	assert.Equal(t, "alpha\n", content)
}

func TestRootPrintExecuteAllowCannotExemptGit(t *testing.T) {
	result, content := printEdit(t, ".git/hooks/pre-commit", "--mode", "execute", "--allowed-tools", "Edit")

	assert.Contains(t, result, "Edit(.git) (built-in)")
	assert.Equal(t, "alpha\n", content)
}

func TestRootPrintStartsInPlanByDefault(t *testing.T) {
	result, content := printEdit(t, "docs/a.md", "--allowed-tools", "Edit(docs/**)")

	assert.Contains(t, result, `tool "Edit" needs execute mode`)
	assert.Equal(t, "alpha\n", content)
}

func TestRootPrintStartsInConfiguredMode(t *testing.T) {
	e := newTestEnv(t)
	e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), `{"mode":"execute"}`)

	_, content := printEditIn(t, e, "docs/a.md", "--allowed-tools", "Edit(docs/**)")

	assert.Equal(t, "beta\n", content)
}

func TestRootModeFlagBeatsConfig(t *testing.T) {
	e := newTestEnv(t)
	e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), `{"mode":"execute"}`)

	result, content := printEditIn(t, e, "docs/a.md", "--mode", "plan", "--allowed-tools", "Edit(docs/**)")

	assert.Contains(t, result, `tool "Edit" needs execute mode`)
	assert.Equal(t, "alpha\n", content)
}

func TestRootDisallowedToolsBeatsAllowedTools(t *testing.T) {
	result, content := printEdit(t, "docs/secret.md", "--mode", "execute", "--allowed-tools", "Edit(docs/**)", "--disallowed-tools", "Edit(docs/secret.md)")

	assert.Contains(t, result, "--disallowed-tools")
	assert.Equal(t, "alpha\n", content)
}

// sandboxAvailable reports whether this machine can run the OS sandbox; it
// cannot on Linux, nor on macOS when the tests themselves run under Seatbelt.
func sandboxAvailable(t *testing.T) bool {
	t.Helper()
	_, err := sandbox.New(t.Context(), sandbox.Policy{})
	if err != nil {
		require.ErrorIs(t, err, sandbox.ErrUnavailable)
	}
	return err == nil
}

// streamBash answers with one Bash call running command.
func streamBash(w http.ResponseWriter, command string) {
	w.Header().Set("Content-Type", "text/event-stream")
	writeChunk(w, map[string]any{"role": "assistant", "content": ""}, nil)
	args, _ := json.Marshal(map[string]any{"command": command})
	writeChunk(w, map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": "call_0", "type": "function",
		"function": map[string]any{"name": "Bash", "arguments": string(args)},
	}}}, nil)
	writeChunk(w, map[string]any{}, "tool_calls")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// printBash runs kopi -p with flags while the model runs command with Bash,
// and returns the tool result the model got back and kopi's stderr.
func printBash(t *testing.T, e testEnv, command string, flags ...string) (result, stderr string) {
	t.Helper()
	var calls atomic.Int32
	server := newMockProvider(t, func(w http.ResponseWriter) {
		if calls.Add(1) == 1 {
			streamBash(w, command)
			return
		}
		streamText(w, "done")
	})
	e.writeMockModels(t, server.URL)

	type outcome struct {
		stderr string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		_, stderr, err := execute(t, "", append([]string{"--model", mockModel, "-p", "run it"}, flags...)...)
		done <- outcome{stderr, err}
	}()
	select {
	case o := <-done:
		require.NoError(t, o.err)
		stderr = o.stderr
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for the print run to finish")
	}

	server.next(t)
	var results []string
	for _, m := range server.next(t).body["messages"].([]any) {
		if msg := m.(map[string]any); msg["role"] == "tool" {
			results = append(results, fmt.Sprint(msg["content"]))
		}
	}
	require.Len(t, results, 1)
	return results[0], stderr
}

func TestRootSandboxEnabledButUnavailableFails(t *testing.T) {
	if sandboxAvailable(t) {
		t.Skip("the sandbox is available here")
	}
	e := newTestEnv(t)
	e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), `{"sandbox":{"enabled":true}}`)
	before := pigruntimetest.Snapshot(t, e.root)

	_, _, err := execute(t, "", "--model", mockModel, "-p", "hi")

	require.ErrorIs(t, err, sandbox.ErrUnavailable)
	assert.Contains(t, err.Error(), "failed to start the sandbox")
	assert.Equal(t, before, pigruntimetest.Snapshot(t, e.root))
}

func TestRootSandboxUnavailableWarns(t *testing.T) {
	if sandboxAvailable(t) {
		t.Skip("the sandbox is available here")
	}
	e := newTestEnv(t)
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "ok") })
	e.writeMockModels(t, server.URL)

	_, stderr, err := execute(t, "", "--model", mockModel, "-p", "hi")

	require.NoError(t, err)
	assert.Contains(t, stderr, "sandbox is unavailable; Bash runs without it and asks before each command")
}

func TestRootSandboxDisabledBashNeedsApproval(t *testing.T) {
	e := newTestEnv(t)
	e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), `{"sandbox":{"enabled":false}}`)

	result, _ := printBash(t, e, "echo hi", "--mode", "execute")

	assert.Contains(t, result, `tool "Bash" needs approval`)
	assert.Contains(t, result, "--allowed-tools")
}

func TestRootSandboxDisabledBashRunsWithAllowRule(t *testing.T) {
	e := newTestEnv(t)
	e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), `{"sandbox":{"enabled":false}}`)

	result, stderr := printBash(t, e, "echo hi", "--mode", "execute", "--allowed-tools", "Bash")

	assert.Equal(t, "hi\n", result)
	assert.NotContains(t, stderr, "sandbox is unavailable")
}

func TestRootSandboxedBashRunsWithoutAllowRule(t *testing.T) {
	if !sandboxAvailable(t) {
		t.Skip("the sandbox is unavailable here")
	}

	result, _ := printBash(t, newTestEnv(t), "echo hi", "--mode", "execute")

	assert.Equal(t, "hi\n", result)
}

func TestRootSandboxedBashRefusesKopiHomeRead(t *testing.T) {
	if !sandboxAvailable(t) {
		t.Skip("the sandbox is unavailable here")
	}
	e := newTestEnv(t)
	secret := filepath.Join(e.kopiHome, "secret's.txt")
	const sentinel = "synthetic kopi Bash secret"
	e.writeFile(t, secret, sentinel+"\n")
	quoted := "'" + strings.ReplaceAll(secret, "'", "'\"'\"'") + "'"
	command := `IFS= read -r secret < ` + quoted + ` && printf '%s\n' "$secret"`

	result, stderr := printBash(t, e, command, "--mode", "execute")

	assert.Contains(t, result, tool.ErrExitStatus.Error())
	assert.Contains(t, result, "exit code")
	assert.NotContains(t, result, "needs approval")
	assert.NotContains(t, result+stderr, sentinel)
}

func TestRootSandboxedBashProtectsGitMetadata(t *testing.T) {
	if !sandboxAvailable(t) {
		t.Skip("the sandbox is unavailable here")
	}
	for _, tc := range []struct {
		name, path, command string
		exists              bool
	}{
		{"no git cannot create HEAD", "HEAD", "printf changed > HEAD", false},
		{"git file cannot be overwritten", ".git", "printf changed > .git", true},
		{"git file cannot be replaced", ".git", "/bin/mv -f replacement .git", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			const original = "gitdir: synthetic-git-dir\n"
			path := filepath.Join(e.workspace, tc.path)
			if tc.exists {
				e.writeFile(t, path, original)
			}
			e.writeFile(t, filepath.Join(e.workspace, "replacement"), "changed\n")

			result, _ := printBash(t, e, tc.command, "--mode", "execute")

			assert.Contains(t, result, tool.ErrExitStatus.Error())
			assert.Contains(t, result, "Operation not permitted")
			if tc.exists {
				content, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, original, string(content))
			} else {
				assert.NoFileExists(t, path)
			}
		})
	}
}

func TestRootSandboxedBashHonorsConfiguredPaths(t *testing.T) {
	if !sandboxAvailable(t) {
		t.Skip("the sandbox is unavailable here")
	}
	e := newTestEnv(t)
	root, err := filepath.EvalSymlinks(e.root)
	require.NoError(t, err)
	// /tmp is always writable; TMPDIR must not cover the external targets.
	for _, temp := range []string{"/tmp", "/private/tmp"} {
		require.False(t, root == temp || strings.HasPrefix(root, temp+"/"), "external targets must be outside %s", temp)
	}
	temp := filepath.Join(root, "tmp")
	require.NoError(t, os.Mkdir(temp, 0o700))
	t.Setenv("TMPDIR", temp)
	allowed := filepath.Join(root, "allowed")
	sibling := filepath.Join(root, "sibling")
	for _, dir := range []string{allowed, sibling} {
		require.NoError(t, os.Mkdir(dir, 0o700))
	}
	secret := filepath.Join(allowed, "denied.txt")
	const sentinel = "synthetic configured Bash secret"
	e.writeFile(t, secret, sentinel+"\n")
	cfg, err := json.Marshal(map[string]any{"sandbox": map[string]any{
		"enabled": true, "allowWrite": []string{allowed}, "denyRead": []string{secret},
	}})
	require.NoError(t, err)
	e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), string(cfg))
	allowedFile := filepath.Join(allowed, "written.txt")
	siblingFile := filepath.Join(sibling, "written.txt")
	quotedAllowed := "'" + strings.ReplaceAll(allowedFile, "'", "'\"'\"'") + "'"
	quotedSibling := "'" + strings.ReplaceAll(siblingFile, "'", "'\"'\"'") + "'"
	quotedSecret := "'" + strings.ReplaceAll(secret, "'", "'\"'\"'") + "'"

	result, _ := printBash(t, e, "printf 'allowed\\n' > "+quotedAllowed+" && printf 'write succeeded\\n'", "--mode", "execute")

	assert.Equal(t, "write succeeded\n", result)
	content, err := os.ReadFile(allowedFile)
	require.NoError(t, err)
	assert.Equal(t, "allowed\n", string(content))

	result, _ = printBash(t, e, "printf changed > "+quotedSibling, "--mode", "execute")

	assert.Contains(t, result, tool.ErrExitStatus.Error())
	assert.Contains(t, result, "Operation not permitted")
	assert.NoFileExists(t, siblingFile)

	result, stderr := printBash(t, e, `IFS= read -r secret < `+quotedSecret+` && printf '%s\n' "$secret"`, "--mode", "execute")

	assert.Contains(t, result, tool.ErrExitStatus.Error())
	assert.Contains(t, result, "Operation not permitted")
	assert.NotContains(t, result+stderr, sentinel)

	result, _ = printBash(t, e, "printf changed > "+quotedSecret, "--mode", "execute")

	assert.Contains(t, result, tool.ErrExitStatus.Error())
	assert.Contains(t, result, "Operation not permitted")
	content, err = os.ReadFile(secret)
	require.NoError(t, err)
	assert.Equal(t, sentinel+"\n", string(content))
}

func TestSandboxPolicy(t *testing.T) {
	home, kopiHome, absolute := t.TempDir(), t.TempDir(), t.TempDir()
	cfg := config.Sandbox{
		AllowWrite: []string{absolute, "~/x", "~", "rel"},
		DenyRead:   []string{absolute, "~/x", "~", "rel"},
		Network:    true,
	}
	gitFiles := func(cwd string) []string {
		var out []string
		for _, name := range []string{"hooks", "config", "commondir", "config.worktree", "worktrees", "modules"} {
			out = append(out, filepath.Join(cwd, ".git", name))
		}
		return out
	}

	t.Run("git directory", func(t *testing.T) {
		cwd := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(cwd, ".git"), 0o755))
		expanded := []string{absolute, filepath.Join(home, "x"), home, filepath.Join(cwd, "rel")}
		write := []string{cwd}
		if filepath.IsAbs("/tmp") {
			write = append(write, "/tmp")
		}
		write = append(write, os.TempDir())

		p := sandboxPolicy(cfg, cwd, home, kopiHome)

		assert.Equal(t, append(write, expanded...), p.Write)
		for _, path := range append(p.Write, p.DenyRead...) {
			assert.True(t, filepath.IsAbs(path), path)
		}
		assert.Equal(t, append([]string{kopiHome}, expanded...), p.DenyRead)
		assert.Equal(t, gitFiles(cwd), p.DenyWrite)
		assert.True(t, p.Network)
	})

	t.Run("git file", func(t *testing.T) {
		cwd := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(cwd, ".git"), []byte("gitdir: /elsewhere\n"), 0o600))

		p := sandboxPolicy(cfg, cwd, home, kopiHome)

		assert.Equal(t, append(gitFiles(cwd), filepath.Join(cwd, ".git")), p.DenyWrite)
	})

	t.Run("no git", func(t *testing.T) {
		cwd := t.TempDir()

		p := sandboxPolicy(cfg, cwd, home, kopiHome)

		assert.Equal(t, append(gitFiles(cwd), filepath.Join(cwd, "HEAD")), p.DenyWrite)
	})

	t.Run("git symlink to a directory", func(t *testing.T) {
		cwd := t.TempDir()
		require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(cwd, ".git")))

		p := sandboxPolicy(cfg, cwd, home, kopiHome)

		assert.Equal(t, gitFiles(cwd), p.DenyWrite)
	})
}

func TestSandboxPolicyPreservesSymlinkParent(t *testing.T) {
	cwd, home, kopiHome := t.TempDir(), t.TempDir(), t.TempDir()
	real := filepath.Join(t.TempDir(), "child")
	require.NoError(t, os.Mkdir(real, 0o700))
	for _, base := range []string{cwd, home} {
		require.NoError(t, os.Symlink(real, filepath.Join(base, "link")))
	}
	for _, tc := range []struct{ name, path, want string }{
		{"relative", "link/../target", cwd + string(filepath.Separator) + "link/../target"},
		{"home", "~/link/../target", home + string(filepath.Separator) + "link/../target"},
		{"absolute", cwd + string(filepath.Separator) + "link/../target", cwd + string(filepath.Separator) + "link/../target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := sandboxPolicy(config.Sandbox{AllowWrite: []string{tc.path}, DenyRead: []string{tc.path}}, cwd, home, kopiHome)
			assert.Equal(t, tc.want, p.Write[len(p.Write)-1])
			assert.Equal(t, tc.want, p.DenyRead[len(p.DenyRead)-1])
		})
	}
}
