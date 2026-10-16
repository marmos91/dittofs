package journalv2

import (
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
)

// Clock is the journal's only source of time. The sync timer, every wait and
// every time the journal reports are read from it, so a test that advances
// its clock controls all of them.
//
// decision: the journal declares its own clock rather than taking the shared
// one, because it may import nothing but the standard library. The shared
// clock reads monotonic time as a type of its own package, which no interface
// built from standard-library types can match, so an adapter converts one to
// the other. Revisit if the shared clock is ever expressed in
// standard-library types alone.
type Clock interface {
	// Now returns the wall time. The journal uses it only for the times it
	// reports, such as the oldest dirty byte's write time and the last sync
	// failure; a write's modification time is always given by its caller.
	Now() time.Time

	// Elapsed returns the time elapsed on the monotonic clock since an origin
	// the clock chose. Only differences between two readings mean anything.
	Elapsed() time.Duration

	// After returns a channel that receives once d has elapsed on the
	// monotonic clock.
	After(d time.Duration) <-chan struct{}
}

// Option configures Init, Open or Restamp.
//
// decision: options are appended to the calls' signatures as a variadic
// parameter, so a call written with only the parameters the journal's
// contract names still compiles and gets every default. The parameter type is
// internal, so only code inside the journal's tree can write an option that
// reaches a field the public API does not offer.
type Option func(*core.Config)

// WithClock makes the journal read time from c. Without it the journal uses
// the system clock.
func WithClock(c Clock) Option {
	return func(*core.Config) {}
}

// WithStorage makes the journal reach its directory and files through fs
// instead of the operating system's. Its parameter type is internal, so only
// code inside the journal's tree, such as the crash-simulating seam its tests
// use, can build a value to pass. Without it the journal uses the operating
// system's files.
func WithStorage(fs storage.FS) Option {
	return func(*core.Config) {}
}

// WithSettings sets the journal's settings. Without it every setting takes its
// default.
func WithSettings(s Settings) Option {
	return func(*core.Config) {}
}

// WithDescriptorBudget makes the journal hold its segment descriptors under b,
// a budget shared with the other journals of the process. Without it the
// journal gets a budget of its own, at the default limit.
func WithDescriptorBudget(b *DescriptorBudget) Option {
	return func(*core.Config) {}
}

// Settings are the journal's settings. A zero field takes its default; a
// negative one is refused by Init, Open and Restamp with ErrInvalidSetting.
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
	// be sealed once it has taken no append for IdleSealAge. Default: 16 MiB.
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

// The journal's fixed values: constants of the implementation, not settings.
//
// decision: each is the value the journal's design proposes, and none is
// measured yet; changing one changes the implementation, not a deployment.
const (
	// SegmentCapDefault is the size of a segment, in bytes.
	SegmentCapDefault = 256 << 20

	// SyncBound is the longest a record written under the Bounded policy waits
	// to be synced.
	SyncBound = time.Second

	// OfferPins is the most segments one offer pins.
	OfferPins = 64

	// JournalPins is the most segments all running offers pin together.
	JournalPins = 256

	// FloorBuckets is the number of floor buckets a file with no index entry
	// is checked against when it is filled.
	FloorBuckets = 4096

	// IdleSealAge is how long an active segment takes no append before it is
	// sealed, once past IdleSealBytes.
	IdleSealAge = 30 * time.Second
)

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
