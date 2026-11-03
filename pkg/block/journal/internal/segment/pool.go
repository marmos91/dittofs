package segment

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"syscall"

	"github.com/marmos91/dittofs/pkg/block/journal/internal/core"
)

// The pool's own refusals, beside the NoSpaceError the shared limits return.
var (
	// ErrBadClass is the refusal of an adoption naming a class the pool does
	// not hold: the spare classes are content, headroom and reserve, and a
	// spare found at open belongs to exactly one of them, which the caller
	// decides in the scan that found it.
	ErrBadClass = errors.New("segment: bad spare class")
	// ErrStarted is the refusal of an adoption into a pool that has started:
	// open syncs the directory once before any re-adopted spare is taken, and
	// a spare adopted after that point could be taken while its entry is one
	// a power cut could still lose.
	ErrStarted = errors.New("segment: the pool has started")
)

// Class is which third of the pool a spare stands in. The classes never lend
// to one another: a roll that finds only the other classes' spares is refused
// `spare` rather than served out of class.
type Class uint8

const (
	// Content is the class a stream's roll takes from.
	Content Class = iota + 1
	// Headroom is the class the header-only records' rolls take from.
	Headroom
	// Reserve is the class the repack copies' rolls take from.
	Reserve
)

// classNames feeds String below, indexed by Class.
var classNames = [...]string{"", "content", "headroom", "reserve"}

// String returns the class's name: content, headroom or reserve.
func (c Class) String() string {
	if int(c) < len(classNames) {
		return classNames[c]
	}
	return fmt.Sprintf("Class(%d)", uint8(c))
}

// Targets is how many spares each class keeps ready, in segments. The caller
// sizes them: content from the spare target it configures, headroom and
// reserve from the capacity rules that size the headroom and the repack
// reserve. A negative target is a caller bug and panics; zero means the class
// keeps none.
//
// ponytail: the targets are static. A burst longer than one segment's fill
// time refuses `spare`. Upgrade to a target that follows the roll rate, once
// spare refusals show it.
type Targets struct {
	Content  int
	Headroom int
	Reserve  int
}

// PoolCounters is the pool's read: one lock and a copy from fields the pool
// maintains at each event that moves one, never a walk of its slices.
type PoolCounters struct {
	// Spares is the ready content spares, the ones held for open holds
	// included.
	Spares int64
	// HeadroomBytes is the ready headroom spares at the segment cap, as
	// allocated now.
	HeadroomBytes int64
	// RepackReserveBytes is the ready reserve spares at the segment cap, as
	// allocated now.
	RepackReserveBytes int64
	// Held is the content spares owed to the pool's open holds.
	Held int64
}

// Pool is the spare pool: the spares of the three classes the Maker made,
// ready to be taken by a roll that costs no I/O, and the worker that keeps
// each class at its target by making spares off the write path.
//
// decision: ready, held, started, stopped, deviceFull and lastErr sit under
// the capacity domain of RFC 1 §8.2 — store-wide counters, no I/O under p.mu,
// no lock acquired while it is held, and never held across a Maker or a Set
// call: those take locks of this same domain, and no two locks of one domain
// are held at once. Withdrawn the day a recorder of the domain's order finds
// p.mu held across one of those calls.
type Pool struct {
	m   *Maker
	t   Targets
	log *slog.Logger

	mu         sync.Mutex
	ready      [Reserve + 1][]core.SegmentID // the ready spares per class, FIFO; guarded by mu
	held       int                           // spares owed to open holds; guarded by mu
	started    bool                          // guarded by mu
	stopped    bool                          // guarded by mu
	deviceFull bool                          // guarded by mu
	lastErr    error                         // guarded by mu

	kick chan struct{}
	done chan struct{}
	stop sync.Once
	wg   sync.WaitGroup
}

// NewPool returns a pool that makes its spares with m and keeps each class of
// t ready. A nil Maker is a caller bug and panics; a negative target names
// storage no class can hold and panics the same way; a nil logger keeps the
// pool silent.
func NewPool(m *Maker, t Targets, log *slog.Logger) *Pool {
	if m == nil {
		panic("segment: a pool needs its Maker")
	}
	if t.Content < 0 || t.Headroom < 0 || t.Reserve < 0 {
		panic(fmt.Sprintf("segment: the spare targets %+v are not sizes classes can hold", t))
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Pool{
		m:    m,
		t:    t,
		log:  log,
		kick: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
}

// Adopt accounts a spare found at open into class c: the pool calls the
// ledger, which accounts the file the directory already holds at its cap,
// and then holds the spare ready in its class, counted toward the headroom,
// the repack reserve or a stream's next segment. It is refused once Start has
// run, because open syncs the directory once before any re-adopted spare is
// taken, and the caller of Start runs that sync before it: a spare adopted
// after could be taken while its entry is one a power cut could still lose.
//
// The id must already be below the Maker's seed; the pool does not check that,
// for the seed that hands an id above the directory's names out has already
// broken what keeps a live segment and a fresh spare from meeting under one
// name, and no check here could hold it back together.
func (p *Pool) Adopt(id core.SegmentID, c Class) error {
	if c != Content && c != Headroom && c != Reserve {
		return ErrBadClass
	}

	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if started {
		return ErrStarted
	}
	if err := p.m.set.AdoptSpare(id); err != nil {
		return fmt.Errorf("segment: the pool did not adopt spare %d: %w", uint64(id), err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// A Stop that raced between the check and here leaves the spare accounted
	// and on disk in its class, which nothing takes once stopped: the next
	// open re-adopts it. Refusing here would strand a cap the ledger already
	// owns with no class holding it.
	p.ready[c] = append(p.ready[c], id)
	return nil
}

// Start marks the pool started and launches the refill worker, kicking it
// once so an empty pool fills. It is idempotent, and a no-op after Stop.
//
// Start is called only once open's directory sync has returned: until it has,
// re-adopted spares may not be taken, and the pool refuses every Take and
// Hold until Start runs.
func (p *Pool) Start() {
	p.mu.Lock()
	if p.started || p.stopped {
		p.mu.Unlock()
		return
	}
	// the worker is registered under mu, so a Stop racing Start cannot set
	// stopped, close done and return from its Wait before the worker it must
	// join exists. The goroutine starts under mu too: no lock of the domain
	// is held across anything but this bookkeeping.
	p.started = true
	p.wg.Add(1)
	go p.worker()
	p.mu.Unlock()

	p.Kick()
}

// Take returns the oldest ready spare of class c into the set, as the segment
// a stream appends to. Making a segment happens off the write path, so a
// roll costs only the take: a Take that finds its class empty makes no seam
// call, waits on nothing and returns at once the NoSpaceError the spare pool
// owes — Limit LimitSpare, one segment missing, the segments unit. Spares
// held for an open hold are not available to an unreserved take, so an open
// reservation is never left short of its spares.
//
// A class never serves another's roll: a content roll and a Reserve never
// take one, and one that finds only those is refused `spare`.
func (p *Pool) Take(c Class) (core.SegmentID, error) {
	defer p.Kick()

	if c != Content && c != Headroom && c != Reserve {
		return 0, ErrBadClass
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return 0, core.ErrClosed
	}
	if !p.started {
		p.mu.Unlock()
		return 0, &core.NoSpaceError{Limit: core.LimitSpare, Missing: 1, Unit: core.UnitSegments}
	}

	// decision: a take of a class falls back to no other, because the
	// headroom and the reserve are what keep a full journal able to free
	// space, and a class that borrowed one would leave the reserve that
	// draws on it short.
	var id core.SegmentID
	switch c {
	case Content:
		// The unreserved take leaves the holds their spares: taking one an
		// open hold owes would let a reserved write meet the no-spare
		// refusal the hold exists to keep it from.
		if int64(len(p.ready[Content])) > int64(p.held) {
			id = p.popLocked(Content)
		}
	case Headroom:
		if len(p.ready[Headroom]) > 0 {
			id = p.popLocked(Headroom)
		}
	case Reserve:
		if len(p.ready[Reserve]) > 0 {
			id = p.popLocked(Reserve)
		}
	}
	p.mu.Unlock()

	if id == 0 {
		return 0, &core.NoSpaceError{Limit: core.LimitSpare, Missing: 1, Unit: core.UnitSegments}
	}
	if err := p.m.set.Take(id); err != nil {
		// The take popped the spare off the ready list above, its class saw
		// it go, and the ledger that cannot find it answers for the rest:
		// the pool and the ledger both hold no spare the other lost.
		return 0, fmt.Errorf("segment: the pool took spare %d: %w", uint64(id), err)
	}
	return id, nil
}

// Hold reserves n content spares for a reservation — "the spare segments its
// own bytes roll into, one per cap they can cross plus one, taken from the
// pool for the reservation" — holding them ahead of need: the spares it holds
// are not available to unreserved takes, so a reserved write never meets the
// no-spare refusal while the pool can be refilled. n must be positive.
func (p *Pool) Hold(n int) (*Hold, error) {
	if n <= 0 {
		panic(fmt.Sprintf("segment: a hold of %d spares reserves nothing a roll could take", n))
	}
	defer p.Kick()

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil, core.ErrClosed
	}
	if !p.started {
		p.mu.Unlock()
		return nil, &core.NoSpaceError{Limit: core.LimitSpare, Missing: int64(n), Unit: core.UnitSegments}
	}
	ready := int64(len(p.ready[Content]))
	if ready-int64(p.held) < int64(n) {
		missing := int64(n) - (ready - int64(p.held))
		p.mu.Unlock()
		if missing > int64(n) {
			// held outnumbers ready, so the whole hold is short: floored at n.
			missing = int64(n)
		}
		return nil, &core.NoSpaceError{Limit: core.LimitSpare, Missing: missing, Unit: core.UnitSegments}
	}
	p.held += n
	p.mu.Unlock()
	return &Hold{p: p, left: n}, nil
}

// Hold is a reservation's share of the pool's content spares. Its Take is a
// reserved roll: it takes a held spare ahead of every unreserved write.
// Replace returns one to the reservation once its write stopped short of an
// interleaved roll, and Close returns what is left. A closed hold is spent:
// its Take returns core.ErrClosed, its Replace a no-op. The hold's fields
// are guarded by its pool's mu, the capacity domain its type names.
type Hold struct {
	p      *Pool
	left   int
	closed bool
}

// Take takes one of the hold's spares: a reserved roll, which goes ahead of
// every unreserved write. A hold whose left has run out behaves as an
// unreserved content take, and a hold whose pool has gone short after a
// Replace — no content spare ready — is refused `spare` like one.
func (h *Hold) Take() (core.SegmentID, error) {
	defer h.p.Kick()

	h.p.mu.Lock()
	if h.closed {
		h.p.mu.Unlock()
		return 0, core.ErrClosed
	}
	if h.left <= 0 {
		// The hold spent its own spares and turns into an unreserved take.
		if h.p.stopped {
			h.p.mu.Unlock()
			return 0, core.ErrClosed
		}
		if int64(len(h.p.ready[Content])) > int64(h.p.held) {
			id := h.p.popLocked(Content)
			h.p.mu.Unlock()
			if err := h.p.m.set.Take(id); err != nil {
				return 0, fmt.Errorf("segment: the hold took spare %d: %w", uint64(id), err)
			}
			return id, nil
		}
		h.p.mu.Unlock()
		return 0, &core.NoSpaceError{Limit: core.LimitSpare, Missing: 1, Unit: core.UnitSegments}
	}

	if int64(len(h.p.ready[Content])) == 0 {
		h.p.mu.Unlock()
		return 0, &core.NoSpaceError{Limit: core.LimitSpare, Missing: 1, Unit: core.UnitSegments}
	}
	h.left--
	h.p.held--
	id := h.p.popLocked(Content)
	h.p.mu.Unlock()

	if err := h.p.m.set.Take(id); err != nil {
		return 0, fmt.Errorf("segment: the hold took spare %d: %w", uint64(id), err)
	}
	return id, nil
}

// Replace returns one spare to the hold: its reserved write was rolled short
// by the stream's other writes instead of its own, so the reservation takes
// a replacement ahead of every unreserved write. It raises the hold without
// refusing, even against a pool with no spare ready: the unreserved takes are
// then refused until the refill catches up. When a reserved roll was caused
// by interleaving is not the pool's to know — the roll, or the caller above
// it, decides and calls Replace.
func (h *Hold) Replace() {
	h.p.mu.Lock()
	if h.closed {
		h.p.mu.Unlock()
		return
	}
	h.left++
	h.p.held++
	h.p.mu.Unlock()

	h.p.Kick()
}

// Close returns the hold's left spares to unreserved takes. It is idempotent,
// and safe after the pool's Stop, which leaves them accounted and on disk
// for the next open to re-adopt.
func (h *Hold) Close() {
	h.p.mu.Lock()
	if h.closed {
		h.p.mu.Unlock()
		return
	}
	h.closed = true
	h.p.held -= h.left
	h.left = 0
	h.p.mu.Unlock()
}

// Kick wakes the refill worker, coalescing: one kick that arrives while a
// pass runs holds the next. The worker checks done between its makes, so a
// kick after Stop is a drop.
func (p *Pool) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// DeviceFull reports whether the worker's last make failed with ENOSPC and
// no make has succeeded since — "making a spare failed with ENOSPC" being
// the state a roll that finds no full-size retired segment to hand answers
// ENOSPC with. The other half of that state — whether a retirement has since
// handed one — is the retirement executor's, not the pool's.
func (p *Pool) DeviceFull() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deviceFull
}

// Counters returns the pool's maintained counters: one lock and a copy, the
// slice lengths read as maintained state, no walk of any class.
func (p *Pool) Counters() PoolCounters {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolCounters{
		Spares:             int64(len(p.ready[Content])),
		HeadroomBytes:      int64(len(p.ready[Headroom])) * p.m.set.cap,
		RepackReserveBytes: int64(len(p.ready[Reserve])) * p.m.set.cap,
		Held:               int64(p.held),
	}
}

// Stop closes the pool and joins the worker. A make in flight completes
// first, and its spare is held — accounted, on disk — for the next open to
// re-adopt; Stop removes nothing, and the untaken spares stay in the ledger
// and on disk. After Stop, Take and Hold return core.ErrClosed, and Start
// and Kick are no-ops.
func (p *Pool) Stop() {
	p.stop.Do(func() {
		p.mu.Lock()
		p.stopped = true
		p.mu.Unlock()
		close(p.done)
	})
	p.wg.Wait()
}

// worker is the refill: on each kick it makes spares, one at a time, until
// every class is at its target or a make fails — then it waits for the next
// kick, which every take, refused or not, sends. Making a segment never
// reaches the write path; making spares here is what keeps that true.
//
// ponytail: retries are demand-driven. A failed make is retried on the next
// kick — every take, refused or not, every replacement, and the kick a
// retirement makes — with no timer of its own. Ceiling: an idle journal
// whose last make failed makes no spare until something takes or retires,
// and a failing device sees one make attempt per refused roll, coalesced by
// the one-slot kick. Upgrade to a timed retry on the pool's clock once
// spare refusals show them persisting on an idle device that has recovered.
func (p *Pool) worker() {
	defer p.wg.Done()

	for {
		select {
		case <-p.done:
			return
		case <-p.kick:
			if !p.refill() {
				return
			}
		}
	}
}

// refill runs one pass. It returns false once the pool is stopped, so the
// worker exits; true otherwise, whether or not the pass made a spare.
func (p *Pool) refill() bool {
	for {
		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			return false
		}
		// decision: the headroom and the reserve fill before the content
		// class, because they are what keeps a full journal able to free
		// space — the records that return space roll into them — so a pass
		// that made a content spare while a reserve sat short would trade a
		// permanent wedge for one more served roll. Withdrawn once spare
		// refusals show content rolls refused while a reserve was filling.
		var c Class
		switch {
		case len(p.ready[Headroom]) < p.t.Headroom:
			c = Headroom
		case len(p.ready[Reserve]) < p.t.Reserve:
			c = Reserve
		case len(p.ready[Content]) < p.t.Content:
			c = Content
		default:
			p.mu.Unlock()
			return true
		}
		p.mu.Unlock()

		id, err := p.m.Make()
		p.mu.Lock()
		if err != nil && id != 0 {
			// Make owns the pairing: it never returns both an id and an
			// error, so this branch is unreachable; the guard keeps the fact
			// readable where the pairing is used.
			id = 0
		}
		if p.stopped {
			// A made spare stays accounted and on disk; the next open
			// re-adopts it, so it is appended to the class the pass chose even
			// now. A failed one returned its reservation in its own cleanup
			// and appended nothing — an id of 0 would name no segment a take
			// could reach.
			if err == nil {
				p.ready[c] = append(p.ready[c], id)
			}
			p.mu.Unlock()
			return false
		}
		if err != nil {
			p.lastErr = err
			p.deviceFull = errors.Is(err, syscall.ENOSPC)
			p.mu.Unlock()

			// The journal maximum's own refusal is the pool working as
			// configured; anything else is a fault to name.
			var ns *core.NoSpaceError
			if errors.As(err, &ns) && ns.Limit == core.LimitJournal {
				p.log.Debug("segment: the pool did not make a spare", "err", err)
			} else {
				p.log.Warn("segment: the pool did not make a spare", "err", err)
			}
			return true
		}
		p.ready[c] = append(p.ready[c], id)
		p.deviceFull = false
		p.lastErr = nil
		p.mu.Unlock()
	}
}

// popLocked returns the oldest spare of class c, removing it from the ready
// list; the caller holds mu and has checked the list is not empty.
func (p *Pool) popLocked(c Class) core.SegmentID {
	id := p.ready[c][0]
	p.ready[c] = p.ready[c][1:]
	if len(p.ready[c]) == 0 {
		// Release the slice's hold on the popped id, so a long-lived pool
		// does not pin one id's storage per take.
		p.ready[c] = nil
	}
	return id
}
