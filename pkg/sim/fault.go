package sim

import (
	"fmt"
	"time"
)

// Fault decides what a seam does with each operation it is about to perform.
// A seam calls At before every operation, with the operation's name and the
// key it acts on, and applies the action returned. At may block: a Delay is
// waited out inside At, on the Fault's clock, before it returns.
type Fault interface {
	At(op string, key []byte) FaultAction
}

// FaultKind is what a seam does with one operation.
type FaultKind int

const (
	// Pass performs the operation as asked. It is the zero FaultKind, so a
	// zero FaultAction passes.
	Pass FaultKind = iota
	// Fail does not perform the operation and returns Err.
	Fail
	// Delay performs the operation after Delay has elapsed on the Fault's
	// clock. At has already waited when it returns this kind, so a seam
	// applies it as Pass.
	Delay
	// Drop reports success, and the operation does not become durable. What
	// that leaves behind for each operation is the seam's contract.
	Drop
	// Tear tears a write after its first N bytes. With Err nil it performs
	// the write and reports it whole, but only the first N bytes can become
	// durable; with Err set only those bytes are written, and it reports them
	// and returns Err, as a write that failed part-way. With N at or past the
	// write's length nothing is torn, and the write is reported whole.
	Tear
	// Reorder performs a write and marks it as one a crash may keep or lose
	// out of order.
	Reorder
)

var kindNames = [...]string{
	Pass:    "pass",
	Fail:    "fail",
	Delay:   "delay",
	Drop:    "drop",
	Tear:    "tear",
	Reorder: "reorder",
}

// String returns the kind's name, as "pass" or "tear".
func (k FaultKind) String() string {
	if k < 0 || int(k) >= len(kindNames) {
		return fmt.Sprintf("FaultKind(%d)", int(k))
	}
	return kindNames[k]
}

// FaultAction is what a Fault decides for one operation. Only the fields of
// its Kind are read: Err for Fail, Delay for Delay, N and Err for Tear.
//
// decision: one struct for every kind, not one type per kind, so a schedule's
// rules are plain data that a test can build in a literal and compare with ==.
// Which ops each kind applies to is the seam's contract, not the Fault's: a
// seam handed a kind it cannot apply to an op panics, naming both, because
// that is a mistake in the test, not a storage fault the code under test
// should see. A kind is added only together with a seam that applies it.
type FaultAction struct {
	Kind  FaultKind
	Err   error         // Fail, and Tear when set: the error returned
	Delay time.Duration // Delay: how long At waits on the Fault's clock
	N     int64         // Tear: the bytes kept
}
