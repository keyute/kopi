package pigruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MichaelKinsy/PiG/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest/mcpfake"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

// startFakes starts a server per fake, each configured as a stdio command the
// dial replaces, and returns them with a function that ends startup.
func startFakes(t *testing.T, fakes map[string]*mcpfake.Server) (*Servers, context.CancelFunc) {
	t.Helper()
	servers := map[string]config.MCPServer{}
	for name := range fakes {
		servers[name] = config.MCPServer{Command: "fake"}
	}
	return startWith(t, servers, func(name string, _ config.MCPServer) (mcp.Transport, error) {
		return fakes[name].Connect(), nil
	})
}

func startWith(t *testing.T, servers map[string]config.MCPServer, dial dialFunc) (*Servers, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := startServers(ctx, cancel, ServerOptions{Servers: servers, Version: "1.2.3"}, dial)
	t.Cleanup(s.Close)
	return s, cancel
}

func echo(_ string, args json.RawMessage) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(args)}}}
}

func echoTool(name string) map[string]any {
	return map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}}
}

func call(t *testing.T, tl tool.Tool, args string) (string, error) {
	t.Helper()
	res, err := tl.Run(t.Context(), json.RawMessage(args), func(policy.Target) (policy.Mode, error) { return policy.Execute, nil })
	return res.Text, err
}

func findTool(t *testing.T, st ServerStatus, name string) tool.Tool {
	t.Helper()
	for _, tl := range st.Tools {
		if tl.Name == name {
			return tl
		}
	}
	require.Failf(t, "tool not found", "%s in %v", name, toolNames(st))
	return tool.Tool{}
}

func toolNames(st ServerStatus) []string {
	var names []string
	for _, tl := range st.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func TestServersRoundTrip(t *testing.T) {
	fake := &mcpfake.Server{
		Instructions: "Use docs for library questions.",
		Tools: []map[string]any{
			{"name": "search", "description": "Search the docs.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}},
			{"name": "fetch", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}},
			{"name": "write", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": false}},
		},
		Reply: echo,
	}
	s, _ := startFakes(t, map[string]*mcpfake.Server{"docs": fake})

	statuses := s.Wait(t.Context())
	require.Len(t, statuses, 1)
	st := statuses[0]
	require.NoError(t, st.Err)
	assert.Equal(t, "docs", st.Name)
	assert.Equal(t, "Use docs for library questions.", st.Instructions)
	assert.Equal(t, []string{"mcp__docs__fetch", "mcp__docs__search", "mcp__docs__write"}, toolNames(st))
	assert.Empty(t, st.Skipped)
	assert.Equal(t, mcp.Implementation{Name: "kopi", Version: "1.2.3"}, fake.ClientInfo())

	search := findTool(t, st, "mcp__docs__search")
	assert.Equal(t, "Search the docs.", search.Description)
	assert.Equal(t, map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}, search.Parameters)
	assert.Equal(t, policy.Exec, search.Capability)
	assert.Equal(t, policy.Read, findTool(t, st, "mcp__docs__fetch").Capability)
	assert.Equal(t, policy.Exec, findTool(t, st, "mcp__docs__write").Capability)

	out, err := call(t, search, `{"q":"go"}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"q":"go"}`, out)
	assert.Same(t, &statuses[0], &s.Wait(t.Context())[0])
}

func TestParametersNormalised(t *testing.T) {
	empty := map[string]any{"type": "object", "properties": map[string]any{}}
	for _, schema := range []string{"", "null", "[]", `"x"`} {
		assert.Equal(t, empty, parameters(json.RawMessage(schema)), schema)
	}
	assert.Equal(t, map[string]any{"type": "object", "properties": map[string]any{}}, parameters(json.RawMessage(`{"type":"object"}`)))
}

func TestServersSkipLongAndDuplicateNames(t *testing.T) {
	long := strings.Repeat("x", tool.MCPNameLimit)
	fake := &mcpfake.Server{Tools: []map[string]any{echoTool("a.b"), echoTool(long), echoTool("a_b"), echoTool("ok")}}
	s, _ := startFakes(t, map[string]*mcpfake.Server{"srv": fake})

	st := s.Wait(t.Context())[0]
	require.NoError(t, st.Err)
	assert.Equal(t, []string{"mcp__srv__a_b", "mcp__srv__ok"}, toolNames(st))
	assert.Equal(t, []string{
		fmt.Sprintf("%s: name longer than 64 characters", long),
		"a_b: same name as a.b",
	}, st.Skipped)
	assert.Equal(t, "Tool a.b of MCP server srv.", st.Tools[0].Description)
}

func TestServersErrorResult(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"short", "no such page"},
		{"empty", ""},
		{"oversized", "head" + strings.Repeat("x", 100000) + "tail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &mcpfake.Server{
				Tools: []map[string]any{echoTool("fail")},
				Reply: func(string, json.RawMessage) map[string]any {
					return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": tc.text}}}
				},
			}
			s, _ := startFakes(t, map[string]*mcpfake.Server{"srv": fake})

			out, err := call(t, s.Wait(t.Context())[0].Tools[0], `{}`)
			require.ErrorIs(t, err, ErrToolError)
			assert.Empty(t, out)
			switch tc.name {
			case "empty":
				assert.Equal(t, ErrToolError.Error(), err.Error())
			case "short":
				assert.Equal(t, ErrToolError.Error()+": "+tc.text, err.Error())
			case "oversized":
				assert.True(t, strings.HasPrefix(err.Error(), ErrToolError.Error()+": head"))
				assert.True(t, strings.HasSuffix(err.Error(), "tail"))
				assert.Contains(t, err.Error(), "bytes cut")
				assert.Less(t, len(err.Error()), len(tc.text))
			}
		})
	}
}

func TestServersNonTextResult(t *testing.T) {
	fake := &mcpfake.Server{
		Tools: []map[string]any{echoTool("shot")},
		Reply: func(string, json.RawMessage) map[string]any {
			return map[string]any{"content": []any{
				map[string]any{"type": "image", "data": "aGk=", "mimeType": "image/png"},
				map[string]any{"type": "text", "text": "taken"},
			}}
		},
	}
	s, _ := startFakes(t, map[string]*mcpfake.Server{"srv": fake})

	out, err := call(t, s.Wait(t.Context())[0].Tools[0], `{}`)
	require.NoError(t, err)
	assert.Equal(t, "[image]\ntaken", out)
}

func TestServersWithoutToolsCapability(t *testing.T) {
	fake := &mcpfake.Server{NoTools: true, Instructions: "hi", Tools: []map[string]any{echoTool("hidden")}}
	s, _ := startFakes(t, map[string]*mcpfake.Server{"srv": fake})

	st := s.Wait(t.Context())[0]
	require.NoError(t, st.Err)
	assert.Empty(t, st.Tools)
	assert.Equal(t, "hi", st.Instructions)
}

func TestServersConfigFailuresStayPerServer(t *testing.T) {
	t.Setenv("KOPI_TEST_UNSET_VAR", "")
	require.NoError(t, os.Unsetenv("KOPI_TEST_UNSET_VAR"))
	good := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}}
	servers := map[string]config.MCPServer{
		"a.b":   {Command: "fake"},
		"a_b":   {Command: "fake"},
		"good":  {Command: "fake"},
		"sse":   {Type: "sse", URL: "https://example.invalid/sse"},
		"unset": {Command: "${KOPI_TEST_UNSET_VAR}"},
	}
	s, _ := startWith(t, servers, func(name string, _ config.MCPServer) (mcp.Transport, error) {
		if name == "good" || name == "a.b" {
			return good.Connect(), nil
		}
		return nil, fmt.Errorf("unexpected dial of %s", name)
	})

	statuses := s.Wait(t.Context())
	require.Len(t, statuses, 5)
	byName := map[string]ServerStatus{}
	for _, st := range statuses {
		byName[st.Name] = st
	}
	assert.Equal(t, []string{"a.b", "a_b", "good", "sse", "unset"}, []string{statuses[0].Name, statuses[1].Name, statuses[2].Name, statuses[3].Name, statuses[4].Name})

	require.NoError(t, byName["a.b"].Err)
	require.ErrorIs(t, byName["a_b"].Err, ErrServerNameClash)
	assert.Contains(t, byName["a_b"].Err.Error(), `"a.b"`)
	require.NoError(t, byName["good"].Err)
	assert.Len(t, byName["good"].Tools, 1)
	require.ErrorIs(t, byName["sse"].Err, config.ErrInvalidValue)
	assert.Contains(t, byName["sse"].Err.Error(), "sse")
	require.ErrorIs(t, byName["unset"].Err, config.ErrUnsetVariable)
	assert.Contains(t, byName["unset"].Err.Error(), "KOPI_TEST_UNSET_VAR")
}

func TestServersValidateExpandedConfig(t *testing.T) {
	t.Setenv("KOPI_TEST_EMPTY_VAR", "")
	servers := map[string]config.MCPServer{
		"url":     {URL: "${KOPI_TEST_EMPTY_VAR:-}"},
		"http":    {Type: "http", URL: "${KOPI_TEST_EMPTY_VAR}", Headers: map[string]string{"A": "b"}},
		"command": {Command: "${KOPI_TEST_EMPTY_VAR}", Args: []string{"x"}},
	}
	s, _ := startWith(t, servers, func(name string, _ config.MCPServer) (mcp.Transport, error) {
		return nil, fmt.Errorf("unexpected dial of %s", name)
	})

	statuses := s.Wait(t.Context())
	require.Len(t, statuses, 3)
	for _, st := range statuses {
		require.ErrorIs(t, st.Err, config.ErrInvalidValue, st.Name)
		assert.Contains(t, st.Err.Error(), "failed to validate expanded config", st.Name)
		assert.Contains(t, st.Err.Error(), "want command or url", st.Name)
	}
}

func TestServersDialFailureStaysPerServer(t *testing.T) {
	good := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}}
	servers := map[string]config.MCPServer{"bad": {Command: "fake"}, "good": {Command: "fake"}}
	s, _ := startWith(t, servers, func(name string, _ config.MCPServer) (mcp.Transport, error) {
		if name == "bad" {
			return nil, errors.New("refused")
		}
		return good.Connect(), nil
	})

	statuses := s.Wait(t.Context())
	assert.ErrorContains(t, statuses[0].Err, "refused")
	require.NoError(t, statuses[1].Err)
	assert.Len(t, statuses[1].Tools, 1)
}

func TestServersAbandonedAtDeadline(t *testing.T) {
	fast := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}}
	stuck := &mcpfake.Server{Hang: true}
	s, endStartup := startFakes(t, map[string]*mcpfake.Server{"fast": fast, "stuck": stuck})

	<-s.servers[0].done
	endStartup()
	statuses := s.Wait(t.Context())
	require.NoError(t, statuses[0].Err)
	assert.Len(t, statuses[0].Tools, 1)
	require.ErrorIs(t, statuses[1].Err, ErrServerStartTimeout)
	<-stuck.Closed()
}

func TestServersWaitHonoursContext(t *testing.T) {
	stuck := &mcpfake.Server{Hang: true}
	s, _ := startFakes(t, map[string]*mcpfake.Server{"stuck": stuck})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	statuses := s.Wait(ctx)
	require.ErrorIs(t, statuses[0].Err, ErrServerStartTimeout)
	<-stuck.Closed()
}

func TestServersWaitAfterStartupCancellation(t *testing.T) {
	for _, method := range []string{"initialize", "notifications/initialized", "tools/list"} {
		t.Run(method, func(t *testing.T) {
			entered := make(chan struct{})
			fake := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}}
			dial := scriptedHTTP(t, fake, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost {
					return nil, nil
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				r.Body = io.NopCloser(strings.NewReader(string(raw)))
				msg, err := mcp.ParseJSONRPCMessage(raw)
				if err != nil {
					return nil, err
				}
				if msg.Method == method {
					close(entered)
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return nil, nil
			})
			t.Setenv("KOPI_TEST_URL_KEY", "synthetic-secret")
			s, cancel := startWith(t, map[string]config.MCPServer{
				"srv": {Type: "http", URL: "https://example.invalid/${KOPI_TEST_URL_KEY}"},
			}, dial)
			<-entered
			cancel()
			<-s.servers[0].done
			require.ErrorIs(t, s.servers[0].status.Err, context.Canceled)
			var redacted *redactedError
			require.ErrorAs(t, s.servers[0].status.Err, &redacted)

			statuses := s.Wait(t.Context())
			require.Len(t, statuses, 1)
			assert.Equal(t, "srv", statuses[0].Name)
			require.ErrorIs(t, statuses[0].Err, ErrServerStartTimeout)
			assert.NotErrorIs(t, statuses[0].Err, context.Canceled)
			assert.Same(t, &statuses[0], &s.Wait(t.Context())[0])
		})
	}
}

// startupExpiry lets the test signal a startup deadline through a channel.
type startupExpiry struct{ context.Context }

func (c startupExpiry) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

func TestServersWaitDoesNotJoinTeardown(t *testing.T) {
	for _, trigger := range []string{"caller cancellation", "startup expiry"} {
		t.Run(trigger, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
				defer stop()
				listed := make(chan struct{})
				closing := make(chan struct{})
				release := make(chan struct{})
				var releaseOnce sync.Once
				var deletes atomic.Int32
				fake := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}, Reply: echo}
				dial := scriptedHTTP(t, fake, func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodDelete {
						if deletes.Add(1) == 1 {
							close(closing)
						}
						<-release
						return nil, nil
					}
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					r.Body = io.NopCloser(strings.NewReader(string(raw)))
					msg, err := mcp.ParseJSONRPCMessage(raw)
					if err != nil {
						return nil, err
					}
					if msg.Method == "tools/list" {
						close(listed)
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return nil, nil
				})
				startup, expire := context.WithCancel(context.Background())
				defer expire()
				s := startServers(startupExpiry{startup}, expire, ServerOptions{Servers: map[string]config.MCPServer{
					"good": {Command: "fake"}, "stuck": {Command: "fake"},
				}}, func(name string, cfg config.MCPServer) (mcp.Transport, error) {
					if name == "good" {
						return fake.Connect(), nil
					}
					return dial(name, cfg)
				})
				t.Cleanup(s.Close)
				t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
				<-s.servers[0].done
				<-listed
				waitCtx, cancelWait := context.WithCancel(ctx)
				defer cancelWait()
				if trigger == "startup expiry" {
					expire()
				} else {
					cancelWait()
				}
				waited := make(chan []ServerStatus, 1)
				go func() { waited <- s.Wait(waitCtx) }()
				var statuses []ServerStatus
				select {
				case statuses = <-waited:
				case <-ctx.Done():
					t.Fatal("Wait blocked on transport teardown")
				}
				require.NoError(t, statuses[0].Err)
				require.ErrorIs(t, statuses[1].Err, ErrServerStartTimeout)
				assert.Empty(t, statuses[1].Tools)
				out, err := call(t, statuses[0].Tools[0], `{"still":"usable"}`)
				require.NoError(t, err)
				assert.JSONEq(t, `{"still":"usable"}`, out)
				<-closing

				closed := make(chan struct{}, 3)
				go func() {
					s.Close()
					closed <- struct{}{}
				}()
				synctest.Wait()
				select {
				case <-closed:
					t.Fatal("Close returned before transport teardown finished")
				default:
				}
				assert.Same(t, &statuses[0], &s.Wait(ctx)[0])
				for range 2 {
					go func() {
						s.Close()
						closed <- struct{}{}
					}()
				}
				releaseOnce.Do(func() { close(release) })
				for range 3 {
					select {
					case <-closed:
					case <-ctx.Done():
						t.Fatal("Close did not join transport teardown")
					}
				}
				s.Close()
				assert.Equal(t, int32(1), deletes.Load())
				assert.Same(t, &statuses[0], &s.Wait(ctx)[0])
				assert.ErrorIs(t, statuses[1].Err, ErrServerStartTimeout)
				assert.Empty(t, statuses[1].Tools)
				for _, srv := range s.servers {
					select {
					case <-srv.done:
					default:
						t.Error("Close did not join startup worker")
					}
				}
			})
		})
	}
}

func TestServersWaitPreservesSettledStatusAfterCancellation(t *testing.T) {
	failure := errors.New("refused")
	fake := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}, Reply: echo}
	s, cancel := startWith(t, map[string]config.MCPServer{
		"bad": {Command: "fake"}, "good": {Command: "fake"},
	}, func(name string, _ config.MCPServer) (mcp.Transport, error) {
		if name == "bad" {
			return nil, failure
		}
		return fake.Connect(), nil
	})
	for _, srv := range s.servers {
		<-srv.done
	}
	cancel()

	statuses := s.Wait(t.Context())
	require.Len(t, statuses, 2)
	require.ErrorIs(t, statuses[0].Err, failure)
	assert.NotErrorIs(t, statuses[0].Err, ErrServerStartTimeout)
	require.NoError(t, statuses[1].Err)
	assert.Equal(t, []string{"mcp__good__t"}, toolNames(statuses[1]))
	out, err := call(t, statuses[1].Tools[0], `{"after":"cancellation"}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"after":"cancellation"}`, out)
	assert.Same(t, &statuses[0], &s.Wait(t.Context())[0])
}

func TestServersReconnectAfterDeath(t *testing.T) {
	fake := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}, Reply: echo}
	dials := 0
	servers := map[string]config.MCPServer{"srv": {Command: "fake"}}
	s, _ := startWith(t, servers, func(string, config.MCPServer) (mcp.Transport, error) {
		dials++
		if dials == 3 {
			return nil, errors.New("refused")
		}
		return fake.Connect(), nil
	})
	tl := s.Wait(t.Context())[0].Tools[0]

	fake.Kill()
	out, err := call(t, tl, `{"n":1}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"n":1}`, out)
	assert.Equal(t, 2, fake.Connections())

	fake.Kill()
	_, err = call(t, tl, `{}`)
	require.ErrorContains(t, err, "failed to reconnect")
	assert.ErrorContains(t, err, "refused")
}

func TestServersCloseBeforeWaitAndTwice(t *testing.T) {
	stuck := &mcpfake.Server{Hang: true}
	ok := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}}
	s, _ := startFakes(t, map[string]*mcpfake.Server{"ok": ok, "stuck": stuck})

	s.Close()
	s.Close()
	<-stuck.Closed()
	statuses := s.Wait(t.Context())
	require.Len(t, statuses, 2)
	require.Error(t, statuses[1].Err)
}

func TestServersCallAfterClose(t *testing.T) {
	fake := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}}
	s, _ := startFakes(t, map[string]*mcpfake.Server{"srv": fake})
	tl := s.Wait(t.Context())[0].Tools[0]

	s.Close()
	<-fake.Closed()
	_, err := call(t, tl, `{}`)
	require.ErrorIs(t, err, ErrServerClosed)
}

const stdioHelperEnv = "KOPI_MCP_STDIO_HELPER"

// TestMCPStdioHelper is not a test: the stdio tests run the test binary as a
// server through it.
func TestMCPStdioHelper(t *testing.T) {
	switch os.Getenv(stdioHelperEnv) {
	case "serve":
		fake := &mcpfake.Server{Tools: []map[string]any{echoTool("echo")}, Reply: echo}
		if err := fake.ServeStdio(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "crash":
		fmt.Fprintln(os.Stderr, "starting")
		fmt.Fprintln(os.Stderr, "boom: missing token")
		os.Exit(1)
	default:
		t.Skip("helper process for the stdio tests")
	}
}

func stdioHelper(mode string) config.MCPServer {
	return config.MCPServer{Command: os.Args[0], Args: []string{"-test.run=^TestMCPStdioHelper$"}, Env: map[string]string{stdioHelperEnv: mode}}
}

func TestStdioTransportLifecycle(t *testing.T) {
	for _, order := range []string{"close before start", "start before close", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			transport, err := dialTransport(t.TempDir())("srv", stdioHelper("serve"))
			require.NoError(t, err)
			stdio, ok := transport.(*stdioTransport)
			require.True(t, ok)
			t.Cleanup(func() { assert.NoError(t, stdio.Close()) })
			_, ordered := transport.(interface {
				SendOrdered(context.Context, mcp.JSONRPCMessage, func()) error
			})
			require.True(t, ordered)

			switch order {
			case "close before start":
				require.NoError(t, stdio.Close())
				require.Error(t, stdio.Start())
			case "start before close":
				require.NoError(t, stdio.Start())
				require.Positive(t, stdio.PID())
				require.NoError(t, stdio.Close())
			case "concurrent":
				// Exercise the lifecycle boundary, not PiG's internal spawn interleaving.
				begin := make(chan struct{})
				started := make(chan error, 1)
				closed := make(chan error, 1)
				go func() {
					<-begin
					started <- stdio.Start()
				}()
				go func() {
					<-begin
					closed <- stdio.Close()
				}()
				close(begin)
				require.NoError(t, <-closed)
				if err := <-started; err != nil {
					var connectionClosed *mcp.McpConnectionClosedError
					require.ErrorAs(t, err, &connectionClosed)
				}
			}
			assert.Zero(t, stdio.PID())
			require.NoError(t, stdio.Close())
			require.Error(t, stdio.Start())
		})
	}
}

func TestServersStdio(t *testing.T) {
	missing := "kopi-no-such-command-7f3a"
	s := StartServers(ServerOptions{
		Servers: map[string]config.MCPServer{
			"crash":   stdioHelper("crash"),
			"echo":    stdioHelper("serve"),
			"missing": {Command: missing},
		},
		CWD:     t.TempDir(),
		Version: "1.2.3",
	})
	t.Cleanup(s.Close)

	statuses := s.Wait(t.Context())
	require.Len(t, statuses, 3)
	require.Error(t, statuses[0].Err)
	assert.Contains(t, statuses[0].Err.Error(), "boom: missing token")
	require.NoError(t, statuses[1].Err)
	require.Error(t, statuses[2].Err)
	assert.Contains(t, statuses[2].Err.Error(), missing)

	out, err := call(t, findTool(t, statuses[1], "mcp__echo__echo"), `{"x":"y"}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"x":"y"}`, out)
}

func TestServersRejectAmbiguousServerName(t *testing.T) {
	first := &mcpfake.Server{Tools: []map[string]any{echoTool("b__x"), echoTool("y")}}
	second := &mcpfake.Server{Tools: []map[string]any{echoTool("x"), echoTool("z")}}
	s, _ := startFakes(t, map[string]*mcpfake.Server{"a": first, "a__b": second})

	statuses := s.Wait(t.Context())
	assert.Equal(t, []string{"mcp__a__b__x", "mcp__a__y"}, toolNames(statuses[0]))
	assert.Empty(t, statuses[0].Skipped)
	assert.ErrorIs(t, statuses[1].Err, tool.ErrInvalidMCPServerName)
	assert.Empty(t, statuses[1].Tools)
	assert.Empty(t, statuses[1].Skipped)
	assert.Zero(t, second.Connections())
	assert.Equal(t, statuses, s.Wait(t.Context()))
}

func TestServersRedactExpandedURL(t *testing.T) {
	const secret = "s3cr\tet-7f3a"
	t.Setenv("KOPI_TEST_URL_KEY", secret)
	configured := "http://127.0.0.1:9/mcp?key=${KOPI_TEST_URL_KEY}"
	servers := map[string]config.MCPServer{
		"invalid": {Type: "http", URL: "http://bad host/${KOPI_TEST_URL_KEY}"},
		"refused": {Type: "http", URL: configured},
		"plain":   {Type: "http", URL: "http://bad host/plain"},
	}
	real := dialTransport(t.TempDir())
	s, _ := startWith(t, servers, func(name string, c config.MCPServer) (mcp.Transport, error) {
		if name == "refused" {
			return nil, &url.Error{Op: "Post", URL: c.URL, Err: errors.New("connection refused")}
		}
		return real(name, c)
	})

	statuses := s.Wait(t.Context())
	for _, st := range statuses {
		require.Error(t, st.Err, st.Name)
		assert.NotContains(t, st.Err.Error(), "s3cr", st.Name)
	}
	assert.Contains(t, statuses[0].Err.Error(), "http://bad host/${KOPI_TEST_URL_KEY}")
	assert.Contains(t, statuses[1].Err.Error(), "http://bad host/plain")
	assert.Contains(t, statuses[2].Err.Error(), `"`+configured+`"`)
	var ue *url.Error
	require.ErrorAs(t, statuses[2].Err, &ue)
}

func TestServersInvalidNamesFailBeforeDial(t *testing.T) {
	for _, name := range []string{"", "github__other", "github_", "github.", "github..other", "github._other"} {
		t.Run(name, func(t *testing.T) {
			fake := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}}
			s, _ := startWith(t, map[string]config.MCPServer{name: {Command: "fake"}, "safe": {Command: "fake"}}, func(got string, _ config.MCPServer) (mcp.Transport, error) {
				require.Equal(t, "safe", got)
				return fake.Connect(), nil
			})
			for _, st := range s.Wait(t.Context()) {
				if st.Name == "safe" {
					require.NoError(t, st.Err)
				} else {
					require.ErrorIs(t, st.Err, tool.ErrInvalidMCPServerName)
				}
			}
		})
	}
}

type mcpFetchFunc func(*http.Request) (*http.Response, error)

func (f mcpFetchFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// scriptedHTTP uses the same wire replies as the in-memory fake, without sockets.
func scriptedHTTP(t *testing.T, fake *mcpfake.Server, intercept mcpFetchFunc) dialFunc {
	t.Helper()
	return func(string, config.MCPServer) (mcp.Transport, error) {
		noGET := false
		return mcp.NewStreamableHTTPTransport(mcp.StreamableHTTPTransportOptions{
			URL: "https://example.invalid/mcp", OpenGetStream: &noGET,
			Fetch: mcpFetchFunc(func(r *http.Request) (*http.Response, error) {
				if intercept != nil {
					if res, err := intercept(r); res != nil || err != nil {
						return res, err
					}
				}
				res := &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}
				if r.Method == http.MethodDelete {
					return res, nil
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				msg, err := mcp.ParseJSONRPCMessage(raw)
				if err != nil {
					return nil, err
				}
				if reply := fake.Answer(msg); reply != nil {
					body, err := json.Marshal(reply)
					if err != nil {
						return nil, err
					}
					res.StatusCode = http.StatusOK
					res.Header.Set("Content-Type", "application/json")
					res.Header.Set("Mcp-Session-Id", "synthetic-session")
					res.Body = io.NopCloser(strings.NewReader(string(body)))
				}
				return res, nil
			}),
		})
	}
}

func TestServersCancelInitializedHTTP(t *testing.T) {
	for _, action := range []string{"cancel startup", "close", "cancel reconnect", "close reconnect"} {
		t.Run(action, func(t *testing.T) {
			entered := make(chan struct{})
			fake := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}, Reply: echo}
			var notifications atomic.Int32
			reconnect := strings.Contains(action, "reconnect")
			dial := scriptedHTTP(t, fake, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost {
					return nil, nil
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				r.Body = io.NopCloser(strings.NewReader(string(raw)))
				msg, err := mcp.ParseJSONRPCMessage(raw)
				if err != nil {
					return nil, err
				}
				if msg.Method == "notifications/initialized" && (notifications.Add(1) > 1 || !reconnect) {
					close(entered)
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return nil, nil
			})
			s, cancel := startWith(t, map[string]config.MCPServer{"srv": {Command: "fake"}}, dial)
			callDone := make(chan error, 1)
			var cancelCall context.CancelFunc
			if reconnect {
				tl := s.Wait(t.Context())[0].Tools[0]
				s.servers[0].invalidate(s.servers[0].client)
				ctx, stop := context.WithCancel(t.Context())
				cancelCall = stop
				defer stop()
				go func() {
					_, err := tl.Run(ctx, json.RawMessage(`{}`), func(policy.Target) (policy.Mode, error) { return policy.Execute, nil })
					callDone <- err
				}()
			}
			<-entered
			switch {
			case strings.HasPrefix(action, "close"):
				s.Close()
			case reconnect:
				cancelCall()
			default:
				cancel()
			}
			if reconnect {
				require.Error(t, <-callDone)
			}
			s.Close()
		})
	}
}

func TestServersHTTPExpiredSessionReconnectsWithoutReplay(t *testing.T) {
	var calls atomic.Int32
	var initializes atomic.Int32
	fake := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}, Reply: echo}
	dial := scriptedHTTP(t, fake, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			return nil, nil
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		msg, err := mcp.ParseJSONRPCMessage(raw)
		if err != nil {
			return nil, err
		}
		if msg.Method == "initialize" {
			initializes.Add(1)
		}
		if msg.Method == "tools/call" && calls.Add(1) == 1 {
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("expired"))}, nil
		}
		return nil, nil
	})
	s, _ := startWith(t, map[string]config.MCPServer{"srv": {Command: "fake"}}, dial)
	tl := s.Wait(t.Context())[0].Tools[0]
	_, err := call(t, tl, `{}`)
	var expired *mcp.McpSessionExpiredError
	require.ErrorAs(t, err, &expired)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, int32(1), initializes.Load())
	out, err := call(t, tl, `{"next":true}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"next":true}`, out)
	assert.Equal(t, int32(2), calls.Load())
	assert.Equal(t, int32(2), initializes.Load())
}

func TestServersCloseJoinsExpiredSessionTeardown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		closing := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		var calls atomic.Int32
		var deletes atomic.Int32
		fake := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}, Reply: echo}
		dial := scriptedHTTP(t, fake, func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodDelete {
				if deletes.Add(1) == 1 {
					close(closing)
				}
				<-release
				return nil, nil
			}
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			msg, err := mcp.ParseJSONRPCMessage(raw)
			if err != nil {
				return nil, err
			}
			if msg.Method == "tools/call" {
				calls.Add(1)
				return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("expired"))}, nil
			}
			return nil, nil
		})
		s, _ := startWith(t, map[string]config.MCPServer{"srv": {Command: "fake"}}, dial)
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		st := s.Wait(t.Context())[0]
		require.NoError(t, st.Err)
		called := make(chan error, 1)
		go func() {
			_, err := call(t, st.Tools[0], `{}`)
			called <- err
		}()
		<-closing
		// Mutex waits are not durably blocked in synctest; check ownership
		// before starting Close, which must acquire the same mutex.
		if s.servers[0].mu.TryLock() {
			s.servers[0].mu.Unlock()
			t.Fatal("expired session teardown released lifecycle ownership")
		}

		closed := make(chan struct{}, 3)
		for range 3 {
			go func() {
				s.Close()
				closed <- struct{}{}
			}()
		}
		<-s.servers[0].life.Done()
		select {
		case <-closed:
			t.Fatal("Close returned before expired session teardown finished")
		default:
		}
		releaseOnce.Do(func() { close(release) })
		for range 3 {
			<-closed
		}
		var expired *mcp.McpSessionExpiredError
		require.ErrorAs(t, <-called, &expired)
		s.Close()
		assert.Equal(t, int32(1), calls.Load())
		assert.Equal(t, int32(1), deletes.Load())
	})
}

func TestServersEstablishedHTTPCallCancellationAndTimeout(t *testing.T) {
	for _, action := range []string{"cancel", "timeout"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				entered := make(chan struct{})
				release := make(chan struct{})
				var releaseOnce sync.Once
				var calls atomic.Int32
				var initializes atomic.Int32
				fake := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}, Reply: echo}
				dial := scriptedHTTP(t, fake, func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodPost {
						return nil, nil
					}
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					r.Body = io.NopCloser(strings.NewReader(string(raw)))
					msg, err := mcp.ParseJSONRPCMessage(raw)
					if err != nil {
						return nil, err
					}
					if msg.Method == "initialize" {
						initializes.Add(1)
					}
					if msg.Method == "tools/call" && calls.Add(1) == 1 {
						close(entered)
						<-release
					}
					return nil, nil
				})
				s, _ := startWith(t, map[string]config.MCPServer{"srv": {Command: "fake"}}, dial)
				t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
				st := s.Wait(t.Context())[0]
				require.NoError(t, st.Err)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				called := make(chan error, 1)
				started := time.Now()
				go func() {
					_, err := st.Tools[0].Run(ctx, json.RawMessage(`{}`), func(policy.Target) (policy.Mode, error) { return policy.Execute, nil })
					called <- err
				}()
				<-entered
				if action == "cancel" {
					cancel()
				} else {
					<-time.NewTimer(59 * time.Second).C
					synctest.Wait()
					select {
					case err := <-called:
						t.Fatalf("call returned before configured 60s timeout: %v", err)
					default:
					}
				}
				err := <-called
				if action == "cancel" {
					var abort *mcp.McpAbortError
					require.ErrorAs(t, err, &abort)
				} else {
					var timeout *mcp.McpTimeoutError
					require.ErrorAs(t, err, &timeout)
					assert.Equal(t, 60000, timeout.TimeoutMs)
					assert.Equal(t, 60*time.Second, time.Since(started))
				}
				synctest.Wait()
				assert.Equal(t, int32(1), calls.Load())
				assert.Equal(t, int32(1), initializes.Load())
				releaseOnce.Do(func() { close(release) })
				synctest.Wait()
				out, err := call(t, st.Tools[0], `{"next":true}`)
				require.NoError(t, err)
				assert.JSONEq(t, `{"next":true}`, out)
				assert.Equal(t, int32(2), calls.Load())
				assert.Equal(t, int32(1), initializes.Load())
			})
		})
	}
}

func TestServersSuccessfulHTTPOutlivesConnectContext(t *testing.T) {
	fake := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}, Reply: echo}
	s, _ := startWith(t, map[string]config.MCPServer{"srv": {Command: "fake"}}, scriptedHTTP(t, fake, nil))
	st := s.Wait(t.Context())[0]
	require.NoError(t, st.Err)
	require.ErrorIs(t, s.connect.Err(), context.Canceled)
	out, err := call(t, st.Tools[0], `{"startup":true}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"startup":true}`, out)

	srv := s.servers[0]
	srv.invalidate(srv.client)
	ctx, cancel := context.WithCancel(t.Context())
	_, err = srv.live(ctx)
	require.NoError(t, err)
	cancel()
	out, err = call(t, st.Tools[0], `{"reconnect":true}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"reconnect":true}`, out)
}

func TestServersInvalidationPreservesNewerClient(t *testing.T) {
	fake := &mcpfake.Server{Tools: []map[string]any{echoTool("t")}, Reply: echo}
	s, _ := startFakes(t, map[string]*mcpfake.Server{"srv": fake})
	st := s.Wait(t.Context())[0]
	require.NoError(t, st.Err)
	srv := s.servers[0]
	old := srv.client
	srv.invalidate(old)
	newer, err := srv.live(t.Context())
	require.NoError(t, err)
	require.NotSame(t, old, newer)
	// A concurrent call may report expiration after a replacement was cached.
	srv.invalidate(old)
	assert.Same(t, newer, srv.client)
	out, err := call(t, st.Tools[0], `{"newer":true}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"newer":true}`, out)
	assert.Equal(t, 2, fake.Connections())
}

func TestServerToolsSaveALongResult(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outputs")
	outputs := tool.NewOutputs(dir)
	require.NoError(t, outputs.Select("s1"))
	long := strings.Repeat("x", 100000)
	fake := &mcpfake.Server{
		Tools: []map[string]any{echoTool("big")},
		Reply: func(string, json.RawMessage) map[string]any {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": long}}}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := startServers(ctx, cancel, ServerOptions{Servers: map[string]config.MCPServer{"srv": {Command: "fake"}}, Version: "1.2.3", Outputs: outputs},
		func(string, config.MCPServer) (mcp.Transport, error) { return fake.Connect(), nil })
	t.Cleanup(s.Close)

	out, err := call(t, s.Wait(t.Context())[0].Tools[0], `{}`)

	require.NoError(t, err)
	files, err := filepath.Glob(filepath.Join(dir, "s1", "*.txt"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Equal(t, long, string(data))
	assert.Contains(t, out, "full output: "+files[0])
}

// startHeld starts the servers "fast" and "slow"; slow does not connect until
// release is closed.
func startHeld(t *testing.T) (s *Servers, release chan struct{}) {
	t.Helper()
	release = make(chan struct{})
	fakes := map[string]*mcpfake.Server{"fast": {Tools: []map[string]any{echoTool("a")}}, "slow": {Tools: []map[string]any{echoTool("b")}, Reply: echo}}
	s, _ = startWith(t, map[string]config.MCPServer{"fast": {Command: "fake"}, "slow": {Command: "fake"}}, func(name string, _ config.MCPServer) (mcp.Transport, error) {
		if name == "slow" {
			<-release
		}
		return fakes[name].Connect(), nil
	})
	return s, release
}

func TestWatchReportsEachServerAsItSettles(t *testing.T) {
	s, release := startHeld(t)
	settled := make(chan ServerStatus, 2)
	s.Watch(func(st ServerStatus) { settled <- st })

	first := <-settled
	require.NoError(t, first.Err)
	assert.Equal(t, []string{"mcp__fast__a"}, toolNames(first))
	assert.Equal(t, []string{"fast", "slow"}, s.Names())
	assert.Equal(t, []string{"slow"}, s.Connecting())

	// The time Wait gives the servers is up, and nothing waits: slow still joins.
	s.wait = 0
	close(release)
	second := <-settled
	require.NoError(t, second.Err)
	assert.Empty(t, s.Connecting())
	out, err := call(t, findTool(t, second, "mcp__slow__b"), `{"late":true}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"late":true}`, out)

	late := make(chan ServerStatus, 2)
	s.Watch(func(st ServerStatus) { late <- st })
	assert.ElementsMatch(t, []string{"fast", "slow"}, []string{(<-late).Name, (<-late).Name}, "a later Watch is told of the servers already settled")
}

func TestWatchReportsAServerWaitGaveUpOn(t *testing.T) {
	s, release := startHeld(t)
	settled := make(chan ServerStatus, 2)
	s.Watch(func(st ServerStatus) { settled <- st })
	require.Equal(t, "fast", (<-settled).Name)

	s.wait = 0
	statuses := s.Wait(t.Context())
	require.ErrorIs(t, statuses[1].Err, ErrServerStartTimeout)
	close(release)

	slow := <-settled
	assert.Equal(t, "slow", slow.Name)
	assert.ErrorIs(t, slow.Err, ErrServerStartTimeout)
}

func TestReadyReturnsWhenServersSettleOrTheirGraceIsOver(t *testing.T) {
	s, release := startHeld(t)
	<-s.servers[0].done

	s.grace = 0
	s.Ready(t.Context())
	assert.Equal(t, []string{"slow"}, s.Connecting(), "a prompt waits no longer than the grace")

	s.grace = time.Hour
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	s.Ready(cancelled)
	assert.Equal(t, []string{"slow"}, s.Connecting())

	close(release)
	s.Ready(t.Context())
	assert.Empty(t, s.Connecting())
}

func TestServersHaveLongerToConnectThanWaitWaits(t *testing.T) {
	s := StartServers(ServerOptions{})
	t.Cleanup(s.Close)

	deadline, ok := s.connect.Deadline()
	require.True(t, ok)
	assert.WithinDuration(t, s.started.Add(30*time.Second), deadline, time.Second)
	assert.Equal(t, 10*time.Second, s.wait)
	assert.Equal(t, 2*time.Second, s.grace)
}
