// Package journalv2test provides what tests of the journal share: a storage
// seam that simulates a crash by discarding unsynced writes and can fail any
// single call, a reference model results are checked against, the profiling
// harness, and the options only tests may set.
//
// It is the only package of the journal that may import the node's clock and
// the shared test clock and fault interfaces.
package journalv2test
