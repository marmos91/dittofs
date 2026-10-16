package sim

import "time"

// Fault is consulted by a seam before each named operation, on a seeded
// schedule read from the Clock.
type Fault interface {
	At(op string, key []byte) FaultAction // pass, fail with an error, delay by d, drop, tear after n bytes, reorder
}

// FaultAction is what a seam does with one operation: Kind says which action,
// and the field that kind needs carries its payload. A zero FaultAction passes
// the operation through unchanged.
type FaultAction struct {
	Kind      FaultKind
	Err       error         // the error a FaultFail returns
	Delay     time.Duration // how long a FaultDelay holds the operation, on the Clock
	TearAfter int           // how many bytes a FaultTear lets through before it stops
}

// FaultKind is the action a FaultAction takes.
type FaultKind uint8

const (
	// FaultPass performs the operation unchanged.
	FaultPass FaultKind = iota
	// FaultFail performs nothing and returns Err.
	FaultFail
	// FaultDelay performs the operation after Delay has elapsed on the Clock.
	FaultDelay
	// FaultDrop reports success and performs nothing.
	FaultDrop
	// FaultTear performs the first TearAfter bytes of the operation and fails
	// the rest.
	FaultTear
	// FaultReorder lets a later operation of the same seam take effect before
	// this one.
	FaultReorder
)
