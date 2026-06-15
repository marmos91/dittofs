// Package schedule provides cadence parsing helpers shared by the snapshot
// policy server-side validation and the dfsctl CLI. It deliberately supports
// only a minimal surface: Go duration strings plus a few @-shorthands. No cron
// expressions (DittoFS has no cron dependency and is single-node, pre-1.0).
package schedule

import (
	"fmt"
	"strings"
	"time"
)

// shorthands maps the supported @-aliases to their duration.
var shorthands = map[string]time.Duration{
	"@hourly": time.Hour,
	"@daily":  24 * time.Hour,
	"@weekly": 7 * 24 * time.Hour,
}

// ParseInterval parses a snapshot cadence. It accepts a Go duration string
// (e.g. "24h", "6h", "1h30m") or one of the @-shorthands ("@hourly",
// "@daily", "@weekly"). Input is trimmed and shorthands are case-insensitive.
// The resulting interval must be strictly positive; zero or negative values
// and unparseable input return an error.
func ParseInterval(s string) (time.Duration, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("schedule: empty interval")
	}
	if d, ok := shorthands[strings.ToLower(trimmed)]; ok {
		return d, nil
	}
	if strings.HasPrefix(trimmed, "@") {
		return 0, fmt.Errorf("schedule: unknown shorthand %q (want @hourly, @daily, @weekly)", trimmed)
	}
	d, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("schedule: invalid interval %q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("schedule: interval must be positive, got %q", s)
	}
	return d, nil
}

// FormatInterval renders a duration for display as a Go duration string.
func FormatInterval(d time.Duration) string {
	return d.String()
}
