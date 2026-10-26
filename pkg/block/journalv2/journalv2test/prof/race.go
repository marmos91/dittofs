//go:build race

package prof

// raceEnabled reports whether the race detector is on. It changes HeapAlloc,
// so the heap check does not run under it.
const raceEnabled = true
