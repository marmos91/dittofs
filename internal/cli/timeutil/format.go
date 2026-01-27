// Package timeutil provides time formatting utilities for CLI output.
package timeutil

import (
	"fmt"
	"time"
)

// LocalTimeFormat is the format used for displaying local times in CLI output.
// Uses Go's reference time: Mon Jan 2 15:04:05 2006.
const LocalTimeFormat = "Mon Jan 2 15:04:05 2006"

// FormatUptime converts a duration string to a human-readable format.
// Input is expected to be a Go duration string (e.g., "72h30m15s").
// Returns a formatted string like "3d 0h 30m 15s" or the original string if parsing fails.
func FormatUptime(uptime string) string {
	d, err := time.ParseDuration(uptime)
	if err != nil {
		return uptime
	}

	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm %ds", days, hours, minutes, seconds)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

// FormatTime parses an RFC3339 timestamp and returns a local time string.
// Returns the original string if parsing fails.
func FormatTime(timestamp string) string {
	t, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return timestamp
	}
	return t.Local().Format(LocalTimeFormat)
}
