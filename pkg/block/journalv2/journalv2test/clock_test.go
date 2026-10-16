package journalv2test

import (
	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/clock"
	"github.com/marmos91/dittofs/pkg/sim"
)

// The node's clock and a test clock are handed to the journal as they are:
// if either stops satisfying the journal's clock, this file stops compiling.
var (
	_ journalv2.Clock = clock.System{}
	_ journalv2.Clock = sim.TestClock(nil)
)
