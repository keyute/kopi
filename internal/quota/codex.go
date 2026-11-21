package quota

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"time"
)

const codexURL = "https://chatgpt.com/backend-api/wham/usage"

type codexWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

// Codex reads the ChatGPT plan's windows, shortest first. Which slot holds
// which window differs by plan, so a window is named by its length.
func Codex(ctx context.Context, client *http.Client, token, account string) ([]Window, error) {
	var usage struct {
		RateLimit *struct {
			Primary   *codexWindow `json:"primary_window"`
			Secondary *codexWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	header := bearer(token)
	header.Set("ChatGPT-Account-Id", account)
	if err := get(ctx, client, codexURL, header, &usage); err != nil {
		return nil, err
	}
	if usage.RateLimit == nil {
		return nil, nil
	}
	var found []codexWindow
	for _, w := range []*codexWindow{usage.RateLimit.Primary, usage.RateLimit.Secondary} {
		if w != nil {
			found = append(found, *w)
		}
	}
	slices.SortFunc(found, func(a, b codexWindow) int { return cmp.Compare(a.LimitWindowSeconds, b.LimitWindowSeconds) })
	var windows []Window
	for _, w := range found {
		windows = append(windows, Window{
			Label:       label(time.Duration(w.LimitWindowSeconds) * time.Second),
			UsedPercent: w.UsedPercent,
			ResetsAt:    epoch(w.ResetAt),
		})
	}
	return windows, nil
}
