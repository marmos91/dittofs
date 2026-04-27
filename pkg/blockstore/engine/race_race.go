//go:build race

package engine

// raceEnabled is true when the test binary was built with -race. See the
// !race counterpart for rationale.
const raceEnabled = true
