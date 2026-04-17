// Package backup implements per-store backup management commands.
package backup

import "fmt"

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
