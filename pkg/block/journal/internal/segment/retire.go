package segment

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// Reason is what the caller says a retirement is for, and the executor holds
// it to the segment's state: wrong pairs are refused ErrWrongReason before
// anything is touched.
type Reason uint8

const (
	// NotHeld is a sealed segment the caller found not held: its leaving
	// records are durable, the caller's word (R44). Sealed only.
	NotHeld Reason = iota + 1
	// ReasonEnded is a segment a failed append or sync ended, or a failed
	// Make's leftover, whose window the caller resolved and whose synced
	// records repack carried forward. Ended only. It is spelled ReasonEnded
	// because the ledger's own Ended names the segment's state and the two
	// share this package.
	ReasonEnded
)

// ErrWrongReason is the refusal of a retirement whose reason does not match
// the segment's state the ledger answered: a NotHeld of a segment that is not
// Sealed, a ReasonEnded of a segment that is not Ended. The claim the
// executor took is released before the refusal returns, so the right reason
// finds the segment free.
var ErrWrongReason = errors.New("segment: retirement reason does not match the segment's state")

// Retirement is what one retirement did.
type Retirement struct {
	// ID is the segment retired.
	ID core.SegmentID
	// Recycled is true when the segment became a spare under Fresh rather
	// than leaving the journal by an unlink.
	Recycled bool
	// Fresh is the spare's id when Recycled, zero otherwise.
	Fresh core.SegmentID
	// Freed is the bytes the retirement added to freed_bytes: the segment's
	// accounted size after an unlink has happened, the cap after a recycle
	// has completed — a recycle returns the cap to the spare pool.
	Freed int64
	// Durable reports whether the directory sync after the unlink or the
	// rename returned nil. A retirement it is false for is owed its signal,
	// and the worker re-syncs it until one returns.
	Durable bool
}

// RetireCounters is the executor's counters, read without a walk and
// maintained at each event that moves one.
type RetireCounters struct {
	// FreedBytes is the storage actually returned by retiring segments: to
	// the filesystem by an unlink, or to the spare pool by a recycle.
	FreedBytes uint64
}

// RetirerConfig gathers the constructor's dependencies. Pool and ReadOnly may
// be nil — no pool means every retirement unlinks, no check means the
// directory is writable — the rest are a caller bug NewRetirer panics on.
type RetirerConfig struct {
	// Set is the ledger the retirement claims through and unlinks in.
	Set *Set
	// Cache is the descriptor cache whose Close drains the readers.
	Cache *Descriptors
	// Pool takes a recycled spare into the class its reservation chose.
	// Nil: never recycle, every retirement unlinks.
	Pool *Pool
	// Maker is the id allocator the recycle draws its fresh id from, over
	// the same FS.
	Maker *Maker
	// FS is the journal's directory.
	FS storage.FS
	// ReadOnly is the read-only check: called before anything is touched,
	// a non-nil error refuses the retirement wrapped, whole. Nil means
	// writable.
	ReadOnly func() error
	// Durable is the durable-retirement signal: called once per retirement
	// whose directory sync returned nil. Nil means no signal.
	Durable func(Retirement)
	// Log is the executor's logger; nil keeps it silent.
	Log *slog.Logger
}

// Retirer is the retirement executor: unlink or recycle, decided at the
// retirement itself, plus the queue a background worker drains. Retire runs
// one retirement now; Enqueue hands the worker one; Stop joins it.
//
// decision: the executor's own state — the queue with its reasons, the set of
// queued ids, the freed counter, the owed durable signals and the extra names
// a failed recycle left — sits under the capacity domain of RFC 1 §8.2, as
// D13 asks of every piece of mutable state this epic adds: no I/O under the
// mutex, no lock acquired while it is held, and never held across a Set,
// Pool, Descriptors or storage call. Withdrawn the day a recorder of the
// domain's order finds the mutex held across one of those.
type Retirer struct {
	cfg RetirerConfig
	log *slog.Logger

	mu     sync.Mutex
	freedB uint64 // freed_bytes; guarded by mu
	queue  []core.SegmentID
	reasons []Reason
	queued map[core.SegmentID]struct{}
	started bool
	// owed holds the retirements whose directory sync failed: their signal
	// is held back until the worker's own re-sync returns.
	owed []Retirement
	// extra holds the names a recycle's failed rename may have put the file
	// under beside the segment's own, keyed by the segment id: the retry's
	// unlink removes them together with the segment name.
	extra map[core.SegmentID][]string

	kick       chan struct{}
	done       chan struct{}
	stop       sync.Once
	wg         sync.WaitGroup
	retryEvery time.Duration // the ticker of the retry pass; a test shortens it
}

// NewRetirer returns the executor over c's dependencies. A nil Set, Cache,
// Maker or FS is a caller bug and panics, as NewPool panics on its own; a nil
// Pool or ReadOnly is a configuration the comment on RetirerConfig names.
func NewRetirer(c RetirerConfig) *Retirer {
	if c.Set == nil {
		panic("segment: a retirer needs its ledger")
	}
	if c.Cache == nil {
		panic("segment: a retirer needs its descriptor cache")
	}
	if c.Maker == nil {
		panic("segment: a retirer needs its Maker")
	}
	if c.FS == nil {
		panic("segment: a retirer needs its storage")
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	return &Retirer{
		cfg:        c,
		log:        c.Log,
		queued:     make(map[core.SegmentID]struct{}),
		extra:      make(map[core.SegmentID][]string),
		kick:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		retryEvery: time.Second,
	}
}

// Retire runs one retirement now: the unlink or the recycle of the segment id
// stands for, under the reason named. The refusals are the ledger's own
// (ErrNotInSet, ErrWrongState, ErrClaimed, ErrWrongReason), the wrapped
// read-only refusal, or the wrapped error of the storage step that failed —
// a failed step leaves the segment in the set, accounted, its claim released,
// for a retry through the queue.
//
// The order of the steps is RFC 28 §2.2's and RFC 1 §8.5's: the read-only
// check first, nothing touched before it passes; the ledger's claim, which
// serialises two retirements of one segment; Close(id), which drains the
// readers and closes the descriptors before any storage moves; the one
// filesystem call — the unlink or the rename — then the rest; the ledger's
// retire or recycle, the freed count, and the directory sync that makes the
// whole thing durable, which the returned Retirement reports.
func (r *Retirer) Retire(id core.SegmentID, reason Reason) (Retirement, error) {
	// R89: the read-only check runs before anything is touched, so a
	// read-only journal makes no ledger call and no seam call.
	if r.cfg.ReadOnly != nil {
		if err := r.cfg.ReadOnly(); err != nil {
			return Retirement{}, fmt.Errorf("segment: retirement refused: %w", err)
		}
	}

	st, full, err := r.cfg.Set.ClaimRetirement(id)
	if err != nil {
		return Retirement{}, err
	}
	// The reason must name the state the ledger answered. The claim is
	// released rather than kept: the segment was not retired, so R57 does
	// not reach it, and the right reason's retirement may claim again.
	if st != reasonState(reason) {
		r.cfg.Set.Release(id)
		return Retirement{}, ErrWrongReason
	}

	// decision: the choice between the unlink and the recycle is made here,
	// at the retirement itself, and never at the seal, because "whether a
	// full-size segment is recycled is decided when repack or reclamation
	// retires it, never at seal" (RFC 28 §2.2, R42): a full seal is
	// recycled while the pool is below its target now, and everything else
	// is unlinked — a segment sealed early, including one whose truncation
	// failed and whose size still stands at the cap, and every ended
	// segment, whatever the pool holds.
	recycle := st == Sealed && full && r.cfg.Pool != nil
	var class Class
	if recycle {
		if class, recycle = r.cfg.Pool.wantRecycle(); !recycle {
			// The pool is at its target: an unlink.
			class = 0
		}
	}

	// RFC 1 §8.5, by way of the cache's own drain: this segment's readers are
	// waited for and its descriptors closed before any storage moves,
	// holding no lock and no reference of our own while Close waits.
	if err := r.cfg.Cache.Close(id); err != nil {
		r.log.Warn("segment: the retirement closed a descriptor with an error",
			"segment", uint64(id), "err", err)
	}

	if recycle {
		return r.recycle(id, class)
	}
	return r.unlink(id)
}

// reasonState is the state the reason names. The pairs are exactly two, and
// nothing between them is served: NotHeld is the seal's reason, ReasonEnded
// the ended window's.
func reasonState(reason Reason) State {
	switch reason {
	case NotHeld:
		return Sealed
	case ReasonEnded:
		return Ended
	}
	return 0
}

// recycle turns a claimed full-sealed segment into a spare under a fresh id:
// rename to the fresh `.seg`, sync the directory, zero the header slot and
// the trailer slot — the cap's last 32 bytes — through a fresh descriptor,
// sync it, in that order and never zero first (R40), then hand the spare to
// the pool in the class its reservation chose. Any failed step is an unlink
// under whichever of its two names the journal finds (R41), reported as one.
func (r *Retirer) recycle(id core.SegmentID, class Class) (Retirement, error) {
	fresh := r.cfg.Maker.drawID()
	oldName := format.SegmentName(id)
	freshName := format.SegmentName(fresh)

	// The rename, then the directory sync that makes it durable: a crash
	// between the two leaves the old name, the same shape as an unlink's
	// own crash before its sync.
	if err := r.cfg.FS.Rename(oldName, freshName); err != nil {
		return r.recycleFailed(id, class, fresh, "rename", err)
	}
	if err := r.cfg.FS.SyncDir(); err != nil {
		return r.recycleFailed(id, class, fresh, "sync_dir", err)
	}

	// ponytail: the zeroing runs through a fresh descriptor, outside the
	// cache and its per-process DescriptorLimit, like Make's own Create. The
	// bound the limit holds counts writers a sync has not covered yet, and
	// this one holds its slot for two small writes; folding it into the cache
	// would charge the limit a spare nobody reads. Withdraw when the writer
	// bound of record counts a recycle's zeroing descriptor too.
	f, err := r.cfg.FS.Open(freshName)
	if err != nil {
		return r.recycleFailed(id, class, fresh, "open", err)
	}
	zeros := make([]byte, format.SegmentHeaderSize)
	if n, werr := f.WriteAt(zeros[:format.SegmentHeaderSize], 0); werr != nil || n != format.SegmentHeaderSize {
		_ = f.Close()
		return r.recycleFailed(id, class, fresh, "zero", shortWrite(werr, n, format.SegmentHeaderSize))
	}
	if n, werr := f.WriteAt(zeros[:format.TrailerSize], r.cfg.Set.cap-format.TrailerSize); werr != nil || n != format.TrailerSize {
		_ = f.Close()
		return r.recycleFailed(id, class, fresh, "zero", shortWrite(werr, n, format.TrailerSize))
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return r.recycleFailed(id, class, fresh, "sync", err)
	}
	if err := f.Close(); err != nil {
		return r.recycleFailed(id, class, fresh, "close", err)
	}

	// The ledger moves the segment without changing the footprint (R78) and
	// takes the spare under its fresh id; the pool holds it ready in the
	// class its reservation chose.
	if err := r.cfg.Set.Recycle(id, fresh); err != nil {
		return r.recycleFailed(id, class, fresh, "ledger", err)
	}
	if r.cfg.Pool != nil {
		r.cfg.Pool.recycled(fresh, class)
	}
	r.cfg.Cache.Forget(id)

	// decision: freed_bytes rises by the cap on a recycle, against the epic
	// plan's own R79 ("unchanged after a recycle"), because RFC 1 §3.9
	// counts "storage actually returned by retiring segments: to the
	// filesystem by an unlink, or to the spare pool by a recycle" — the
	// same words the public Stats.Counters.FreedBytes comment carries, and
	// where the plan and the RFC disagree the RFC rules. Withdrawn if the
	// RFC's author rules the gauge counts only bytes back to the filesystem.
	r.mu.Lock()
	r.freedB += uint64(r.cfg.Set.cap)
	r.mu.Unlock()

	ret := Retirement{ID: id, Recycled: true, Fresh: fresh, Freed: r.cfg.Set.cap, Durable: true}
	r.signal(ret)
	return ret, nil
}

// recycleFailed is what a failed recycle step becomes: not a spare, never
// taken, an unlink under both of the segment's names — "since a failed
// rename may or may not have happened" (RFC 28 §2.2, R41). The pool's
// reservation is given back, the fallback unlink proceeds exactly as
// unlink's own, and the error that returns names the step that failed.
func (r *Retirer) recycleFailed(id core.SegmentID, class Class, fresh core.SegmentID, step string, err error) (Retirement, error) {
	if r.cfg.Pool != nil {
		r.cfg.Pool.recycleFailed(class)
	}
	// The fresh name is remembered beside the segment's own, so the unlink
	// here — and the retry, should the unlink itself fail — removes both.
	r.extraNames(id, format.SegmentName(fresh))
	ret, uerr := r.unlink(id)
	if uerr != nil {
		return ret, fmt.Errorf("segment: retiring %d: recycle %s: %w", uint64(id), step, errors.Join(err, uerr))
	}
	// The fallback unlink's own error, if one gets this far, is nil: the
	// recycle's step leads.
	return ret, fmt.Errorf("segment: retiring %d: recycle %s: %w", uint64(id), step, err)
}

// unlink unlinks a claimed segment: theStorageSync first — under the
// segment's own name, under the spare's temporary name for an ended segment,
// and under every extra name a failed recycle recorded — then the ledger's
// retire and the bytes' freed count, and last the directory sync that makes
// the removal durable, whose result the returned Retirement reports.
func (r *Retirer) unlink(id core.SegmentID) (Retirement, error) {
	ret := Retirement{ID: id}

	names := []string{format.SegmentName(id)}
	// decision: an Ended segment's unlink removes the spare's temporary name
	// too, because the ledger keeps no name and the one file a failed make
	// left may sit under either (R83): a remove that finds one absent counts
	// it gone, so the extra remove costs nothing when the file sits under
	// the first. A Sealed segment was never a spare, so it pays no second
	// remove.
	if st, _, serr := r.cfg.Set.state(id); serr == nil && st == Ended {
		names = append(names, format.SpareName(id))
	}
	r.mu.Lock()
	if extra := r.extra[id]; len(extra) > 0 {
		names = append(names, extra...)
	}
	r.mu.Unlock()

	for _, name := range names {
		if err := r.cfg.FS.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			// The segment stays in the set, accounted, and the claim goes
			// back so a retry can take it again: it was not retired.
			r.cfg.Set.Release(id)
			return ret, fmt.Errorf("segment: retiring %d: unlink %s: %w", uint64(id), name, err)
		}
	}

	freed, err := r.cfg.Set.Retire(id)
	if err != nil {
		return ret, fmt.Errorf("segment: retiring %d: %w", uint64(id), err)
	}
	ret.Freed = freed
	r.cfg.Cache.Forget(id)
	if r.cfg.Pool != nil {
		r.cfg.Pool.Kick()
	}
	r.mu.Lock()
	delete(r.extra, id)
	r.mu.Unlock()

	// freed_bytes rises only now, after the unlink has happened: a Wait on
	// the remove still sees the old figure.
	r.mu.Lock()
	r.freedB += uint64(freed)
	r.mu.Unlock()

	if err := r.cfg.FS.SyncDir(); err != nil {
		// The retirement is done, only its durability is not; the worker
		// re-syncs and the signal waits for it.
		r.mu.Lock()
		r.owed = append(r.owed, ret)
		r.mu.Unlock()
		return ret, nil
	}

	ret.Durable = true
	r.signal(ret)
	return ret, nil
}

// shortWrite joins a short write's error with its shortness, so the failed
// step names what happened to it: a write that lands fewer bytes than asked
// is a failed step of the recycle, whatever the error says.
func shortWrite(err error, n, want int) error {
	if err == nil {
		return fmt.Errorf("%d bytes of %d", n, want)
	}
	return fmt.Errorf("%d bytes of %d: %w", n, want, err)
}

// extraNames records names the unlink must remove beside the segment's own,
// under r.mu.
func (r *Retirer) extraNames(id core.SegmentID, names ...string) {
	r.mu.Lock()
	r.extra[id] = append(r.extra[id], names...)
	r.mu.Unlock()
}

// signal calls the durable-retirement signal, when it is configured.
func (r *Retirer) signal(ret Retirement) {
	if r.cfg.Durable == nil {
		return
	}
	r.cfg.Durable(ret)
}

// Enqueue hands a segment to the worker. The queue holds one entry per id —
// a second hand-over of the same segment changes nothing — and the kick
// wakes the worker, coalesced. After Stop it is a drop.
func (r *Retirer) Enqueue(id core.SegmentID, reason Reason) {
	r.mu.Lock()
	if r.stoppedLocked() {
		r.mu.Unlock()
		return
	}
	if _, ok := r.queued[id]; !ok {
		r.queue = append(r.queue, id)
		r.reasons = append(r.reasons, reason)
		r.queued[id] = struct{}{}
	}
	r.mu.Unlock()

	r.kickWorker()
}

// stoppedLocked reports whether Stop has closed done; the caller holds r.mu.
// The closed channel is the flag, so a Stop that races Enqueue cannot leave
// a queue the mutex still accepted behind a worker that already exited.
func (r *Retirer) stoppedLocked() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// kickWorker wakes the worker, coalescing.
func (r *Retirer) kickWorker() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Start launches the worker, idempotent; after Stop it is a no-op. Until
// Start runs, Enqueue still collects: the worker's first pass drains what
// collected before it.
func (r *Retirer) Start() {
	r.mu.Lock()
	if r.started || r.stoppedLocked() {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.wg.Add(1)
	go r.worker()
	r.mu.Unlock()

	r.kickWorker()
}

// Stop stops the worker and joins it. An in-flight retirement completes
// first; the queue is dropped rather than drained.
//
// decision: the queue is dropped at Stop, because epic 9 re-hands every
// not-held segment at the next open — a segment left queued is found again by
// the open's scan, so draining it can only spend storage time at close.
// Withdrawn if T5.11's close ever needs the queue drained before the set
// shuts.
func (r *Retirer) Stop() {
	r.stop.Do(func() { close(r.done) })
	r.wg.Wait()
}

// Counters returns the executor's counters, read without a walk.
func (r *Retirer) Counters() RetireCounters {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RetireCounters{FreedBytes: r.freedB}
}

// worker drains the queue: one goroutine, woken by a coalesced kick or by
// its retry ticker, running one pass per wake.
//
// ponytail: the retry pass runs every retryEach, one retry per second on a
// failing device, plain and unbacked-off. Ceiling: a segment whose unlink
// keeps failing retries once a second forever, which a watched device reads
// as a steady one-line error. Upgrade to a backoff once a stuck unlink shows
// in logs; the field is the interval to shorten with it.
func (r *Retirer) worker() {
	defer r.wg.Done()

	t := time.NewTicker(r.retryEvery)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-r.kick:
			r.pass()
		case <-t.C:
			r.pass()
		}
	}
}

// pass runs one drain: the owed directory syncs first, then every queued id
// once. A read-only journal changes nothing and leaves the queue and the
// owed signals as they are (R89), so a queue lives a read-only interval and
// drains once the journal is writable again.
func (r *Retirer) pass() {
	// R89: a read-only journal unlinks nothing, so the pass retires nothing
	// either — the queue and the owed signals wait.
	if r.cfg.ReadOnly != nil {
		if err := r.cfg.ReadOnly(); err != nil {
			return
		}
	}

	// The owed durable signals: one sync returns them all, for the
	// retirements whose own sync failed after the storage had moved.
	r.mu.Lock()
	owed := r.owed
	r.mu.Unlock()
	if len(owed) > 0 {
		if err := r.cfg.FS.SyncDir(); err == nil {
			for _, ret := range owed {
				ret.Durable = true
				r.signal(ret)
			}
			r.mu.Lock()
			r.owed = nil
			r.mu.Unlock()
		}
	}

	// The queue: one id at a time, each hand-over's own retirement.
	r.mu.Lock()
	if r.stoppedLocked() {
		r.mu.Unlock()
		return
	}
	queue := r.queue
	reasons := r.reasons
	r.queue = nil
	r.reasons = nil
	r.mu.Unlock()

	for i, id := range queue {
		_, err := r.Retire(id, reasons[i])
		switch {
		case err == nil:
			r.dropQueued(id)
		case errors.Is(err, ErrNotInSet), errors.Is(err, ErrWrongState), errors.Is(err, ErrWrongReason):
			// ErrNotInSet is the ledger's own double retire, which it
			// already counted and logged at Warn; the other two are a bug
			// in what the caller handed over, and the queue carries
			// neither forward. The drop is what stops the queue from
			// retrying a segment it can never retire.
			if !errors.Is(err, ErrNotInSet) {
				r.log.Warn("segment: the retire queue dropped a segment",
					"segment", uint64(id), "err", err)
			}
			r.dropQueued(id)
		case errors.Is(err, ErrClaimed), errors.Is(err, fs.ErrClosed):
			// Another path holds the claim, or the journal has closed: the
			// id stays for a later pass, which is the retry.
			r.requeue(id, reasons[i])
		default:
			// A failed step, the read-only refusal included: the id stays
			// for the next pass.
			r.requeue(id, reasons[i])
		}
	}
}

// dropQueued forgets a queued id: its retirement is done, or the queue is
// wrong to carry it further.
func (r *Retirer) dropQueued(id core.SegmentID) {
	r.mu.Lock()
	delete(r.queued, id)
	r.mu.Unlock()
}

// requeue puts an id back at the head of the queue with its reason, for the
// next pass.
func (r *Retirer) requeue(id core.SegmentID, reason Reason) {
	r.mu.Lock()
	r.queue = append([]core.SegmentID{id}, r.queue...)
	r.reasons = append([]Reason{reason}, r.reasons...)
	r.mu.Unlock()
}

// state answers a segment's state and the seal's full word. The unlink's
// both-names decision is its only caller: the ledger exposes no read of its
// own because nothing else may reach a segment's state without a claim.
func (s *Set) state(id core.SegmentID) (st State, full bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.segs[id]
	if !ok {
		return 0, false, ErrNotInSet
	}
	return e.state, e.full, nil
}
