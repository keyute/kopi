package pigruntime

import (
	"context"
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
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	active := s.inner.ActiveToolNames()
	if slices.Contains(active, name) == on {
		return false
	}
	if on {
		active = append(active, name)
	} else {
		active = slices.DeleteFunc(slices.Clone(active), func(n string) bool { return n == name })
	}
	// PiG's SetActiveToolsByName ignores names the session does not have.
	s.inner.SetActiveToolsByName(active)
	return slices.Contains(s.inner.ActiveToolNames(), name) == on
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
