package durability

import (
	"time"
)

// SyncFailure is the most recent sync failure, and when it happened (R64).
type SyncFailure struct {
	Err error
	At  time.Time // the clock's Now at the failure
}

// failStamp is a stream's last sync failure, with the monotonic reading that
// orders it against the other streams' failures.
type failStamp struct {
	SyncFailure
	mono time.Duration
}
