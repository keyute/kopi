package app

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/quota"
)

// fakeLimits is a read the test releases, one reading at a time.
type fakeLimits struct {
	started chan string
	release chan limitsResult
}

type limitsResult struct {
	windows []quota.Window
	err     error
}

func newFakeLimits() *fakeLimits {
	return &fakeLimits{started: make(chan string, 8), release: make(chan limitsResult)}
}

func (f *fakeLimits) read(ctx context.Context, provider string) ([]quota.Window, error) {
	f.started <- provider
	select {
	case res := <-f.release:
		return res.windows, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func newTestLimits(f *fakeLimits) (*limits, chan event.Event) {
	published := make(chan event.Event, 8)
	l := newLimits(f.read, func() string { return "codex" }, func(ev event.Event) { published <- ev })
	return l, published
}

var weekly = []quota.Window{{Label: "wk", UsedPercent: 40}}

func TestLimitsReadOnActivityOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLimits()
		l, _ := newTestLimits(f)
		t.Cleanup(l.close)

		for _, ev := range []event.Event{
			event.TurnStart{},
			event.SessionChanged{},
			event.ModeChanged{},
			event.ToolEnd{},
			event.TurnEnd{},
			event.TasksChanged{},
			event.Session{ID: "a", Event: event.TurnEnd{}},
		} {
			l.observe(ev)
			synctest.Wait()
			assert.Equal(t, "codex", <-f.started, "%T", ev)
			f.release <- limitsResult{}
			synctest.Wait()
			time.Sleep(limitsEvery)
		}

		for _, ev := range []event.Event{event.TextDelta{}, event.LimitsChanged{}, event.Notice{}} {
			l.observe(ev)
			synctest.Wait()
			assert.Empty(t, f.started, "%T", ev)
		}
	})
}

func TestLimitsOneReadInFlightAndNoneQueued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLimits()
		l, published := newTestLimits(f)
		t.Cleanup(l.close)

		l.observe(event.TurnEnd{})
		synctest.Wait()
		<-f.started
		time.Sleep(2 * limitsEvery)
		l.observe(event.TurnEnd{})
		synctest.Wait()
		assert.Empty(t, f.started, "a trigger during a read is dropped")

		f.release <- limitsResult{windows: weekly}
		synctest.Wait()
		assert.Equal(t, event.LimitsChanged{}, <-published)
		assert.Empty(t, f.started, "the dropped trigger is not run afterwards")
		got := l.get()
		assert.Equal(t, "codex", got.Provider)
		assert.Equal(t, weekly, got.Windows)
		assert.False(t, got.At.IsZero())
	})
}

func TestLimitsAtMostOncePerMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLimits()
		l, _ := newTestLimits(f)
		t.Cleanup(l.close)

		l.observe(event.TurnEnd{})
		synctest.Wait()
		<-f.started
		f.release <- limitsResult{}
		synctest.Wait()

		time.Sleep(limitsEvery - time.Second)
		l.observe(event.TurnEnd{})
		synctest.Wait()
		assert.Empty(t, f.started, "under a minute since the last read")

		time.Sleep(time.Second)
		l.observe(event.TurnEnd{})
		synctest.Wait()
		assert.Equal(t, "codex", <-f.started)
		f.release <- limitsResult{}
	})
}

func TestLimitsFailureKeepsTheLastReading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLimits()
		l, published := newTestLimits(f)
		t.Cleanup(l.close)

		l.observe(event.TurnEnd{})
		synctest.Wait()
		<-f.started
		f.release <- limitsResult{windows: weekly}
		synctest.Wait()
		require.Equal(t, event.LimitsChanged{}, <-published)

		for range 2 {
			time.Sleep(limitsEvery)
			l.observe(event.TurnEnd{})
			synctest.Wait()
			<-f.started
			f.release <- limitsResult{err: errors.New("status 500")}
			synctest.Wait()
		}

		assert.Empty(t, published, "a failed read publishes nothing")
		assert.Equal(t, weekly, l.get().Windows)

		time.Sleep(limitsEvery)
		l.observe(event.TurnEnd{})
		synctest.Wait()
		<-f.started
		f.release <- limitsResult{}
		synctest.Wait()
		assert.Equal(t, event.LimitsChanged{}, <-published)
		assert.Empty(t, l.get().Windows, "a reading without windows replaces the last one")
	})
}

func TestLimitsWaitForAProvider(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLimits()
		l := newLimits(f.read, func() string { return "" }, func(event.Event) {})
		t.Cleanup(l.close)

		l.observe(event.TurnEnd{})
		synctest.Wait()

		assert.Empty(t, f.started)
	})
}

func TestLimitsCloseCancelsAndJoins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLimits()
		l, published := newTestLimits(f)

		l.observe(event.TurnEnd{})
		synctest.Wait()
		<-f.started

		l.close()
		l.close()

		assert.Empty(t, published, "a cancelled read publishes nothing")
		assert.Empty(t, l.get().Windows)
		time.Sleep(limitsEvery)
		l.observe(event.TurnEnd{})
		synctest.Wait()
		assert.Empty(t, f.started, "nothing reads after close")
	})
}

func TestNilLimitsDoNothing(t *testing.T) {
	var l *limits

	l.observe(event.TurnEnd{})
	l.refresh()
	l.close()

	assert.Equal(t, quota.Limits{}, l.get())
}
