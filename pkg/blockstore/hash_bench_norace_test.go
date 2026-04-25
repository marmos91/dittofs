//go:build !race

package blockstore_test

// raceEnabled reports whether the binary was built with -race.
// Normal (non-race) build: false — the D-41 perf gate runs.
const raceEnabled = false
