// Package mcpfake provides a scripted MCP server for tests of pigruntime's MCP
// client. It is apart from pigruntimetest because pigruntime's own tests cannot
// import a package that imports pigruntime.
package mcpfake

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/MichaelKinsy/PiG/mcp"
	"github.com/MichaelKinsy/PiG/mcp/mcptest"
)

// Server answers initialize, tools/list and tools/call as scripted. Set its
// fields before the first Connect.
type Server struct {
	Instructions string
	// NoTools leaves the tools capability undeclared.
	NoTools bool
	// Tools are the tools/list entries as sent.
	Tools []map[string]any
	// Reply returns the tools/call result object; nil answers with no content.
	Reply func(name string, args json.RawMessage) map[string]any
	// Hang leaves initialize unanswered.
	Hang bool

	mu         sync.Mutex
	conns      []*conn
	connected  chan struct{} // closed at the first Connect
	clientInfo mcp.Implementation
}

type conn struct {
	end    *mcptest.InMemoryTransport
	closed chan struct{}
}

// Connect returns the client end of a new in-memory connection to s.
func (s *Server) Connect() mcp.Transport {
	client, end := mcptest.NewInMemoryTransportPair()
	c := &conn{end: end, closed: make(chan struct{})}
	end.OnClose(func() { close(c.closed) })
	end.OnMessage(func(m mcp.JSONRPCMessage) {
		if reply := s.Answer(m); reply != nil {
			_ = end.Send(context.Background(), *reply)
		}
	})
	_ = end.Start()
	connected := s.connectedChan()
	s.mu.Lock()
	s.conns = append(s.conns, c)
	if len(s.conns) == 1 {
		close(connected)
	}
	s.mu.Unlock()
	return client
}

// Kill closes the latest connection, as a crashed server would. Once it
// returns, the client has seen the connection close.
func (s *Server) Kill() {
	_ = s.latest().end.Close()
}

// Closed waits for the first Connect, then returns a channel closed once the
// latest connection has closed.
func (s *Server) Closed() <-chan struct{} {
	<-s.connectedChan()
	return s.latest().closed
}

// Connections is how many times Connect was called.
func (s *Server) Connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// ClientInfo is the client implementation the last initialize named.
func (s *Server) ClientInfo() mcp.Implementation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientInfo
}

func (s *Server) connectedChan() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connected == nil {
		s.connected = make(chan struct{})
	}
	return s.connected
}

func (s *Server) latest() *conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[len(s.conns)-1]
}

// Answer returns the reply to m, or nil for a notification or a request left
// unanswered.
func (s *Server) Answer(m mcp.JSONRPCMessage) *mcp.JSONRPCMessage {
	if !m.IsRequest() {
		return nil
	}
	var result any
	switch m.Method {
	case "initialize":
		if s.Hang {
			return nil
		}
		var params mcp.InitializeParams
		_ = json.Unmarshal(m.Params, &params)
		s.mu.Lock()
		s.clientInfo = params.ClientInfo
		s.mu.Unlock()
		capabilities := map[string]any{}
		if !s.NoTools {
			capabilities["tools"] = map[string]any{}
		}
		r := map[string]any{
			"protocolVersion": mcp.LatestProtocolVersion,
			"capabilities":    capabilities,
			"serverInfo":      map[string]any{"name": "fake", "version": "1"},
		}
		if s.Instructions != "" {
			r["instructions"] = s.Instructions
		}
		result = r
	case "tools/list":
		tools := s.Tools
		if tools == nil {
			tools = []map[string]any{}
		}
		result = map[string]any{"tools": tools}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(m.Params, &params)
		r := map[string]any{"content": []any{}}
		if s.Reply != nil {
			r = s.Reply(params.Name, params.Arguments)
		}
		result = r
	default:
		reply := mcp.NewErrorResponse(*m.ID, mcp.JSONRPCMethodNotFound, "Method not found: "+m.Method, nil)
		return &reply
	}
	raw, err := json.Marshal(result)
	if err != nil {
		panic(fmt.Sprintf("failed to marshal fake MCP result: %v", err))
	}
	reply := mcp.NewResult(*m.ID, raw)
	return &reply
}

// ServeStdio answers newline-delimited messages read from r on w until r ends.
func (s *Server) ServeStdio(r io.Reader, w io.Writer) error {
	lines := bufio.NewScanner(r)
	lines.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for lines.Scan() {
		m, err := mcp.ParseJSONRPCMessage(lines.Bytes())
		if err != nil {
			return fmt.Errorf("failed to parse message: %w", err)
		}
		reply := s.Answer(m)
		if reply == nil {
			continue
		}
		raw, err := json.Marshal(*reply)
		if err != nil {
			return fmt.Errorf("failed to marshal reply: %w", err)
		}
		if _, err := w.Write(append(raw, '\n')); err != nil {
			return fmt.Errorf("failed to write reply: %w", err)
		}
	}
	return lines.Err()
}
