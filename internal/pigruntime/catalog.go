package pigruntime

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
)

const (
	catalogRefreshTimeout   = 5 * time.Second
	modelListRefreshTimeout = 15 * time.Second
)

// refreshModels fetches provider's model list from the remote catalog, once
// per provider per process, and returns the failure if there was one. The first
// caller for a provider fetches; concurrent callers for it wait for that
// outcome or for their own context, and other providers do not wait. provider
// must be known and have a credential. A fetch cut short by its caller's own
// context is not remembered: a later call, or a waiter, fetches again.
//
// It relies on PiG v0.7.0: a provider refresh fetches only with a credential;
// an unset AllowNetwork leaves PiG's offline gate in force (PI_OFFLINE set at
// all, else PIG_OFFLINE set to 1, true or yes); and Force skips the four-hour
// freshness window of the stored list. Recheck on a bump.
func (r *Runtime) refreshModels(ctx context.Context, provider string) error {
	for {
		r.refreshMu.Lock()
		if err, ok := r.refreshed[provider]; ok {
			r.refreshMu.Unlock()
			return err
		}
		if done, ok := r.fetching[provider]; ok {
			r.refreshMu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		done := make(chan struct{})
		r.fetching[provider] = done
		r.refreshMu.Unlock()

		err := r.fetchModels(ctx, provider)

		r.refreshMu.Lock()
		delete(r.fetching, provider)
		if ctx.Err() == nil {
			r.refreshed[provider] = err
		}
		r.refreshMu.Unlock()
		close(done)
		return err
	}
}

func (r *Runtime) fetchModels(ctx context.Context, provider string) error {
	ctx, cancel := context.WithTimeout(ctx, catalogRefreshTimeout)
	defer cancel()
	res := r.svcs.ModelRuntime().Refresh(ctx, ai.ModelsRefreshOptions{Providers: []string{provider}, Force: new(true)})
	err := res.Errors[provider]
	if err == nil && res.Aborted {
		err = ctx.Err()
	}
	if err == nil {
		return nil
	}
	err = &ModelListError{Provider: provider, Err: reword(err)}
	slog.Warn("failed to refresh the model list", "provider", provider, "error", err)
	return err
}

// StartModelListRefresh refreshes in the background the model lists of the
// providers whose models this process has resolved, so a known model's window,
// prices and base URL follow the catalog. Failures are only logged. It runs
// once, after the start checks and never while a forced fetch can be in
// flight; Close cancels it and waits.
//
// It relies on PiG v0.7.0: a nil or empty Providers selects every provider,
// hence the return on no used provider; a later refresh of a provider cancels
// one in flight and the cancelled one reports no error, so a refresh begun
// during a forced fetch would make that fetch return no list; an unset Force
// skips the request while the stored list was checked under four hours ago;
// and an unset AllowNetwork leaves PiG's offline gate in force. Recheck on a
// bump.
func (r *Runtime) StartModelListRefresh() {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	if r.refreshDone != nil {
		return
	}
	providers := slices.Sorted(maps.Keys(r.used))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.stopRefresh, r.refreshDone = cancel, done
	go func() {
		defer close(done)
		if len(providers) == 0 {
			return
		}
		r.refreshModelList(ctx, providers)
	}()
}

func (r *Runtime) refreshModelList(ctx context.Context, providers []string) {
	timed, cancel := context.WithTimeout(ctx, modelListRefreshTimeout)
	defer cancel()
	res := r.svcs.ModelRuntime().Refresh(timed, ai.ModelsRefreshOptions{Providers: providers})
	if ctx.Err() != nil {
		return
	}
	for _, provider := range slices.Sorted(maps.Keys(res.Errors)) {
		slog.Debug("failed to refresh the model list", "provider", provider, "error", reword(res.Errors[provider]))
	}
	if res.Aborted && errors.Is(timed.Err(), context.DeadlineExceeded) {
		slog.Debug("model list refresh timed out", "providers", providers)
	}
}
