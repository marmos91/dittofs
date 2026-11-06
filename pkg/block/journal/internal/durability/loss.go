package durability

import (
	"context"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// Loss is one extent the journal stopped holding without being asked.
//
// Extent is the piece lost, not necessarily the whole dropped record: a
// reconcile can drop one piece of a record and keep the rest under the same
// Seq. A caller hands Lose each piece it drops, and each piece is reported
// on its own and appends a loss record of its own over its own extent: that
// extent is what outranks the older records below it after a crash.
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

	// Raised says the caller already raised the generation for this loss, as
	// the read path does before it hands the loss on. Lose then counts and
	// publishes it but raises nothing.
	Raised bool

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
	case core.LossSuperseded:
		return format.LossSuperseded
	case core.LossAcknowledged:
		return format.LossAcknowledged
	}
	panic(fmt.Sprintf("durability: loss reason %d names no reason", r))
}

// Lose records a loss: it appends and syncs the loss record, leaves the
// index's marker for a synced loss, raises the generation by the reason's
// rule, counts the loss, and publishes its event, in that order. The event
// is published last, so a reader that sees it is already owed the record.
// A piece (Seq and Extent) already taken by an earlier Lose or Place does
// nothing and returns the zero event. Every other piece, of a Seq already
// reported or not, goes through all of it, so no event precedes its own
// record.
func (d *Durability) Lose(ctx context.Context, l Loss) (core.LossEvent, error) {
	if err := ctx.Err(); err != nil {
		return core.LossEvent{}, err
	}

	// A reason that names none is a caller bug refused here, before the seq
	// is named, so every path refuses it alike.
	toFormat(l.Reason)

	d.mu.Lock()
	k := piece{l.Seq, l.Extent}
	if _, ok := d.pieces[k]; ok {
		d.mu.Unlock()
		return core.LossEvent{}, nil
	}
	d.pieces[k] = struct{}{}
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
	// decision: a superseded loss leaves no marker even when synced. It
	// reports no write lost, so Since does not yield it and it needs no
	// Settle; a marker would make a later DropOlder inherit one that nothing
	// settles. Withdraw this if a superseded loss ever has to be settled.
	if l.Synced && l.Reason != core.LossSuperseded {
		if err := d.markLoss(l.File, l.Extent, l.Version); err != nil {
			d.log.Error("a synced loss left no marker", "file", l.File, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	// decision: the generation is raised by the reason and the offloaded bit
	// alone, never by what the caller asks, bar a raise it already made and
	// says so with Raised: a corrupt or acknowledged extent
	// whose bytes are not offloaded is data a client may have been told was
	// stored and is now gone, and anything else can be fetched again. A
	// failed window raises it once, for the window, through RaiseWindow.
	if (l.Reason == core.LossCorrupt || l.Reason == core.LossAcknowledged) && !l.Offloaded && !l.Raised {
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
// ponytail: the sync is the segment's own, one per loss, outside the group
// commit, and a failure here can consume the segment's write error before the
// window's owner sees it, so the segment's no-further-append rule after a
// failed sync rests on this error alone. Upgrade by moving the sync to the
// group commit's Durable and WaitDurable, with the failed-window handling,
// once those land.
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
