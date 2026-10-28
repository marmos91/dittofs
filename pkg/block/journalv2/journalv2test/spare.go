package journalv2test

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// SpareAttempts bounds the calls RetrySpare makes before it gives up on a
// journal that never makes a spare: a count of yields, never a time.
const SpareAttempts = 1 << 22

// RetrySpare calls op and, while it is refused for want of a spare, yields
// the processor and calls it again, up to SpareAttempts times, and returns
// op's last error, or an error saying the journal never made a spare once
// the attempts run out. It waits on no clock.
//
// decision: spares are made off the write path, and a roll that finds none
// ready is refused at once, so with the fixture's small segments a correct
// journal can refuse a write now and accept it a moment later. Retrying is
// what its caller does. The wait yields rather than holding a seam call,
// because no seam call marks the moment a spare is in the pool: the journal
// records it after the directory sync that follows the spare's rename
// returns into its own goroutine, and a refusal in that gap would wait on a
// sync that never comes. Upgrade to a hold on a point the journal reports
// through the seam once it records a spare, if the seam ever carries such
// points.
func RetrySpare(op func() error) error {
	var err error
	for range SpareAttempts {
		err = op()
		var ns *journalv2.NoSpaceError
		if !errors.As(err, &ns) || ns.Limit != journalv2.LimitSpare {
			return err
		}
		runtime.Gosched()
	}

	return fmt.Errorf("journalv2test: %d calls refused for want of a spare; the journal never made one: %w", SpareAttempts, err)
}
