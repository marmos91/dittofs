package bench

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// latencyStats holds computed percentile values in microseconds.
type latencyStats struct {
	P50 float64
	P95 float64
	P99 float64
	Avg float64
}

// computePercentiles calculates p50, p95, p99, and average latency from a
// slice of durations. Returns zero stats if the input is empty.
func computePercentiles(samples []time.Duration) latencyStats {
	if len(samples) == 0 {
		return latencyStats{}
	}

	// Sort a copy to avoid mutating the caller's slice.
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	slices.Sort(sorted)

	percentile := func(p float64) float64 {
		idx := max(int(math.Ceil(p/100*float64(len(sorted))))-1, 0)
		return float64(sorted[idx].Microseconds())
	}

	var total float64
	for _, d := range sorted {
		total += float64(d.Microseconds())
	}

	return latencyStats{
		P50: percentile(50),
		P95: percentile(95),
		P99: percentile(99),
		Avg: total / float64(len(sorted)),
	}
}

// FormatSize converts bytes to a human-readable string (e.g., "1.0 GiB").
func FormatSize(bytes int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
		tib = 1 << 40
	)

	switch {
	case bytes >= tib:
		return fmt.Sprintf("%.1f TiB", float64(bytes)/float64(tib))
	case bytes >= gib:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(gib))
	case bytes >= mib:
		return fmt.Sprintf("%.1f MiB", float64(bytes)/float64(mib))
	case bytes >= kib:
		return fmt.Sprintf("%.1f KiB", float64(bytes)/float64(kib))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// ParseSize parses a human-readable size string into bytes.
// Accepted suffixes (case-insensitive): B, KiB, MiB, GiB, TiB, KB, MB, GB, TB.
// A bare number is treated as bytes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}

	// Map of suffix → multiplier.
	suffixes := map[string]int64{
		"TIB": 1 << 40,
		"GIB": 1 << 30,
		"MIB": 1 << 20,
		"KIB": 1 << 10,
		"TB":  1 << 40,
		"GB":  1 << 30,
		"MB":  1 << 20,
		"KB":  1 << 10,
		"B":   1,
	}

	upper := strings.ToUpper(s)

	// Try longest suffixes first to avoid "B" matching before "GIB".
	for _, suffix := range []string{"TIB", "GIB", "MIB", "KIB", "TB", "GB", "MB", "KB", "B"} {
		if strings.HasSuffix(upper, suffix) {
			numStr := strings.TrimSpace(s[:len(s)-len(suffix)])
			var val float64
			if _, err := fmt.Sscanf(numStr, "%f", &val); err != nil {
				return 0, fmt.Errorf("invalid size %q: %w", s, err)
			}
			return int64(val * float64(suffixes[suffix])), nil
		}
	}

	// No suffix — treat as bytes.
	var val float64
	if _, err := fmt.Sscanf(s, "%f", &val); err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	return int64(val), nil
}
