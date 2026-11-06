package durability

import (
	"context"
	"fmt"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// Loss is one extent the journal stopped holding without being asked.
type Loss struct {
	Share   core.ShareTag
	File    core.FileID
	Extent  core.Extent
	Version core.Version

	// Seq is the dropped record's sequence number.
	Seq core.Seq

	Reason core.LossReason

	// Offloaded is the extent's offloaded bit as it was.
	Offloaded bool

	// Synced is whether the dropped record was synced when dropped.
	Synced bool

	// AtOpen marks an open's own loss.
	AtOpen bool

	// Deferred appends no record: the copy that retires the dropped
	// record's segment writes it.
	Deferred bool
}

// toFormat maps a reason to the number its loss record carries. It panics on
// a reason that names none, which is a caller bug.
func toFormat(r core.LossReason) format.LossReason {
	switch r {
	case core.LossCorrupt:
		return 1
	case core.LossStale:
		return 2
	case core.LossFailedWindow:
		return 3
	case core.LossAcknowledged:
		return format.LossAcknowledged
	}
	panic(fmt.Sprintf("durability: loss reason %d names no reason", r))
}

// Lose records a loss: it appends and syncs the loss record, leaves the
// index's marker for a synced loss, raises the generation by the reason's
// rule, counts the loss, and publishes its event, in that order. The event
// is published last, so a reader that sees it is already owed the record.
// A Seq already named by an earlier Lose or Place does nothing and returns
// the zero event.
func (d *Durability) Lose(ctx context.Context, l Loss) (core.LossEvent, error) {
	if err := ctx.Err(); err != nil {
		return core.LossEvent{}, err
	}

	d.mu.Lock()
	if _, ok := d.named[l.Seq]; ok {
		d.mu.Unlock()
		return core.LossEvent{}, nil
	}
	d.named[l.Seq] = struct{}{}
	d.mu.Unlock()

	var firstErr error
	if !l.Deferred {
		firstErr = d.appendRecord(l)
		if d.afterAppend != nil {
			d.afterAppend()
		}
	}

	if l.Synced {
		if err := d.markLoss(l.File, l.Extent, l.Version); err != nil {
			d.log.Error("a synced loss left no marker", "file", l.File, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	// decision: the generation is raised by the reason and the offloaded bit
	// alone, never by what the caller asks: a corrupt or acknowledged extent
	// whose bytes are not offloaded is data a client may have been told was
	// stored and is now gone, and anything else can be fetched again. A
	// failed window raises it once, for the window, through RaiseWindow.
	if (l.Reason == core.LossCorrupt || l.Reason == core.LossAcknowledged) && !l.Offloaded {
		d.dir.raise()
	}

	d.mu.Lock()
	switch {
	case l.Reason == core.LossCorrupt && !l.Offloaded:
		d.corruptDataLoss++
	case l.Reason == core.LossCorrupt:
		d.corruptRepairable++
	case l.Reason == core.LossStale:
		d.stale++
	}
	d.mu.Unlock()

	if d.beforePublish != nil {
		d.beforePublish()
	}

	d.mu.Lock()
	ev := d.pushLocked(core.LossEvent{
		Share:     l.Share,
		File:      l.File,
		Extent:    l.Extent,
		Version:   l.Version,
		Reason:    l.Reason,
		Offloaded: l.Offloaded,
		AtOpen:    l.AtOpen,
		Deferred:  l.Deferred,
	})
	d.mu.Unlock()
	return ev, firstErr
}

// appendRecord appends the loss record to the file's stream and syncs it.
//
// ponytail: the sync is the stream's own, one per loss, outside the group
// commit. A loss is rare and never on the read path. Upgrade to the group
// commit leader when a burst of losses shows up in a profile.
func (d *Durability) appendRecord(l Loss) error {
	seq, err := d.counters.NextSeq()
	if err != nil {
		return err
	}
	h := format.RecordHeader{
		Kind:    format.KindLoss,
		File:    l.File,
		Extent:  l.Extent,
		Seq:     seq,
		Version: l.Version,
		Tag:     l.Share,
	}
	body := format.RecordBody{Loss: format.Loss{
		Dropped:   l.Seq,
		Synced:    l.Synced,
		Offloaded: l.Offloaded,
		Reason:    toFormat(l.Reason),
	}}
	s := d.streams.For(l.File)
	if _, err := s.Append(h, body); err != nil {
		return err
	}
	return s.Sync()
}
