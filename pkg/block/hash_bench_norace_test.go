//go:build !race

package block_test

// raceEnabled reports whether the binary was built with -race.
// Normal (non-race) build: false — the perf gate runs.
const raceEnabled = false
