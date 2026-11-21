package pigruntime

import (
	"context"
	"log/slog"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
)

const catalogRefreshTimeout = 5 * time.Second

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
