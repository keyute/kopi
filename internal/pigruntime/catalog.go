package pigruntime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
)

const catalogRefreshTimeout = 5 * time.Second

// refreshModels fetches provider's model list from the remote catalog, once
// per provider per process, and returns the failure if there was one. It does
// nothing for a provider PiG does not know or one without a credential; that
// case is not remembered, so a later sign-in can still fetch.
//
// It relies on PiG v0.7.0: a provider refresh fetches only with a credential;
// an unset AllowNetwork leaves PiG's PIG_OFFLINE gate in force; and Force
// skips the four-hour freshness window of the stored list. Recheck on a bump.
func (r *Runtime) refreshModels(ctx context.Context, provider string) error {
	if r.svcs.ModelRuntime().GetProvider(provider) == nil {
		return nil
	}
	ok, err := r.hasCredential(ctx, provider)
	if err != nil || !ok {
		return nil
	}
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	if r.refreshed[provider] {
		return nil
	}
	r.refreshed[provider] = true
	ctx, cancel := context.WithTimeout(ctx, catalogRefreshTimeout)
	defer cancel()
	res := r.svcs.ModelRuntime().Refresh(ctx, ai.ModelsRefreshOptions{Providers: []string{provider}, Force: new(true)})
	err = res.Errors[provider]
	if err == nil && res.Aborted {
		err = ctx.Err()
	}
	if err == nil {
		return nil
	}
	err = fmt.Errorf("failed to refresh the model list of provider %q: %w", provider, reword(err))
	slog.Warn("failed to refresh the model list", "provider", provider, "error", err)
	return err
}
