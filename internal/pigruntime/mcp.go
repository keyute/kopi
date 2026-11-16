package pigruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/mcp"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/tool"
)

const (
	startupWait = 10 * time.Second
	callTimeout = 60 * time.Second
	// stderrLines is how much of a stdio server's stderr a start failure shows.
	stderrLines = 5
)

type ServerOptions struct {
	Servers map[string]config.MCPServer // as configured; not yet validated or expanded
	CWD     string                      // working directory for stdio servers
	Version string                      // Kopi's version, reported to servers as the client version
}

type ServerStatus struct {
	Name string
	Err  error // nil when the server connected
	// Tools are sorted by name.
	Tools []tool.Tool
	// Skipped has one line per tool left out, e.g. "<tool>: name longer than 64 characters".
	Skipped      []string
	Instructions string
}

type Servers struct {
	startup context.Context // ends at startupWait, at Wait's end, or at Close
	cancel  context.CancelFunc
	servers []*server // sorted by name
	wg      sync.WaitGroup

	waitOnce  sync.Once
	result    []ServerStatus
	closeOnce sync.Once
}

// dialFunc builds a new transport for the named server's expanded config.
type dialFunc func(name string, c config.MCPServer) (mcp.Transport, error)

type server struct {
	name    string
	rawURL  string // as configured, before expansion
	cfg     config.MCPServer
	version string
	dial    dialFunc
	life    context.Context // ends at close; bounds a reconnect
	stop    context.CancelFunc

	done   chan struct{} // closed once status is final
	status ServerStatus
	// toolNames maps each of status.Tools' names to the server's own name for it.
	toolNames map[string]string

	mu     sync.Mutex
	client *mcp.Client
	closed bool
}

// StartServers begins connecting every server, each on its own goroutine, and
// returns at once.
func StartServers(o ServerOptions) *Servers {
	ctx, cancel := context.WithTimeout(context.Background(), startupWait)
	return startServers(ctx, cancel, o, dialTransport(o.CWD))
}

func startServers(ctx context.Context, cancel context.CancelFunc, o ServerOptions, dial dialFunc) *Servers {
	s := &Servers{startup: ctx, cancel: cancel}
	prefixes := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(o.Servers)) {
		life, stop := context.WithCancel(context.Background())
		srv := &server{name: name, version: o.Version, dial: dial, life: life, stop: stop, done: make(chan struct{}), status: ServerStatus{Name: name}}
		s.servers = append(s.servers, srv)
		prefix := tool.MCPName(name, "")
		if first, ok := prefixes[prefix]; ok {
			srv.fail(fmt.Errorf("failed to name tools: %w: %q and %q", ErrServerNameClash, first, name))
			continue
		}
		prefixes[prefix] = name
		cfg := o.Servers[name]
		srv.rawURL = cfg.URL
		if err := cfg.Validate(); err != nil {
			srv.fail(fmt.Errorf("failed to validate config: %w", err))
			continue
		}
		cfg, err := cfg.Expand(os.LookupEnv)
		if err != nil {
			srv.fail(fmt.Errorf("failed to expand config: %w", err))
			continue
		}
		srv.cfg = cfg
		s.wg.Go(func() { srv.start(ctx) })
	}
	return s
}

func dialTransport(cwd string) dialFunc {
	return func(_ string, c config.MCPServer) (mcp.Transport, error) {
		if c.URL == "" {
			// Env is added on top of Kopi's own environment.
			return mcp.NewStdioTransport(mcp.StdioTransportOptions{Command: c.Command, Args: c.Args, Cwd: cwd, Env: c.Env}), nil
		}
		t, err := mcp.NewStreamableHTTPTransport(mcp.StreamableHTTPTransportOptions{URL: c.URL, Headers: c.Headers})
		if err != nil {
			return nil, err
		}
		return t, nil
	}
}

// Wait returns every server's status, sorted by name, once all have settled,
// when startupWait has passed since StartServers, or when ctx is done,
// whichever is first. A server not settled by then is abandoned and closed,
// with ErrServerStartTimeout. Later calls return the same result.
func (s *Servers) Wait(ctx context.Context) []ServerStatus {
	s.waitOnce.Do(func() {
		out := make([]ServerStatus, len(s.servers))
		expired := false
		for i, srv := range s.servers {
			if !expired {
				select {
				case <-srv.done:
				case <-s.startup.Done():
					expired = true
				case <-ctx.Done():
					expired = true
				}
			}
			select {
			case <-srv.done:
				out[i] = srv.status
			default:
				out[i] = ServerStatus{Name: srv.name, Err: fmt.Errorf("failed to start: %w", ErrServerStartTimeout)}
				srv.close()
			}
		}
		s.cancel()
		s.result = s.dropClashes(out)
	})
	return s.result
}

// dropClashes leaves out a tool whose name an earlier server's tool already
// has, since the gate tells tools apart by name only.
func (s *Servers) dropClashes(out []ServerStatus) []ServerStatus {
	owners := map[string]string{}
	for i, srv := range s.servers {
		var kept []tool.Tool
		skipped := slices.Clone(out[i].Skipped)
		for _, tl := range out[i].Tools {
			name := srv.toolNames[tl.Name]
			if owner, ok := owners[tl.Name]; ok {
				skipped = append(skipped, fmt.Sprintf("%s: same name as %s", name, owner))
				continue
			}
			owners[tl.Name] = fmt.Sprintf("%s of server %s", name, srv.name)
			kept = append(kept, tl)
		}
		out[i].Tools, out[i].Skipped = kept, skipped
	}
	return out
}

// Close stops every server; it is safe to call more than once and before Wait.
func (s *Servers) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		for _, srv := range s.servers {
			srv.close()
		}
		s.wg.Wait()
	})
}

func (s *server) fail(err error) {
	s.status.Err = err
	s.stop()
	close(s.done)
}

func (s *server) start(ctx context.Context) {
	defer close(s.done)
	c, init, err := s.connect(ctx)
	if err != nil {
		s.status.Err = s.redact(err)
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.closeClient(c)
		s.status.Err = ErrServerClosed
		return
	}
	s.client = c
	s.mu.Unlock()
	if init.Capabilities.Tools != nil {
		listed, err := c.ListTools(ctx, mcp.RequestOptions{})
		if err != nil {
			s.status.Err = s.redact(fmt.Errorf("failed to list tools: %w", err))
			s.close()
			return
		}
		s.status.Tools, s.status.Skipped = s.tools(listed)
	}
	if init.Instructions != nil {
		s.status.Instructions = *init.Instructions
	}
}

func (s *server) connect(ctx context.Context) (*mcp.Client, *mcp.InitializeResult, error) {
	t, err := s.dial(s.name, s.cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create transport: %w", err)
	}
	c := mcp.NewClient(mcp.ClientOptions{Implementation: mcp.Implementation{Name: "kopi", Version: s.version}})
	init, err := c.Connect(ctx, t)
	if err != nil {
		err = fmt.Errorf("failed to connect: %w", err)
		// Connect has closed the transport, so its stderr is complete.
		if stdio, ok := t.(*mcp.StdioTransport); ok {
			if tail := lastLines(stdio.Stderr(), stderrLines); tail != "" {
				err = fmt.Errorf("%w; stderr:\n%s", err, tail)
			}
		}
		return nil, nil, err
	}
	return c, init, nil
}

func lastLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

func (s *server) tools(listed []mcp.Tool) ([]tool.Tool, []string) {
	var tools []tool.Tool
	var skipped []string
	seen := map[string]string{}
	s.toolNames = seen
	for _, mt := range listed {
		name := tool.MCPName(s.name, mt.Name)
		if len(name) > tool.MCPNameLimit {
			skipped = append(skipped, fmt.Sprintf("%s: name longer than %d characters", mt.Name, tool.MCPNameLimit))
			continue
		}
		if earlier, ok := seen[name]; ok {
			skipped = append(skipped, fmt.Sprintf("%s: same name as %s", mt.Name, earlier))
			continue
		}
		seen[name] = mt.Name
		tools = append(tools, tool.NewMCP(tool.MCPTool{
			Server:      s.name,
			Tool:        mt.Name,
			Description: mt.Description,
			Parameters:  parameters(mt.InputSchema),
			ReadOnly:    mt.Annotations != nil && mt.Annotations.ReadOnlyHint != nil && *mt.Annotations.ReadOnlyHint,
		}, s.call(mt.Name)))
	}
	slices.SortFunc(tools, func(a, b tool.Tool) int { return strings.Compare(a.Name, b.Name) })
	return tools, skipped
}

func parameters(schema json.RawMessage) map[string]any {
	var p map[string]any
	if json.Unmarshal(schema, &p) != nil || p == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return p
}

func (s *server) call(name string) func(context.Context, json.RawMessage) (string, error) {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		c, err := s.live(ctx)
		if err != nil {
			return "", s.redact(fmt.Errorf("failed to call %s: %w", name, err))
		}
		result, err := c.CallTool(ctx, name, args, mcp.RequestOptions{TimeoutMs: int(callTimeout / time.Millisecond)})
		if err != nil {
			return "", s.redact(fmt.Errorf("failed to call %s: %w", name, err))
		}
		text := resultText(result)
		if result.IsError != nil && *result.IsError {
			return text, ErrToolError
		}
		return text, nil
	}
}

// redact replaces the expanded URL in err's text with the URL as configured,
// so a substituted secret is not shown. *url.Error prints its URL quoted and
// possibly not as given, hence the separate forms.
func (s *server) redact(err error) error {
	if s.cfg.URL == s.rawURL {
		return err
	}
	pairs := []string{strconv.Quote(s.cfg.URL), strconv.Quote(s.rawURL), s.cfg.URL, s.rawURL}
	if ue, ok := errors.AsType[*url.Error](err); ok && ue.URL != "" {
		pairs = append([]string{strconv.Quote(ue.URL), strconv.Quote(s.rawURL), ue.URL, s.rawURL}, pairs...)
	}
	return &redactedError{text: strings.NewReplacer(pairs...).Replace(err.Error()), err: err}
}

// redactedError shows text instead of err's message; errors.Is and errors.As
// still see err.
type redactedError struct {
	text string
	err  error
}

func (e *redactedError) Error() string { return e.text }
func (e *redactedError) Unwrap() error { return e.err }

func resultText(r *mcp.CallToolResult) string {
	var parts []string
	for _, c := range mcp.ToLLMContent(*r) {
		if c.Type == mcp.LLMContentTypeImage {
			parts = append(parts, "[image]")
			continue
		}
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "\n")
}

// live returns the connected client, reconnecting once with the same config
// when the connection has died.
func (s *server) live(ctx context.Context) (*mcp.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrServerClosed
	}
	if s.client != nil && s.client.ConnectionState() == mcp.ClientStateConnected {
		return s.client, nil
	}
	if s.client != nil {
		s.closeClient(s.client)
		s.client = nil
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	defer context.AfterFunc(s.life, cancel)()
	c, _, err := s.connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to reconnect: %w", err)
	}
	s.client = c
	return c, nil
}

func (s *server) close() {
	s.stop()
	s.mu.Lock()
	s.closed = true
	c := s.client
	s.client = nil
	s.mu.Unlock()
	if c != nil {
		s.closeClient(c)
	}
}

func (s *server) closeClient(c *mcp.Client) {
	if err := c.Close(); err != nil {
		slog.Warn("failed to close MCP server", "server", s.name, "error", s.redact(err))
	}
}
