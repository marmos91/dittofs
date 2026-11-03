package segment

import (
	"container/list"
	"errors"
	"io/fs"
	"sync"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// The cache's sentinel errors. A failed acquire names why the segment cannot
// be read, so the read's caller can answer missing, refused or gone.
var (
	// ErrSegmentGone is returned by Acquire of a segment whose Close has
	// started or whose file no longer exists. It is not retried: ids are
	// never reused, so the segment is gone for good. An acquire after the
	// cache's Shutdown returns ErrCacheClosed instead, whatever the segment's
	// own state.
	ErrSegmentGone = errors.New("segment: segment is closed for reclamation")

	// ErrNoDescriptor is returned by Acquire when every descriptor the budget
	// allows is held by a reference or pinned by writes a sync has not yet
	// covered, so no descriptor can be opened for this segment. It is not
	// waited on or retried: the bound is meant to exceed what the process's
	// streams, seals and spare fills hold at once, so it means the bound is
	// set too low.
	ErrNoDescriptor = errors.New("segment: descriptor limit reached")

	// ErrCacheClosed is returned by Acquire after Shutdown. Nothing is
	// opened; the journal's operations are refused by its own close before
	// they reach here.
	ErrCacheClosed = errors.New("segment: descriptor cache is shut down")
)

// Descriptors is one journal's cache of open segment descriptors, bounded by
// the process's descriptor budget. Acquire returns a reference to the
// segment's descriptor, opening the file on a miss outside the mutex and
// counting the reopen; Release returns the reference and, when it is the last
// one, makes the descriptor evictable. Close(id) starts the segment's
// reference drain: it rejects new acquires of the segment and waits for the
// references its own readers hold, so the descriptor is closed — and the
// caller may free the segment's storage — before it returns.
//
// decision: the cache's mutex is none of RFC 1 §8.2's six domains. It is a
// leaf: never held across a storage call or a wait, and never held while
// another lock is (R69). The per-segment reference drain stands for the
// storage guard domain, which is why the storage guard's own ordering rules
// bind nothing here — nothing but the drain's wait runs while storage is
// freed.
//
// ponytail: one mutex around one LRU per cache, with cross-cache reclaim
// round-robin through the budget rather than one process-wide LRU. Two
// journals' hits contend on one mutex only when both reclaim at once. Shard
// the mutex per segment range, or make the LRU process-wide, when the mixed
// read soak's profile shows contention on this mutex or reopens concentrated
// in one journal.
type Descriptors struct {
	fs         storage.FS
	name       func(core.SegmentID) string
	budget     *core.DescriptorBudget
	unregister func()

	mu      sync.Mutex
	entries map[core.SegmentID]*descEntry
	idle    list.List // LRU of idle descriptors: front is the most recently used

	open      int    // descriptors this cache holds open
	reopens   uint64 // reads that reopened a closed segment
	totalRefs int    // references outstanding across the cache, for Shutdown's wait
	shut      bool
	shutDone  chan struct{} // closed by the Release that brings totalRefs to zero after Shutdown
	// shutFinished is made by the first Shutdown and closed when it returns,
	// so a concurrent or later Shutdown waits for the files to be closed.
	shutFinished chan struct{}
}

// descEntry is one segment's descriptor state: the file, nil when the
// descriptor is closed; the references outstanding on it; and the drain that
// makes Close(id) wait for them.
//
// decision: every field but written and synced is guarded by the owning
// cache's leaf mutex, the one the Descriptors marker names — including the
// ones a reader of the entry reaches through its Ref, which are first
// survivor-checked under the same mutex (refAfterOpen). written and synced
// are lock-free atomics by design: a write and a sync through the same
// descriptor run on different references with no lock held, and their
// ordering carries the covered-write pin, so they are never written under
// the mutex; dirty, their one read, runs under it, on the last release and
// in the reclaimer's scan, where no write through the entry can still be in
// flight to reorder against.
type descEntry struct {
	id       core.SegmentID
	file     storage.File // nil when the descriptor is closed
	refs     int          // references held; an in-progress open counts as one
	opening  *openCall    // non-nil while an open is in flight
	everOpen bool         // opened by this cache before, so the next miss is a reopen
	gone     bool         // Close(id) started: no new reference, never reopened

	// absent is set when an open failed not-exist: the file is proven gone,
	// so the entry drops from the map once its last reference releases — a
	// later Acquire would fail the same way, so nothing it could reach needs
	// a tombstone. A Close tombstone is the opposite: its file exists until
	// the caller unlinks it, so it stays for the Forget that ends the
	// retirement.
	absent  bool
	drained chan struct{} // set by Close(id) while references remain; closed by the last Release

	// idle is the entry's element in the cache's idle list when the
	// descriptor is open: a referenced or dirty entry stays linked, and the
	// reclaimer skips both of those — the first by its refs, the second by
	// the dirty check that is the pin's one rule.
	idle *list.Element

	written atomic.Uint64 // writes issued through the descriptor, counted once each has landed
	synced  atomic.Uint64 // the highest written value a returned Sync covered, or that failed
}

// dirty reports whether a write issued through the descriptor has not been
// covered by a sync that returned or failed. Only Release, on the last
// reference, and reclaimOne, in its scan, read it — both under c.mu, where no
// write through the entry can still be in flight.
func (e *descEntry) dirty() bool { return e.written.Load() > e.synced.Load() }

// openCall is one in-flight open of a segment: the waiters' rendezvous and
// the outcome, written once by the opening Acquire before the channel closes.
// It is per open, not per entry, so a failed open's waiters keep its error
// even after a newer Acquire's miss has started its own.
//
// decision: err is written under the cache's mutex and published by the
// close of done, which the waiters synchronise on; a waiter reads only its
// own open's err, after <-done, never another's state. The mutex alone is
// not the publish: a waiter that had not yet parked when the open finished
// arrives at a closed done channel and still needs the err the close carries.
type openCall struct {
	done chan struct{} // closed once err is set
	err  error         // the open's outcome: nil when it yields a descriptor
}

// waiterHook is a test hook in refAfterOpen's wake-to-lock window: a null
// func outside the check that stages a failed waiter's late drop. It lives
// on the cache's package rather than in a test file only because the window
// it parks the waiter in is inside refAfterOpen, which no test file can
// reach.
var waiterHook func()

// shutWakeHook is a test hook in Shutdown's wake-to-lock window, where a
// reclaim can take a reference across the wait's signal: a check parks the
// Shutdown there to stage that order instead of leaning on the scheduler.
// nil outside the checks that use it.
var shutWakeHook func()

// Ref is a reference to a segment's open descriptor. Its methods need no
// lock of the cache's: the reference holds the descriptor open.
//
// Release returns the reference. It is called exactly once per Acquire that
// returned a Ref; a second Release on the same value corrupts the cache's
// counts.
type Ref struct {
	c *Descriptors
	e *descEntry
}

// DescriptorStats is the cache's view of the descriptor gauges: how many
// descriptors this journal holds open, the bound the process's journals hold
// them under together, and how many reads reopened a closed segment.
type DescriptorStats struct {
	Open    int
	Limit   int
	Reopens uint64
}

// NewDescriptors returns a cache over fs, naming a segment's file with name,
// holding its descriptors under budget. The cache registers itself with the
// budget as a reclaimer, so another journal's acquire can ask it to close one
// idle descriptor of its own; Shutdown unregisters it.
func NewDescriptors(fs storage.FS, name func(core.SegmentID) string, budget *core.DescriptorBudget) *Descriptors {
	c := &Descriptors{
		fs:      fs,
		name:    name,
		budget:  budget,
		entries: map[core.SegmentID]*descEntry{},
	}
	c.unregister = budget.Register(c.reclaimOne)
	return c
}

// Acquire returns a reference to segment id's open descriptor, opening the
// file on a miss. The descriptor is opened outside the cache's mutex, and a
// concurrent Acquire of the same segment waits on the open rather than
// issuing its own.
//
// It returns ErrSegmentGone when the segment's Close has started or its file
// no longer exists, ErrCacheClosed after Shutdown, and ErrNoDescriptor when
// the budget's slots are all held by references or pinned by dirty
// descriptors and none of the process's caches can close one for this
// acquire. The refusal performs no I/O and waits for nothing.
func (c *Descriptors) Acquire(id core.SegmentID) (Ref, error) {
	c.mu.Lock()
	if c.shut {
		c.mu.Unlock()
		return Ref{}, ErrCacheClosed
	}
	e := c.entries[id]
	if e != nil && e.gone {
		c.mu.Unlock()
		return Ref{}, ErrSegmentGone
	}

	// A hit: the descriptor is open. The entry stays linked in the idle
	// list — the reclaimer skips a referenced entry, and unlinking it here
	// would only cost the allocation of linking it back on Release.
	if e != nil && e.file != nil {
		e.refs++
		c.totalRefs++
		c.mu.Unlock()
		return Ref{c: c, e: e}, nil
	}

	// An open is in flight: hold the reference from before the wait, then
	// wait on it outside the mutex and take its result under it.
	if e != nil && e.opening != nil {
		e.refs++
		c.totalRefs++
		op := e.opening
		c.mu.Unlock()
		<-op.done
		return c.refAfterOpen(e, op)
	}

	// A miss: make the entry, if it is absent, and hold the reference the
	// in-flight open runs under.
	if e == nil {
		e = &descEntry{id: id}
		c.entries[id] = e
	}
	e.refs++
	c.totalRefs++
	op := &openCall{done: make(chan struct{})}
	e.opening = op
	c.mu.Unlock()

	// The budget slot and the open run with no lock held: a reclaimer, the
	// budget's own count or another cache's reclaim must not wait on this
	// mutex, and a fault installed for the leaf-lock check must never see it.
	if !c.takeSlot() {
		c.openFailed(e, op, ErrNoDescriptor)
		return Ref{}, ErrNoDescriptor
	}
	f, err := c.fs.Open(c.name(id))
	notExist := errors.Is(err, fs.ErrNotExist)
	if notExist {
		err = ErrSegmentGone
	}

	c.mu.Lock()
	if err != nil {
		op.err = err
		if notExist {
			// The file is gone, which no retry undoes: an id is never reused,
			// so new acquires must be refused from here on — the entry is
			// marked gone — and its last reference's drop deletes it, because
			// nothing it could reach needs a tombstone.
			e.gone = true
			e.absent = true
		}
		if e.opening == op {
			e.opening = nil
		}
		c.mu.Unlock()
		close(op.done)
		c.budget.Give()
		c.dropRef(e)
		return Ref{}, err
	}

	// Close(id) or Shutdown started while the open ran: the file is not
	// cached, so it is closed after the mutex is released, and its slot
	// given back.
	if e.gone || c.shut {
		gone := e.gone
		op.err = ErrCacheClosed
		if gone {
			op.err = ErrSegmentGone
		}
		if e.opening == op {
			e.opening = nil
		}
		c.mu.Unlock()
		close(op.done)
		_ = f.Close()
		c.budget.Give()
		c.dropRef(e)
		return Ref{}, op.err
	}

	e.file = f
	c.open++
	if e.everOpen {
		c.reopens++
	}
	e.everOpen = true
	if e.opening == op {
		e.opening = nil
	}
	c.mu.Unlock()
	close(op.done)
	return Ref{c: c, e: e}, nil
}

// refAfterOpen builds the reference for an Acquire that waited on an
// in-flight open, which is the one it read openCall.done from: the open's
// outcome is taken under the cache's mutex, and the reference held from before
// the wait is dropped the way Release drops one when the open did not yield a
// descriptor the waiter can use. An open that raced a newer one does not
// read the newer's state: its outcome is its own.
func (c *Descriptors) refAfterOpen(e *descEntry, op *openCall) (Ref, error) {
	// waiterHook, a test hook, runs between this waiter's wake and the lock,
	// so a check can stage another acquire's open against it. nil outside
	// the one check that uses it.
	if waiterHook != nil {
		waiterHook()
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if op.err != nil {
		c.dropRefLocked(e)
		return Ref{}, op.err
	}
	if e.gone || c.shut {
		c.dropRefLocked(e)
		if e.gone {
			return Ref{}, ErrSegmentGone
		}
		return Ref{}, ErrCacheClosed
	}
	// op.err is nil, so the opening Acquire set e.file before closing
	// op.done; only Close, Shutdown or a reclaim can nil it since, and each
	// is caught above — the reclaim cannot, because it takes only a
	// descriptor no reference holds, and this waiter's has been held since
	// before the wait.
	return Ref{c: c, e: e}, nil
}

// dropRef drops the reference an Acquire took before a wait it failed, with
// the cache's mutex held, and signals the drain or Shutdown when it was the
// last reference. The cache's mutex is not held on entry.
func (c *Descriptors) dropRef(e *descEntry) {
	c.mu.Lock()
	c.dropRefLocked(e)
	c.mu.Unlock()
}

// dropRefLocked is dropRef with the cache's mutex held. It ends with the
// shared tail the last reference runs: what lastRefLocked does, so a
// reference dropped by a failed open's unwind leaves the entry exactly as
// released one does.
func (c *Descriptors) dropRefLocked(e *descEntry) {
	e.refs--
	c.totalRefs--
	if e.refs == 0 {
		c.lastRefLocked(e)
	}
}

// lastRefLocked runs the tail every path that takes a entry's reference
// count to zero must share: signal a drain Close is waiting on and a
// Shutdown that is waiting on the cache's count, drop an entry whose file
// was proven gone, and make the descriptor evictable when it is still
// cached, clean and free of a drain that owns its close. The caller holds
// c.mu.
func (c *Descriptors) lastRefLocked(e *descEntry) {
	draining := e.drained != nil
	if draining {
		close(e.drained)
		e.drained = nil
	}
	if c.shut && c.totalRefs == 0 && c.shutDone != nil {
		close(c.shutDone)
		// Nilled at once: a reclaim can take and drop a reference across
		// this signal — the wait's channel was made for one signal — and a
		// second close here would panic on a closed channel.
		c.shutDone = nil
	}
	if e.absent {
		// The file was proven gone while it was being opened, and no
		// reference is left that parked on that open: the entry drops, so a
		// stray acquire of a retired segment leaves no trace in the map.
		delete(c.entries, e.id)
		return
	}
	if e.file != nil && !draining {
		// A dirty entry stays linked, as a referenced one does: the
		// reclaimer's own scan is what skips it — a write through it may
		// have reached no sync, and closing it to make room would drop the
		// writeback error the platform reports only through the open
		// handle. Staying linked keeps the entry reachable by the scan, so
		// the release that follows a covering or failed Sync needs no path
		// of its own to make it evictable again.
		//
		// A cache whose Shutdown has begun relinks too: its wait is not a
		// reason to hold the descriptor out of the reclaimer's reach, and
		// Shutdown takes only the files still attached once the wait ends.
		c.idlePushFront(e)
	}
}

// takeSlot draws one descriptor slot from the budget, reclaiming across every
// cache the process holds until one yields. It returns false when the
// budget's slots are all held by references and no cache can close one,
// which Acquire answers with ErrNoDescriptor.
//
// decision: Acquire refuses rather than waiting when every slot is pinned,
// because the bound is meant to exceed what the process's journals hold at
// once: a refusal means the bound is set too low or the journal leaks
// references, and a wait would turn that misconfiguration into a hang.
func (c *Descriptors) takeSlot() bool {
	for !c.budget.TryTake() {
		if !c.reclaimOne() && !c.budget.Reclaim() {
			return false
		}
	}
	return true
}

// ReadAt reads len(p) bytes at offset off through the reference's open
// descriptor. The reference holds the descriptor open, so the read needs no
// lock of the cache's.
func (r Ref) ReadAt(p []byte, off int64) (int, error) {
	return r.e.file.ReadAt(p, off)
}

// WriteAt writes len(p) bytes at offset off through the reference's open
// descriptor. The write numbers the descriptor once it has landed: a Sync
// whose fsync starts while the write is still in flight loads a count that
// does not include it, so the write stays uncovered and the descriptor stays
// pinned until a later sync covers it — the safe direction, never the
// reverse, where a sync would mark a write covered that no fsync reached.
func (r Ref) WriteAt(p []byte, off int64) (int, error) {
	n, err := r.e.file.WriteAt(p, off)
	r.e.written.Add(1)
	return n, err
}

// Sync makes the writes issued through the reference's descriptor durable,
// covering every write numbered before it: whether the sync returned nil or
// failed, the covered-or-failed count rises to the write number the sync
// started at, so the descriptor becomes evictable either way and a failed
// sync never pins it open forever.
//
// decision: a failed sync unpins, because its caller has seen the failure,
// and the durability layer's failed window owns the record from there — the
// bound is not to hold a descriptor for writes nobody will retry through it.
// A write in flight when the sync started is not covered, so the entry stays
// pinned; it does not name a write the fsync cannot have reached.
func (r Ref) Sync() error {
	g := r.e.written.Load()
	err := r.e.file.Sync()
	for {
		old := r.e.synced.Load()
		if old >= g || r.e.synced.CompareAndSwap(old, g) {
			break
		}
	}
	return err
}

// Release returns the reference. When it is the last one on the segment's
// descriptor, the descriptor enters the idle list — a referenced or dirty
// entry rides it like a clean one — and the drain Close(id) started, or the
// Shutdown of the cache, is signalled. It performs no I/O.
//
// decision: a descriptor with an unsynced write is not closed to make room
// under the bound until a sync through it has covered every such write or has
// failed, because a reopened descriptor may never see a writeback error the
// platform dropped along with the file's in-memory state — so the bound
// counts such descriptors, and Acquire refuses with ErrNoDescriptor rather
// than evicting one. The pin is the reclaimer's dirty check alone; the entry
// itself stays linked. Withdraw the exemption only if a close ever reaches a
// segment whose writes are not yet covered (Close(id) and Shutdown are it:
// below).
func (r Ref) Release() {
	c, e := r.c, r.e

	c.mu.Lock()
	e.refs--
	c.totalRefs--
	if e.refs == 0 {
		// The last reference runs the same tail every drop of a last
		// reference runs: a failed open's unwind must leave the entry as
		// settled as a released one, an evictable descriptor included.
		c.lastRefLocked(e)
	}
	c.mu.Unlock()
}

// idlePushFront moves the entry to the front of the idle list, the most
// recently used end; the caller holds c.mu and the entry's descriptor is
// open, unreferenced and clean.
func (c *Descriptors) idlePushFront(e *descEntry) {
	if e.idle != nil {
		c.idle.MoveToFront(e.idle)
		return
	}
	e.idle = c.idle.PushFront(e)
}

// idleRemove takes the entry out of the idle list; the caller holds c.mu.
func (c *Descriptors) idleRemove(e *descEntry) {
	if e.idle == nil {
		return
	}
	c.idle.Remove(e.idle)
	e.idle = nil
}

// reclaimOne closes the least recently used idle descriptor this cache holds,
// giving its budget slot back. It reports whether it closed one.
//
// It is the reclaimer the cache registered with the budget, so the budget
// calls it — and it re-enters the budget with Give — without the cache's
// mutex or the budget's held.
//
// decision: the close's error is ignored, because the descriptor and the
// budget slot are released either way — the file's handle is gone from this
// process whatever the platform reports — and the cache has no logger of its
// own. A failed close surfaces as the missing durability the next Sync of a
// reopened descriptor reports.
func (c *Descriptors) reclaimOne() bool {
	c.mu.Lock()

	// decision: a cache whose Shutdown has begun still yields its idle
	// descriptors, because a shutdown waiting on one slow reader is a
	// journal going quiet, not one the rest of the process may starve: the
	// descriptors its readers still hold are pinned by their references,
	// exactly like any journal's. Shutdown takes only the files still
	// attached after its wait, so a reclaim that detached one first is
	// skipped and never closed or given twice — this scan need not, and must
	// not, refuse for it.

	// The least recently used idle entry: the list's back. Referenced and
	// dirty entries stay linked, so the scan passes over them, and the
	// reclaimer never takes one of those: a dirty descriptor is the rule
	// here, not defence in depth — the pin is this one check.
	//
	// ponytail: a reclaim walks past every dirty entry before it reaches a
	// clean one, O(dirty) per scan, for dirty entries are pinned at the
	// list's two ends by use rather than gathered in a class of their own.
	// The ceiling is the bound itself, so the walk cannot outrun the
	// process's whole descriptor budget; upgrade to a clean list and a
	// dirty list, moved between on the covering or failed Sync, when a
	// reclaim-heavy profile shows scans dominated by pinned entries.
	var victim *descEntry
	for el := c.idle.Back(); el != nil; el = el.Prev() {
		e := el.Value.(*descEntry)
		if e.refs == 0 && e.file != nil && !e.dirty() {
			victim = e
			break
		}
	}
	if victim == nil {
		c.mu.Unlock()
		return false
	}

	file := victim.file
	victim.file = nil
	c.idleRemove(victim)
	c.open--
	// The close runs with no lock held, so a Close of this very segment can
	// reach the entry meanwhile and see no file. The reference taken here is
	// what makes that Close wait: the drain it starts sleeps until this close
	// and the slot's return are done, and only then may the caller free the
	// segment's storage. Shutdown's walk sees the same thing and skips the
	// file this reference still owes a close to.
	victim.refs++
	c.totalRefs++
	c.mu.Unlock()

	_ = file.Close()
	c.budget.Give()
	c.dropRef(victim)
	return true
}

// Close starts the segment's reference drain and returns once the segment's
// descriptor is closed: new acquires of the segment fail from the moment
// Close starts, and the references its own readers hold drop before it
// returns, so the caller can free the segment's storage — unlink or recycle
// it — as the last step, after no reader can hold the old location.
//
// A second Close of the same segment returns nil, and so does a Close of a
// segment whose descriptor the cache does not hold.
//
// Close must not be called while the caller itself holds a Ref on the
// segment: the drain waits for the last reference to drop, so such a caller
// waits for itself, forever. The caller releases its own references before
// it calls Close — the close path is: last use, Release, Close, then free
// the storage.
//
// decision: a per-segment reference drain that rejects new readers once it
// starts, in place of RFC 1 §8.5's shared/exclusive guard, which §8.5
// permits because freeing storage is still the last step: the caller unlinks
// or renames only after Close returns. Segment ids are never reused, so the
// rejection is permanent, and a read that finds the drain started reports
// the extent missing rather than waiting for a segment whose bytes are
// about to be freed.
//
// decision: Close still closes a dirty descriptor, exempt from the pin the
// reclaim paths hold, because by then the segment is being retired — its
// records durable by the retirement's own precondition — and the writer that
// would be robbed of a writeback error is gone. Withdraw the exemption if a
// Close ever reaches a segment whose writes are not yet covered.
func (c *Descriptors) Close(id core.SegmentID) error {
	c.mu.Lock()

	e := c.entries[id]
	if e == nil {
		// The segment was never acquired, or is already forgotten: a
		// tombstone marks it gone, so a read racing this Close cannot open
		// its descriptor and read storage about to be freed.
		e = &descEntry{id: id, gone: true}
		c.entries[id] = e
		c.mu.Unlock()
		return nil
	}
	e.gone = true
	c.idleRemove(e)

	// References remain: this Close waits for them — without holding the
	// mutex (R69), so the readers' own acquires and releases of other
	// segments, and the drain of another segment, still run. A second Close
	// that arrives while the drain runs joins the same channel: a fresh one
	// per Close would strand the first waiter on a channel no Release will
	// close, and returning without waiting would free storage while the last
	// reader's call, or an open still in flight, holds the descriptor.
	//
	// The wait loops: the last Release wakes every waiter, and whichever
	// locks first detaches the file and holds a reference across its close.
	// A waiter that locks after it finds that reference and waits again, so
	// no Close returns while another holder is still closing the segment's
	// file. The loop is bounded: the entry is gone, so no reader can take a
	// new reference, and the references it can still wait on are closes.
	for e.refs > 0 {
		if e.drained == nil {
			e.drained = make(chan struct{})
		}
		drained := e.drained
		c.mu.Unlock()
		<-drained
		c.mu.Lock()
	}

	// The descriptor, if the cache still holds it, is closed outside the
	// mutex, and its slot returned.
	file := e.file
	if file != nil {
		e.file = nil
		c.open--
		// The reference held across the close is what makes a second
		// Close of this segment — or the cache's Shutdown — wait it out:
		// without it, a caller that reaches the entry while the close runs
		// sees no file and no reference and returns nil, freeing storage
		// the close has not finished with. The drop after the close
		// settles any drain that took the wait in the meantime, and the
		// error is the close's own: the caller of the first Close is the
		// one to report it.
		e.refs++
		c.totalRefs++
	}
	c.mu.Unlock()

	if file == nil {
		return nil
	}
	// The slot goes back only once the file is closed, as every other close
	// path orders it: the process never holds a descriptor past its bound
	// while a close runs.
	err := file.Close()
	c.budget.Give()
	c.dropRef(e)
	return err
}

// openFailed records that an Acquire's open did not come to it — the slot was
// refused — releasing the reference the Acquire took and ending the in-flight
// open with the refusal, so its waiters see it and the entry is left with no
// file and no error state, ready for a later Acquire's retry.
func (c *Descriptors) openFailed(e *descEntry, op *openCall, err error) {
	c.mu.Lock()
	op.err = err
	if e.opening == op {
		e.opening = nil
	}
	c.mu.Unlock()
	close(op.done)
	c.dropRef(e)
}

// Forget drops the cache's entry for a segment whose Close returned, so the
// entry map stays bounded by the live segments. It is a no-op while the
// segment has a descriptor or a reference.
//
// The caller unlinks or recycles the segment's file first, so a later
// Acquire's open fails not-exist and answers ErrSegmentGone; forgetting
// before the file is gone would let a reader open the descriptor of storage
// the caller is about to free.
func (c *Descriptors) Forget(id core.SegmentID) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[id]
	if ok && e.gone && e.refs == 0 && e.file == nil {
		delete(c.entries, id)
	}
}

// Shutdown closes the cache: new acquires fail, the references outstanding are
// waited for — without the mutex — and every descriptor the cache holds is
// closed and its budget slot returned. It returns the join of the close
// errors. A later or concurrent Shutdown waits for the first to finish, then
// returns nil.
//
// The descriptors stay attached and yieldable while the references drain: a
// reclaim the budget runs meanwhile — another journal asking this one that is
// shutting down — may close one of this cache's idle descriptors, and this
// Shutdown then skips the file the reclaim already took, so nothing is closed
// or given back twice. The unregister from the budget happens once the
// references have drained, so the shutdown's own close walk is the last thing
// that needs the cache registered.
//
// decision: Shutdown closes the dirty descriptors too, exempt from the pin
// the reclaim paths hold, because the process is stopping: no writer survives
// it to be robbed of a writeback error, and the caller of Shutdown owns what
// is still unsynced. Withdraw the exemption if a Shutdown ever runs while
// writes through the cache are still wanted.
func (c *Descriptors) Shutdown() error {
	c.mu.Lock()
	if c.shut {
		finished := c.shutFinished
		c.mu.Unlock()
		<-finished
		return nil
	}
	c.shut = true
	finished := make(chan struct{})
	c.shutFinished = finished
	defer close(finished)

	// decision: the idle list stays as it is while the wait runs, so a
	// reclaim the budget makes meanwhile — another journal asking this one
	// it is shutting down — still finds this journal's idle descriptors
	// yieldable. Pinning them for the length of the wait would refuse every
	// other journal of the process a slot until this one's last reader
	// returned.

	// References remain: wait for them with no mutex held, signalled by the
	// Release that brings the cache's total to zero. None is held: the wait
	// ends at once.
	//
	// The wait loops rather than answering one signal, because a signal
	// does not mean the total stays zero: a reclaim the budget makes while
	// Shutdown waits — another journal asking this one that is shutting
	// down — takes a reference for its close and drops it after, moving the
	// total across the signal. Looping re-arms the wait until zero holds
	// under the mutex, so Shutdown cannot return while a late reclaim is
	// mid-close.
	for c.totalRefs > 0 {
		done := make(chan struct{})
		c.shutDone = done
		c.mu.Unlock()
		<-done
		if shutWakeHook != nil {
			shutWakeHook()
		}
		c.mu.Lock()
		c.shutDone = nil
	}

	// The drain is done: no reader can still be inside a call. The files are
	// taken now — only those still attached, because a Close that drained
	// while Shutdown waited took its own segment's file and gave its slot
	// back, and so did a reclaim that evicted an idle one — and closed
	// outside the mutex. Whichever holder reached a file first under the
	// mutex owns it: taking only the attached ones is what keeps every file
	// closed, and every slot given, exactly once.
	// The files close with the mutex released, so a Close of any of these
	// segments can reach its entry meanwhile and see no file — the same gap
	// the reclaimer closes with a reference. Each walk entry takes its
	// segment's reference under the mutex and drops it once its close and
	// slot are settled.
	var errs []error
	var files []storage.File
	var entries []*descEntry
	for _, e := range c.entries {
		if e.file == nil {
			continue
		}
		files = append(files, e.file)
		entries = append(entries, e)
		e.file = nil
		c.open--
		e.refs++
		c.totalRefs++
	}
	c.mu.Unlock()

	// The budget asks this cache no more.
	if c.unregister != nil {
		c.unregister()
	}

	for i, f := range files {
		if err := f.Close(); err != nil {
			errs = append(errs, err)
		}
		c.budget.Give()
		c.dropRef(entries[i])
	}
	return errors.Join(errs...)
}

// Stats returns the cache's descriptor gauges: the descriptors this journal
// holds open, the bound the process's journals hold them under together, and
// the reads that reopened a closed segment. They are counters the cache
// maintains, never a walk of its entries (R60).
func (c *Descriptors) Stats() DescriptorStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return DescriptorStats{
		Open:    c.open,
		Limit:   c.budget.Limit(),
		Reopens: c.reopens,
	}
}
