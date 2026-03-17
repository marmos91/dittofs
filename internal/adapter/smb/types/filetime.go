package types

import (
	"math"
	"time"
)

// Windows FILETIME epoch: January 1, 1601 UTC
// Difference from Unix epoch (January 1, 1970) in 100-nanosecond intervals
const filetimeUnixDiff = 116444736000000000

// ticksPerSecond is the number of 100-nanosecond intervals per second.
const ticksPerSecond = 10000000

// TimeToFiletime converts Go time.Time to Windows FILETIME.
// FILETIME is a 64-bit value representing the number of 100-nanosecond intervals
// since January 1, 1601 UTC.
//
// Uses seconds + nanosecond remainder to avoid int64 overflow that occurs
// with time.UnixNano() for dates beyond year ~2262.
func TimeToFiletime(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	sec := t.Unix()
	nsec := int64(t.Nanosecond())
	return uint64(sec*ticksPerSecond + nsec/100 + int64(filetimeUnixDiff))
}

// FiletimeToTime converts Windows FILETIME to Go time.Time.
//
// Handles the full FILETIME range including pre-Unix-epoch dates (1601-1970)
// and far-future dates (beyond 2262). Splits into seconds and sub-second
// remainder to avoid int64 overflow on the nanosecond multiplication.
func FiletimeToTime(ft uint64) time.Time {
	if ft == 0 || ft > uint64(math.MaxInt64) {
		return time.Time{}
	}
	ticks := int64(ft) - int64(filetimeUnixDiff)
	sec := ticks / ticksPerSecond
	rem := ticks % ticksPerSecond
	return time.Unix(sec, rem*100)
}

// NowFiletime returns the current time as a Windows FILETIME
func NowFiletime() uint64 {
	return TimeToFiletime(time.Now())
}
