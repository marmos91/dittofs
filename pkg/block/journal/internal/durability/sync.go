package durability

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Domains is the file append domain's table, which the datapath owns and
// passes in.
type Domains interface {
	// Lock takes id's file append domain and returns its release.
	Lock(id core.FileID) (unlock func())
}

// Config is what a Durability is built over.
type Config struct {
	Clock   core.Clock   // required
	Domains Domains      // required
	Tracer  core.Tracer  // optional
	Log     *slog.Logger // optional; nil means discard
}

// SyncDurationBounds are the upper bounds of the sync duration histogram's
// buckets; the last bucket counts everything above the last bound.
var SyncDurationBounds = []time.Duration{
	time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond,
	50 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond, time.Second,
}

// Histogram is a copy of a duration histogram: Counts has len(Bounds)+1
// buckets, Counts[i] counting the observations at or below Bounds[i].
type Histogram struct {
	Bounds []time.Duration
	Counts []uint64
	Sum    time.Duration
}

// Part is the layer's report, read from counters kept at each sync.
type Part struct {
	Syncs, SyncFailures uint64
	SyncDuration        Histogram
	// LastSyncError is the most recent sync failure across the streams, nil
	// when none has failed.
	LastSyncError *SyncFailure
}

// Durability is the ledger and the workers of every stream.
type Durability struct {
	clock   core.Clock
	domains Domains
	tracer  core.Tracer
	log     *slog.Logger

	// mu guards made, started and stopped, and is never held across a stream
	// call.
	mu      sync.Mutex
	made    map[uint32]*stream
	started bool
	stopped bool

	// afterSync, when set by a test, runs in the worker after a stream sync
	// returns and before the worker takes the stream's lock.
	afterSync func()

	// cmu guards change, a layer-wide channel closed and replaced on every
	// stream change, so a waiter on several streams wakes on any of them. It
	// is a leaf lock, taken under a stream's mu.
	//
	// ponytail: one channel serves every stream, so a report on any stream
	// wakes every waiter of every stream and each re-scans its ledger targets.
	// The cost grows with waiters times reports, and is paid whether or not
	// the waiter's stream changed. Upgrade to a channel per stream, which a
	// waiter on several streams then selects over, when a profile of many
	// concurrent Sync callers shows the wake-ups.
	cmu    sync.Mutex
	change chan struct{}

	done chan struct{}
	stop sync.Once
	wg   sync.WaitGroup
}

// New returns a Durability. A nil Clock or Domains is a caller bug and panics.
func New(cfg Config) *Durability {
	if cfg.Clock == nil {
		panic("durability: the layer needs its clock")
	}
	if cfg.Domains == nil {
		panic("durability: the layer needs the file append domains")
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Durability{
		clock:   cfg.Clock,
		domains: cfg.Domains,
		tracer:  cfg.Tracer,
		log:     log,
		made:    map[uint32]*stream{},
		done:    make(chan struct{}),
		change:  make(chan struct{}),
	}
}

// changes returns the channel the next stream change closes.
func (d *Durability) changes() <-chan struct{} {
	d.cmu.Lock()
	defer d.cmu.Unlock()
	return d.change
}

// notifyAll wakes every waiter of every stream. It is called with a stream's
// mu held, which is safe since cmu is a leaf.
func (d *Durability) notifyAll() {
	d.cmu.Lock()
	close(d.change)
	d.change = make(chan struct{})
	d.cmu.Unlock()
}

// entry is one unsynced record of a stream's ledger.
type entry struct {
	w segment.Written
	// seen is when the worker first observed the record, on the clock.
	seen    time.Duration
	seenSet bool
	// done is set once the worker has completed the entry under the file's
	// append domain.
	done bool
}

// stream is one stream's state.
//
// decision: mu is a leaf mutex, never held across I/O, a sync, the clock, a
// file domain or another lock, because both sinks run under the
// segment's append domain and must not wait on anything.
type stream struct {
	d  *Durability
	no uint32
	s  *segment.Stream

	mu sync.Mutex
	// ledger holds the entries in Track order.
	//
	// ponytail: one entry per unsynced record, bounded by what a stream
	// accepts within one sync bound. Durable scans it linearly. Upgrade
	// to a per-file index when a profile shows the scan.
	ledger []entry
	// synced is the largest offset a sync reported, per segment.
	synced map[core.SegmentID]int64
	// failed holds the segments whose sync failed.
	failed map[core.SegmentID]error
	// ending holds the segments a sync found already ended whose end event
	// has not been taken yet: no waiter leads a sync for them and the timer
	// skips them.
	ending map[core.SegmentID]struct{}
	// ends are the end events the stream's consumer queued for the worker.
	ends []segment.EndEvent
	// windows are the pending windows, in end order.
	windows []*window
	// lastSeg is the newest segment the stream is known to have reached: the
	// segment of a tracked record or of a sync, whichever is later.
	lastSeg  core.SegmentID
	haveLast bool
	closed   bool
	// want is set by the waiter that leads a sync and cleared by the worker
	// as it starts one; syncing is set while the worker is inside the
	// stream's Sync.
	want    bool
	syncing bool

	// kick wakes the worker; capacity 1, sent without blocking.
	kick chan struct{}

	// reported is set by the Synced sink and cleared by the worker's
	// completion pass, so a pass with no report since the last has nothing
	// to complete. stamped counts the ledger's leading entries the worker
	// has stamped; entries are stamped in Track order.
	reported bool
	stamped  int

	// The counters have one writer, the worker, and are read by Stats
	// without mu, so a poll never waits on a lock an append takes.
	syncs, failures atomic.Uint64
	// lastFail is the last sync failure, stored by the worker and read by
	// Stats without mu.
	lastFail atomic.Pointer[failStamp]
	buckets  []atomic.Uint64
	sum      atomic.Int64

	// scratch and live are the worker's own, reused by each pass: the indices
	// it completes and the segments that still hold an entry.
	scratch []int
	live    map[core.SegmentID]struct{}
}

func newStream(d *Durability, no uint32) *stream {
	return &stream{
		d:       d,
		no:      no,
		live:    map[core.SegmentID]struct{}{},
		synced:  map[core.SegmentID]int64{},
		failed:  map[core.SegmentID]error{},
		ending:  map[core.SegmentID]struct{}{},
		kick:    make(chan struct{}, 1),
		buckets: make([]atomic.Uint64, len(SyncDurationBounds)+1),
	}
}

// wake asks the worker to run a pass, without blocking.
func (st *stream) wake() {
	select {
	case st.kick <- struct{}{}:
	default:
	}
}

// covered reports whether a sync has reached the end of the record. The caller
// holds mu.
func (st *stream) covered(loc segment.Location) bool {
	return st.synced[loc.Segment] >= loc.Off+loc.Len
}

// Sink returns stream no's sink; pass d.Sink to segment.NewStreams. Calling it
// twice for one number panics.
func (d *Durability) Sink(no uint32) segment.Sink {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.made[no]; dup {
		panic("durability: a second sink was asked for one stream")
	}
	st := newStream(d, no)
	d.made[no] = st
	return segment.Sink{
		Track: func(w segment.Written) {
			st.mu.Lock()
			st.ledger = append(st.ledger, entry{w: w})
			st.lastSeg, st.haveLast = w.Loc.Segment, true
			// A record a sync already reached, reported before the sink saw
			// it, still needs a completion pass.
			if st.covered(w.Loc) {
				st.reported = true
			}
			st.mu.Unlock()
			st.wake()
		},
		Synced: func(seg core.SegmentID, through int64) {
			st.mu.Lock()
			if through > st.synced[seg] {
				st.synced[seg] = through
			}
			st.reported = true
			// A waiter is released here without waiting for the worker: a
			// seal's sync covers records no worker sync of its own follows.
			d.notifyAll()
			st.mu.Unlock()
			st.wake()
		},
	}
}

// Start binds the streams, whose count must equal the number of sinks made,
// registers the layer for their end events and launches one worker per stream.
// It is idempotent and a no-op after Stop. It must run before any stream's
// first append: the stream panics on a consumer registered after one.
func (d *Durability) Start(streams *segment.Streams) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started || d.stopped {
		return
	}
	if streams.Len() != len(d.made) {
		panic("durability: the streams do not match the sinks made")
	}
	started := make([]*stream, streams.Len())
	for i := range started {
		st, ok := d.made[uint32(i)]
		if !ok {
			panic("durability: no sink was made for a stream")
		}
		st.s = streams.Stream(i)
		started[i] = st
	}
	d.started = true
	streams.OnEnd(d.onEnd)
	for _, st := range started {
		d.wg.Add(1)
		go d.run(st)
	}
}

// Durable reports whether the record at the location is durable: its segment
// has been synced through the record's end.
//
// decision: a location with no ledger entry reports true. A caller holding a
// location Append returned has a record that was tracked and, if it is gone
// from the ledger, already completed, so true is right for every location the
// contract lets a caller hold. It fails open for a location Append never
// returned, which reads durable although nothing was written there. Overturn
// this if a caller can ask about an arbitrary location, which then needs a
// record of the completed ones to tell the two apart.
//
// ponytail: it scans every stream's ledger, O(streams x ledger). Upgrade to a
// per-segment index when a profile shows the scan.
func (d *Durability) Durable(at segment.Location) bool {
	made := d.snapshot()
	for _, st := range made {
		st.mu.Lock()
		for i := range st.ledger {
			if st.ledger[i].w.Loc == at {
				ok := st.covered(at)
				st.mu.Unlock()
				return ok
			}
		}
		// A record of a pending window is never durable, whatever the
		// segment's synced offset says.
		if st.inWindowLocked(at) {
			st.mu.Unlock()
			return false
		}
		st.mu.Unlock()
	}
	return true
}

// SyncBound returns the longest a record waits for a sync.
func (d *Durability) SyncBound() time.Duration { return core.SyncBound }

// Stats returns the counters kept at each sync, without walking the ledger. A
// sync is counted just after the waiters it covered are released, so a Sync
// that has just returned may not be in the counters yet.
func (d *Durability) Stats() Part {
	made := d.snapshot()

	p := Part{SyncDuration: Histogram{
		Bounds: append([]time.Duration(nil), SyncDurationBounds...),
		Counts: make([]uint64, len(SyncDurationBounds)+1),
	}}
	var last *failStamp
	for _, st := range made {
		if f := st.lastFail.Load(); f != nil && (last == nil || f.mono > last.mono) {
			last = f
		}
		p.Syncs += st.syncs.Load()
		p.SyncFailures += st.failures.Load()
		p.SyncDuration.Sum += time.Duration(st.sum.Load())
		for i := range st.buckets {
			p.SyncDuration.Counts[i] += st.buckets[i].Load()
		}
	}
	if last != nil {
		sf := last.SyncFailure
		p.LastSyncError = &sf
	}
	return p
}

// Stop stops the workers and joins them. A second call is a no-op.
func (d *Durability) Stop() {
	d.stop.Do(func() {
		d.mu.Lock()
		d.stopped = true
		d.mu.Unlock()
		made := d.snapshot()
		close(d.done)
		for _, st := range made {
			st.mu.Lock()
			st.closed = true
			d.notifyAll()
			st.mu.Unlock()
		}
	})
	d.wg.Wait()
}

// target is the last unsynced record of one file in one segment of a stream.
type target struct {
	file core.FileID
	loc  segment.Location
}

// work is a caller's targets on one stream.
type work struct {
	st      *stream
	targets []target
}

// pending reports whether the record at loc is in a pending window, or still in
// the ledger and no sync covers it. A record gone from both was covered and completed, so
// the answer does not depend on the segment's synced offset, which the worker
// forgets once a segment has no entries. The caller holds mu.
//
// ponytail: a linear scan, O(ledger) per target. Upgrade to a per-file index
// when a profile shows the scan.
func (st *stream) pending(loc segment.Location) bool {
	for i := len(st.ledger) - 1; i >= 0; i-- {
		if st.ledger[i].w.Loc == loc {
			return !st.covered(loc)
		}
	}
	// A record of a pending window is pending whatever the segment's synced
	// offset: no sync value satisfies a record of a failed segment (R14).
	return st.inWindowLocked(loc)
}

// inWindowLocked reports whether a pending window holds the record at loc. The
// caller holds mu.
func (st *stream) inWindowLocked(loc segment.Location) bool {
	for _, w := range st.windows {
		for i := range w.entries {
			if w.entries[i].w.Loc == loc {
				return true
			}
		}
	}
	return false
}

// Sync returns once every record of the named files appended before the call
// is durable on its stream, or the first failure, whichever comes first. A
// file with nothing unsynced needs nothing, so a call naming only such files
// returns at once with no sync. The error is a *core.SyncError when a sync
// covering a record failed, core.ErrClosed once the layer stopped, and
// ctx.Err() when ctx is done first; a cancelled caller changes no state and
// the sync it may have led runs on for the other waiters.
//
// Syncs are issued by the stream's worker, never by the caller's goroutine,
// so a cancelled caller is not stuck in the syscall.
func (d *Durability) Sync(ctx context.Context, ids ...core.FileID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-d.done:
		return core.ErrClosed
	default:
	}
	var ws []work
	for _, st := range d.snapshot() {
		if ts := st.targetsOf(ids); len(ts) > 0 {
			ws = append(ws, work{st: st, targets: ts})
		}
	}
	return d.await(ctx, ws, true)
}

// WaitDurable returns nil once the record at the location is durable, or
// core.ErrClosed once the layer stopped. It takes no context and never
// returns a *core.SyncError: a record of a segment whose sync failed keeps
// waiting until Stop, since following a re-append and the read-only wake are
// not part of this layer yet. It consults and clears no marks, none existing
// yet. A record with no sync running for it gets one, as a Sync would.
func (d *Durability) WaitDurable(at segment.Location) error {
	var ws []work
	for _, st := range d.snapshot() {
		st.mu.Lock()
		ok := st.pending(at)
		st.mu.Unlock()
		if ok {
			ws = append(ws, work{st: st, targets: []target{{loc: at}}})
		}
	}
	return d.await(context.Background(), ws, false)
}

func (d *Durability) snapshot() []*stream {
	d.mu.Lock()
	defer d.mu.Unlock()
	made := make([]*stream, 0, len(d.made))
	for _, st := range d.made {
		made = append(made, st)
	}
	return made
}

// targetsOf returns, for each id, the last record of that file in each
// segment of the ledger that no sync covers, and in each pending window. A sync covers a prefix of one
// segment, so covering a file's last record says nothing of its records in an
// older segment; each segment gets its own target and waits for its seal or
// fails through the failed map. Compaction removes only covered entries, so a
// target stays in the ledger until a sync covers it.
func (st *stream) targetsOf(ids []core.FileID) []target {
	st.mu.Lock()
	defer st.mu.Unlock()
	var ts []target
	consider := func(e *entry) {
		for _, id := range ids {
			if e.w.File != id {
				continue
			}
			dup := false
			for _, t := range ts {
				dup = dup || (t.file == id && t.loc.Segment == e.w.Loc.Segment)
			}
			if !dup {
				ts = append(ts, target{file: id, loc: e.w.Loc})
			}
		}
	}
	for i := len(st.ledger) - 1; i >= 0; i-- {
		if e := &st.ledger[i]; !e.done && !st.covered(e.w.Loc) {
			consider(e)
		}
	}
	// A pending window's records are targets whatever the segment's synced
	// offset; the segment is failed, so the waiter gets its SyncError.
	for j := len(st.windows) - 1; j >= 0; j-- {
		w := st.windows[j]
		for i := len(w.entries) - 1; i >= 0; i-- {
			consider(&w.entries[i])
		}
	}
	return ts
}

// await waits until no target of any stream is pending. failFast makes a
// target of a failed segment end the wait with its SyncError.
func (d *Durability) await(ctx context.Context, ws []work, failFast bool) error {
	for {
		// Taken before the checks, so a change after one is never missed, and
		// shared by every stream so a change on any of them wakes the caller.
		wait := d.changes()
		pending := false
		for _, w := range ws {
			p, err := w.st.check(w.targets, failFast)
			if err != nil {
				return err
			}
			pending = pending || p
		}
		if !pending {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		case <-d.done:
			return core.ErrClosed
		}
	}
}

// check looks at one stream's targets. It reports whether any is pending, and
// returns an error for a closed layer or, with failFast, a failed segment. It
// also leads a sync when the targets need one.
//
// decision: one sync per stream runs at a time and covers every waiter (group
// commit). The waiter that finds none in flight and none asked for leads: it sets
// want and the worker issues the sync. A sync per waiter would multiply
// fsyncs by the waiter count. Joining whatever sync is running would be wrong
// as well: a record whose slot completed after that sync began would be told
// it is durable, so every waiter re-checks its own target once a sync ends
// and a target still uncovered leads the next one. The worker, not the
// waiter's goroutine, issues the sync so a cancelled waiter is never stuck in
// the syscall.
//
// decision: a target in a pending window answers its *core.SyncError as soon
// as the window is taken, before the window's records are failed through the
// index. The answer is never nil whenever it comes (R10), and a pending window
// never reads durable, so answering early costs only the order of two
// events. Move it after the records are lost once the code that fails a window
// owns that step, so that the answer follows the loss.
func (st *stream) check(targets []target, failFast bool) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	pending, lead := false, false
	var failSeg core.SegmentID
	var failErr error
	for _, t := range targets {
		if !st.pending(t.loc) {
			continue
		}
		pending = true
		if err, bad := st.failed[t.loc.Segment]; bad {
			if failFast && failErr == nil {
				failSeg, failErr = t.loc.Segment, err
			}
			continue
		}
		// A target in an older segment is waited on and never leads: the
		// seal of its segment syncs it and reports through the sink. One in a
		// segment a sync found ended never leads either: the segment takes
		// no sync, and its end event is on its way.
		if _, ended := st.ending[t.loc.Segment]; !ended && st.haveLast && t.loc.Segment == st.lastSeg {
			lead = true
		}
	}
	if !pending {
		return false, nil
	}
	if st.closed {
		return false, core.ErrClosed
	}
	if failErr != nil {
		se := &core.SyncError{Segment: failSeg, Err: failErr}
		for _, t := range targets {
			if t.loc.Segment == failSeg && st.pending(t.loc) {
				se.Files = append(se.Files, t.file)
			}
		}
		return false, se
	}
	if lead && !st.syncing && !st.want {
		st.want = true
		st.wake()
	}
	return true, nil
}
