// Package quota reads the usage windows of a provider's subscription plan.
// The endpoints are undocumented; their shapes come from other clients of them.
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxBody bounds the response read from a usage endpoint.
const maxBody = 1 << 20

// Window is one usage window of a plan.
type Window struct {
	Label       string
	UsedPercent float64
	// ResetsAt is zero when the provider gives no reset.
	ResetsAt time.Time
}

// Limits is what a provider reported at At.
type Limits struct {
	Provider string
	Windows  []Window
	At       time.Time
}

// label names a window of length d: 5h, wk, mo, else whole days or hours.
func label(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d == 7*day:
		return "wk"
	case d >= 28*day && d <= 31*day:
		return "mo"
	case d >= day:
		return fmt.Sprintf("%dd", d/day)
	}
	return fmt.Sprintf("%dh", d/time.Hour)
}

// get decodes the JSON body of a successful GET into out. The error names the
// status only, never a header.
func get(ctx context.Context, client *http.Client, url string, header http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to build the request: %w", err)
	}
	req.Header = header.Clone()
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send the request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("failed to read usage: status %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("failed to decode the response: %w", err)
	}
	return nil
}

func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

// epoch is the time of a Unix second count, zero when there is none.
func epoch(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}
