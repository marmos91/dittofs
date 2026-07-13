package postgres

import (
	"testing"
	"time"
)

// TestTimestampRoundTrip mirrors the SQLite store's test: it locks in
// FILETIME-encoded timestamp fidelity across the full range and at the sentinel
// boundary. The extreme cases are the exact values smbtorture
// smb2.timestamps.time_t_* set — they regressed on the old unix-nanosecond
// encoding (overflow past year ~2262; unix epoch colliding with the zero-time
// sentinel). Keeping this in lockstep with the SQLite test guards against the
// two verbatim-port codecs drifting apart.
func TestTimestampRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
	}{
		{"unix-epoch-1970 (time_t_0)", time.Unix(0, 0).UTC()},
		{"year-2286 (time_t_10000000000)", time.Unix(10000000000, 0).UTC()},
		{"year-2446 (time_t_15032385535)", time.Unix(15032385535, 0).UTC()},
		{"100ns-aligned (storetest)", time.Unix(1700000001, 100).UTC()},
		{"sub-second 100ns", time.Unix(1699999998, 999999900).UTC()},
		{"pre-1970 negative ticks", time.Date(1801, 6, 15, 12, 30, 0, 500, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pgNanosToTime(timeToPGNanos(c.in))
			if !got.Equal(c.in) {
				t.Fatalf("round-trip mismatch: in=%v got=%v", c.in, got)
			}
		})
	}

	if n := timeToPGNanos(time.Time{}); n != 0 {
		t.Fatalf("zero time should encode to 0 sentinel, got %d", n)
	}
	if got := pgNanosToTime(0); !got.IsZero() {
		t.Fatalf("0 sentinel should decode to zero time, got %v", got)
	}
	if timeToPGNanos(time.Unix(0, 0)) == 0 {
		t.Fatal("unix epoch must not collide with the 0 zero-time sentinel")
	}
}
