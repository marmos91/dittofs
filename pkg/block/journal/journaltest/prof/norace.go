//go:build !race

package prof

// RaceEnabled reports whether the race detector is on. It changes how much
// heap the runtime keeps, so prof's own heap check does not run under it,
// and the allocation-bounding checks that borrow it skip their count there:
// the detector instruments every lock and atomic operation, so an allocation
// count taken under it says nothing about the code under test.
const RaceEnabled = false
