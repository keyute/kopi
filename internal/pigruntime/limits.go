package pigruntime

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/keyute/kopi/internal/quota"
)

const limitsTimeout = 10 * time.Second

// limitReaders is every provider whose subscription plan Kopi reads the usage
// windows of. A provider is listed only where Kopi's usage there draws from the
// plan's own windows.
var limitReaders = map[string]func(context.Context, *Runtime) ([]quota.Window, error){
	"openai-codex": func(ctx context.Context, r *Runtime) ([]quota.Window, error) {
		token, err := r.accessToken(ctx, "openai-codex")
		if err != nil {
			return nil, err
		}
		return quota.Codex(ctx, r.limitsClient, token, ai.CodexAccountID(token))
	},
	// PiG keeps the GitHub token in the refresh field and exchanges it for the
	// short-lived Copilot token held in access; the usage endpoint wants the
	// GitHub one, which is read here so no exchange is triggered.
	"github-copilot": func(ctx context.Context, r *Runtime) ([]quota.Window, error) {
		cred, err := r.svcs.Credentials().Read(ctx, "github-copilot")
		if err != nil {
			return nil, fmt.Errorf("failed to read the credential: %w", err)
		}
		if cred == nil || cred.Refresh == "" {
			return nil, nil
		}
		return quota.Copilot(ctx, r.limitsClient, cred.Refresh, cred.EnterpriseDomain)
	},
	"kimi-coding": func(ctx context.Context, r *Runtime) ([]quota.Window, error) {
		token, err := r.accessToken(ctx, "kimi-coding")
		if err != nil {
			return nil, err
		}
		return quota.Kimi(ctx, r.limitsClient, token)
	},
	"xai": func(ctx context.Context, r *Runtime) ([]quota.Window, error) {
		token, err := r.accessToken(ctx, "xai")
		if err != nil {
			return nil, err
		}
		return quota.XAI(ctx, r.limitsClient, token)
	},
}

// Limits reads the usage windows of the provider's subscription plan. It
// returns none, and makes no request, for a provider without a reader and one
// the user has not signed in to with an account; an API key has no plan.
func (r *Runtime) Limits(ctx context.Context, provider string) ([]quota.Window, error) {
	read, ok := limitReaders[provider]
	if !ok {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, limitsTimeout)
	defer cancel()
	creds, err := r.svcs.ModelRuntime().ListCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read the plan limits of %s: failed to list credentials: %w", provider, err)
	}
	if !slices.Contains(creds, ai.CredentialInfo{ProviderID: provider, Type: ai.CredentialOAuth}) {
		return nil, nil
	}
	windows, err := read(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("failed to read the plan limits of %s: %w", provider, err)
	}
	return windows, nil
}

// accessToken is the provider's OAuth access token, which PiG refreshes when
// it has expired. Kimi's is returned only in the Authorization header.
func (r *Runtime) accessToken(ctx context.Context, provider string) (string, error) {
	res, err := r.svcs.ModelRuntime().GetAuth(ctx, provider)
	if err != nil {
		return "", fmt.Errorf("failed to resolve the credential: %w", err)
	}
	if res != nil {
		if res.Auth.APIKey != "" {
			return res.Auth.APIKey, nil
		}
		if h := res.Auth.Headers["Authorization"]; h != nil && *h != "" {
			return strings.TrimPrefix(*h, "Bearer "), nil
		}
	}
	return "", fmt.Errorf("failed to resolve the credential: %w", ErrNoCredential)
}
