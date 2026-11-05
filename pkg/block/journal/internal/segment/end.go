package segment

import (
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// ErrStreamStopped is returned by every append after the stream stopped,
// wrapping the failure that stopped it; no spare is taken and nothing is
// reserved.
var ErrStreamStopped = errors.New("segment: the stream stopped: a second segment in a row failed, or the device is read-only")

// EndEvent is the one event a stream hands its consumer per ended segment.
type EndEvent struct {
	// Stream is the number of the stream whose segment ended.
	Stream uint32
	// Segment is the ended segment itself.
	Segment core.SegmentID
	// Cause names why the segment was ended.
	Cause EndCause
	// Failed is the offset of the lowest slot whose write failed; when no
	// write of the segment failed — a sync's failure, a requested end — it
	// is the stream's next free slot when the segment ended, the end of
	// every reservation the segment holds.
	Failed int64
	// SyncedThrough is the tracker's synced value when the event is handed
	// over: the window is the records at or above it.
	SyncedThrough int64
	// Err is the failure that ended the segment: the write's error, the
	// seal's, End's cause.
	Err error
	// Stopped says the stream stopped with this end and will take no
	// further spare.
	Stopped bool
}

// EndCause names why a segment was ended rather than sealed.
type EndCause uint8

const (
	// EndAppend is a record's write that failed or landed short.
	EndAppend EndCause = iota + 1
	// EndSeal is a seal that found or met a failure and ended its segment.
	EndSeal
	// EndRequested is an End the durability code asked for after a failed sync.
	EndRequested
)

// String names the causes, so a failing check reads which one it saw.
func (c EndCause) String() string {
	switch c {
	case EndAppend:
		return "append"
	case EndSeal:
		return "seal"
	case EndRequested:
		return "requested"
	}
	return fmt.Sprintf("EndCause(%d)", uint8(c))
}

// endRec is the record of a segment the stream ended, from the end itself
// until the end's event has been handed over: the one end a stream owns per
// segment, waiting for the reservations that segment held.
type endRec struct {
	id    core.SegmentID
	cause EndCause
	err   error
	// next is the stream's next free slot when the end ran: the end of every
	// reservation the segment holds, and the lower bound of the window the
	// durability code reads for it.
	next int64
}

// endLocked ends the stream's active segment, the one place a segment becomes
// ended in the stream's own state: sealState passes to sealEnded, the end is
// recorded for the notifier, and the reservations the segment still holds are
// named to it, so the last completion of the segment wakes whoever waits for
// the end to settle. The caller holds appendMu and the segment stands
// sealOpen or sealSealing; the second answer says whether this call owned the
// end, so its caller — and only it — ends the segment in the ledger and
// starts the notifier.
//
// The end is recorded only once: a segment a failure already ended keeps the
// end the first failure made, whatever a second path would say of it, so no
// later append, seal or sync of the segment starts a second notifier or lays
// a second end over the first.
//
// decision: the notifier is counted in the in-flight group Close waits for,
// so an end's event is handed over before the stream closes, while the end
// itself was made under this same lock — the Add therefore never starts
// against a counter a Wait is already running on, which the group forbids at
// zero. Withdrawn the day an end is ever made outside this lock, which must
// then carry its own in-flight accounting.
func (s *Stream) endLocked(cause EndCause, err error) (core.SegmentID, bool) {
	id := s.id
	s.sealState = sealEnded
	if s.ending == nil {
		s.ending = &endRec{id: id, cause: cause, err: err, next: s.next}
		s.inflight.Add(1)
		s.drained.Broadcast()
		return id, true
	}
	return id, false
}

// notifyEnd hands the ended segment's end to its consumer and clears the
// record the end left, so the stream may take its next segment past the ended
// one. It runs on its own goroutine, counted in the in-flight group, so Close
// waits for it before it releases the active segment's ref: a consumer must
// not call Close from it, for it would wait on itself.
//
// ponytail: nothing is handed to any consumer yet — the end only clears the
// record, and a nil consumer drops it. The end event and its wait on every
// slot the segment reserved, the tracker's synced value among the record's
// fields, and the roll's own `OnEnd` registration are the next step of the
// change this stub holds a place for; until then the record's fields are
// written by no one past the end that made them. Replace the whole stub rather
// than growing it beside a real hand-off.
func (s *Stream) notifyEnd() {
	defer s.inflight.Done()

	s.appendMu.Lock()
	s.ending = nil
	s.appendMu.Unlock()
	s.drained.Broadcast() // the roll waiting out the ended segment may pass
}

// OnEnd registers the consumer of the stream's end events. It must be called
// before the first append — before any reservation, roll or end — for a later
// call would hand a consumer an event whose segment's history it never asked
// to watch; a later call panics. A nil consumer (the default) drops the
// events.
func (s *Stream) OnEnd(fn func(EndEvent)) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.started {
		panic("segment: OnEnd after the stream started: the consumer is registered before the first append")
	}
	s.onEnd = fn
}

// End ends segment seg at the durability code's request after a failed sync,
// with cause as its reason.
func (s *Stream) End(seg core.SegmentID, cause error) error {
	if cause == nil {
		panic("segment: End with no cause: a requested end names the failure that asked for it")
	}
	s.appendMu.Lock()
	if s.closed {
		s.appendMu.Unlock()
		return ErrStreamClosed
	}
	s.appendMu.Unlock()
	return fmt.Errorf("%w: the end is not implemented in this commit", ErrWrongState)
}
