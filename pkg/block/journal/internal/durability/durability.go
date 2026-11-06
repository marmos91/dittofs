package durability

import (
	"log/slog"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Config is what a Durability is built from.
type Config struct {
	// Dir is the journal's directory, the key of its per-process state.
	Dir string

	// Streams are the append streams a loss record is written through.
	Streams *segment.Streams

	// Counters issue the loss record's sequence number.
	Counters *index.Counters

	// MarkLoss leaves the index's loss marker for a synced loss. It is
	// required.
	MarkLoss func(core.FileID, core.Extent, core.Version) error

	// Log receives what Lose could not do. Nil keeps the Durability silent.
	Log *slog.Logger
}

// Durability is the journal's loss state: the loss ring, the counters and the
// directory's generation.
//
// decision: mu is a leaf mutex over the ring, the counters and the named set.
// It is never held across an Append, a Sync or a MarkLoss, which take locks
// of other domains, so it adds no edge to the journal's lock order.
type Durability struct {
	dir      *dirState
	streams  *segment.Streams
	counters *index.Counters
	markLoss func(core.FileID, core.Extent, core.Version) error
	log      *slog.Logger

	// readOnly reports why the journal takes no append, or nil. It is nil
	// until the read-only state installs one.
	readOnly func() error

	// afterAppend and beforePublish are test hooks, nil in production.
	afterAppend   func()
	beforePublish func()
	beforeSync    func()

	mu                sync.Mutex
	ring              [core.LossRingSize]core.LossEvent
	head              int // index of the oldest event
	count             int
	nextEvent         uint64
	corruptDataLoss   uint64
	corruptRepairable uint64
	stale             uint64

	// pieces holds every (dropped seq, extent) a Lose or a Place has taken,
	// so one piece of a dropped record is reported once, while another piece
	// of the same record, which a reconcile can drop alone, is reported on
	// its own.
	//
	// ponytail: it grows by one entry per piece lost for the life of the
	// Durability and is never pruned, so it follows the number of loss
	// records the journal holds, not the ring. Upgrade to dropping an entry
	// once no copy of its dropped record remains to be reported again, when
	// a journal with a long loss history shows up in a heap profile.
	pieces map[piece]struct{}

	// named holds the dropped seqs a loss record already names, written by
	// a Lose, owed by a deferred Lose or found by a Place. A record names
	// the whole dropped record, so a later piece of that seq appends none; a
	// superseded one excludes its record only over its own extent and is
	// never held here. It is as unbounded as pieces, and shares its upgrade.
	named map[core.Seq]struct{}
}

// piece is one extent of one dropped record.
type piece struct {
	seq core.Seq
	ext core.Extent
}

// New returns a Durability over cfg. It panics on a nil Streams, Counters or
// MarkLoss, which are caller bugs, and returns the error of resolving Dir.
func New(cfg Config) (*Durability, error) {
	if cfg.Streams == nil || cfg.Counters == nil || cfg.MarkLoss == nil {
		panic("durability: a Durability needs its streams, counters and loss marker")
	}
	st, err := stateFor(cfg.Dir)
	if err != nil {
		return nil, err
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Durability{
		dir:      st,
		streams:  cfg.Streams,
		counters: cfg.Counters,
		markLoss: cfg.MarkLoss,
		log:      log,
		readOnly: func() error { return nil },
		pieces:   map[piece]struct{}{},
		named:    map[core.Seq]struct{}{},
	}, nil
}

// Generation returns the directory's loss generation.
func (d *Durability) Generation() uint64 { return d.dir.load() }

// RaiseWindow raises the generation once for a failed sync window, however
// many records the window held, and returns the new generation.
func (d *Durability) RaiseWindow() uint64 { return d.dir.raise() }

// Failed returns the sequence numbers, sorted, whose loss record could not be
// appended, in the directory's life in this process. A later Durability on
// the directory sees them too.
func (d *Durability) Failed() []core.Seq { return d.dir.failedSeqs() }

// Part is the loss share of the journal's statistics.
type Part struct {
	// Losses are the events in the ring, oldest first.
	Losses []core.LossEvent

	LossGeneration uint64

	CorruptDataLoss   uint64
	CorruptRepairable uint64
	StaleExtents      uint64
}

// Stats returns a copy of the loss state.
//
// The generation is read after the ring is copied: Lose raises it before it
// publishes, so every event in the copy has already raised it and a load
// made afterwards includes that raise.
func (d *Durability) Stats() Part {
	d.mu.Lock()
	losses := make([]core.LossEvent, d.count)
	for i := range losses {
		losses[i] = d.ring[(d.head+i)%core.LossRingSize]
	}
	p := Part{
		Losses:            losses,
		CorruptDataLoss:   d.corruptDataLoss,
		CorruptRepairable: d.corruptRepairable,
		StaleExtents:      d.stale,
	}
	d.mu.Unlock()

	p.LossGeneration = d.dir.load()
	return p
}

// pushLocked publishes ev with the next event number, overwriting the oldest
// when the ring is full. The caller holds mu.
func (d *Durability) pushLocked(ev core.LossEvent) core.LossEvent {
	d.nextEvent++
	ev.SeqNum = d.nextEvent
	if d.count < core.LossRingSize {
		d.ring[(d.head+d.count)%core.LossRingSize] = ev
		d.count++
		return ev
	}
	d.ring[d.head] = ev
	d.head = (d.head + 1) % core.LossRingSize
	return ev
}

// Place publishes ev, a loss record found at open, for the dropped record
// seq. It appends nothing, raises nothing and counts nothing: the counters
// are of drops this run made, and the record is already on disk. A piece
// (seq and ev.Extent) already taken by a Lose or a Place publishes nothing,
// so one piece is never reported twice, while another piece of the same
// record is.
func (d *Durability) Place(ev core.LossEvent, dropped core.Seq) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := piece{dropped, ev.Extent}
	if _, ok := d.pieces[k]; ok {
		return
	}
	d.pieces[k] = struct{}{}
	if ev.Reason != core.LossSuperseded {
		d.named[dropped] = struct{}{}
	}
	ev.AtOpen = true
	d.pushLocked(ev)
}
