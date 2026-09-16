package smb

import (
	"testing"
	"time"
)

// TestClampOplockBreakTimeout pins the clamp against the range the settings API
// advertises. A stored value outside the range must not reach the break-wait
// path unclamped: zero would collapse the wait, and a huge value would park a
// conflicting CREATE indefinitely.
func TestClampOplockBreakTimeout(t *testing.T) {
	cases := []struct {
		name    string
		seconds int
		want    time.Duration
	}{
		{"in range", 90, 90 * time.Second},
		{"at min", 5, 5 * time.Second},
		{"at max", 120, 120 * time.Second},
		{"below min clamps up", 1, 5 * time.Second},
		{"zero clamps up", 0, 5 * time.Second},
		{"negative clamps up", -30, 5 * time.Second},
		{"above max clamps down", 9999, 120 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampOplockBreakTimeout(tc.seconds); got != tc.want {
				t.Errorf("clampOplockBreakTimeout(%d) = %v, want %v", tc.seconds, got, tc.want)
			}
		})
	}
}
