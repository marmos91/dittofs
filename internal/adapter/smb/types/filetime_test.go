package types

import (
	"testing"
	"time"
)

func TestFiletimeRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		unixSec int64
	}{
		{"time_t_0", 0},                     // Unix epoch: 1970-01-01 00:00:00
		{"time_t_-1", -1},                   // 1969-12-31 23:59:59
		{"time_t_-2", -2},                   // 1969-12-31 23:59:58
		{"time_t_1968", -63158400},          // ~1968-01-01
		{"time_t_10000000000", 10000000000}, // 2286-11-20
		{"time_t_15032385535", 15032385535}, // 2446-05-10
		{"time_t_recent", 1710000000},       // 2024-03-09
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := time.Unix(tt.unixSec, 0)
			ft := TimeToFiletime(original)
			if ft == 0 {
				t.Fatalf("TimeToFiletime returned 0 for %v", original)
			}
			roundtrip := FiletimeToTime(ft)

			// Compare to second precision (FILETIME has 100ns granularity,
			// but time.Unix(sec, 0) has no sub-second component)
			if roundtrip.Unix() != original.Unix() {
				t.Errorf("roundtrip mismatch: got %v (unix %d), want %v (unix %d)",
					roundtrip, roundtrip.Unix(), original, original.Unix())
			}
		})
	}
}

func TestFiletimeToTime_Zero(t *testing.T) {
	result := FiletimeToTime(0)
	if !result.IsZero() {
		t.Errorf("FiletimeToTime(0) should return zero time, got %v", result)
	}
}

func TestTimeToFiletime_Zero(t *testing.T) {
	result := TimeToFiletime(time.Time{})
	if result != 0 {
		t.Errorf("TimeToFiletime(zero) should return 0, got %d", result)
	}
}

func TestFiletimeToTime_PreUnixEpoch(t *testing.T) {
	// FILETIME for 1969-12-31 23:59:59 UTC = filetimeUnixDiff - 10000000
	ft := uint64(filetimeUnixDiff - 10000000)
	result := FiletimeToTime(ft)

	expected := time.Unix(-1, 0)
	if result.Unix() != expected.Unix() {
		t.Errorf("FiletimeToTime(%d) = %v (unix %d), want %v (unix %d)",
			ft, result, result.Unix(), expected, expected.Unix())
	}
}

func TestFiletimeToTime_WindowsEpoch(t *testing.T) {
	// FILETIME = 1 represents 100ns after Jan 1, 1601
	result := FiletimeToTime(1)
	if result.IsZero() {
		t.Error("FiletimeToTime(1) should not return zero time")
	}
	// Should be way before Unix epoch
	if result.Year() != 1601 {
		t.Errorf("FiletimeToTime(1) year = %d, want 1601", result.Year())
	}
}
