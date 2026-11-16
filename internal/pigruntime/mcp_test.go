package pigruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

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
	return tl.Run(t.Context(), json.RawMessage(args), func(policy.Target) error { return nil })
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
	assert.Equal(t, map[string]any{"type": "object"}, parameters(json.RawMessage(`{"type":"object"}`)))
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
	assert.Equal(t, "srv/a.b", st.Tools[0].Label)
}

func TestServersErrorResult(t *testing.T) {
	fake := &mcpfake.Server{
		Tools: []map[string]any{echoTool("fail")},
		Reply: func(string, json.RawMessage) map[string]any {
			return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "no such page"}}}
		},
	}
	s, _ := startFakes(t, map[string]*mcpfake.Server{"srv": fake})

	_, err := call(t, s.Wait(t.Context())[0].Tools[0], `{}`)
	require.ErrorIs(t, err, ErrToolError)
	assert.Contains(t, err.Error(), "no such page")
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

func TestServersToolNameClashAcrossServers(t *testing.T) {
	first := &mcpfake.Server{Tools: []map[string]any{echoTool("b__x"), echoTool("y")}}
	second := &mcpfake.Server{Tools: []map[string]any{echoTool("x"), echoTool("z")}}
	s, _ := startFakes(t, map[string]*mcpfake.Server{"a": first, "a__b": second})

	statuses := s.Wait(t.Context())
	assert.Equal(t, []string{"mcp__a__b__x", "mcp__a__y"}, toolNames(statuses[0]))
	assert.Empty(t, statuses[0].Skipped)
	assert.Equal(t, []string{"mcp__a__b__z"}, toolNames(statuses[1]))
	assert.Equal(t, []string{"x: same name as b__x of server a"}, statuses[1].Skipped)
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
