package journalv2test

import (
	"errors"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// spareWait bounds, in real time, how long a call refused for want of a spare
// segment is retried.
//
// decision: spares are made off the write path, and a roll that finds none
// ready is refused at once, so with the fixture's small segments a correct
// journal can refuse a write now and accept it a moment later. Retrying is
// what its caller does. The wait is real time because spare making runs on
// the journal's own goroutines; it is spent only when a spare is missing.
const spareWait = 2 * time.Second

// RetrySpare calls op until it returns anything but a NoSpaceError naming
// LimitSpare, or spareWait has passed, and returns op's last error.
func RetrySpare(op func() error) error {
	deadline := time.Now().Add(spareWait)
	for {
		err := op()
		var ns *journalv2.NoSpaceError
		if !errors.As(err, &ns) || ns.Limit != journalv2.LimitSpare || time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
}
