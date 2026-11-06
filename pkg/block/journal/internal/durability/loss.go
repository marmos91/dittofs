package durability

import (
	"context"
	"errors"
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
		return format.LossCorrupt
	case core.LossStale:
		return format.LossStale
	case core.LossFailedWindow:
		return format.LossFailedSync
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

	// A reason that names none is a caller bug refused here, before the seq
	// is named, so every path refuses it alike.
	toFormat(l.Reason)

	d.mu.Lock()
	if _, ok := d.named[l.Seq]; ok {
		d.mu.Unlock()
		return core.LossEvent{}, nil
	}
	d.named[l.Seq] = struct{}{}
	d.mu.Unlock()

	var recErr error
	refused := false
	if !l.Deferred {
		if recErr = d.readOnly(); recErr != nil {
			refused = true
		} else {
			recErr = d.appendRecord(l)
		}
		if d.afterAppend != nil {
			d.afterAppend()
		}
	}

	firstErr := recErr
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

	// decision: a drop whose record could not be appended is never undone.
	// The journal stops holding what it cannot produce, and a loss only errs
	// toward fetching the extent again, so the caller's drop stands, the
	// event is published and the sequence number joins the directory's
	// failed list for the next open to name. A deferred loss is not on that
	// list: its record is owed to the copy that retires the segment.
	if recErr != nil {
		d.dir.addFailed(l.Seq)
		if refused || errors.Is(recErr, core.ErrNoSpace) {
			d.log.Debug("a loss record was not appended", "file", l.File, "seq", l.Seq, "err", recErr)
		} else {
			d.log.Error("a loss record was not appended", "file", l.File, "seq", l.Seq, "err", recErr)
		}
	}
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
	placed, err := s.Append(h, body)
	if err != nil {
		return err
	}
	if d.beforeSync != nil {
		d.beforeSync()
	}
	// The record's own segment is synced, not whatever the stream is on by
	// now: a roll between the two would otherwise let the sync cover a
	// successor and report the record durable.
	return s.SyncAt(placed.Loc)
}
