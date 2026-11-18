package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/headless"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/sandbox"
	"github.com/keyute/kopi/internal/tool"
	"github.com/keyute/kopi/internal/trust"
	"github.com/keyute/kopi/internal/tui"
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

func TestRootToolSearchRules(t *testing.T) {
	for _, tc := range []struct {
		name, config, flag string
	}{
		{name: "flag allow", flag: "--allowed-tools"},
		{name: "flag deny", flag: "--disallowed-tools"},
		{name: "config allow", config: "allow"},
		{name: "config ask", config: "ask"},
		{name: "config deny", config: "deny"},
	} {
		for _, rule := range []string{"ToolSearch", "ToolSearch(docs/**)"} {
			t.Run(tc.name+"/"+rule, func(t *testing.T) {
				e := newTestEnv(t)
				args := []string{"--model", "nosuch/model-1", "-p", "hi"}
				if tc.flag != "" {
					args = append(args, tc.flag, rule)
				} else {
					e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), fmt.Sprintf(`{"permissions":{%q:[%q]}}`, tc.config, rule))
				}

				_, _, err := execute(t, "", args...)

				require.Error(t, err)
				if rule == "ToolSearch" {
					assert.EqualError(t, err, `unknown model "nosuch/model-1": check --model`)
				} else {
					assert.Contains(t, err.Error(), `tool "ToolSearch" takes no path`)
					assert.NoDirExists(t, e.agentDir)
				}
			})
		}
	}
}

func TestRootRejectsPathRuleForBash(t *testing.T) {
	e := newTestEnv(t)

	_, _, err := execute(t, "", "--model", mockModel, "--allowed-tools", "Bash(ls)", "-p", "hi")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "takes no path")
	assert.NoDirExists(t, e.agentDir)
}

func TestRootRejectsPathRuleForWebFetch(t *testing.T) {
	e := newTestEnv(t)

	_, _, err := execute(t, "", "--model", mockModel, "--allowed-tools", "WebFetch(x)", "-p", "hi")

	require.Error(t, err)
	assert.Contains(t, err.Error(), `tool "WebFetch" takes no path`)
	assert.NoDirExists(t, e.agentDir)
}

func TestRootUnknownSmallModelFailsAtStart(t *testing.T) {
	e := newTestEnv(t)
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "unused") })
	e.writeMockModels(t, server.URL)
	configFile := filepath.Join(e.kopiHome, "config.json")
	e.writeFile(t, configFile, `{"smallModel":"nosuch/model-1"}`)

	_, _, err := execute(t, "", "--model", mockModel, "-p", "hi")

	require.ErrorIs(t, err, pigruntime.ErrUnknownModel)
	assert.Contains(t, err.Error(), `"smallModel" in `+configFile)
	assert.Empty(t, server.requests)
}

// The check on smallModel runs before any session opens, so the credential
// error must still name the login command.
func TestRootSmallModelWithoutCredentialFailsAtStart(t *testing.T) {
	e := newTestEnv(t)
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "unused") })
	e.writeMockModels(t, server.URL)
	configFile := filepath.Join(e.kopiHome, "config.json")
	e.writeFile(t, configFile, fmt.Sprintf(`{"smallModel":%q}`, builtinModel(t, "openai-codex")))

	_, _, err := execute(t, "", "--model", mockModel, "-p", "hi")

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"smallModel" in `+configFile)
	assert.Contains(t, err.Error(), `run "kopi login openai-codex"`)
	assert.NotContains(t, err.Error(), "failed to open session")
	assert.Empty(t, server.requests)
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

func TestRootWithoutCredentialSuggestsLoginOrAPIKey(t *testing.T) {
	newTestEnv(t)
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	for _, provider := range []string{"mistral", "openai"} {
		_, _, err := execute(t, "", "--model", builtinModel(t, provider), "-p", "hi")

		require.Error(t, err)
		assert.Equal(t, fmt.Sprintf(`no credential for provider %q: run "kopi login %s" or set its API key in the environment`, provider, provider), err.Error())
	}
}

func TestDiagnoseProviderWithoutLogin(t *testing.T) {
	err := diagnose(fmt.Errorf("failed to open session: %w", pigruntime.ErrNoCredential), "ambient/model-1", "flags", config.Paths{}, app.OpenRequest{},
		[]pigruntime.Provider{{ID: "ambient"}})

	assert.Equal(t, `no credential for provider "ambient": supply its credential in the environment`, err.Error())
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

func TestRootDiagnosesOpenFailureWhenCredentialsCannotBeListed(t *testing.T) {
	e := newTestEnv(t)
	e.writeFile(t, filepath.Join(e.agentDir, "auth.json"), `{not json`)

	_, _, err := execute(t, "", "--model", "nosuch/model-1", "-p", "hi")

	require.Error(t, err)
	assert.Equal(t, `unknown model "nosuch/model-1": check --model`, err.Error())
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
	assert.Equal(t, []string{"Read", "Write", "Edit", "Bash", "TaskOutput", "TaskStop", "WebFetch"}, tools)
	messages := req.body["messages"].([]any)
	first := messages[0].(map[string]any)
	assert.Contains(t, []string{"system", "developer"}, first["role"])
	assert.Contains(t, first["content"], "You are Kopi, a coding agent working in")
	assert.NotContains(t, stdout+stderr, syntheticKey)
	assert.NoFileExists(t, filepath.Join(e.kopiHome, "history.jsonl"), "headless runs record no prompt history")
}

func TestRootPrintMCPTool(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		t.Run(fmt.Sprintf("allowed=%t", allowed), func(t *testing.T) {
			e := newTestEnv(t)
			e.writeServers(t, filepath.Join(e.kopiHome, "config.json"), map[string]any{"fake": map[string]any{
				"command": os.Args[0],
				"args":    []string{"-test.run=^TestMCPStdioHelper$"},
				"env":     map[string]string{stdioHelperEnv: "serve"},
			}})
			callTool := func(w http.ResponseWriter, id, name, arguments string) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeChunk(w, map[string]any{"role": "assistant", "content": ""}, nil)
				writeChunk(w, map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": arguments},
				}}}, nil)
				writeChunk(w, map[string]any{}, "tool_calls")
				fmt.Fprint(w, "data: [DONE]\n\n")
			}
			var calls atomic.Int32
			server := newMockProvider(t, func(w http.ResponseWriter) {
				switch calls.Add(1) {
				case 1:
					callTool(w, "call_search", "ToolSearch", `{"query":"mcp__fake__search"}`)
				case 2:
					callTool(w, "call_0", "mcp__fake__search", "{}")
				default:
					streamText(w, "done")
				}
			})
			e.writeMockModels(t, server.URL)
			args := []string{"--model", mockModel, "-p", "search"}
			if allowed {
				args = append(args, "--allowed-tools", "mcp__fake__search")
			}

			stdout, _, err := execute(t, "", args...)

			require.NoError(t, err)
			assert.Equal(t, "done\n", stdout)
			declared := func(body map[string]any) []string {
				var tools []string
				for _, tl := range body["tools"].([]any) {
					tools = append(tools, tl.(map[string]any)["function"].(map[string]any)["name"].(string))
				}
				return tools
			}
			first := declared(server.next(t).body)
			assert.Contains(t, first, "ToolSearch")
			assert.NotContains(t, first, "mcp__fake__search")
			assert.Contains(t, declared(server.next(t).body), "mcp__fake__search")
			var results []string
			for _, m := range server.next(t).body["messages"].([]any) {
				if msg := m.(map[string]any); msg["role"] == "tool" && msg["tool_call_id"] == "call_0" {
					results = append(results, fmt.Sprint(msg["content"]))
				}
			}
			require.Len(t, results, 1)
			if allowed {
				assert.Contains(t, results[0], "synthetic MCP result from search")
				assert.NotContains(t, results[0], "needs approval")
			} else {
				assert.Contains(t, results[0], `tool "mcp__fake__search" needs approval`)
				assert.Contains(t, results[0], "--allowed-tools")
				assert.NotContains(t, results[0], "synthetic MCP result")
			}
		})
	}
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

func TestRootPrintExecuteAllowCannotExemptMCPFile(t *testing.T) {
	result, content := printEdit(t, "sub/.mcp.json", "--mode", "execute", "--allowed-tools", "Edit")

	assert.Contains(t, result, "Edit(.mcp.json) (built-in)")
	assert.Equal(t, "alpha\n", content)
}

func TestRootPrintExecuteAllowCannotExemptLinkedMCPFile(t *testing.T) {
	e := newTestEnv(t)
	require.NoError(t, os.Symlink("servers.json", filepath.Join(e.workspace, ".mcp.json")))

	result, content := printEditIn(t, e, "servers.json", "--mode", "execute", "--allowed-tools", "Edit")

	assert.Contains(t, result, "Edit(.mcp.json) (built-in)")
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
	assert.Contains(t, stderr, "sandbox is unavailable; Bash runs without it and needs approval or an allow rule, and plan mode refuses it")
}

func TestRootSandboxDisabledBashNeedsApproval(t *testing.T) {
	e := newTestEnv(t)
	e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), `{"sandbox":{"enabled":false}}`)

	result, _ := printBash(t, e, "echo hi", "--mode", "execute")

	assert.Contains(t, result, `tool "Bash" needs approval`)
	assert.Contains(t, result, "--allowed-tools")
}

func TestRootSandboxUnavailableBashNeedsApproval(t *testing.T) {
	if sandboxAvailable(t) {
		t.Skip("the sandbox is available here")
	}

	result, _ := printBash(t, newTestEnv(t), "echo hi", "--mode", "execute")

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

func TestRootSandboxDisabledPlanRefusesBash(t *testing.T) {
	e := newTestEnv(t)
	e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), `{"sandbox":{"enabled":false}}`)

	result, _ := printBash(t, e, "echo hi", "--mode", "plan", "--allowed-tools", "Bash")

	assert.Contains(t, result, `tool "Bash" needs execute mode; the session is in plan mode, where it runs only in the OS sandbox`)
	assert.NotEqual(t, "hi\n", result)
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

func TestRootSandboxedBashRefusesKopiHomeWrite(t *testing.T) {
	if !sandboxAvailable(t) {
		t.Skip("the sandbox is unavailable here")
	}
	e := newTestEnv(t)
	path := filepath.Join(e.kopiHome, "config.json")
	const original = "{}\n"
	e.writeFile(t, path, original)
	quoted := "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'"

	result, _ := printBash(t, e, "printf changed >> "+quoted, "--mode", "execute")

	assert.Contains(t, result, tool.ErrExitStatus.Error())
	assert.NotContains(t, result, "needs approval")
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, string(content))
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

func TestRootSandboxedBashProtectsMCPFile(t *testing.T) {
	if !sandboxAvailable(t) {
		t.Skip("the sandbox is unavailable here")
	}
	for _, tc := range []struct {
		name, path, command string
		link                string
		exists              bool
	}{
		{"cannot be created", ".mcp.json", "printf changed > .mcp.json", "", false},
		{"cannot be replaced", ".mcp.json", "/bin/mv -f replacement .mcp.json", "", true},
		{"nested cannot be overwritten", "sub/.mcp.json", "printf changed > sub/.mcp.json", "", true},
		{"linked cannot be overwritten", "servers.json", "printf changed > servers.json", "servers.json", true},
		{"linked cannot be replaced", "servers.json", "/bin/mv -f replacement servers.json", "servers.json", true},
		{"linked directory cannot be written", "conf/keep", "printf changed > conf/keep", "conf", true},
		{"linked directory cannot be replaced", "conf/keep", "/bin/mv conf moved", "conf", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			const original = `{"mcpServers":{}}` + "\n"
			path := filepath.Join(e.workspace, tc.path)
			if tc.exists {
				e.writeFile(t, path, original)
			}
			if tc.link != "" {
				require.NoError(t, os.Symlink(tc.link, filepath.Join(e.workspace, ".mcp.json")))
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

func TestRootSandboxedBashCannotWriteWorkspaceInPlan(t *testing.T) {
	if !sandboxAvailable(t) {
		t.Skip("the sandbox is unavailable here")
	}
	e := newTestEnv(t)
	// The workspace lies inside a write root, which must not make it writable.
	cfg, err := json.Marshal(map[string]any{"sandbox": map[string]any{"allowWrite": []string{e.root}}})
	require.NoError(t, err)
	e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), string(cfg))
	wsFile := filepath.Join(e.workspace, "written.txt")
	tempFile := filepath.Join(t.TempDir(), "written.txt")
	quotedWS := "'" + strings.ReplaceAll(wsFile, "'", "'\"'\"'") + "'"
	quotedTemp := "'" + strings.ReplaceAll(tempFile, "'", "'\"'\"'") + "'"

	result, _ := printBash(t, e, "printf changed > "+quotedWS, "--mode", "plan")

	assert.Contains(t, result, tool.ErrExitStatus.Error())
	assert.Contains(t, result, "Operation not permitted")
	assert.NoFileExists(t, wsFile)

	result, _ = printBash(t, e, "printf 'temp\\n' > "+quotedTemp+" && printf 'write succeeded\\n'", "--mode", "plan")

	assert.Equal(t, "write succeeded\n", result)
	content, err := os.ReadFile(tempFile)
	require.NoError(t, err)
	assert.Equal(t, "temp\n", string(content))

	result, _ = printBash(t, e, "printf 'workspace\\n' > "+quotedWS+" && printf 'write succeeded\\n'", "--mode", "execute")

	assert.Equal(t, "write succeeded\n", result)
	content, err = os.ReadFile(wsFile)
	require.NoError(t, err)
	assert.Equal(t, "workspace\n", string(content))
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
		write := []string{cwd, "/tmp", os.TempDir()}

		p := sandboxPolicy(cfg, cwd, home, kopiHome)

		assert.Equal(t, append(write, expanded...), p.Write)
		for _, path := range append(p.Write, p.DenyRead...) {
			assert.True(t, filepath.IsAbs(path), path)
		}
		assert.Equal(t, append([]string{kopiHome}, expanded...), p.DenyRead)
		assert.Equal(t, append(gitFiles(cwd), filepath.Join(cwd, ".mcp.json")), p.DenyWrite)
		assert.Equal(t, []string{".mcp.json"}, p.DenyWriteNames)
		assert.True(t, p.Network)
	})

	t.Run("git file", func(t *testing.T) {
		cwd := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(cwd, ".git"), []byte("gitdir: /elsewhere\n"), 0o600))

		p := sandboxPolicy(cfg, cwd, home, kopiHome)

		assert.Equal(t, append(gitFiles(cwd), filepath.Join(cwd, ".git"), filepath.Join(cwd, ".mcp.json")), p.DenyWrite)
		assert.Equal(t, []string{".mcp.json"}, p.DenyWriteNames)
	})

	t.Run("no git", func(t *testing.T) {
		cwd := t.TempDir()

		p := sandboxPolicy(cfg, cwd, home, kopiHome)

		assert.Equal(t, append(gitFiles(cwd), filepath.Join(cwd, "HEAD"), filepath.Join(cwd, ".mcp.json")), p.DenyWrite)
		assert.Equal(t, []string{".mcp.json"}, p.DenyWriteNames)
	})

	t.Run("git symlink to a directory", func(t *testing.T) {
		cwd := t.TempDir()
		require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(cwd, ".git")))

		p := sandboxPolicy(cfg, cwd, home, kopiHome)

		assert.Equal(t, append(gitFiles(cwd), filepath.Join(cwd, ".mcp.json")), p.DenyWrite)
		assert.Equal(t, []string{".mcp.json"}, p.DenyWriteNames)
	})

	t.Run("plan", func(t *testing.T) {
		cwd := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(cwd, ".git"), 0o755))
		p := sandboxPolicy(cfg, cwd, home, kopiHome)

		plan := planPolicy(p, cwd)

		assert.Equal(t, append(gitFiles(cwd), filepath.Join(cwd, ".mcp.json"), cwd), plan.DenyWrite)
		assert.Equal(t, p.DenyWriteNames, plan.DenyWriteNames)
		assert.Equal(t, p.Write, plan.Write)
		assert.Equal(t, p.DenyRead, plan.DenyRead)
		assert.Equal(t, p.Network, plan.Network)
		plan.DenyWrite[0] = "changed"
		assert.Equal(t, append(gitFiles(cwd), filepath.Join(cwd, ".mcp.json")), p.DenyWrite)
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

// trustFolders is the folders object of the trust file, nil when there is none.
func trustFolders(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var f struct{ Folders map[string]string }
	require.NoError(t, json.Unmarshal(b, &f))
	return f.Folders
}

func canonical(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return p
}

const projectMCP = `{"mcpServers":{"shared":{"command":"npx","args":["-y","docs-mcp"],"env":{"TOKEN":"${TOKEN}"}},"remote":{"type":"http","url":"https://example.com/mcp"}},"other":true}`

var globalServers = map[string]config.MCPServer{
	"shared": {URL: "https://global.example.com/mcp"},
	"solo":   {Command: "solo-mcp"},
}

func noAsk(t *testing.T) askTrust {
	return func(tui.TrustRequest) (tui.TrustChoice, error) {
		t.Error("asked whether to trust the folder")
		return tui.TrustCancelled, nil
	}
}

func TestResolveServersWithoutProjectFile(t *testing.T) {
	e := newTestEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")

	set, err := resolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, noAsk(t))

	require.NoError(t, err)
	assert.Equal(t, globalServers, set.servers)
	assert.Empty(t, set.project)
	assert.Empty(t, set.untrusted)
	assert.Empty(t, set.errs)
	assert.NoFileExists(t, trustFile)

	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), `{"mcpServers":{}}`)
	_, err = resolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, noAsk(t))
	require.NoError(t, err)
	assert.NoFileExists(t, trustFile, "a file declaring no servers needs no decision")
}

func TestResolveServersTrustedProjectReplacesGlobal(t *testing.T) {
	e := newTestEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), projectMCP)
	b, err := json.Marshal(map[string]any{"version": 1, "folders": map[string]string{canonical(t, e.root): "trusted"}})
	require.NoError(t, err)
	e.writeFile(t, trustFile, string(b))

	set, err := resolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, noAsk(t))

	require.NoError(t, err)
	assert.Equal(t, map[string]config.MCPServer{
		"shared": {Command: "npx", Args: []string{"-y", "docs-mcp"}, Env: map[string]string{"TOKEN": "${TOKEN}"}},
		"remote": {Type: "http", URL: "https://example.com/mcp"},
		"solo":   {Command: "solo-mcp"},
	}, set.servers)
	assert.Equal(t, map[string]bool{"shared": true, "remote": true}, set.project)
	assert.Empty(t, set.untrusted)
	assert.False(t, set.undecided)
}

func TestResolveServersUntrustedSkipsProject(t *testing.T) {
	e := newTestEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), projectMCP)
	b, err := json.Marshal(map[string]any{"version": 1, "folders": map[string]string{canonical(t, e.workspace): "untrusted"}})
	require.NoError(t, err)
	e.writeFile(t, trustFile, string(b))

	set, err := resolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, noAsk(t))

	require.NoError(t, err)
	assert.Equal(t, globalServers, set.servers)
	assert.Empty(t, set.project)
	assert.Len(t, set.untrusted, 2)
	assert.False(t, set.undecided)
	assert.Empty(t, set.errs)
}

func TestResolveServersUndecidedWithoutAsk(t *testing.T) {
	e := newTestEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), projectMCP)

	set, err := resolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, nil)

	require.NoError(t, err)
	assert.Equal(t, globalServers, set.servers)
	assert.Len(t, set.untrusted, 2)
	assert.True(t, set.undecided)
	assert.NoFileExists(t, trustFile)
}

func TestResolveServersAsksAndSaves(t *testing.T) {
	for _, tt := range []struct {
		name    string
		choice  tui.TrustChoice
		trusted bool
		saved   func(e testEnv) map[string]string
	}{
		{"folder", tui.TrustFolder, true, func(e testEnv) map[string]string { return map[string]string{canonical(t, e.workspace): "trusted"} }},
		{"parent", tui.TrustParent, true, func(e testEnv) map[string]string { return map[string]string{canonical(t, e.root): "trusted"} }},
		{"deny", tui.TrustDeny, false, func(e testEnv) map[string]string { return map[string]string{canonical(t, e.workspace): "untrusted"} }},
		{"cancel", tui.TrustCancelled, false, func(testEnv) map[string]string { return nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			trustFile := filepath.Join(e.kopiHome, "trust.json")
			e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), projectMCP)
			var asked []tui.TrustRequest
			ask := func(req tui.TrustRequest) (tui.TrustChoice, error) {
				asked = append(asked, req)
				return tt.choice, nil
			}

			set, err := resolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, ask)

			require.NoError(t, err)
			assert.Equal(t, []tui.TrustRequest{{
				Folder:  canonical(t, e.workspace),
				Parent:  canonical(t, e.root),
				Servers: []string{"remote: https://example.com/mcp", "shared: npx -y docs-mcp (env: TOKEN)"},
			}}, asked)
			assert.Equal(t, tt.saved(e), trustFolders(t, trustFile))
			assert.Equal(t, tt.trusted, set.project["shared"])
			assert.Equal(t, !tt.trusted, len(set.untrusted) == 2)
			assert.False(t, set.undecided)
		})
	}
}

// askOnce records the request and cancels, which saves nothing.
func askOnce(asked *tui.TrustRequest) askTrust {
	return func(req tui.TrustRequest) (tui.TrustChoice, error) {
		*asked = req
		return tui.TrustCancelled, nil
	}
}

func TestDecideTrustOffersNoParentAtHomeOrRoot(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	proj := filepath.Join(home, "proj")
	require.NoError(t, os.MkdirAll(proj, 0o755))
	homeLink := filepath.Join(root, "home-link")
	require.NoError(t, os.Symlink(home, homeLink))
	for _, tt := range []struct{ name, cwd, home string }{
		{"home", proj, home},
		{"at home", home, home},
		{"at home through a symlink", homeLink, home},
		{"home with a trailing slash", proj, home + string(filepath.Separator)},
		{"home through a symlink", proj, homeLink},
		{"working directory through a symlinked home", filepath.Join(homeLink, "proj"), home},
		{"root", "/", home},
		{"directly under root", "/usr", home},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var asked tui.TrustRequest
			_, err := decideTrust(nil, tt.cwd, tt.home, map[string]config.MCPServer{"a": {Command: "a"}}, nil, nil, nil, askOnce(&asked))
			require.NoError(t, err)
			assert.Equal(t, canonical(t, tt.cwd), asked.Folder)
			assert.Empty(t, asked.Parent)
		})
	}
}

func TestDecideTrustTreatsHomeTypedInAnotherCaseAsHome(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("case-insensitive folders are checked on darwin")
	}
	home := filepath.Join(canonical(t, t.TempDir()), "home")
	require.NoError(t, os.Mkdir(home, 0o755))
	upper := filepath.Join(filepath.Dir(home), "HOME")
	if _, err := os.Stat(upper); err != nil {
		t.Skip("the temporary directory is on a case-sensitive volume")
	}
	trustFile := filepath.Join(t.TempDir(), "trust.json")
	store, err := trust.Open(trustFile)
	require.NoError(t, err)

	d, err := decideTrust(store, upper, home, map[string]config.MCPServer{"a": {Command: "a"}}, nil, nil, nil, func(tui.TrustRequest) (tui.TrustChoice, error) { return tui.TrustFolder, nil })

	require.NoError(t, err)
	assert.Equal(t, trust.Trusted, d)
	assert.Nil(t, trustFolders(t, trustFile), "an answer at home holds for this run only")
}

func TestServerTargetNamesEnvAndHeadersWithoutValues(t *testing.T) {
	for _, tt := range []struct {
		name   string
		server config.MCPServer
		want   string
	}{
		{"command", config.MCPServer{Command: "npx", Args: []string{"-y", "pkg"}}, "npx -y pkg"},
		{"env", config.MCPServer{Command: "npx", Args: []string{"-y", "pkg"}, Env: map[string]string{"TOKEN": "secret", "NODE_OPTIONS": "--require x"}}, "npx -y pkg (env: NODE_OPTIONS, TOKEN)"},
		{"headers", config.MCPServer{URL: "https://x", Headers: map[string]string{"X-Api-Key": "secret", "Authorization": "Bearer secret"}}, "https://x (headers: Authorization, X-Api-Key)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, serverTarget(tt.server))
		})
	}
}

func TestDecideTrustAtHomeOrRootTrustsForThisRunOnly(t *testing.T) {
	home := canonical(t, t.TempDir())
	for _, tt := range []struct {
		name, cwd string
		choice    tui.TrustChoice
		want      trust.Decision
		saved     map[string]string
	}{
		{"home trusted", home, tui.TrustFolder, trust.Trusted, nil},
		{"root trusted", "/", tui.TrustFolder, trust.Trusted, nil},
		{"home denied", home, tui.TrustDeny, trust.Untrusted, nil},
		{"root denied", "/", tui.TrustDeny, trust.Untrusted, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			trustFile := filepath.Join(t.TempDir(), "trust.json")
			store, err := trust.Open(trustFile)
			require.NoError(t, err)

			d, err := decideTrust(store, tt.cwd, home, map[string]config.MCPServer{"a": {Command: "a"}}, nil, nil, nil, func(tui.TrustRequest) (tui.TrustChoice, error) { return tt.choice, nil })

			require.NoError(t, err)
			assert.Equal(t, tt.want, d)
			assert.Equal(t, tt.saved, trustFolders(t, trustFile))
		})
	}
}

func TestDecideTrustResolvesASymlinkedWorkingDirectory(t *testing.T) {
	e := newTestEnv(t)
	real := filepath.Join(e.root, "real", "proj")
	require.NoError(t, os.MkdirAll(real, 0o755))
	link := filepath.Join(e.root, "links", "proj")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
	require.NoError(t, os.Symlink(real, link))
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	store, err := trust.Open(trustFile)
	require.NoError(t, err)
	var asked tui.TrustRequest
	ask := func(req tui.TrustRequest) (tui.TrustChoice, error) {
		asked = req
		return tui.TrustParent, nil
	}

	d, err := decideTrust(store, link, e.home, map[string]config.MCPServer{"a": {Command: "a"}}, nil, nil, nil, ask)

	require.NoError(t, err)
	assert.Equal(t, trust.Trusted, d)
	assert.Equal(t, canonical(t, real), asked.Folder)
	assert.Equal(t, canonical(t, filepath.Dir(real)), asked.Parent, "the parent of the real folder, not of the link")
	assert.Equal(t, map[string]string{asked.Parent: "trusted"}, trustFolders(t, trustFile), "exactly the path shown is saved")
	again, err := trust.Open(trustFile)
	require.NoError(t, err)
	got, ok, err := again.Lookup(link)
	require.NoError(t, err)
	assert.True(t, ok, "the saved entry covers the folder on the next run")
	assert.Equal(t, trust.Trusted, got)
}

func TestDecideTrustFailsOnAnUnresolvableHome(t *testing.T) {
	_, err := decideTrust(nil, t.TempDir(), filepath.Join(t.TempDir(), "missing"), map[string]config.MCPServer{"a": {Command: "a"}}, nil, nil, nil, noAsk(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to resolve")
}

func TestWaitServersFailsWhenInterrupted(t *testing.T) {
	servers := pigruntime.StartServers(pigruntime.ServerOptions{Servers: map[string]config.MCPServer{"missing": {Command: "kopi-no-such-command-7f3a"}}, CWD: t.TempDir()})
	t.Cleanup(servers.Close)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	statuses, err := waitServers(ctx, servers)

	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "startup was interrupted")
	assert.Nil(t, statuses)
}

func TestRootPrintStripsControlsFromNotices(t *testing.T) {
	e := newTestEnv(t)
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "ok") })
	e.writeMockModels(t, server.URL)
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), `{"mcpServers":{"evil\u001b[31mRED\u001b[0m":{"command":5}}}`)

	_, stderr, err := execute(t, "", "--model", mockModel, "-p", "hi")

	require.NoError(t, err)
	assert.Contains(t, stderr, "failed to decode MCP server")
	assert.Contains(t, stderr, "evil[31mRED[0m")
	assert.NotContains(t, stderr, "\x1b")
}

func TestResolveServersInvalidDeclarations(t *testing.T) {
	for _, decision := range []string{"trusted", "untrusted", "undecided", "ask"} {
		t.Run(decision, func(t *testing.T) {
			e := newTestEnv(t)
			trustFile := filepath.Join(e.kopiHome, "trust.json")
			e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), `{"mcpServers":{"shared":{"command":42},"bad":{"args":false},"healthy":{"command":"healthy-mcp"}}}`)
			if decision == "trusted" || decision == "untrusted" {
				b, err := json.Marshal(map[string]any{"version": 1, "folders": map[string]string{canonical(t, e.workspace): decision}})
				require.NoError(t, err)
				e.writeFile(t, trustFile, string(b))
			}
			var ask askTrust
			if decision == "ask" {
				ask = func(req tui.TrustRequest) (tui.TrustChoice, error) {
					assert.Equal(t, []string{"bad: invalid, will not start", "healthy: healthy-mcp", "shared: invalid, will not start"}, req.Servers)
					return tui.TrustCancelled, nil
				}
			}

			set, err := resolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, ask)

			require.NoError(t, err)
			require.Len(t, set.errs, 2)
			assert.ErrorContains(t, set.errs[0], "MCP server bad: failed to decode")
			assert.ErrorContains(t, set.errs[1], "MCP server shared: failed to decode")
			if decision == "trusted" {
				assert.Equal(t, map[string]config.MCPServer{"solo": {Command: "solo-mcp"}, "healthy": {Command: "healthy-mcp"}}, set.servers)
				assert.Len(t, set.invalid, 2)
				assert.True(t, set.project["shared"])
			} else {
				assert.Equal(t, globalServers, set.servers)
				assert.Len(t, set.untrusted, 3)
				assert.Empty(t, set.invalid)
				assert.Equal(t, decision == "undecided", set.undecided)
			}
		})
	}
}

func TestResolveServersMalformedProjectFileIsANotice(t *testing.T) {
	e := newTestEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), `{"mcpServers":`)

	set, err := resolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, noAsk(t))

	require.NoError(t, err)
	assert.Equal(t, globalServers, set.servers)
	require.Len(t, set.errs, 1)
	assert.ErrorContains(t, set.errs[0], "skipped this folder's MCP servers: failed to parse MCP config "+filepath.Join(e.workspace, ".mcp.json"))
	assert.NoFileExists(t, trustFile)
}

func TestResolveServersAsksForInstructionFiles(t *testing.T) {
	e := newTestEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	files := []string{filepath.Join(e.workspace, "AGENTS.md")}
	var asked []tui.TrustRequest
	ask := func(req tui.TrustRequest) (tui.TrustChoice, error) {
		asked = append(asked, req)
		return tui.TrustFolder, nil
	}

	set, err := resolveServers(globalServers, trustFile, e.workspace, e.home, files, nil, ask)

	require.NoError(t, err)
	assert.Equal(t, []tui.TrustRequest{{Folder: canonical(t, e.workspace), Parent: canonical(t, e.root), Instructions: files}}, asked)
	assert.True(t, set.trusted)
	assert.Equal(t, globalServers, set.servers)
	assert.Equal(t, map[string]string{canonical(t, e.workspace): "trusted"}, trustFolders(t, trustFile))
}

func TestResolveServersInstructionFilesUndecidedWithoutAsk(t *testing.T) {
	e := newTestEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")

	set, err := resolveServers(globalServers, trustFile, e.workspace, e.home, []string{filepath.Join(e.workspace, "AGENTS.md")}, nil, nil)

	require.NoError(t, err)
	assert.True(t, set.undecided)
	assert.False(t, set.trusted)
	assert.Empty(t, set.untrusted)
	assert.NoFileExists(t, trustFile)
}

func TestResolveServersMalformedProjectFileStillAsksForInstructionFiles(t *testing.T) {
	e := newTestEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), `{"mcpServers":`)
	files := []string{filepath.Join(e.workspace, "AGENTS.md")}
	var asked tui.TrustRequest
	ask := func(req tui.TrustRequest) (tui.TrustChoice, error) {
		asked = req
		return tui.TrustFolder, nil
	}

	set, err := resolveServers(globalServers, trustFile, e.workspace, e.home, files, nil, ask)

	require.NoError(t, err)
	assert.Equal(t, files, asked.Instructions)
	assert.Empty(t, asked.Servers)
	assert.True(t, set.trusted)
	assert.Equal(t, globalServers, set.servers)
	assert.Empty(t, set.project)
	require.Len(t, set.errs, 1)
	assert.ErrorContains(t, set.errs[0], "skipped this folder's MCP servers: failed to parse MCP config")
}

func TestResolveServersWithoutServersOrInstructionsLeavesTheTrustFileAlone(t *testing.T) {
	e := newTestEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	e.writeFile(t, trustFile, `{"version":1,"folders":{"/x":"maybe"}}`)

	set, err := resolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, noAsk(t))

	require.NoError(t, err, "an invalid trust file is not opened")
	assert.False(t, set.trusted)
	assert.False(t, set.undecided)
}

func TestResolveServersInvalidTrustFileIsFatal(t *testing.T) {
	e := newTestEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), projectMCP)
	e.writeFile(t, trustFile, `{"version":1,"folders":{"/x":"maybe"}}`)

	_, err := resolveServers(globalServers, trustFile, e.workspace, e.home, nil, nil, noAsk(t))

	require.Error(t, err)
	assert.Contains(t, err.Error(), trustFile)
}

func TestRootPrintWarnsOnceInUndecidedFolder(t *testing.T) {
	e := newTestEnv(t)
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "ok") })
	e.writeMockModels(t, server.URL)
	e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), `{"mcpServers":{"proj":{"command":"kopi-no-such-command-7f3a"}}}`)

	stdout, stderr, err := execute(t, "", "--model", mockModel, "-p", "hi")

	require.NoError(t, err)
	assert.Equal(t, "ok\n", stdout)
	assert.Equal(t, 1, strings.Count(stderr, "this folder is not trusted, so the MCP servers in its .mcp.json are skipped; run kopi here in a terminal to be asked whether to trust it"), stderr)
	assert.NotContains(t, stderr, "kopi-no-such-command-7f3a", "the project's server is not started")
	assert.NoFileExists(t, filepath.Join(e.kopiHome, "trust.json"))
}

func TestRootPrintLogsServerFailures(t *testing.T) {
	e := newTestEnv(t)
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "ok") })
	e.writeMockModels(t, server.URL)
	e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), `{"mcpServers":{"broken":{"command":"kopi-no-such-command-7f3a"}}}`)

	_, stderr, err := execute(t, "", "--model", mockModel, "-p", "hi")

	require.NoError(t, err)
	assert.Contains(t, stderr, "MCP server broken failed to start: ")
	assert.Contains(t, stderr, "kopi-no-such-command-7f3a")
}

// systemText is the system message of a provider request.
func systemText(t *testing.T, req mockRequest) string {
	t.Helper()
	first := req.body["messages"].([]any)[0].(map[string]any)
	require.Contains(t, []string{"system", "developer"}, first["role"])
	return first["content"].(string)
}

func (e testEnv) writeTrust(t *testing.T, dir, decision string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"version": 1, "folders": map[string]string{canonical(t, dir): decision}})
	require.NoError(t, err)
	e.writeFile(t, filepath.Join(e.kopiHome, "trust.json"), string(b))
}

func TestRootPrintLoadsTheGlobalInstructionFile(t *testing.T) {
	e := newTestEnv(t)
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "ok") })
	e.writeMockModels(t, server.URL)
	e.writeFile(t, filepath.Join(e.kopiHome, "AGENTS.md"), "global rule 51c2")

	_, stderr, err := execute(t, "", "--model", mockModel, "-p", "hi")

	require.NoError(t, err)
	assert.Contains(t, systemText(t, server.next(t)), "global rule 51c2")
	assert.NotContains(t, stderr, "not trusted")
	assert.NoFileExists(t, filepath.Join(e.kopiHome, "trust.json"))
}

func TestRootPrintSkipsInstructionFilesInAnUndecidedFolder(t *testing.T) {
	for _, tt := range []struct {
		name, servers, warning string
	}{
		{"instruction files", "", "this folder is not trusted, so its instruction files are skipped; run kopi here in a terminal to be asked whether to trust it"},
		{"servers and instruction files", `{"mcpServers":{"proj":{"command":"kopi-no-such-command-7f3a"}}}`, "this folder is not trusted, so the MCP servers in its .mcp.json and its instruction files are skipped; run kopi here in a terminal to be asked whether to trust it"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "ok") })
			e.writeMockModels(t, server.URL)
			e.writeFile(t, filepath.Join(e.kopiHome, "AGENTS.md"), "global rule 51c2")
			e.writeFile(t, filepath.Join(e.workspace, "AGENTS.md"), "project rule 8e07")
			if tt.servers != "" {
				e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), tt.servers)
			}

			_, stderr, err := execute(t, "", "--model", mockModel, "-p", "hi")

			require.NoError(t, err)
			prompt := systemText(t, server.next(t))
			assert.Contains(t, prompt, "global rule 51c2")
			assert.NotContains(t, prompt, "project rule 8e07")
			assert.Equal(t, 1, strings.Count(stderr, tt.warning), stderr)
			assert.Equal(t, 1, strings.Count(stderr, "not trusted"), stderr)
			assert.NoFileExists(t, filepath.Join(e.kopiHome, "trust.json"))
		})
	}
}

func TestRootPrintLoadsInstructionFilesOfATrustedFolder(t *testing.T) {
	e := newTestEnv(t)
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "ok") })
	e.writeMockModels(t, server.URL)
	e.writeFile(t, filepath.Join(e.kopiHome, "AGENTS.md"), "global rule 51c2")
	e.writeFile(t, filepath.Join(e.workspace, "AGENTS.md"), "project rule 8e07")
	e.writeTrust(t, e.workspace, "trusted")

	_, _, err := execute(t, "", "--model", mockModel, "-p", "hi")

	require.NoError(t, err)
	prompt := systemText(t, server.next(t))
	global, project := strings.Index(prompt, "global rule 51c2"), strings.Index(prompt, "project rule 8e07")
	require.GreaterOrEqual(t, global, 0)
	assert.Greater(t, project, global)
}

func TestRootPrintSkipsInstructionFilesOfAnUntrustedFolderSilently(t *testing.T) {
	e := newTestEnv(t)
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "ok") })
	e.writeMockModels(t, server.URL)
	e.writeFile(t, filepath.Join(e.workspace, "AGENTS.md"), "project rule 8e07")
	e.writeTrust(t, e.workspace, "untrusted")

	_, stderr, err := execute(t, "", "--model", mockModel, "-p", "hi")

	require.NoError(t, err)
	assert.NotContains(t, systemText(t, server.next(t)), "project rule 8e07")
	assert.NotContains(t, stderr, "not trusted")
}

func TestRootPrintInstructionFallback(t *testing.T) {
	for _, tt := range []struct {
		name, config string
		agents       bool
		want, absent []string
	}{
		{name: "AGENTS.md wins", agents: true, want: []string{"agents rule 2b4d"}, absent: []string{"claude rule 6f1a"}},
		{name: "fallback without AGENTS.md", want: []string{"claude rule 6f1a"}},
		{name: "fallback off", config: `{"instructionFallbacks":[]}`, absent: []string{"claude rule 6f1a"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "ok") })
			e.writeMockModels(t, server.URL)
			if tt.config != "" {
				e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), tt.config)
			}
			if tt.agents {
				e.writeFile(t, filepath.Join(e.workspace, "AGENTS.md"), "agents rule 2b4d")
			}
			e.writeFile(t, filepath.Join(e.workspace, "CLAUDE.md"), "claude rule 6f1a")
			e.writeTrust(t, e.workspace, "trusted")

			_, _, err := execute(t, "", "--model", mockModel, "-p", "hi")

			require.NoError(t, err)
			prompt := systemText(t, server.next(t))
			for _, s := range tt.want {
				assert.Contains(t, prompt, s)
			}
			for _, s := range tt.absent {
				assert.NotContains(t, prompt, s)
			}
		})
	}
}

func TestRootPrintLoadsInstructionFilesFromTheGitRootDown(t *testing.T) {
	e := newTestEnv(t)
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "ok") })
	e.writeMockModels(t, server.URL)
	e.writeFile(t, filepath.Join(e.workspace, ".git"), "gitdir: /elsewhere/.git/worktrees/ws\n")
	e.writeFile(t, filepath.Join(e.workspace, "AGENTS.md"), "root rule 4c19")
	sub := filepath.Join(e.workspace, "sub")
	e.writeFile(t, filepath.Join(sub, "AGENTS.md"), "sub rule 93ad")
	e.writeTrust(t, e.workspace, "trusted")
	t.Chdir(sub)

	_, _, err := execute(t, "", "--model", mockModel, "-p", "hi")

	require.NoError(t, err)
	prompt := systemText(t, server.next(t))
	root, below := strings.Index(prompt, "root rule 4c19"), strings.Index(prompt, "sub rule 93ad")
	require.GreaterOrEqual(t, root, 0)
	assert.Greater(t, below, root)
}

func TestRootPrintWalksInstructionFilesOnTheTrustedPath(t *testing.T) {
	e := newTestEnv(t)
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "ok") })
	e.writeMockModels(t, server.URL)
	good, evil := filepath.Join(e.root, "code", "good"), filepath.Join(e.root, "code", "evil")
	require.NoError(t, os.MkdirAll(filepath.Join(good, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(evil, ".git"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(good, "pkg"), 0o755))
	e.writeFile(t, filepath.Join(good, "AGENTS.md"), "good rule 1d7e")
	e.writeFile(t, filepath.Join(evil, "AGENTS.md"), "evil rule 0b3f")
	require.NoError(t, os.Symlink(filepath.Join("..", "good", "pkg"), filepath.Join(evil, "sub")))
	e.writeTrust(t, good, "trusted")
	t.Chdir(filepath.Join(evil, "sub"))
	wd, err := os.Getwd()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(evil, "sub"), wd, "the process sees the symlinked path")

	_, _, err = execute(t, "", "--model", mockModel, "-p", "hi")

	require.NoError(t, err)
	prompt := systemText(t, server.next(t))
	assert.Contains(t, prompt, "good rule 1d7e")
	assert.NotContains(t, prompt, "evil rule 0b3f")
}

func (e testEnv) writeSkill(t *testing.T, skillsDir, name, description, body string) {
	t.Helper()
	e.writeFile(t, filepath.Join(skillsDir, name, "SKILL.md"), fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n%s\n", name, description, body))
}

// messageTexts is each message's content of a provider request, in order.
func messageTexts(t *testing.T, req mockRequest) []string {
	t.Helper()
	var texts []string
	for _, m := range req.body["messages"].([]any) {
		switch content := m.(map[string]any)["content"].(type) {
		case string:
			texts = append(texts, content)
		case []any:
			var parts []string
			for _, p := range content {
				parts = append(parts, p.(map[string]any)["text"].(string))
			}
			texts = append(texts, strings.Join(parts, ""))
		}
	}
	return texts
}

func declaredTools(req mockRequest) []string {
	var names []string
	for _, tl := range req.body["tools"].([]any) {
		names = append(names, tl.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	return names
}

// printWithMock runs print mode against a mock provider that answers "ok" and
// returns the first request it received.
func printWithMock(t *testing.T, e testEnv, args ...string) (req mockRequest, stderr string) {
	t.Helper()
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "ok") })
	e.writeMockModels(t, server.URL)
	_, stderr, err := execute(t, "", append([]string{"--model", mockModel}, args...)...)
	require.NoError(t, err)
	return server.next(t), stderr
}

func TestResolveServersAsksForSkillFolders(t *testing.T) {
	e := newTestEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")
	folders := []string{filepath.Join(e.workspace, ".agents", "skills")}
	var asked []tui.TrustRequest
	ask := func(req tui.TrustRequest) (tui.TrustChoice, error) {
		asked = append(asked, req)
		return tui.TrustFolder, nil
	}

	set, err := resolveServers(globalServers, trustFile, e.workspace, e.home, nil, folders, ask)

	require.NoError(t, err)
	assert.Equal(t, []tui.TrustRequest{{Folder: canonical(t, e.workspace), Parent: canonical(t, e.root), Skills: folders}}, asked)
	assert.True(t, set.trusted)
	assert.Equal(t, map[string]string{canonical(t, e.workspace): "trusted"}, trustFolders(t, trustFile))
}

func TestResolveServersSkillFoldersUndecidedWithoutAsk(t *testing.T) {
	e := newTestEnv(t)
	trustFile := filepath.Join(e.kopiHome, "trust.json")

	set, err := resolveServers(globalServers, trustFile, e.workspace, e.home, nil, []string{filepath.Join(e.workspace, ".agents", "skills")}, nil)

	require.NoError(t, err)
	assert.True(t, set.undecided)
	assert.False(t, set.trusted)
	assert.NoFileExists(t, trustFile)
}

func TestSkippedList(t *testing.T) {
	assert.Equal(t, "A are", skippedList([]string{"A"}))
	assert.Equal(t, "A and B are", skippedList([]string{"A", "B"}))
	assert.Equal(t, "A, B and C are", skippedList([]string{"A", "B", "C"}))
}

func TestRootPrintSkipsSkillsInAnUndecidedFolder(t *testing.T) {
	const tail = " skipped; run kopi here in a terminal to be asked whether to trust it"
	for _, tt := range []struct {
		name         string
		servers      bool
		instructions bool
		warning      string
	}{
		{"skills", false, false, "this folder is not trusted, so its skills are" + tail},
		{"skills and instruction files", false, true, "this folder is not trusted, so its instruction files and its skills are" + tail},
		{"servers and skills", true, false, "this folder is not trusted, so the MCP servers in its .mcp.json and its skills are" + tail},
		{"all three", true, true, "this folder is not trusted, so the MCP servers in its .mcp.json, its instruction files and its skills are" + tail},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.writeSkill(t, filepath.Join(e.workspace, ".agents", "skills"), "bar", "project skill", "project body 6a1c")
			if tt.servers {
				e.writeFile(t, filepath.Join(e.workspace, ".mcp.json"), `{"mcpServers":{"proj":{"command":"kopi-no-such-command-7f3a"}}}`)
			}
			if tt.instructions {
				e.writeFile(t, filepath.Join(e.workspace, "AGENTS.md"), "project rule 8e07")
			}

			req, stderr := printWithMock(t, e, "-p", "/skill:bar go")

			assert.Equal(t, 1, strings.Count(stderr, tt.warning), stderr)
			assert.Equal(t, 1, strings.Count(stderr, "not trusted"), stderr)
			assert.NotContains(t, declaredTools(req), "Skill")
			assert.NotContains(t, strings.Join(messageTexts(t, req), "\n"), "project body 6a1c")
			assert.NoFileExists(t, filepath.Join(e.kopiHome, "trust.json"))
		})
	}
}

func TestRootPrintLoadsAKopiHomeSkillWithThePrompt(t *testing.T) {
	e := newTestEnv(t)
	e.writeSkill(t, filepath.Join(e.kopiHome, "skills"), "foo", "a skill", "foo body 3d9e")

	req, stderr := printWithMock(t, e, "-p", "/skill:foo do")

	texts := messageTexts(t, req)
	require.Len(t, texts, 3, texts)
	assert.Equal(t, "/skill:foo do", texts[1])
	assert.Contains(t, texts[2], `<skill name="foo"`)
	assert.Contains(t, texts[2], "foo body 3d9e")
	assert.Contains(t, declaredTools(req), "Skill")
	assert.NotContains(t, stderr, "skill")
}

func TestRootPrintWithoutSkillsHasNoSkillTool(t *testing.T) {
	e := newTestEnv(t)

	req, _ := printWithMock(t, e, "-p", "hi")

	assert.NotContains(t, declaredTools(req), "Skill")
}

func TestRootPrintLoadsAUserHomeSkill(t *testing.T) {
	e := newTestEnv(t)
	e.writeSkill(t, filepath.Join(e.home, ".agents", "skills"), "baz", "a user skill", "baz body 21f0")

	req, _ := printWithMock(t, e, "-p", "/skill:baz go")

	assert.Contains(t, messageTexts(t, req)[2], "baz body 21f0")
	assert.Contains(t, declaredTools(req), "Skill")
}

func TestRootPrintKopiHomeSkillBeatsUserHomeSkill(t *testing.T) {
	e := newTestEnv(t)
	e.writeSkill(t, filepath.Join(e.kopiHome, "skills"), "foo", "kopi", "kopi body 5b2a")
	e.writeSkill(t, filepath.Join(e.home, ".agents", "skills"), "foo", "user", "user body 77c4")

	req, _ := printWithMock(t, e, "-p", "/skill:foo go")

	text := strings.Join(messageTexts(t, req), "\n")
	assert.Contains(t, text, "kopi body 5b2a")
	assert.NotContains(t, text, "user body 77c4")
}

func TestRootPrintTrustedProjectSkillBeatsUserSkill(t *testing.T) {
	e := newTestEnv(t)
	e.writeSkill(t, filepath.Join(e.workspace, ".agents", "skills"), "bar", "project", "project body 6a1c")
	e.writeSkill(t, filepath.Join(e.home, ".agents", "skills"), "bar", "user", "user body 77c4")
	e.writeTrust(t, e.workspace, "trusted")

	req, stderr := printWithMock(t, e, "-p", "/skill:bar go")

	text := strings.Join(messageTexts(t, req), "\n")
	assert.Contains(t, text, "project body 6a1c")
	assert.NotContains(t, text, "user body 77c4")
	assert.NotContains(t, stderr, "not trusted")
}

func TestRootPrintUntrustedProjectSkillIsNotLoaded(t *testing.T) {
	e := newTestEnv(t)
	e.writeSkill(t, filepath.Join(e.workspace, ".agents", "skills"), "bar", "project", "project body 6a1c")
	e.writeSkill(t, filepath.Join(e.home, ".agents", "skills"), "bar", "user", "user body 77c4")
	e.writeTrust(t, e.workspace, "untrusted")

	req, stderr := printWithMock(t, e, "-p", "/skill:bar go")

	text := strings.Join(messageTexts(t, req), "\n")
	assert.NotContains(t, text, "project body 6a1c")
	assert.Contains(t, text, "user body 77c4")
	assert.NotContains(t, stderr, "not trusted", "a decided folder is skipped silently")
}

func TestRootPrintSkillsInTheHomeDirectoryAreNotProjectSkills(t *testing.T) {
	e := newTestEnv(t)
	e.writeSkill(t, filepath.Join(e.home, ".agents", "skills"), "baz", "a user skill", "baz body 21f0")
	t.Chdir(e.home)

	req, stderr := printWithMock(t, e, "-p", "/skill:baz go")

	assert.Contains(t, messageTexts(t, req)[2], "baz body 21f0")
	assert.NotContains(t, stderr, "not trusted")
	assert.NoFileExists(t, filepath.Join(e.kopiHome, "trust.json"))
}

func TestRootPrintSkillPrefixFromConfig(t *testing.T) {
	e := newTestEnv(t)
	e.writeSkill(t, filepath.Join(e.kopiHome, "skills"), "foo", "a skill", "foo body 3d9e")
	e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), `{"skills":{"prefix":""}}`)

	req, _ := printWithMock(t, e, "-p", "/foo do")

	texts := messageTexts(t, req)
	require.Len(t, texts, 3, texts)
	assert.Equal(t, "/foo do", texts[1])
	assert.Contains(t, texts[2], "foo body 3d9e")
}

func TestRootPrintReportsSkillProblemsAndRuns(t *testing.T) {
	e := newTestEnv(t)
	e.writeFile(t, filepath.Join(e.kopiHome, "skills", "broken", "SKILL.md"), "---\nname: broken\n---\nno description\n")
	e.writeSkill(t, filepath.Join(e.kopiHome, "skills"), "foo", "a skill", "foo body 3d9e")

	req, stderr := printWithMock(t, e, "-p", "/skill:foo go")

	assert.Contains(t, stderr, "broken")
	assert.Contains(t, strings.Join(messageTexts(t, req), "\n"), "foo body 3d9e")
}

func TestRootSkillRules(t *testing.T) {
	for _, flag := range []string{"--allowed-tools", "--disallowed-tools"} {
		t.Run(flag, func(t *testing.T) {
			newTestEnv(t)

			_, _, err := execute(t, "", "--model", "nosuch/model-1", "-p", "hi", flag, "Skill")

			assert.EqualError(t, err, `unknown model "nosuch/model-1": check --model`)
		})
	}
	t.Run("path", func(t *testing.T) {
		newTestEnv(t)

		_, _, err := execute(t, "", "--model", "nosuch/model-1", "-p", "hi", "--allowed-tools", "Skill(x)")

		assert.EqualError(t, err, `unknown model "nosuch/model-1": check --model`)
	})
}

func streamSkill(w http.ResponseWriter, args map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	writeChunk(w, map[string]any{"role": "assistant", "content": ""}, nil)
	raw, _ := json.Marshal(args)
	writeChunk(w, map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": "call_0", "type": "function",
		"function": map[string]any{"name": "Skill", "arguments": string(raw)},
	}}}, nil)
	writeChunk(w, map[string]any{}, "tool_calls")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func TestRootPathRuleDeniesASkillFile(t *testing.T) {
	e := newTestEnv(t)
	e.writeSkill(t, filepath.Join(e.kopiHome, "skills"), "foo", "a skill", "foo body 3d9e")
	e.writeFile(t, filepath.Join(e.kopiHome, "skills", "foo", "secret.md"), "secret body 9c41")
	var calls atomic.Int32
	server := newMockProvider(t, func(w http.ResponseWriter) {
		if calls.Add(1) == 1 {
			streamSkill(w, map[string]any{"name": "foo", "file": "secret.md"})
			return
		}
		streamText(w, "done")
	})
	e.writeMockModels(t, server.URL)

	_, _, err := execute(t, "", "--model", mockModel, "-p", "read it", "--disallowed-tools", "*("+filepath.Join(e.kopiHome, "skills", "foo", "secret.md")+")")

	require.NoError(t, err)
	server.next(t)
	second := server.next(t)
	var results []string
	for _, m := range second.body["messages"].([]any) {
		if msg := m.(map[string]any); msg["role"] == "tool" {
			results = append(results, fmt.Sprint(msg["content"]))
		}
	}
	require.Len(t, results, 1)
	assert.Contains(t, results[0], "--disallowed-tools")
	assert.NotContains(t, fmt.Sprint(second.body), "secret body 9c41")
}

func TestRootDisallowedSkillRemovesTheTool(t *testing.T) {
	e := newTestEnv(t)
	e.writeSkill(t, filepath.Join(e.kopiHome, "skills"), "foo", "a skill", "foo body 3d9e")

	req, _ := printWithMock(t, e, "-p", "hi", "--disallowed-tools", "Skill")

	assert.NotContains(t, declaredTools(req), "Skill")
}

func TestRootRejectsInvalidSkillPrefix(t *testing.T) {
	e := newTestEnv(t)
	e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), `{"skills":{"prefix":"Bad Prefix"}}`)

	_, _, err := execute(t, "", "--model", mockModel, "-p", "hi")

	require.Error(t, err)
	assert.ErrorContains(t, err, "skills.prefix")
}

func TestConfigShowsSkillsPrefix(t *testing.T) {
	newTestEnv(t)

	stdout, _, err := execute(t, "", "config")

	require.NoError(t, err)
	assert.Regexp(t, `skills\.prefix\s+= skill\s+\(default\)`, stdout)
}
