// Package backupfmt provides table-formatting helpers shared by the
// `dfsctl store metadata <name> backup` and `... backup job` command
// subtrees. The subpackage exists so both can import a single copy —
// `backup/job` cannot import its parent `backup` without a cycle.
package backupfmt

import (
	"fmt"
	"strings"
	"time"
)

// ShortULID returns the first 8 chars of a ULID followed by an ellipsis
// (U+2026 "…") so table listings stay compact while remaining copy-paste
// friendly for "dfsctl ... backup show" prefix-hunting in scripts. D-26.
func ShortULID(id string) string {
	const prefixLen = 8
	if len(id) <= prefixLen {
		return id
	}
	return id[:prefixLen] + "\u2026"
}

// TimeAgo renders a duration relative to t such as "30s ago", "3m ago",
// "3h ago", or "2d ago". Used in table-mode rendering for D-26's CREATED
// / STARTED columns — JSON/YAML modes surface the raw RFC3339 timestamp.
func TimeAgo(t time.Time) string {
	return TimeAgoSince(t, time.Now())
}

// TimeAgoSince is the testable seam — callers can pin "now" deterministically.
func TimeAgoSince(t, now time.Time) string {
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

// RenderProgressBar renders a fixed-width 20-cell progress bar for D-47.
// Caller is responsible for suppressing it in non-table modes (JSON/YAML
// surfaces the numeric Progress field instead). Out-of-range inputs are
// clamped rather than errored — this is a rendering helper, not input
// validation.
func RenderProgressBar(pct int) string {
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
