//go:build !race

package segment_test

// descRace reports whether the race detector is on. It is off here, so the
// hit-allocation check runs and bounds the read path.
const descRace = false
