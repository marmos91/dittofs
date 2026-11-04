package core

import (
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journal/internal/storage"
)

// Config is the journal's configuration as its internal packages read it,
// filled from the options the journal is opened with.
type Config struct {
	Settings
	Clock   Clock
	Budget  *DescriptorBudget
	Tracer  Tracer
	Logger  *slog.Logger
	Storage storage.FS
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

	// segmentCap overrides the segment size when positive; zero or less
	// leaves the format's. It is not a setting: only SetSegmentCap writes it,
	// so no value of it is refused.
	segmentCap int64
}

// SetSegmentCap makes s give every segment a cap of n bytes instead of the
// format's.
//
// decision: the cap is not a setting, and no name journal exports may set
// it, yet a check over hundreds of segments cannot run at the format's size
// in a temporary directory. So the field is unexported, and this setter lives
// in internal/core, which only code inside the journal's tree can import: its
// tests' fixture sets it and passes the settings through WithSettings, and
// the public surface does not change.
func SetSegmentCap(s *Settings, n int64) {
	s.segmentCap = n
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
// A journal takes a slot from the budget for every descriptor it opens and
// gives one back when it closes one. When no slot is open, the journal asks
// the budget to Reclaim one: the budget asks every registered journal, in
// round-robin order, to close one idle descriptor, and the first that does
// yields its slot.
//
// decision: the budget is a value built once and passed to each journal,
// because the limit is per process while each journal is built separately.
// A journal opened without one gets a private budget at the default limit.
type DescriptorBudget struct {
	// limit is the bound, resolved at construction. Zero was resolved to the
	// default; a negative value is the invalid setting passed in, kept as is
	// so Init and Open can refuse it with ErrInvalidSetting.
	limit int

	// decision: mu is a leaf, none of RFC 1 §8.2's six domains, and is never
	// held while a reclaimer runs or while any I/O is made. A reclaimer may
	// call TryTake, Give and Open on the budget, so holding mu across the call
	// would deadlock; everything mu guards is counters and the reclaimer
	// registry, none of which reach storage.
	mu         sync.Mutex
	open       int
	reclaimers map[uint64]func() bool
	nextID     uint64
	order      []uint64 // registration order, which the round-robin walks

	// last is the reclaimer the last successful Reclaim stopped asking after.
	// Ids start at 1, so last == 0 means none has succeeded and the next
	// Reclaim starts at the head. The next Reclaim resolves last against the
	// registry as it stands then: an index copied from an older order would
	// shift with every unregister.
	last uint64
}

// NewDescriptorBudget returns a budget of limit open descriptors. A limit of
// zero means a quarter of the process's open-file limit; a negative one is
// refused by the first journal opened with it, with ErrInvalidSetting.
//
// decision: the zero value of DescriptorBudget is not usable — its limit is
// zero, so TryTake never succeeds — and callers must build the budget here,
// which also makes the reclaimer registry.
func NewDescriptorBudget(limit int) *DescriptorBudget {
	if limit == 0 {
		limit = defaultDescriptorLimit()
	}
	return &DescriptorBudget{
		limit:      limit,
		reclaimers: map[uint64]func() bool{},
		nextID:     1,
	}
}

// Limit returns the resolved bound: the number of descriptors the journals of
// the process hold open together at most under this budget. It is negative
// when the budget was built from a negative limit, which Init and Open refuse.
func (b *DescriptorBudget) Limit() int { return b.limit }

// Open returns how many descriptors the process's journals hold open under
// this budget now.
func (b *DescriptorBudget) Open() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open
}

// TryTake takes one open-descriptor slot, reporting whether it did. It fails
// when the budget is at its limit, and always when the limit is negative.
func (b *DescriptorBudget) TryTake() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 || b.open >= b.limit {
		return false
	}
	b.open++
	return true
}

// Give returns one open-descriptor slot. It panics when no slot was taken:
// a count driven below zero makes TryTake admit descriptors again, and the
// bound would silently stop holding, so the accounting bug fails loudly
// instead.
func (b *DescriptorBudget) Give() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.open <= 0 {
		panic("journal: a descriptor budget slot was given that was never taken")
	}
	b.open--
}

// Register adds reclaim to the budget's reclaimer list: Reclaim calls it to
// close one idle descriptor, and it reports whether it did. It may call
// TryTake, Give and Open on the budget. The returned unregister func removes
// it; a journal whose cache has shut down unregisters, so the budget stops
// asking it.
func (b *DescriptorBudget) Register(reclaim func() bool) (unregister func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	b.reclaimers[id] = reclaim
	b.order = append(b.order, id)

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.reclaimers, id)
		b.order = slices.DeleteFunc(b.order, func(o uint64) bool { return o == id })
		// The cursor names the reclaimer that last succeeded, not a position,
		// so it needs no adjustment here: the next Reclaim resolves it against
		// this order as it stands now. Unregistering that very reclaimer loses
		// the resume point — Reclaim's lookup finds no such id — and the next
		// walk starts at the head, which is where registration order would
		// have it.
	}
}

// Reclaim asks the registered reclaimers for one open-descriptor slot: each
// is asked, in round-robin order starting after the last one that succeeded,
// until one closes an idle descriptor — which gives a slot back — or every
// one has been asked. It reports whether one did.
//
// The reclaimers are called without the budget's mutex held, so a reclaimer
// may call TryTake, Give and Open on the budget. The resume point is the
// reclaimer that last succeeded, resolved against the registry as it stands
// when the walk starts, because the registry it was recorded against may
// have shrunk or shifted since.
func (b *DescriptorBudget) Reclaim() bool {
	b.mu.Lock()
	if len(b.order) == 0 {
		b.mu.Unlock()
		return false
	}
	// start is the position the walk begins at: after the reclaimer that last
	// succeeded, wherever it now stands, or the head when there was none or
	// it has since unregistered — last == 0 means none.
	start := 0
	if b.last != 0 {
		for i, id := range b.order {
			if id == b.last {
				start = (i + 1) % len(b.order)
				break
			}
		}
	}
	order := append([]uint64(nil), b.order...)
	b.mu.Unlock()

	// asked counts from that position, so the walk wraps at the registration
	// order rather than restarting at its head; this round's starting point
	// would otherwise never advance.
	asked := 0
	for asked < len(order) {
		id := order[(start+asked)%len(order)]
		asked++

		b.mu.Lock()
		reclaim, ok := b.reclaimers[id]
		b.mu.Unlock()
		if !ok {
			continue // unregistered since the walk began
		}

		if reclaim() {
			b.mu.Lock()
			b.last = id
			b.mu.Unlock()
			return true
		}
	}
	return false
}

// fallbackDescriptorLimit is the default bound where the platform reports no
// open-file limit.
//
// ponytail: the platform has no open-file limit to read, so a fixed default
// stands in; 1024 leaves room for the process's own descriptors beside every
// journal's. Replace it with a measured value once the reopen rate under the
// mixed read soak on such a platform shows it is too low or wasteful.
const fallbackDescriptorLimit = 1024

// defaultDescriptorLimit returns the default bound: a quarter of the
// process's open-file limit where the platform reports one — the floor at 1
// covers a limit of 2 or 3, whose quarter rounds away — and the fallback
// where it does not. The arithmetic runs on the uint64 the limit was read
// as, and the quarter is held to what an int represents before the
// conversion, so it cannot wrap on a platform whose int is narrower than a
// limit it reports. That bound is a representability guard, not a policy cap:
// no limit a host reports in practice reaches it.
func defaultDescriptorLimit() int {
	cur, known := platformOpenFileLimit()
	if !known {
		return fallbackDescriptorLimit
	}

	return int(max(min(cur/4, uint64(math.MaxInt)), 1))
}
