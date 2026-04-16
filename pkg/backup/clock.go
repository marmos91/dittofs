package backup

import "time"

// Clock is an injectable time source. Tests inject a fake clock so
// time-dependent assertions are deterministic.
type Clock interface {
	Now() time.Time
}

// RealClock returns the current UTC time.
type RealClock struct{}

// Now returns time.Now().UTC().
func (RealClock) Now() time.Time { return time.Now().UTC() }
