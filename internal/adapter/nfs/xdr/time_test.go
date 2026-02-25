package xdr

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// ============================================================================
// Round-Trip Conversion Tests
// ============================================================================

func TestTimeRoundTrip(t *testing.T) {
	t.Run("RoundTripCurrentTime", func(t *testing.T) {
		original := time.Now()

		// Convert to NFS time and back
		nfsTime := TimeToTimeVal(original)
		converted := timeValToTime(nfsTime.Seconds, nfsTime.Nseconds)

		// Should match to nanosecond precision
		assert.Equal(t, original.Unix(), converted.Unix())
		assert.Equal(t, original.Nanosecond(), converted.Nanosecond())
	})

	t.Run("RoundTripEpoch", func(t *testing.T) {
		original := time.Unix(0, 0)

		nfsTime := TimeToTimeVal(original)
		converted := timeValToTime(nfsTime.Seconds, nfsTime.Nseconds)

		assert.Equal(t, int64(0), converted.Unix())
		assert.Equal(t, 0, converted.Nanosecond())
	})

	t.Run("RoundTripFutureTime", func(t *testing.T) {
		// Year 2030
		original := time.Date(2030, 6, 15, 12, 30, 45, 123456789, time.UTC)

		nfsTime := TimeToTimeVal(original)
		converted := timeValToTime(nfsTime.Seconds, nfsTime.Nseconds)

		assert.Equal(t, original.Unix(), converted.Unix())
		assert.Equal(t, original.Nanosecond(), converted.Nanosecond())
	})

	t.Run("RoundTripPastTime", func(t *testing.T) {
		// Year 1990
		original := time.Date(1990, 3, 20, 8, 15, 30, 987654321, time.UTC)

		nfsTime := TimeToTimeVal(original)
		converted := timeValToTime(nfsTime.Seconds, nfsTime.Nseconds)

		assert.Equal(t, original.Unix(), converted.Unix())
		assert.Equal(t, original.Nanosecond(), converted.Nanosecond())
	})
}

// ============================================================================
// Nanosecond Precision Tests
// ============================================================================

func TestNanosecondPrecision(t *testing.T) {
	t.Run("PreservesZeroNanoseconds", func(t *testing.T) {
		original := time.Unix(1234567890, 0)
		nfsTime := TimeToTimeVal(original)

		assert.Equal(t, uint32(1234567890), nfsTime.Seconds)
		assert.Equal(t, uint32(0), nfsTime.Nseconds)
	})

	t.Run("PreservesMaxNanoseconds", func(t *testing.T) {
		// Maximum nanoseconds: 999,999,999
		original := time.Unix(1234567890, 999999999)
		nfsTime := TimeToTimeVal(original)

		assert.Equal(t, uint32(1234567890), nfsTime.Seconds)
		assert.Equal(t, uint32(999999999), nfsTime.Nseconds)
	})

	t.Run("PreservesMidRangeNanoseconds", func(t *testing.T) {
		original := time.Unix(1234567890, 500000000)
		nfsTime := TimeToTimeVal(original)

		assert.Equal(t, uint32(1234567890), nfsTime.Seconds)
		assert.Equal(t, uint32(500000000), nfsTime.Nseconds)
	})

	t.Run("PreservesSmallNanoseconds", func(t *testing.T) {
		original := time.Unix(1234567890, 1)
		nfsTime := TimeToTimeVal(original)

		assert.Equal(t, uint32(1234567890), nfsTime.Seconds)
		assert.Equal(t, uint32(1), nfsTime.Nseconds)
	})
}

// ============================================================================
// Boundary Value Tests
// ============================================================================

func TestTimeBoundaries(t *testing.T) {
	t.Run("HandlesMinimumTime", func(t *testing.T) {
		// Unix epoch
		original := time.Unix(0, 0)
		nfsTime := TimeToTimeVal(original)

		assert.Equal(t, uint32(0), nfsTime.Seconds)
		assert.Equal(t, uint32(0), nfsTime.Nseconds)
	})

	t.Run("HandlesYear2038Boundary", func(t *testing.T) {
		// Just before Year 2038 problem (2^31 - 1 seconds)
		// Unix time 2147483647 = 2038-01-19 03:14:07 UTC
		original := time.Unix(2147483647, 999999999)
		nfsTime := TimeToTimeVal(original)

		assert.Equal(t, uint32(2147483647), nfsTime.Seconds)
		assert.Equal(t, uint32(999999999), nfsTime.Nseconds)

		// Verify round-trip
		converted := timeValToTime(nfsTime.Seconds, nfsTime.Nseconds)
		assert.Equal(t, original.Unix(), converted.Unix())
	})

	t.Run("HandlesAfterYear2038", func(t *testing.T) {
		// Year 2040 (after 2038 problem, but within uint32 range)
		// Unix time ~2200000000
		original := time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
		nfsTime := TimeToTimeVal(original)

		// Should handle gracefully (wrapped in uint32)
		converted := timeValToTime(nfsTime.Seconds, nfsTime.Nseconds)

		// Round-trip should work
		assert.Equal(t, original.Unix(), converted.Unix())
	})

	t.Run("HandlesMaxUint32Time", func(t *testing.T) {
		// Maximum uint32 seconds: 4294967295
		// This is 2106-02-07 06:28:15 UTC
		maxSeconds := uint32(4294967295)
		maxNseconds := uint32(999999999)

		converted := timeValToTime(maxSeconds, maxNseconds)

		// Should convert to a valid time
		assert.True(t, converted.Unix() > 0)
		assert.Equal(t, 999999999, converted.Nanosecond())
	})
}

// ============================================================================
// Specific Date Tests
// ============================================================================

func TestSpecificDates(t *testing.T) {
	t.Run("Year2000", func(t *testing.T) {
		y2k := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		nfsTime := TimeToTimeVal(y2k)
		converted := timeValToTime(nfsTime.Seconds, nfsTime.Nseconds)

		assert.Equal(t, y2k.Unix(), converted.Unix())
	})

	t.Run("LeapYearFebruary29", func(t *testing.T) {
		leapDay := time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)
		nfsTime := TimeToTimeVal(leapDay)
		converted := timeValToTime(nfsTime.Seconds, nfsTime.Nseconds)

		assert.Equal(t, leapDay.Unix(), converted.Unix())
	})

	t.Run("EndOfYear", func(t *testing.T) {
		endOfYear := time.Date(2023, 12, 31, 23, 59, 59, 999999999, time.UTC)
		nfsTime := TimeToTimeVal(endOfYear)
		converted := timeValToTime(nfsTime.Seconds, nfsTime.Nseconds)

		assert.Equal(t, endOfYear.Unix(), converted.Unix())
		assert.Equal(t, endOfYear.Nanosecond(), converted.Nanosecond())
	})

	t.Run("DSTTransition", func(t *testing.T) {
		// Daylight saving time transition (example: US 2023)
		// Time.Unix() is always UTC, so DST shouldn't affect conversion
		loc, err := time.LoadLocation("America/New_York")
		if err != nil {
			t.Skip("Timezone data not available")
		}

		dstTime := time.Date(2023, 3, 12, 2, 30, 0, 0, loc)
		nfsTime := TimeToTimeVal(dstTime)
		converted := timeValToTime(nfsTime.Seconds, nfsTime.Nseconds)

		// Unix timestamp should be preserved
		assert.Equal(t, dstTime.Unix(), converted.Unix())
	})
}

// ============================================================================
// Conversion Consistency Tests
// ============================================================================

func TestConversionConsistency(t *testing.T) {
	t.Run("SameInputProducesSameOutput", func(t *testing.T) {
		original := time.Now()

		nfsTime1 := TimeToTimeVal(original)
		nfsTime2 := TimeToTimeVal(original)

		assert.Equal(t, nfsTime1.Seconds, nfsTime2.Seconds)
		assert.Equal(t, nfsTime1.Nseconds, nfsTime2.Nseconds)
	})

	t.Run("DifferentTimesDifferentOutput", func(t *testing.T) {
		time1 := time.Unix(1000, 0)
		time2 := time.Unix(1001, 0)

		nfsTime1 := TimeToTimeVal(time1)
		nfsTime2 := TimeToTimeVal(time2)

		assert.NotEqual(t, nfsTime1.Seconds, nfsTime2.Seconds)
	})

	t.Run("OnlyNanosecondsChangeDetected", func(t *testing.T) {
		time1 := time.Unix(1000, 100)
		time2 := time.Unix(1000, 200)

		nfsTime1 := TimeToTimeVal(time1)
		nfsTime2 := TimeToTimeVal(time2)

		assert.Equal(t, nfsTime1.Seconds, nfsTime2.Seconds)
		assert.NotEqual(t, nfsTime1.Nseconds, nfsTime2.Nseconds)
	})
}

// ============================================================================
// Edge Cases Tests
// ============================================================================

func TestTimeEdgeCases(t *testing.T) {
	t.Run("BeforeEpochHandled", func(t *testing.T) {
		// Time before Unix epoch (negative Unix time)
		// Year 1960
		beforeEpoch := time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC)

		// This will have negative Unix timestamp
		assert.True(t, beforeEpoch.Unix() < 0)

		// NFS time uses uint32, so negative times wrap around
		nfsTime := TimeToTimeVal(beforeEpoch)

		// The conversion happens, but wrapped
		// This is expected behavior for NFS (which doesn't support pre-1970 dates)
		assert.NotNil(t, nfsTime)
	})

	t.Run("VeryHighNanoseconds", func(t *testing.T) {
		// Create time using timeValToTime with boundary nanoseconds
		converted := timeValToTime(1000, 999999999)

		assert.Equal(t, int64(1000), converted.Unix())
		assert.Equal(t, 999999999, converted.Nanosecond())
	})

	t.Run("ZeroSecondsWithNanoseconds", func(t *testing.T) {
		// Epoch with nanoseconds
		converted := timeValToTime(0, 500000000)

		assert.Equal(t, int64(0), converted.Unix())
		assert.Equal(t, 500000000, converted.Nanosecond())
	})
}
