package shares

import (
	"math"
	"testing"
	"time"
)

// The dirty_expire_seconds knob decides how long an acknowledged write may stay
// non-durable, so every branch of its parse is worth pinning: a value silently
// read as zero would hand the share the default instead of what the operator
// asked for, and a value read as negative would switch the loop off entirely.
func TestDirtyExpiryFromConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  map[string]any
		want time.Duration
	}{
		{"absent takes the journal default", map[string]any{}, 0},
		{"plain value", map[string]any{"dirty_expire_seconds": float64(5)}, 5 * time.Second},
		{"fractional value above the floor", map[string]any{"dirty_expire_seconds": 2.5}, 2500 * time.Millisecond},
		{"sub-second clamps to the floor", map[string]any{"dirty_expire_seconds": 0.001}, minDirtyExpire},
		{"just under the floor clamps", map[string]any{"dirty_expire_seconds": 0.999}, minDirtyExpire},
		{"at the floor is kept", map[string]any{"dirty_expire_seconds": float64(1)}, time.Second},
		{"negative disables", map[string]any{"dirty_expire_seconds": float64(-1)}, -time.Second},
		{"explicit zero takes the default", map[string]any{"dirty_expire_seconds": float64(0)}, 0},
		{"non-numeric ignored", map[string]any{"dirty_expire_seconds": "30s"}, 0},
		{"bool ignored", map[string]any{"dirty_expire_seconds": true}, 0},
		{"NaN ignored", map[string]any{"dirty_expire_seconds": math.NaN()}, 0},
		{"+Inf ignored", map[string]any{"dirty_expire_seconds": math.Inf(1)}, 0},
		{"-Inf ignored", map[string]any{"dirty_expire_seconds": math.Inf(-1)}, 0},
		{"overflows a Duration, ignored", map[string]any{"dirty_expire_seconds": 1e18}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dirtyExpiryFromConfig(tc.cfg); got != tc.want {
				t.Fatalf("dirtyExpiryFromConfig = %v, want %v", got, tc.want)
			}
		})
	}
}
