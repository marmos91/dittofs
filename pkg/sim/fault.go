package sim

import "time"

// Fault decides what a seam does with each operation it is about to perform.
// A seam calls At before every operation, with the operation's name and the
// key it acts on, and applies the results. Its results are standard-library
// values, so a seam that may import only the standard library declares this
// interface in its own package and a Schedule satisfies it as is.
//
// Zero results pass. err alone fails the operation. tearAfter > 0 tears a
// write after that many bytes: with err nil the write is reported whole and
// only those bytes can become durable; with err set only they land, and the
// write fails. tearAfter < 0, Lose, loses a write or a sync: it is reported
// done and makes nothing durable. A tear of any other operation, or a loss of
// one or with err set, is a mistake in the test, which the seam refuses.
//
// Which unsynced writes a crash keeps, and in what order, is the seam's
// simulated crash, never a call's fault.
type Fault interface {
	At(op string, key []byte) (delay time.Duration, tearAfter int, err error)
}

// Lose is the tearAfter that loses a write or a sync.
const Lose = -1

// FaultAction is what a Schedule's rule decides for one operation: the three
// results of Fault.At.
//
// decision: one plain struct, so a schedule's rules are data that a test can
// build in a literal and compare with ==. Which ops a tear or a loss applies
// to is the seam's contract, not the Fault's: a seam handed one it cannot
// apply to an op panics, naming both, because that is a mistake in the test,
// not a storage fault the code under test should see.
type FaultAction struct {
	Delay     time.Duration // how long At waits on the schedule's clock first
	Err       error         // fails the operation, or a torn write
	TearAfter int           // > 0: the bytes a write keeps; Lose: the operation is lost
}
