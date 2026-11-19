package app

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
)

const (
	statusCommandTimeout = 5 * time.Second
	// statusOutputLimit bounds the stdout kept per run; the rest is drained and dropped.
	statusOutputLimit = 64 << 10
)

type statusRunner interface {
	run(context.Context, string, string) (string, error)
}

type shellRunner struct{}

type statusCommand struct {
	name, command string
	// changes publishes the parsed line counts as event.Changes.
	changes          bool
	interval         time.Duration
	running, failing bool
}

// StatusCommands owns the ephemeral footer commands for one frontend run.
// Construction does not execute commands; only Start admits work.
type StatusCommands struct {
	workspace string
	commands  []*statusCommand
	runner    statusRunner
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	publish   func(event.Event)
	closed    bool
	active    sync.WaitGroup
}

func NewStatusCommands(workspace string, line config.StatusLine) *StatusCommands {
	s := &StatusCommands{workspace: workspace, runner: shellRunner{}}
	seen := map[string]bool{}
	names := config.SegmentNames(line.Left, line.Right)
	for _, name := range names {
		def := line.Segments[name]
		if slices.Contains(config.StatusSegments, name) || seen[name] || def.Command == "" {
			continue
		}
		seen[name] = true
		interval, _ := time.ParseDuration(def.Interval)
		s.commands = append(s.commands, &statusCommand{name: name, command: def.Command, interval: interval})
	}
	if slices.Contains(names, "changes") && gitBranch(workspace) != "" {
		s.commands = append(s.commands, &statusCommand{name: "changes", command: changesCommand, changes: true})
	}
	return s
}

// Start publishes raw results in completion order. publish must not block or
// call back into StatusCommands.
func (s *StatusCommands) Start(ctx context.Context, publish func(event.Event)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx != nil {
		return
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.publish = publish
	for _, command := range s.commands {
		s.launch(command)
		if command.interval > 0 {
			s.active.Add(1)
			go s.tick(command)
		}
	}
}

func (s *StatusCommands) tick(command *statusCommand) {
	defer s.active.Done()
	ticker := time.NewTicker(command.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			s.launch(command)
			s.mu.Unlock()
		}
	}
}

// launch requires mu. Busy refreshes are discarded, never queued.
func (s *StatusCommands) launch(command *statusCommand) {
	if s.closed || s.ctx == nil || s.ctx.Err() != nil || command.running {
		return
	}
	command.running = true
	s.active.Add(1)
	go func() {
		defer s.active.Done()
		s.mu.Lock()
		if s.closed || s.ctx.Err() != nil {
			command.running = false
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(s.ctx, statusCommandTimeout)
		defer cancel()
		out, err := s.runner.run(ctx, s.workspace, command.command)
		s.mu.Lock()
		// A failing changes run is a documented absent state (no commit yet), not a fault.
		if err != nil && !command.changes && !command.failing && s.ctx.Err() == nil {
			slog.Warn("status line command failed", "segment", command.name, "error", err)
		}
		command.failing = err != nil
		command.running = false
		if command.changes {
			if ctx.Err() == context.DeadlineExceeded {
				// exec may report "signal: killed" instead of the deadline error; keep the last counts.
				s.mu.Unlock()
				return
			}
			var changed event.Changes
			if err == nil {
				changed.Added, changed.Removed = parseShortstat(out)
			}
			s.publish(changed)
		} else {
			s.publish(event.StatusCommandResult{Name: command.name, Output: out, Err: err})
		}
		s.mu.Unlock()
	}()
}

func (s *StatusCommands) Observe(ev event.Event) {
	switch ev.(type) {
	case event.TurnStart, event.SessionChanged, event.ModeChanged, event.ToolEnd, event.TurnEnd, event.TasksChanged:
	default:
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, command := range s.commands {
		s.launch(command)
	}
}

// Close closes admission before cancellation and joins execution and timers.
func (s *StatusCommands) Close() {
	s.mu.Lock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.active.Wait()
}
