package durability

import (
	"context"
	"log/slog"
	"sync"
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
// file domain or another lock (D13), because both sinks run under the
// segment's append domain and must not wait on anything.
type stream struct {
	d  *Durability
	no uint32
	s  *segment.Stream

	mu sync.Mutex
	// ledger holds the entries in Track order.
	//
	// ponytail: one entry per unsynced record, bounded by what a stream
	// accepts within one sync bound (D4). Durable scans it linearly. Upgrade
	// to a per-file index when a profile shows the scan.
	ledger []entry
	// synced is the largest offset a sync reported, per segment.
	synced map[core.SegmentID]int64
	// failed holds the segments whose sync failed.
	failed map[core.SegmentID]error
	// lastSeg is the segment of the last record tracked.
	lastSeg  core.SegmentID
	haveLast bool
	closed   bool
	// want is set by the waiter that leads a sync and cleared by the worker
	// as it starts one; syncing is set while the worker is inside the
	// stream's Sync.
	want    bool
	syncing bool

	// changed is closed and replaced on every change a waiter cares about.
	changed chan struct{}
	// kick wakes the worker; capacity 1, sent without blocking.
	kick chan struct{}

	syncs, failures uint64
	buckets         []uint64
	sum             time.Duration

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
		changed: make(chan struct{}),
		kick:    make(chan struct{}, 1),
		buckets: make([]uint64, len(SyncDurationBounds)+1),
	}
}

// wake asks the worker to run a pass, without blocking.
func (st *stream) wake() {
	select {
	case st.kick <- struct{}{}:
	default:
	}
}

// notifyLocked wakes every waiter. The caller holds mu.
func (st *stream) notifyLocked() {
	close(st.changed)
	st.changed = make(chan struct{})
	st.d.notifyAll()
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
			st.mu.Unlock()
			st.wake()
		},
		Synced: func(seg core.SegmentID, through int64) {
			st.mu.Lock()
			if through > st.synced[seg] {
				st.synced[seg] = through
			}
			st.notifyLocked()
			st.mu.Unlock()
			st.wake()
		},
	}
}

// Start binds the streams, whose count must equal the number of sinks made,
// and launches one worker per stream. It is idempotent and a no-op after Stop.
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
	for _, st := range started {
		d.wg.Add(1)
		go d.run(st)
	}
}

// Durable reports whether the record at the location is durable: its segment
// has been synced through the record's end. A location with no ledger entry
// reports true, for the record was completed or never tracked.
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
	for _, st := range made {
		st.mu.Lock()
		p.Syncs += st.syncs
		p.SyncFailures += st.failures
		p.SyncDuration.Sum += st.sum
		for i, c := range st.buckets {
			p.SyncDuration.Counts[i] += c
		}
		st.mu.Unlock()
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
			st.notifyLocked()
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

// pending reports whether the record at loc is still in the ledger and no
// sync covers it. A record gone from the ledger was covered and completed, so
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
	return false
}

// Sync returns once every record of the named files appended before the call
// is durable on its stream, or the first failure, whichever comes first. A
// file with nothing unsynced needs nothing, so a call naming only such files
// returns at once with no sync. The error is a *core.SyncError when a sync
// covering a record failed, core.ErrClosed once the layer stopped, and
// ctx.Err() when ctx is done first; a cancelled caller changes no state and
// the sync it may have led runs on for the other waiters (R6).
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
// segment of the ledger that no sync covers. A sync covers a prefix of one
// segment, so covering a file's last record says nothing of its records in an
// older segment; each segment gets its own target and waits for its seal or
// fails through the failed map. Compaction removes only covered entries, so a
// target stays in the ledger until a sync covers it.
func (st *stream) targetsOf(ids []core.FileID) []target {
	st.mu.Lock()
	defer st.mu.Unlock()
	var ts []target
	for i := len(st.ledger) - 1; i >= 0; i-- {
		e := &st.ledger[i]
		if e.done || st.covered(e.w.Loc) {
			continue
		}
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
			ch, err := w.st.check(w.targets, failFast)
			if err != nil {
				return err
			}
			pending = pending || ch != nil
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

// check looks at one stream's targets. It returns a nil channel when none is
// pending, the channel to wait on otherwise, and an error for a closed layer
// or, with failFast, a failed segment. It also leads a sync when the targets
// need one.
//
// decision: one sync per stream runs at a time and covers every waiter (D3,
// R4). The waiter that finds none in flight and none asked for leads: it sets
// want and the worker issues the sync. A sync per waiter would multiply
// fsyncs by the waiter count. Joining whatever sync is running would be wrong
// as well: a record whose slot completed after that sync began would be told
// it is durable, so every waiter re-checks its own target once a sync ends
// and a target still uncovered leads the next one. The worker, not the
// waiter's goroutine, issues the sync so a cancelled waiter is never stuck in
// the syscall.
func (st *stream) check(targets []target, failFast bool) (chan struct{}, error) {
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
		// seal of its segment syncs it and reports through the sink.
		if st.haveLast && t.loc.Segment == st.lastSeg {
			lead = true
		}
	}
	if !pending {
		return nil, nil
	}
	if st.closed {
		return nil, core.ErrClosed
	}
	if failErr != nil {
		se := &core.SyncError{Segment: failSeg, Err: failErr}
		for _, t := range targets {
			if t.loc.Segment == failSeg && st.pending(t.loc) {
				se.Files = append(se.Files, t.file)
			}
		}
		return nil, se
	}
	if lead && !st.syncing && !st.want {
		st.want = true
		st.wake()
	}
	return st.changed, nil
}
