package durability

import (
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// piece is one extent of one dropped record.
type piece struct {
	seq core.Seq
	ext core.Extent
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
	d.lmu.Lock()
	defer d.lmu.Unlock()
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
