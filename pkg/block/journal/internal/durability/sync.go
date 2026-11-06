package durability

import (
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
	streams []*stream // by stream number, set by Start
	started bool
	stopped bool

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
	}
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

	// changed is closed and replaced on every change a waiter cares about.
	changed chan struct{}
	// kick wakes the worker; capacity 1, sent without blocking.
	kick chan struct{}

	syncs, failures uint64
	buckets         []uint64
	sum             time.Duration

	// scratch is the worker's own, the indices it completes in one pass.
	scratch []int
}

func newStream(no uint32) *stream {
	return &stream{
		no:      no,
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
	st := newStream(no)
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
	d.streams = make([]*stream, streams.Len())
	for i := range d.streams {
		st, ok := d.made[uint32(i)]
		if !ok {
			panic("durability: no sink was made for a stream")
		}
		st.s = streams.Stream(i)
		d.streams[i] = st
	}
	d.started = true
	for _, st := range d.streams {
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
	d.mu.Lock()
	made := make([]*stream, 0, len(d.made))
	for _, st := range d.made {
		made = append(made, st)
	}
	d.mu.Unlock()
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

// Stats returns the counters kept at each sync, without walking the ledger.
func (d *Durability) Stats() Part {
	d.mu.Lock()
	made := make([]*stream, 0, len(d.made))
	for _, st := range d.made {
		made = append(made, st)
	}
	d.mu.Unlock()

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
		made := make([]*stream, 0, len(d.made))
		for _, st := range d.made {
			made = append(made, st)
		}
		d.mu.Unlock()
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
