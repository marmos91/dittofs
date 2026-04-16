package scheduler

import (
	"hash/fnv"
	"time"
)

// DefaultMaxJitter is the default jitter window applied to cron firings so
// repos sharing a schedule fire at spread times (D-04). 5 minutes spreads
// ~20 repos by ~15 seconds each, enough to avoid S3 rate-limit spikes
// without meaningfully delaying individual backups.
const DefaultMaxJitter = 5 * time.Minute

// PhaseOffset returns a stable per-repo time offset within [0, max).
// Stability is guaranteed by FNV-1a over the repo ID — the same ID
// always produces the same offset across restarts (D-03). Operators
// can correlate "repo X always fires at 00:03:42" with ops events.
//
// max <= 0 returns 0. A max below one second also returns 0 because
// the offset is quantized to seconds (aligning with cron-resolution
// granularity and keeping arithmetic in unsigned integer space).
func PhaseOffset(repoID string, max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	seconds := uint64(max / time.Second)
	if seconds == 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(repoID))
	return time.Duration(h.Sum64()%seconds) * time.Second
}
