package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/quota"
)

// limitsEvery is the least time between two reads of the plan limits, so
// activity never calls the provider more than once a minute.
const limitsEvery = time.Minute

// limits holds the last reading of the main model's provider's plan limits,
// refreshed on activity. Its mutex is a leaf: nothing is called with it held.
// A nil *limits reads nothing and holds nothing.
type limits struct {
	read     func(ctx context.Context, provider string) ([]quota.Window, error)
	provider func() string
	publish  func(event.Event)
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	mu        sync.Mutex
	last      quota.Limits
	attempted time.Time
	// running is set while a read is in flight; failing while the reads since
	// the last good one have failed.
	running, failing, closed bool
}

func newLimits(read func(context.Context, string) ([]quota.Window, error), provider func() string, publish func(event.Event)) *limits {
	ctx, cancel := context.WithCancel(context.Background())
	return &limits{read: read, provider: provider, publish: publish, ctx: ctx, cancel: cancel}
}

func (l *limits) observe(ev event.Event) {
	if l != nil && refreshes(ev) {
		l.refresh()
	}
}

// refresh starts a read unless one is in flight or the last attempt was under
// limitsEvery ago; such a trigger is dropped, not queued.
func (l *limits) refresh() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.running || (!l.attempted.IsZero() && time.Since(l.attempted) < limitsEvery) {
		return
	}
	l.running = true
	l.attempted = time.Now()
	l.wg.Add(1)
	// The provider is resolved off the caller, which is a relay subscriber and
	// must not call into the Controller.
	go func() {
		defer l.wg.Done()
		provider := l.provider()
		if provider == "" {
			l.mu.Lock()
			l.running = false
			l.attempted = time.Time{}
			l.mu.Unlock()
			return
		}
		windows, err := l.read(l.ctx, provider)
		l.finish(provider, windows, err)
	}()
}

func (l *limits) finish(provider string, windows []quota.Window, err error) {
	l.mu.Lock()
	l.running = false
	if l.ctx.Err() != nil {
		l.mu.Unlock()
		return
	}
	if err != nil {
		first := !l.failing
		l.failing = true
		l.mu.Unlock()
		if first {
			slog.Warn("failed to read the plan limits", "provider", provider, "error", err)
		}
		return
	}
	l.failing = false
	l.last = quota.Limits{Provider: provider, Windows: windows, At: time.Now()}
	l.mu.Unlock()
	l.publish(event.LimitsChanged{})
}

func (l *limits) get() quota.Limits {
	if l == nil {
		return quota.Limits{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

// close cancels the read in flight and waits for it; it is safe to call again.
func (l *limits) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.cancel()
	l.wg.Wait()
}
