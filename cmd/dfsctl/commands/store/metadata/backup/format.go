// Package backup implements per-store backup management commands.
package backup

import (
	"fmt"
	"strings"
	"time"
)

// shortULID returns the first 8 chars of a ULID followed by an ellipsis
// (U+2026 "…") so table listings stay compact while remaining copy-paste
// friendly for "dfsctl ... backup show" prefix-hunting in scripts. D-26.
func shortULID(id string) string {
	const prefixLen = 8
	if len(id) <= prefixLen {
		return id
	}
	return id[:prefixLen] + "\u2026"
}

// timeAgo renders a duration relative to t such as "30s ago", "3m ago",
// "3h ago", or "2d ago". Used in table-mode rendering for D-26's CREATED
// / STARTED columns — JSON/YAML modes surface the raw RFC3339 timestamp.
func timeAgo(t time.Time) string {
	return timeAgoSince(t, time.Now())
}

// timeAgoSince is the testable seam — callers can pin "now" deterministically.
func timeAgoSince(t, now time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// humanSize renders a byte count using binary units ("1.0MB", "234KB",
// "12B"). Matches the existing dfsctl convention of a single decimal place
// and no space before the suffix (see `internal/cli/timeutil` analogs).
func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// renderProgressBar renders a fixed-width 20-cell progress bar for D-47.
// Caller is responsible for suppressing it in non-table modes (JSON/YAML
// surfaces the numeric Progress field instead). Out-of-range inputs are
// clamped rather than errored — this is a rendering helper, not input
// validation.
func renderProgressBar(pct int) string {
	const width = 20
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := pct * width / 100
	return fmt.Sprintf("%d%%  [%s%s]", pct,
		strings.Repeat("\u2593", filled),
		strings.Repeat("\u2591", width-filled))
}
