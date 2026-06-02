//go:build !race

package fs

// raceEnabled reports whether the binary was built with -race.
// yellow-flag perf benches honor this flag and skip
// under -race so detector overhead (mutex tracking, atomic checks
// goroutine bookkeeping) doesn't false-fail the perf ratios.
const raceEnabled = false
