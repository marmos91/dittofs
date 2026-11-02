package segment

import (
	"container/list"
	"errors"
	"io/fs"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
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
	// allows is pinned by a reference or an unsynced write, so no descriptor
	// can be opened for this segment. It is not waited on or retried: the
	// bound is meant to exceed what the process's journals hold at once, so
	// it means the bound is set too low.
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
}

// descEntry is one segment's descriptor state: the file, nil when the
// descriptor is closed; the references outstanding on it; and the drain that
// makes Close(id) wait for them.
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
// the mutex; their reads (dirty) happen under it, on the last release or in
// the reclaimer's scan, where no write through the entry can still be in
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
	// descriptor is open and clean: a referenced entry stays linked, and the
	// reclaimer skips it by its refs; a dirty one is unlinked, because the
	// reclaimer is then not to reach it at all.
	idle *list.Element

	written atomic.Uint64 // writes issued through the descriptor
	synced  atomic.Uint64 // the highest written value a returned Sync covered, or that failed
}

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

// dirty reports whether a write issued through the descriptor has not been
// covered by a sync that returned or failed. A call through the entry's
// descriptor must not race this read: only Release, which the caller
// serialises with the last reference's, and reclaimOne, under c.mu, read it.
func (e *descEntry) dirty() bool { return e.written.Load() > e.synced.Load() }

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
// the budget's slots are all pinned by references or unsynced writes and
// none of the process's caches can close one for this acquire. The refusal
// performs no I/O and waits for nothing.
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

// dropRefLocked is dropRef with the cache's mutex held.
func (c *Descriptors) dropRefLocked(e *descEntry) {
	e.refs--
	c.totalRefs--
	if e.refs == 0 {
		if e.drained != nil {
			close(e.drained)
			e.drained = nil
		}
		if c.shut && c.totalRefs == 0 && c.shutDone != nil {
			close(c.shutDone)
		}
		if e.absent {
			// The file was proven gone while it was being opened, and no
			// reference is left that parked on that open: the entry drops, so
			// a stray acquire of a retired segment leaves no trace in the map.
			delete(c.entries, e.id)
		}
	}
}

// takeSlot draws one descriptor slot from the budget, reclaiming across every
// cache the process holds until one yields. It returns false when the
// budget's slots are all pinned by references or unsynced writes and no cache
// can close one, which Acquire answers with ErrNoDescriptor.
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
// descriptor, the descriptor becomes evictable — pushed to the idle list's
// front when it is clean, kept open with a slot when it is dirty — and the
// drain Close(id) started, or the Shutdown of the cache, is signalled. It
// performs no I/O.
//
// decision: a descriptor with an unsynced write is not closed to make room
// under the bound until a sync through it has covered its writes or failed,
// because a reopened descriptor may never see a writeback error the platform
// dropped along with the file's in-memory state; so the bound counts such
// descriptors, and Acquire refuses with ErrNoDescriptor rather than evicting
// one. A failed Sync raises the covered count too, so the descriptor does
// not stay pinned by writes the caller already saw fail.
func (r Ref) Release() {
	c, e := r.c, r.e

	c.mu.Lock()
	e.refs--
	c.totalRefs--
	if e.refs > 0 {
		c.mu.Unlock()
		return
	}

	// The last reference: signal the drain or Shutdown, then make the
	// descriptor evictable if it is still cached, open and clean. A dirty one
	// is unlinked, so the reclaimer cannot reach it while its writes are
	// uncovered, and a draining entry is left unlinked: Close, which is
	// waiting on the drain signal, closes the descriptor itself. A cache
	// whose Shutdown has begun relinks too: its wait is not a reason to
	// hold the descriptor out of the reclaimer's reach, and Shutdown takes
	// only the files still attached once the wait ends.
	draining := e.drained != nil
	if draining {
		close(e.drained)
		e.drained = nil
	}
	if c.shut && c.totalRefs == 0 && c.shutDone != nil {
		close(c.shutDone)
	}
	if e.file != nil && !draining {
		if e.dirty() {
			c.idleRemove(e)
		} else {
			c.idlePushFront(e)
		}
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
	// dirty entries are skipped — a referenced one stays linked, so the
	// scan passes over it — and the reclaimer never takes one of those.
	// Release also unlinks a dirty entry when its last reference drops, so
	// the dirty check here is deliberate redundancy, defence in depth on
	// the leaf-contract rule that keeps a reclaimer and a Release from
	// disagreeing about a victim: Release may not relink what it unlinked,
	// and this scan may not take what it skipped.
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
	c.mu.Unlock()

	_ = file.Close()
	c.budget.Give()
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
func (c *Descriptors) Close(id core.SegmentID) error {
	c.mu.Lock()

	e := c.entries[id]
	if e == nil {
		// The segment was never acquired, or is already forgotten: a
		// tombstone marks it gone, so a read racing this Close cannot open
		// its descriptor and read storage about to be freed.
		e = &descEntry{gone: true}
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
	if e.refs > 0 {
		if e.drained == nil {
			e.drained = make(chan struct{})
		}
		drained := e.drained
		c.mu.Unlock()
		<-drained

		// The last Release closed drained and nilled the field. Whatever
		// detached the descriptor below — this Close on a later round, a
		// concurrent Close, or the cache's Shutdown — took the file with it,
		// so the entries the drain leaves are settled exactly once.
		c.mu.Lock()
	}

	// The descriptor, if the cache still holds it, is closed outside the
	// mutex, and its slot returned.
	file := e.file
	if file != nil {
		e.file = nil
		c.open--
	}
	c.mu.Unlock()

	if file == nil {
		return nil
	}
	c.budget.Give()
	return file.Close()
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
// errors. A second Shutdown returns nil.
//
// The descriptors are taken out of the reclaimer's reach at once, but stay
// attached until the references have drained, so a reader that holds one
// finishes its reads while Shutdown waits. The unregister from the budget
// happens once the references have drained, so a reclaim the budget runs
// meanwhile asks a cache that still holds its descriptors.
func (c *Descriptors) Shutdown() error {
	c.mu.Lock()
	if c.shut {
		c.mu.Unlock()
		return nil
	}
	c.shut = true

	// decision: the idle list stays as it is while the wait runs, so a
	// reclaim the budget makes meanwhile — another journal asking this one
	// it is shutting down — still finds this journal's idle descriptors
	// yieldable. Pinning them for the length of the wait would refuse every
	// other journal of the process a slot until this one's last reader
	// returned.

	// References remain: wait for them with no mutex held, signalled by the
	// Release that brings the cache's total to zero. None is held: the wait
	// ends at once.
	if c.totalRefs > 0 {
		c.shutDone = make(chan struct{})
		c.mu.Unlock()
		<-c.shutDone
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
	var errs []error
	var files []storage.File
	for _, e := range c.entries {
		if e.file == nil {
			continue
		}
		files = append(files, e.file)
		e.file = nil
		c.open--
	}
	c.mu.Unlock()

	// The budget asks this cache no more.
	if c.unregister != nil {
		c.unregister()
	}

	for _, f := range files {
		if err := f.Close(); err != nil {
			errs = append(errs, err)
		}
		c.budget.Give()
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
