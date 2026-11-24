package pigruntime

import (
	"context"
	"fmt"
	"slices"

	"github.com/MichaelKinsy/PiG/coding/extension/builtin/toolsearch"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

// LoadTools makes the named tools callable from the next model request of
// the Send that supplied ctx to a tool. Names the session does not have are
// ignored.
func LoadTools(ctx context.Context, names []string) error {
	s, ok := ctx.Value(turnSessionKey{}).(*Session)
	if !ok {
		return ErrNoTurn
	}
	s.loadTools(names)
	return nil
}

// AddTools gives the session tools it did not open with, from its next model
// request; none is callable until LoadTools makes it so. Each is a Deferred
// tool of a server OpenOptions.Joining named. A session with a Parent takes
// none.
func (s *Session) AddTools(tools []tool.Tool) error {
	if len(tools) == 0 {
		return nil
	}
	s.gate.Add(tool.Specs(tools))
	if err := register(s.joining, tools); err != nil {
		return fmt.Errorf("failed to add tools: %w", err)
	}
	s.toolsMu.Lock()
	err := s.inner.RefreshTools()
	s.toolsMu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to add tools: %w", reword(err))
	}
	s.notify(event.ToolsChanged{})
	return nil
}

func (s *Session) loadTools(names []string) {
	if s.addTools(names) {
		s.notify(event.ToolsChanged{})
	}
}

func (s *Session) addTools(names []string) bool {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	active := s.inner.ActiveToolNames()
	before := len(active)
	for _, name := range names {
		if !slices.Contains(active, name) {
			active = append(active, name)
		}
	}
	if len(active) == before {
		return false
	}
	// PiG's SetActiveToolsByName ignores names the session does not have.
	s.inner.SetActiveToolsByName(active)
	return len(s.inner.ActiveToolNames()) > before
}

// SetCallable makes the named tool callable from the session's next model
// request, or not callable. A name the session does not have is ignored.
func (s *Session) SetCallable(name string, on bool) {
	if s.setCallable(name, on) {
		s.notify(event.ToolsChanged{})
	}
}

func (s *Session) setCallable(name string, on bool) bool {
	if on {
		return s.addTools([]string{name})
	}
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	active := s.inner.ActiveToolNames()
	if !slices.Contains(active, name) {
		return false
	}
	s.inner.SetActiveToolsByName(slices.DeleteFunc(slices.Clone(active), func(n string) bool { return n == name }))
	return true
}

// Callable reports whether the named tool is callable from the session's next
// model request.
func (s *Session) Callable(name string) bool {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	return slices.Contains(s.inner.ActiveToolNames(), name)
}

// RankTools returns the names of at most limit tools matching query by name
// and description, best first.
func RankTools(query string, tools []tool.Tool, limit int) []string {
	docs := make([]toolsearch.Document, 0, len(tools))
	for _, t := range tools {
		docs = append(docs, toolsearch.Document{Name: t.Name, Text: t.Description})
	}
	matches := toolsearch.NewBm25Ranker().Rank(query, docs, limit)
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m.Name)
	}
	return names
}
