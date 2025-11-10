package xdr

import (
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
)

// ============================================================================
// Time Conversion Helpers
// ============================================================================

// timeValToTime converts NFS time (nfstime3) to Go time.Time.
//
// Per RFC 1813 Section 2.2 (nfstime3):
//
//	struct nfstime3 {
//	    uint32 seconds;   // Seconds since Unix epoch (1970-01-01 00:00:00 UTC)
//	    uint32 nseconds;  // Nanoseconds within the second (0-999,999,999)
//	};
//
// Parameters:
//   - seconds: Seconds since Unix epoch
//   - nseconds: Nanoseconds component (0-999,999,999)
//
// Returns:
//   - time.Time: Go time representation
func timeValToTime(seconds, nseconds uint32) time.Time {
	return time.Unix(int64(seconds), int64(nseconds))
}

// TimeToTimeVal converts Go time.Time to NFS time (nfstime3).
//
// Parameters:
//   - t: Go time to convert
//
// Returns:
//   - TimeVal: NFS time representation (seconds + nanoseconds)
func TimeToTimeVal(t time.Time) types.TimeVal {
	return types.TimeVal{
		Seconds:  uint32(t.Unix()),
		Nseconds: uint32(t.Nanosecond()),
	}
}

// getCurrentTime returns the current time.
//
// This is separated into its own function to facilitate:
// - Testing: can be mocked to return deterministic times
// - Consistency: single source of truth for "now"
// - Future extensions: could add time skew correction, etc.
//
// Returns:
//   - time.Time: Current system time
func getCurrentTime() time.Time {
	return time.Now()
}
