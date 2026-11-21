package quota

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
)

const kimiURL = "https://api.kimi.com/coding/v1/usages"

type kimiEntry struct {
	// UsedRatio is a number or a numeric string.
	UsedRatio json.RawMessage `json:"used_ratio"`
	ResetTime string          `json:"reset_time"`
}

// Kimi reads the Kimi Code plan's windows. An entry without a valid ratio is left out.
func Kimi(ctx context.Context, client *http.Client, token string) ([]Window, error) {
	var usage struct {
		Usages map[string]kimiEntry `json:"usages"`
	}
	if err := get(ctx, client, kimiURL, bearer(token), &usage); err != nil {
		return nil, err
	}
	var windows []Window
	for _, w := range []struct{ key, label string }{
		{"limit_5h", "5h"}, {"limit_7d", "wk"}, {"limit_month_total", "mo"}, {"limit_month_code", "mo code"},
	} {
		entry, ok := usage.Usages[w.key]
		if !ok {
			continue
		}
		ratio, ok := parseRatio(entry.UsedRatio)
		if !ok {
			continue
		}
		windows = append(windows, Window{Label: w.label, UsedPercent: ratio * 100, ResetsAt: parseTime(entry.ResetTime)})
	}
	return windows, nil
}

func parseRatio(raw json.RawMessage) (float64, bool) {
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
		if f, err = strconv.ParseFloat(s, 64); err != nil {
			return 0, false
		}
	}
	return f, !math.IsNaN(f) && !math.IsInf(f, 0)
}
