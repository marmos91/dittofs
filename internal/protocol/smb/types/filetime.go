package types

import "time"

// Windows FILETIME epoch: January 1, 1601 UTC
// Difference from Unix epoch (January 1, 1970) in 100-nanosecond intervals
const filetimeUnixDiff = 116444736000000000

// TimeToFiletime converts Go time.Time to Windows FILETIME
// FILETIME is a 64-bit value representing the number of 100-nanosecond intervals
// since January 1, 1601 UTC
func TimeToFiletime(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	// Convert to Unix nanoseconds, then to 100-ns intervals, then add offset
	return uint64(t.UnixNano()/100) + filetimeUnixDiff
}

// FiletimeToTime converts Windows FILETIME to Go time.Time
func FiletimeToTime(ft uint64) time.Time {
	if ft == 0 || ft < filetimeUnixDiff {
		return time.Time{}
	}
	// Subtract offset, convert from 100-ns to nanoseconds
	nsec := int64(ft-filetimeUnixDiff) * 100
	return time.Unix(0, nsec)
}

// NowFiletime returns the current time as a Windows FILETIME
func NowFiletime() uint64 {
	return TimeToFiletime(time.Now())
}
