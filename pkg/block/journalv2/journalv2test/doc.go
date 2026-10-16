// Package journalv2test provides what tests of the journal share: a storage
// seam that simulates a crash by discarding unsynced writes and can fail any
// single call, a reference model results are checked against, the profiling
// harness, and the adapter from the shared clock to the journal's clock.
//
// It is the only package of the journal that may import the shared clock and
// fault interfaces.
package journalv2test
