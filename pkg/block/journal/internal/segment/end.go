package segment

import (
	"errors"
	"fmt"
	"syscall"

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
	// Stopped says the stream had stopped by the time the event was handed
	// over, by this end or by a failure that met the segment after it was
	// ended, and will take no further spare.
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
	// stopped says the stream stopped with this end: it will take no
	// further spare.
	stopped bool
}

// stopLocked marks the stream stopped with the failure that stopped it. The
// caller holds appendMu. A second stop keeps the first failure's word: the
// stream is already stopped, and the failure that first said so is the one
// the journal-wide read-only state of the durability code will read.
func (s *Stream) stopLocked(err error) {
	if s.stopped == nil {
		s.stopped = fmt.Errorf("%w: %w", ErrStreamStopped, err)
	}
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
	// The refusal that ends a segment also decides whether the stream
	// survives it: an EROFS says the device is read-only, so every later
	// journal call would fail too, and a failure in a segment taken to
	// leave an ended one — before any sync of it has succeeded — is the
	// second segment in a row (RFC 1 §5.4). Either way the stream stops:
	// its later appends fail at once, reserving nothing and taking no
	// spare, and the event this end hands over says so.
	if errors.Is(err, syscall.EROFS) || (s.leftFailed && !s.syncedOnce) {
		s.stopLocked(err)
	}
	// The stop travels with the end's record, so the one event the ended
	// segment hands its consumer says whether the stream stopped with it.
	if s.ending == nil {
		s.ending = &endRec{id: id, cause: cause, err: err, next: s.next, stopped: s.stopped != nil}
		s.inflight.Add(1)
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
// The hand-over waits for every reservation the segment held — those
// reserved before the failed one and those reserved after it alike — and for
// every Sync in flight on it, on the stream's own cond, which drops appendMu
// while it waits. A Sync that took its prefix before the failure and returns
// after it has covered records the window must not name, so the window is
// read only once the drain count is empty. The wait ends: an ended segment
// takes no new append or Sync (a Sync refuses before it enters the drain),
// and the roll that could start one waits on this very record.
//
// decision: the wait covers the later slots, a superset of the ones before
// the failed slot, because RFC 28 §4.2's zero-slot row and epic 6's window
// require the record of the append that completed after the failure to be in
// the window the durability code reads: an event handed over while a later
// slot still writes would leave an acknowledged record out of the window, and
// the window is the record's only word that its bytes were not made durable.
// Withdrawn only by a window that reads the segment's own ledger, which
// would then carry the slots itself.
//
// decision: the named completion is the append's own completion section, not
// R88's Track registration, for R88's durability sink is not part of this
// stack yet: "registered its record" here means the completion ran the
// tracker's complete under appendMu. The day R88 lands, its Track call goes
// before that completion section, and this wait then implies the registration
// too — the order is R88's to keep, not this wait's.
//
// The roll cannot move the segment the wait reads — s.id, and the tracker's
// synced value with it — for it installs only once the end's record is
// cleared, and the record is what this hand-over clears.
func (s *Stream) notifyEnd() {
	defer s.inflight.Done()

	// notifyParkHook is a test hook at the notifier's top, before its wait:
	// the one place the notifier can be held between the ended segment's last
	// completion and its event's snapshot, so the roll's install wait can be
	// proven to hold past the wake the completion broadcasts. nil outside the
	// check that parks there; the lock is never held across it.
	if notifyParkHook != nil {
		notifyParkHook()
	}
	s.appendMu.Lock()
	for s.apending > 0 || s.pending > 0 {
		s.drained.Wait()
	}
	rec := s.ending
	ev := EndEvent{
		Stream:  s.no,
		Segment: rec.id,
		Cause:   rec.cause,
		Err:     rec.err,
		// The stop the end itself made, or one a straggler's EROFS made
		// while the event waited for it: the event says whether the stream
		// has stopped by the time it is handed over.
		Stopped: rec.stopped || s.stopped != nil,
		// No write of the segment failed — a sync's failure, a requested
		// end — so the end is the end of every reservation the segment
		// held: the record's own next.
		Failed:        rec.next,
		SyncedThrough: s.tracker.synced,
	}
	if s.failedAt >= 0 {
		ev.Failed = s.failedAt
	}
	s.ending = nil
	fn := s.onEnd
	s.appendMu.Unlock()
	s.drained.Broadcast() // the roll waiting out the ended segment may pass

	// The consumer runs with no lock held, on this goroutine, so it may
	// read the ledger and its own tables freely — and Close waits for it
	// before it releases the segment's ref, so the segment it names is
	// still in the set while it runs.
	if fn != nil {
		fn(ev)
	}
}

// notifyParkHook is the test hook at notifyEnd's top; see its call site. A
// check that parks there must nil it before it releases, or every later
// notifier of the process parks with it.
var notifyParkHook func()

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
// with cause as its reason. The request names the segment it failed on, so
// the stream's own routing decides what the request means: its active segment
// takes the end, a segment the stream already left needs nothing, and a
// segment the stream never owned answers the ledger's own word.
//
// The request returns as soon as the end is made — the event's hand-over is
// the notifier's own wait on the reservations the segment still holds, and
// the request is not held hostage to a write it never owned.
func (s *Stream) End(seg core.SegmentID, cause error) error {
	// A requested end with no cause names no failure, so the caller's own
	// sequencing is broken: the durability code asks for an end on the
	// heels of a failed sync it owns, and a nil cause would hand its
	// consumer an event whose failure names nothing.
	if cause == nil {
		panic("segment: End with no cause: a requested end names the failure that asked for it")
	}
	s.appendMu.Lock()
	if s.closed {
		s.appendMu.Unlock()
		return ErrStreamClosed
	}
	// A seal still running owns the segment: wait it out on the drain's
	// cond, the way an append does, for the seal may be ending the segment
	// itself — its own failure paths — and a second end over the seal's
	// decision would race the seal's own word for the segment.
	for s.sealState == sealSealing {
		s.drained.Wait()
	}
	// A close that landed while the request waited the seal out names the
	// call's whole answer now — a segment a close raced its seal to is ended
	// with no event anyway (the close's own exemption), and a request past a
	// close owns nothing to end.
	if s.closed {
		s.appendMu.Unlock()
		return ErrStreamClosed
	}
	// A stream that holds no segment has none to end: the request names a
	// segment that does not exist, and ending it would hand the consumer an
	// event for segment 0.
	if s.id == 0 && seg == 0 {
		s.appendMu.Unlock()
		return ErrNoSegment
	}
	if s.id == seg {
		switch s.sealState {
		case sealEnded:
			// The segment is already ended — a failed write or sync got
			// there first, or an earlier request did: one event per
			// segment, and this request owns none. An EROFS it names still
			// stops the stream: the device is read-only whichever failure
			// ended the segment first.
			if errors.Is(cause, syscall.EROFS) {
				s.stopLocked(cause)
			}
			s.appendMu.Unlock()
			return nil
		case sealSealed:
			s.appendMu.Unlock()
			return fmt.Errorf("%w: the segment is sealed", ErrWrongState)
		}
		endID, notify := s.endLocked(EndRequested, cause)
		// The notifier starts whatever the ledger said, for the end already
		// named it to the in-flight group Close waits on: a refusal here is
		// a ledger and a stream that disagree, and it must not turn into a
		// hang of every later roll on the end's record, which nothing else
		// would ever clear.
		s.appendMu.Unlock()
		if unerr := s.set.End(endID); unerr != nil {
			// The end of a segment the ledger may not know is the
			// stream's own and this refusal names it; a ledger that
			// cannot end it carries its failure to the caller, after
			// the segment's stream state has taken the end, which is
			// true: the segment takes no further append either way.
			if notify {
				go s.notifyEnd()
			}
			return unerr
		}
		if notify {
			go s.notifyEnd()
		}
		return nil
	}
	s.appendMu.Unlock()

	// A segment that is not the active one: the stream left it — sealed,
	// ended — or never held it. The ledger's word decides, and passes
	// through as itself: an id out of the set reaches its caller as
	// ErrNotInSet, for the request named a segment this journal does not
	// own, which the durability code's own bookkeeping must answer for.
	state, serr := s.set.State(seg)
	if serr != nil {
		return serr
	}
	switch state {
	case Ended:
		// The same read-only rule as an ended active segment: the request's
		// EROFS stops the stream although the segment's end is not its own.
		if errors.Is(cause, syscall.EROFS) {
			s.appendMu.Lock()
			s.stopLocked(cause)
			s.appendMu.Unlock()
		}
		return nil
	case Sealed:
		return fmt.Errorf("%w: the segment is sealed", ErrWrongState)
	}
	return fmt.Errorf("%w: the segment stands %d in the set, not active on its stream", ErrWrongState, uint8(state))
}
