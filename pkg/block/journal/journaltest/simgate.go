package journaltest

import (
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/sim"
)

// SimGate is a fault that parks a chosen class of calls until the test
// answers each of them with one turn. While armed, every call At matches
// parks and reports one arrival; Release frees exactly one parked call — the
// turn it banks is consumed by the call that wakes, not left for the next
// to ride — so one release answers one park, and a freed call that matches
// again parks afresh. Disarm frees every parked call and passes every later
// one.
//
// Match decides which calls park while the gate is armed; a call it answers
// false passes without reporting. The gate is deterministic by construction:
// a parked call moves only when the test releases or disarms it, so no
// schedule chooses what the test sees.
//
// decision: an armed gate that parks more than arrivalsMax calls drops the
// announcement of the ones past it rather than blocking inside At, which
// would deadlock every later call of the gate. A dropped announcement leaves
// its call parked with the test never told, and Dropped reports it: the test
// fails on it explicitly instead of hanging, and a design that parks that
// many calls at once is not one the protocol driving this gate can answer
// anyway.
type SimGate struct {
	Match func(op string, key []byte) bool

	mu        sync.Mutex
	cond      *sync.Cond
	Arrivals  chan struct{}
	tokens    int
	parked    int
	armed     bool
	dropped   int
	armedOnce sync.Once
}

// arrivalsMax is how many announcements a gate holds while armed: enough
// for every window the journal tests arm over one gate, and the point past
// which a caller is told, through Dropped, that its arrivals stopped being
// observable.
const arrivalsMax = 1024

func (g *SimGate) init() {
	g.armedOnce.Do(func() {
		g.Arrivals = make(chan struct{}, arrivalsMax)
		g.cond = sync.NewCond(&g.mu)
	})
}

// Arm makes every later matched call park until released or disarmed, and
// starts with no banked turn: a release that arrived while the gate was
// disarmed answers no call, so no turn may survive into the next window for
// its first parking call to ride.
func (g *SimGate) Arm() {
	g.init()
	g.mu.Lock()
	g.armed = true
	g.tokens = 0
	g.mu.Unlock()
}

// Disarm frees every parked call and passes every later one, dropping turns
// not yet taken: a release that arrived after its asking call left costs
// nothing. Announcements left in Arrivals are the test's to drain.
func (g *SimGate) Disarm() {
	g.init()
	g.mu.Lock()
	g.armed = false
	g.tokens = 0
	g.cond.Broadcast()
	g.mu.Unlock()
}

// Release frees exactly one parked call. The released call takes the turn on
// waking, so a release answers one park and no other: the call released
// cannot be the next one that parks, which must ask for a turn of its own.
// A release with no parking call banks its turn until one matches, Disarm
// drops it, or the next Arm opens with no turn carried over.
func (g *SimGate) Release() {
	g.init()
	g.mu.Lock()
	g.tokens++
	g.cond.Signal()
	g.mu.Unlock()
}

// Armed reports whether the gate parks matched calls right now: a driver
// asserting a call never met the gate reads it before and after the call.
func (g *SimGate) Armed() bool {
	g.init()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.armed
}

// Parked reports how many calls are parked right now.
func (g *SimGate) Parked() int {
	g.init()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.parked
}

// Dropped reports how many announcements a full Arrivals buffer lost while
// the gate was armed. The protocol driving the gate fails on a nonzero
// count rather than answering calls it was never told about.
func (g *SimGate) Dropped() int {
	g.init()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dropped
}

// announce queues one arrival without holding the gate's lock: a caller
// would otherwise block every other call of the gate on the test draining
// a full buffer, which the disarm below cannot do inside a park.
func (g *SimGate) announce() {
	// park must be visible to Parked before the announcement is observable,
	// so the caller counts itself, then announces without the lock.
	select {
	case g.Arrivals <- struct{}{}:
	default:
		g.mu.Lock()
		g.dropped++
		g.mu.Unlock()
	}
}

// At is sim.Fault's hook: matched calls park while the gate is armed, each
// reporting one arrival per park, and each wakes only on a turn of its own.
func (g *SimGate) At(op string, key []byte) (delay time.Duration, tearAfter int, err error) {
	if g.Match == nil || !g.Match(op, key) {
		return 0, 0, nil
	}
	g.init()
	g.mu.Lock()
	if !g.armed {
		g.mu.Unlock()
		return 0, 0, nil
	}
	if g.tokens > 0 {
		// A banked turn: take it, and answer for it, so this call is the
		// release's one caller and not a free pass beside it.
		g.tokens--
		g.mu.Unlock()
		return 0, 0, nil
	}
	g.parked++
	g.mu.Unlock()

	g.announce()

	g.mu.Lock()
	for g.armed && g.tokens == 0 {
		g.cond.Wait()
	}
	if g.armed {
		g.tokens--
	}
	g.parked--
	g.mu.Unlock()
	return 0, 0, nil
}

var _ sim.Fault = (*SimGate)(nil)
