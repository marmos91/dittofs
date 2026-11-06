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

	// afterAppend and beforePublish are test hooks, nil in production.
	afterAppend   func()
	beforePublish func()

	mu                sync.Mutex
	ring              [core.LossRingSize]core.LossEvent
	head              int // index of the oldest event
	count             int
	nextEvent         uint64
	corruptDataLoss   uint64
	corruptRepairable uint64
	stale             uint64
	named             map[core.Seq]struct{}
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
		named:    map[core.Seq]struct{}{},
	}, nil
}

// Generation returns the directory's loss generation.
func (d *Durability) Generation() uint64 { return d.dir.load() }

// RaiseWindow raises the generation once for a failed sync window, however
// many records the window held, and returns the new generation.
func (d *Durability) RaiseWindow() uint64 { return d.dir.raise() }

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
func (d *Durability) Stats() Part {
	gen := d.dir.load()

	d.mu.Lock()
	defer d.mu.Unlock()
	losses := make([]core.LossEvent, d.count)
	for i := range losses {
		losses[i] = d.ring[(d.head+i)%core.LossRingSize]
	}
	return Part{
		Losses:            losses,
		LossGeneration:    gen,
		CorruptDataLoss:   d.corruptDataLoss,
		CorruptRepairable: d.corruptRepairable,
		StaleExtents:      d.stale,
	}
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
// are of drops this run made, and the record is already on disk.
func (d *Durability) Place(ev core.LossEvent, dropped core.Seq) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.named[dropped] = struct{}{}
	ev.AtOpen = true
	d.pushLocked(ev)
}
