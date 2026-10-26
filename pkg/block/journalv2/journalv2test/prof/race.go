//go:build race

package prof

// raceEnabled reports whether the race detector is on. It changes how much
// heap the runtime keeps, so the heap check does not run under it.
const raceEnabled = true
