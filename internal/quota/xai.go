package quota

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

const xaiURL = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"

// XAI reads the Grok plan's credit window.
func XAI(ctx context.Context, client *http.Client, token string) ([]Window, error) {
	var billing struct {
		Config struct {
			// Absent when zero.
			CreditUsagePercent float64 `json:"creditUsagePercent"`
			CurrentPeriod      *struct {
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"currentPeriod"`
		} `json:"config"`
	}
	if err := get(ctx, client, xaiURL, bearer(token), &billing); err != nil {
		return nil, err
	}
	period := billing.Config.CurrentPeriod
	if period == nil {
		return nil, nil
	}
	start, err := time.Parse(time.RFC3339, period.Start)
	if err != nil {
		return nil, fmt.Errorf("failed to read the billing period: %w", err)
	}
	end, err := time.Parse(time.RFC3339, period.End)
	if err != nil {
		return nil, fmt.Errorf("failed to read the billing period: %w", err)
	}
	return []Window{{Label: label(end.Sub(start)), UsedPercent: billing.Config.CreditUsagePercent, ResetsAt: end}}, nil
}
