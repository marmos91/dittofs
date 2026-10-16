package journalv2test

import (
	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/sim"
)

// Clock adapts the shared clock c to the journal's clock. The journal's
// monotonic reading is the time c reports elapsed since the reading taken
// when the adapter is built, so advancing c advances the journal.
func Clock(c sim.Clock) journalv2.Clock {
	return nil
}
