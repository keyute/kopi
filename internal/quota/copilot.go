package quota

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var hostname = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

type copilotQuota struct {
	Entitlement      *float64 `json:"entitlement"`
	Remaining        *float64 `json:"remaining"`
	PercentRemaining float64  `json:"percent_remaining"`
	Unlimited        bool     `json:"unlimited"`
}

// Copilot reads the GitHub Copilot plan's premium and chat quotas with the
// GitHub token; domain is the enterprise host, empty for github.com.
func Copilot(ctx context.Context, client *http.Client, token, domain string) ([]Window, error) {
	// PiG's sign-in stores a bare host, but a credential written otherwise can
	// hold a URL, which PiG normalises to its host.
	if _, rest, ok := strings.Cut(domain, "://"); ok {
		domain, _, _ = strings.Cut(rest, "/")
	}
	if domain == "" {
		domain = "github.com"
	} else if !hostname.MatchString(domain) {
		return nil, fmt.Errorf("failed to use the enterprise domain %q: not a hostname", domain)
	}
	var usage struct {
		Snapshots struct {
			Premium *copilotQuota `json:"premium_interactions"`
			Chat    *copilotQuota `json:"chat"`
		} `json:"quota_snapshots"`
		ResetDate    string `json:"quota_reset_date"`
		ResetDateUTC string `json:"quota_reset_date_utc"`
	}
	if err := get(ctx, client, "https://api."+domain+"/copilot_internal/user", bearer(token), &usage); err != nil {
		return nil, err
	}
	reset := parseTime(cmp.Or(usage.ResetDateUTC, usage.ResetDate))
	var windows []Window
	for _, q := range []struct {
		label string
		quota *copilotQuota
	}{{"premium", usage.Snapshots.Premium}, {"chat", usage.Snapshots.Chat}} {
		if q.quota == nil || q.quota.Unlimited {
			continue
		}
		if q.quota.Entitlement != nil && *q.quota.Entitlement == 0 && q.quota.Remaining != nil && *q.quota.Remaining == 0 {
			continue
		}
		windows = append(windows, Window{Label: q.label, UsedPercent: max(0, 100-q.quota.PercentRemaining), ResetsAt: reset})
	}
	return windows, nil
}

// parseTime reads an RFC 3339 time, else a date; the zero time when neither.
func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	t, _ := time.Parse(time.DateOnly, s)
	return t
}
