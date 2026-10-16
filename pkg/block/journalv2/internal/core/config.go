package core

import "time"

// Config is the journal's configuration as its internal packages read it,
// filled from the options the journal is opened with.
type Config struct {
	Settings
	Clock  Clock
	Budget *DescriptorBudget
}

// Settings are the journal's settings. A zero field takes its default; a
// negative one is refused by Init and Open with ErrInvalidSetting.
type Settings struct {
	// MaxBytes is the most local storage the journal allocates, in bytes. A
	// write that would take it past this is refused with a NoSpaceError
	// naming LimitJournal. Default: 80% of the filesystem's size at open.
	MaxBytes int64

	// Streams is the number of append streams. Default: 16.
	Streams int

	// ExtentLimit is the most placement index entries the journal holds. A
	// write that would pass it is refused with a NoSpaceError naming
	// LimitIndex. Default: 2^26.
	ExtentLimit int64

	// HeadroomRecords is the number of header-only records reserved for
	// removals, losses, holds and stamps, so they can still be written once
	// content writes are refused. Exhausting it refuses them with a
	// NoSpaceError naming LimitHeadroom. Default: 65,536.
	HeadroomRecords int

	// IdleSealBytes is how full an active segment must be, in bytes, for it to
	// be sealed once it has taken no append for the idle-seal age. Default:
	// 16 MiB.
	IdleSealBytes int64

	// SpareTarget is the number of spare segments the journal keeps ready. A
	// roll that finds no spare is refused with a NoSpaceError naming
	// LimitSpare.
	//
	// decision: the default is one spare per append stream, so every stream
	// can roll once without waiting for a spare to be made. No measured value
	// exists yet; raise it if spare refusals are counted under a sustained
	// write load.
	SpareTarget int
}

// Clock is the journal's only source of time. The sync timer, every wait and
// every time the journal reports are read from it, so a test that advances
// its clock controls all of them.
//
// It is the part of the node's one clock the journal reads, declared here
// from standard-library types because the journal imports nothing else. The
// node's clock and a test clock both satisfy it as they are.
type Clock interface {
	// Now returns the wall time. The journal uses it only for the times it
	// reports, such as the oldest dirty byte's write time and the last sync
	// failure; a write's modification time is always given by its caller.
	Now() time.Time

	// Elapsed returns the time elapsed on the monotonic clock since an origin
	// the clock chose. Only differences between two readings mean anything.
	Elapsed() time.Duration

	// After returns a channel that is closed once d has elapsed on the
	// monotonic clock.
	After(d time.Duration) <-chan struct{}
}

// DescriptorBudget bounds the segment descriptors the journals of one process
// hold open together. The bound protects the process's open-file limit, so one
// budget is shared by every journal of the process rather than set per
// journal. A read of a segment whose descriptor was closed reopens it.
//
// decision: the budget is a value built once and passed to each journal,
// because the limit is per process while each journal is built separately.
// A journal opened without one gets a private budget at the default limit.
type DescriptorBudget struct{}

// NewDescriptorBudget returns a budget of limit open descriptors. A limit of
// zero means a quarter of the process's open-file limit; a negative one is
// refused by the first journal opened with it, with ErrInvalidSetting.
func NewDescriptorBudget(limit int) *DescriptorBudget {
	return &DescriptorBudget{}
}
