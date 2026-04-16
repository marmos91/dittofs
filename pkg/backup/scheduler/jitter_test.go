package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPhaseOffset_InRange — T1: PhaseOffset stays within [0, max).
func TestPhaseOffset_InRange(t *testing.T) {
	tests := []struct {
		name   string
		repoID string
		max    time.Duration
	}{
		{"repo-a 5min", "repo-a", 5 * time.Minute},
		{"repo-b 5min", "repo-b", 5 * time.Minute},
		{"short repo 1min", "x", 1 * time.Minute},
		{"long repo 1h", "long-repo-id-that-is-quite-verbose", time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PhaseOffset(tc.repoID, tc.max)
			require.GreaterOrEqual(t, int64(got), int64(0), "offset should be >= 0")
			require.Less(t, int64(got), int64(tc.max), "offset should be < max")
		})
	}
}

// TestPhaseOffset_Stable — T2: same repo ID always yields the same offset.
// Runs many iterations to catch any non-deterministic behavior.
func TestPhaseOffset_Stable(t *testing.T) {
	const iterations = 1000
	id := "stable-repo-id"
	max := 5 * time.Minute
	first := PhaseOffset(id, max)
	for i := 0; i < iterations; i++ {
		require.Equal(t, first, PhaseOffset(id, max), "PhaseOffset must be deterministic (iter %d)", i)
	}
}

// TestPhaseOffset_DifferentIDs — T3: different repo IDs yield different offsets
// with overwhelming probability (the FNV-1a hash differs).
func TestPhaseOffset_DifferentIDs(t *testing.T) {
	max := 5 * time.Minute
	a := PhaseOffset("repo-a", max)
	b := PhaseOffset("repo-b", max)
	// These two specific IDs should not collide under FNV-1a % 300.
	require.NotEqual(t, a, b, "repo-a and repo-b should hash to different offsets")
}

// TestPhaseOffset_ZeroMax — T4: max<=0 returns 0 (safe guard).
func TestPhaseOffset_ZeroMax(t *testing.T) {
	tests := []struct {
		name string
		max  time.Duration
	}{
		{"zero", 0},
		{"negative", -5 * time.Minute},
		{"sub-second", 500 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, time.Duration(0), PhaseOffset("any", tc.max))
		})
	}
}

// TestPhaseOffset_Spread — T5: 20 distinct repo IDs with max=300s should yield
// reasonable spread. We allow at most 2 collisions (statistical check, not a
// strict correctness assertion; catches degenerate implementations).
func TestPhaseOffset_Spread(t *testing.T) {
	const (
		n         = 20
		maxColl   = 2
		windowSec = 300
	)
	max := time.Duration(windowSec) * time.Second
	seen := make(map[time.Duration]int)
	for i := 0; i < n; i++ {
		id := "repo-" + string(rune('a'+i))
		seen[PhaseOffset(id, max)]++
	}
	collisions := 0
	for _, c := range seen {
		if c > 1 {
			collisions += c - 1
		}
	}
	require.LessOrEqualf(t, collisions, maxColl,
		"expected <=%d collisions over %d ids/%d-sec window, got %d", maxColl, n, windowSec, collisions)
}

// TestPhaseOffset_EmptyID — edge case, still deterministic.
func TestPhaseOffset_EmptyID(t *testing.T) {
	max := 5 * time.Minute
	a := PhaseOffset("", max)
	b := PhaseOffset("", max)
	require.Equal(t, a, b)
	require.GreaterOrEqual(t, int64(a), int64(0))
	require.Less(t, int64(a), int64(max))
}

// TestDefaultMaxJitter — exposed constant matches D-04 (5 minutes).
func TestDefaultMaxJitter(t *testing.T) {
	require.Equal(t, 5*time.Minute, DefaultMaxJitter)
}
