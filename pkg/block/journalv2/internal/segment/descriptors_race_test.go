//go:build race

package segment_test

// descRace reports whether the race detector is on. It instruments every lock
// and atomic operation, so an allocation count taken under it says nothing
// about the code under test: the hit-allocation check bounds the read path
// only where this is false.
const descRace = true
