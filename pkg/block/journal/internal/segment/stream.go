package segment

import (
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sync"
	"syscall"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// ErrSegmentFull is returned by Append when the record does not fit even an
// empty segment: nothing was reserved and nothing was written. It is the one
// refusal the roll cannot answer, for a segment that cannot hold the record
// cannot be rolled out of either; every other record that does not fit rolls.
var ErrSegmentFull = errors.New("segment: no room for the record before the seal's reserve")

// ErrStreamClosed is returned by Append after Close: the stream publishes no
// further location, so every caller stops writing.
var ErrStreamClosed = errors.New("segment: stream closed")

// ErrBadRecord is returned by Append for a record the format could not have
// encoded: an unknown kind, or a write or fill whose payload length differs
// from the extent the header names. The refusal happens before the stream
// takes its lock, so one such call leaves nothing behind — no reserved
// position, no slot, no bytes on the way to storage. Encode errors that
// survive it still panic, as the format's own panics do: they name caller
// bugs the checks here cannot spell.
var ErrBadRecord = errors.New("segment: the record cannot be encoded")

// sealRoom is the bytes a segment's tail must keep for the seal that may
// close it once it holds n records: the marker, one footer entry per record,
// and the trailer.
//
// "A stream seals a segment while what remains still holds the seal marker,
// a footer entry for every record in it, and the trailer." — RFC 28 §2.5
func sealRoom(n int64) int64 {
	return format.SealMarkerSize + n*format.CatalogEntrySize + format.TrailerSize
}

// Location is where Append placed one record: its segment, its own offset
// and its padded size.
type Location struct {
	Segment core.SegmentID
	// Off is the record's own offset, a multiple of 8, at or above the
	// segment header's size.
	Off int64
	// Len is format.RecordHeader.Size(): the header, the body and the padding.
	Len int64
}

// Placed is what one Append wrote.
type Placed struct {
	Loc Location
	// N is the payload bytes placed, for a write and a fill. Below
	// len(body.Payload) means the record was split, and the caller continues
	// with the rest in the next segment. Kinds with no payload place 0.
	N int64
}

// tslot is one in-flight record slot of the tracker: the range the record was
// reserved at, and whether its write has landed, and well.
type tslot struct {
	off  int64
	end  int64
	done bool
	ok   bool
}

// tracker holds the ordered-completion prefix a record's synced-through
// value is drawn from. A slot is reserved when a record's position is
// reserved and completed when its write has landed; the prefix is the end of
// the longest run of slots, from the oldest, whose writes landed well.
// syncStart names it, syncDone records what a Sync that returned therefore
// covered.
//
// decision: a plain "last synced offset" is wrong here, because a later slot
// can complete and be synced while an earlier one is still in flight, and a
// synced-through value that outran an in-flight slot would claim durability
// for bytes no fsync had reached. "The synced-through offset a record carries
// is the offset in this segment up to which a sync had completed when the
// record was written." — RFC 28 §2.3. "A stream's synced-through offset never
// advances past a slot whose write failed or has not completed." — §2.2. So
// the prefix stops at the first slot that has not completed, and a slot whose
// write failed freezes it: nothing after the failure moves it. A sync that
// failed freezes the recorded value, for no later sync of the segment may
// claim what the failure left (RFC 1 §5.3).
//
// The value is read at the reservation, not when the record's write lands —
// the safe side of RFC 28 §2.3's "when the record was written": a sync can
// only have covered more by the time the write runs, so the value recorded
// at the reservation is at or below the one at the write, never above.
//
// decision: every field is guarded by the owning stream's append mutex, the
// segment append domain. Nothing else may touch the tracker.
//
// ponytail: the in-flight slots sit in one slice whose head is always index 0,
// grown by doubling only when full, so reserves allocate nothing in steady
// state — but every pop shifts the slice down, so a completion costs O(live)
// copies. Upgrade to a true ring with a moving head index, unrolled on
// growth, if the in-flight depth of a real journal makes the shift show in a
// profile.
type tracker struct {
	ring []tslot // the in-flight slots, the oldest at index 0
	live int     // slots held

	next uint64 // the next slot number to hand out

	// headSlot is the slot number that sits at the slice's start: a slot's
	// place is its distance from headSlot, since reservations are numbered
	// in order and pops advance the head.
	headSlot uint64

	completed  int64 // the end of the well-completed prefix
	synced     int64 // the highest completed value a returned Sync covered
	freeze     bool  // a slot's write failed: the prefix never moves again
	syncFailed bool  // a sync of the segment failed: no later sync may claim anything
}

// The failed slot's own offset needs no field of its own: slots are
// contiguous, so when the head fails, completed — the end of the prefix
// before it — is exactly the failed slot's offset, and it never moves again.

// reset starts the tracker for a segment whose first record slot is first:
// the header write is not a tracker slot — it completes before any record is
// reserved — so the prefix begins at it while no sync has covered anything.
func (t *tracker) reset(first int64) {
	t.ring = t.ring[:0]
	t.live, t.next, t.headSlot = 0, 0, 0
	t.completed = first
	t.synced = 0
	t.freeze = false
	t.syncFailed = false
}

// reserve takes the slot for the record at [off, end) and returns its
// number. It allocates only when the slice is full, growing it by doubling,
// so after a warm-up nothing on this path touches the allocator.
//
// Past a freeze a slot is handed out but not held: no completion after a
// failed write can be counted, because the prefix is frozen below it, so the
// slot's bytes are tracked by the position alone.
func (t *tracker) reserve(off, end int64) uint64 {
	slot := t.next
	t.next++
	if t.freeze {
		return slot
	}
	if t.live == len(t.ring) {
		if len(t.ring) == 0 {
			t.ring = make([]tslot, 8)
		} else {
			grown := make([]tslot, 2*len(t.ring))
			copy(grown, t.ring[:t.live])
			t.ring = grown
		}
	}
	t.ring[t.live] = tslot{off: off, end: end}
	t.live++
	return slot
}

// complete marks the slot's write done, well when ok. The oldest in-flight
// slot is then popped while it is done and well, advancing the prefix to its
// end; the head landing badly freezes the prefix instead — for good, since
// no sync after a failed write can cover the bytes it left.
//
// A slot reserve handed out past a freeze is not held; its completion is
// ignored.
func (t *tracker) complete(slot uint64, ok bool) {
	if t.freeze {
		return
	}
	// The slot's distance from the head: reservations are numbered in order,
	// so the live slots are exactly [headSlot, headSlot+live).
	idx := int64(slot) - int64(t.headSlot)
	if idx < 0 || idx >= int64(t.live) {
		return
	}
	t.ring[idx].done = true
	t.ring[idx].ok = ok
	for t.live > 0 && t.ring[0].done && t.ring[0].ok {
		t.completed = t.ring[0].end
		copy(t.ring, t.ring[1:t.live])
		t.ring[t.live-1] = tslot{}
		t.live--
		t.headSlot++
	}
	if t.live > 0 && t.ring[0].done && !t.ring[0].ok {
		// The head slot failed: the prefix is complete up to the failed
		// slot's own offset and never moves again, which is the whole
		// freeze — completed already sits exactly there, so no sync
		// recorded from syncStart can name more, and the reservations
		// after the failure are handed out but not held.
		t.freeze = true
	}
}

// syncStart returns the offset a Sync that starts now will cover once it
// returns: the well-completed prefix it found.
func (t *tracker) syncStart() int64 {
	return t.completed
}

// syncDone records that a returned Sync covered up to through. The freeze needs
// no clamp of its own: through is what syncStart named, and a frozen prefix
// never names an offset past the failed slot.
//
// decision: a sync that failed owns its window from here on, so a later
// successful sync of the same segment may not claim what the failure left: RFC
// 1 §5.3's failed window is exactly the case where "a second sync can succeed
// without having written them", and a value that moved past the failure would
// name bytes durable that no fsync made so. The window's records are the
// durability layer's to reissue; the segment they sit in belongs to the
// failure handling that ends it.
func (t *tracker) syncDone(through int64) {
	if t.syncFailed {
		return
	}
	if through > t.synced {
		t.synced = through
	}
}

// syncFail freezes the value a failed sync left behind: no later sync of the
// segment claims anything. The value that stood when the failure returned is
// the last truth this segment's records carry.
func (t *tracker) syncFail() {
	t.syncFailed = true
}

// Stream is one append stream of a journal: it takes segments from the spare
// pool, appends records to one active segment at a time, seals the segment
// once a record does not fit or a seal is asked for, and rolls — takes its
// next segment from the pool with no I/O, naming the segment it sealed as
// the new one's predecessor — so the caller's next append lands there. It
// reads back positioned records through the descriptor cache.
type Stream struct {
	no       uint32
	identity core.JournalID
	set      *Set
	pool     *Pool
	cache    *Descriptors

	// decision: every field below is guarded by appendMu, the segment append
	// domain of RFC 1 §8.2 — next, count, entries, the tracker, the active
	// segment and its ref, the roll's single-flight gate, the closed flag,
	// and the seal state with its pending-drain. The lock is held for a
	// record's reservation and its completion only — never across I/O, and
	// nothing else is acquired under it, so no set call and no storage call
	// sit inside it.
	appendMu sync.Mutex
	next     int64 // the first free record slot of the active segment
	count    int64 // records reserved in the active segment
	// entries are the catalog entries of the segment's records, kept in
	// completion order for the seal, which sorts them by offset.
	entries  []format.CatalogEntry
	tracker  tracker
	id       core.SegmentID // 0 until a roll installs its segment
	ref      Ref
	rolling  chan struct{} // armed while one caller runs a roll for all
	closed   bool
	inflight sync.WaitGroup // the appends, syncs and seals Close waits for, added to under the lock

	// sealState names where the stream's seal stands. The zero value is
	// sealOpen: the stream is appending.
	sealState sealState
	// pending is the appends and the stream syncs of the active segment
	// between their reservation and their final exit, so a seal can wait
	// for every one that started before it or alongside it (R85, RFC 1
	// §5.3): an append, so the marker lands past every record and every
	// entry is charged, and a sync, so a failure of its window reaches the
	// seal's ended-check before the seal writes anything.
	pending int
	// drained wakes the seal when pending reaches zero, and wakes the
	// stream syncs that parked while the seal ran when it settles.
	drained *sync.Cond
}

// sealState is where a stream's seal stands: open, closing, closed for good,
// or ended instead of sealed.
type sealState uint8

const (
	// sealOpen is a stream whose segment takes appends: the zero value, so
	// a fresh stream needs no seal initialisation of its own.
	sealOpen sealState = iota
	// sealSealing is a seal that has taken the segment but not yet
	// finished: its appends are refused, its syncs pass.
	sealSealing
	// sealSealed is a stream whose segment is sealed: the roll takes the
	// next segment at the next Append.
	sealSealed
	// sealEnded is a stream whose segment was ended rather than sealed: a
	// write or a sync of it failed. No seal and no append runs on it.
	sealEnded
)

// NewStream returns the append stream no of the journal identity over set,
// taking its segments from the spare pool and reading and writing them through
// cache. The cache must name segments as format.SegmentName does; the segment
// cap is the set's own, so the stream and the ledger can never disagree about
// it. It panics on a nil dependency, on a pool that keeps another ledger's
// spares — its rolls would draw on storage the stream's set does not own — and
// on a set whose cap is above MaxUint32, because a record's own offset is a
// 32-bit column.
func NewStream(no uint32, identity core.JournalID, set *Set, pool *Pool, cache *Descriptors) *Stream {
	if set == nil {
		panic("segment: a stream needs its ledger")
	}
	if pool == nil {
		panic("segment: a stream needs its spare pool")
	}
	if pool.m.set != set {
		panic("segment: the stream's pool keeps another ledger's spares")
	}
	if cache == nil {
		panic("segment: a stream needs its descriptor cache")
	}
	if set.cap > math.MaxUint32 {
		panic("segment: the segment cap is above MaxUint32, so an offset cannot be carried in a record header")
	}
	s := &Stream{no: no, identity: identity, set: set, pool: pool, cache: cache}
	s.drained = sync.NewCond(&s.appendMu)
	return s
}

// Roll says which spares the roll an append may need can take.
type Roll struct {
	// Hold is the reservation the write runs under: its roll takes one of
	// the hold's spares ahead of every unreserved write (R80). Nil for an
	// unreserved write.
	Hold *Hold
	// Repack marks a repack copy: its roll takes a repack-reserve spare,
	// never a content one (R23).
	Repack bool
}

// ErrBadRoll is returned by AppendRoll for roll options that do not fit the
// record it was handed: a reservation and a repack copy at once, or a
// reservation on a kind with no payload, which is not a content write a hold
// covers. The refusal happens before the stream takes its lock, so nothing
// is reserved and nothing written.
var ErrBadRoll = errors.New("segment: the roll options do not fit the record")

// takeSpare takes the spare the roll needs, and classifies the refusal when
// it finds none. The roll options name the class: a repack copy's takes a
// repack-reserve spare, never a content one (R23); a reserved roll's takes one
// of its hold's spares ahead of every unreserved write (R80); and an
// unreserved content roll's takes a content spare by the unreserved take, so
// holds keep theirs. A header-only record takes a content spare first the
// same way, and only when no content spare is ready does it take a headroom
// spare (D12): the classes never lend to one another, so the content miss is
// answered by the headroom's readiness alone, never by the reserve's.
//
// The refusal is classified here, the one place the roll knows its final
// answer. A refusal is an errors.As *core.NoSpaceError whose Limit is
// LimitSpare — one drawn from the content, headroom, reserve or hold path
// alike. A stopped pool's core.ErrClosed passes through unchanged and
// uncounted, so the stream's answer names the close, not a space limit.
//
// When the pool reports making a spare failed with ENOSPC, the refusal is
// answered with the platform's own ENOSPC, wrapped so errors.Is holds for
// syscall.ENOSPC and not for core.ErrNoSpace: the journal's limits are the
// caller's to drain and a full device is the operator's, and the two must not
// be answered alike (R21). It is not counted. Otherwise the refusal naming
// `spare` is counted once here and returned unchanged (R20, R22).
//
// It runs with no lock held and makes no I/O: the takes go to the pool and
// the hold, which reach the ledger alone, so a roll costs only the take
// (R15, R17).
func (s *Stream) takeSpare(hasPayload bool, r Roll) (core.SegmentID, error) {
	id, err := s.drawSpare(hasPayload, r)
	if err == nil {
		return id, nil
	}

	// A header-only roll that found neither class ready is refused naming
	// the headroom, not the spare: the record goes on drawing on the
	// allocated headroom, and the spare pool never held what it asked for,
	// so the refusal is neither a spare's nor counted (R23).
	if !hasPayload && !r.Repack && r.Hold == nil {
		var ns *core.NoSpaceError
		if errors.As(err, &ns) && ns.Limit == core.LimitSpare {
			return 0, &core.NoSpaceError{Limit: core.LimitHeadroom, Missing: 1, Unit: core.UnitSegments}
		}
	}

	// A stopped pool's core.ErrClosed passes through unchanged and uncounted,
	// so the stream's answer names the close, not a space limit.
	if errors.Is(err, core.ErrClosed) {
		return 0, err
	}
	// The full device, answered as itself rather than as a limit.
	//
	// ponytail: no full-size retired segment is consulted, because nothing
	// hands one over yet. Ceiling: a roll answers ENOSPC while a retirement
	// could have supplied a spare. Upgrade to asking the retirement queue
	// once one exists — the fact it hands over is what clears this answer.
	var spare *core.NoSpaceError
	if errors.As(err, &spare) && spare.Limit == core.LimitSpare && s.pool.DeviceFull() {
		return 0, fmt.Errorf("segment: no spare is ready and making one failed for lack of device space: %w", syscall.ENOSPC)
	}

	// The spare refusal, counted once here.
	if errors.As(err, &spare) && spare.Limit == core.LimitSpare {
		s.pool.countSpareRefusal()
	}
	return 0, err
}

// drawSpare draws the spare the roll options name: a repack copy from the
// reserve, a reserved roll from its hold, and everything else from the
// content class, where a header-only record falls back to the headroom once
// the content miss names `spare`.
//
// A header-only roll that takes a headroom spare makes it an ordinary
// segment: the stream finishes it with whatever records follow, content
// included, and the worker refills the headroom class to its size, so the
// headroom's ready bytes dip until the refill lands.
//
// ponytail: content that follows lands in that segment, spending headroom
// space until the refill. The upgrade is keeping the rest of that segment
// for header-only records, once the headroom's draws show the cost.
func (s *Stream) drawSpare(hasPayload bool, r Roll) (core.SegmentID, error) {
	switch {
	case r.Repack:
		return s.pool.Take(Reserve)
	case r.Hold != nil:
		return r.Hold.Take()
	case hasPayload:
		return s.pool.Take(Content)
	}
	id, err := s.pool.Take(Content)
	if err != nil {
		var ns *core.NoSpaceError
		if errors.As(err, &ns) && ns.Limit == core.LimitSpare {
			if hid, herr := s.pool.Take(Headroom); herr == nil {
				return hid, nil
			}
		}
	}
	return id, err
}

// endTaken unwinds a take that failed past Take: the member segment is ended
// in the ledger — held at its cap until a retirement unlinks it — and the
// ref, when one was taken, is released. The failing error comes back first in
// the join, so its class still reads.
func (s *Stream) endTaken(id core.SegmentID, ref Ref, err error) error {
	if unerr := s.set.End(id); unerr != nil {
		err = errors.Join(err, unerr)
	}
	if ref != (Ref{}) {
		ref.Release()
	}
	return err
}

// Append writes one record and returns where it landed and how many payload
// bytes it placed, with the roll options of the plain, unreserved roll. It is
// AppendRoll for every caller that has no reservation to run under and no
// class of its own.
func (s *Stream) Append(h format.RecordHeader, body format.RecordBody) (Placed, error) {
	return s.AppendRoll(h, body, Roll{})
}

// AppendRoll writes one record and returns where it landed and how many
// payload bytes it placed, taking the spare its roll may need as r names. The
// reservation — filling the header's segment and synced-through, sizing the
// record against the seal's reserve, advancing the byte position and taking
// the tracker's slot — happens under the append lock with no I/O; the
// encoding and the write run outside it; the completion — the tracker's slot
// and the record's catalog entry — runs back under it. The location is
// returned only once the bytes have been written, so a caller may publish it
// as its last step (R5).
//
// When the record does not fit what remains of the active segment — or the
// segment is sealed, or the stream has none — the append arms the roll's gate
// and one caller runs the roll for all; the others park on the gate with no
// lock. The roll takes the spare r names, seals the full segment, and writes
// the new segment's header naming the sealed one as predecessor; the record
// is then reserved in the new segment. A write or fill whose whole record
// still does not fit is split at the cap: this call returns the largest piece
// that fits and N below the payload's length, and the caller's next call
// rolls. A record that fits nowhere at all — not even one payload byte, or a
// kind with no payload whose record is larger than an empty segment's room —
// answers ErrSegmentFull with nothing reserved and nothing written.
func (s *Stream) AppendRoll(h format.RecordHeader, body format.RecordBody, r Roll) (Placed, error) {
	// The caller bugs the format would panic on mid-append are answered
	// here, before the lock: a panic with appendMu held would strand every
	// later appender on a lock nobody releases, and a panic after the
	// reservation would strand a slot no bytes will ever complete. Only a
	// write and a fill carry a payload, and for them the length must name
	// exactly the bytes given.
	if h.Kind < format.KindWrite || h.Kind > format.KindForget {
		return Placed{}, fmt.Errorf("%w: kind %d is not one this format encodes", ErrBadRecord, uint8(h.Kind))
	}
	if h.Kind == format.KindWrite || h.Kind == format.KindFill {
		if int64(len(body.Payload)) != h.Extent.Len {
			return Placed{}, fmt.Errorf("%w: %s carries %d payload bytes against a length of %d", ErrBadRecord, h.Kind, len(body.Payload), h.Extent.Len)
		}
	} else if len(body.Payload) != 0 {
		return Placed{}, fmt.Errorf("%w: kind %d carries a payload, want none", ErrBadRecord, uint8(h.Kind))
	}
	if h.Extent.Off < 0 {
		// The format panics on this one too, so it lands here for the same
		// reason the unknown kind does.
		return Placed{}, fmt.Errorf("%w: file offset %d is below 0", ErrBadRecord, h.Extent.Off)
	}

	// The roll options that do not fit the record are refused here too,
	// before the lock, the same way the record's own bugs are: a hold
	// covers content writes only, so it never rides a header-only record,
	// and a repack copy runs under no hold.
	hasPayload := h.Kind == format.KindWrite || h.Kind == format.KindFill
	if r.Hold != nil && r.Repack {
		return Placed{}, fmt.Errorf("%w: a roll cannot be a reserved repack copy", ErrBadRoll)
	}
	if r.Hold != nil && !hasPayload {
		return Placed{}, fmt.Errorf("%w: a hold covers content writes, not a kind with no payload", ErrBadRoll)
	}

	s.appendMu.Lock()
	if s.closed {
		s.appendMu.Unlock()
		return Placed{}, ErrStreamClosed
	}
	// Counted before anything is reserved and under the same lock Close sets
	// closed under, so every Add happens before Close's Wait: the group is
	// never reused while a Wait runs. The roll a rejected append runs below
	// is inside the count too, so Close waits a roll out before it releases
	// the active segment's ref — the roll may be sealing that very segment.
	s.inflight.Add(1)
	defer s.inflight.Done()
	for {
		// appendParkHook is a test hook at the loop's top, past the head
		// checks and with the lock dropped: the one place an append that
		// has already entered the loop can be held while another
		// goroutine takes the lock — the state a take-gate park leaves
		// the append in, staged without the scheduler the real gate
		// hands the wake to. It lives in this file rather than a test's
		// because it sits in the window the reservations run in, which
		// no test file can reach. nil outside the one check that parks
		// there; the lock is never held across it.
		if appendParkHook != nil {
			s.appendMu.Unlock()
			appendParkHook()
			s.appendMu.Lock()
			continue
		}
		if s.closed {
			s.appendMu.Unlock()
			return Placed{}, ErrStreamClosed
		}
		// The seal states refuse before anything is reserved (R1, R85): once
		// a seal has taken the segment — while it drains the reservations
		// below it, and while its own steps run — and once it has completed
		// or the segment has ended, no append is taken. The refusal reserves
		// nothing, writes nothing and charges nothing, so the marker a seal
		// chose and the bytes its footer names cannot be placed under.
		// sealOpen, the stream that is still appending, alone falls through.
		//
		// The check runs at every loop top, beside the closed check it
		// mirrors: an append parked on the first take's gate resumes at the
		// loop's top, and a seal that took the segment while it parked —
		// past its drain, its marker read — must refuse it there, or its
		// record lands exactly at the marker the seal's own write is
		// driving at, unlisted by the footer and overwriting or
		// overwritten by it.
		// decision: the seal states are not re-checked here, the way an
		// append resumed from the take gate once re-checked one. The roll
		// replaced the take, and the roll's own gate now parks the same
		// caller the take gate parked — with no lock and no in-flight
		// reservation — so a seal that took the segment while this append
		// parked is waited by the sealRunning branch the loop's next turn
		// runs, and the roll the append arms after it names the segment the
		// settled seal left. The re-check would refuse what the turn after
		// the wait places. Withdraw it if a roll is ever armed with the
		// segment left open, which the roll's own first step would have to
		// account for again.

		// A seal someone else runs — the idle seal, or one the caller asked
		// for — parks this append on the drain's cond until the seal
		// settles, for a reservation racing the seal could land under the
		// marker it places. This appender holds nothing in-flight yet, so
		// the seal never waits for it; the append rolls on past a settled
		// seal and refuses nothing for an ended segment either.
		if s.sealState == sealSealing {
			s.drained.Wait()
			continue
		}

		// The roll an append may need, drawn before anything of the record
		// is reserved: the stream holds no segment, or the one it holds is
		// sealed or ended, or the record has no piece that fits what
		// remains. One caller runs it for all — the gate below — and the
		// others park on it, holding no lock. A refused roll costs its
		// runner alone — the active segment stays exactly as it was, open
		// and full, or sealed — and each parked caller retries on its own
		// turn: with a spare nothing is served by eight refusals where one
		// would carry the same answer.
		roll := s.id == 0 || s.sealState == sealSealed || s.sealState == sealEnded
		size, placed := h.Size(), int64(len(body.Payload))
		if !roll {
			// The seal's reserve bounds the room (R11): a record fits when
			// what remains after it still holds the marker, a footer entry
			// for every record in the segment — this one included — and
			// the trailer.
			//
			// "A write that does not fit in what remains of the current
			// segment is split at the cap into records, one per segment,
			// each carrying its own file offset and the write's version."
			// — RFC 28 §2.2. The whole record is one piece when it fits:
			// no empty piece, no early split. A piece that does not fit
			// whole is sized by the closed form of Size — the header, the
			// kind's own body bytes and the payload that fits, padded to
			// the slot's grid — never by a search, so a refusal costs the
			// same as a fit.
			if size+sealRoom(s.count+1) > s.set.cap-s.next {
				// The bytes of body a piece takes besides its payload,
				// named by the format's own body-size table: a write its
				// modification time, and nothing for a fill.
				overhead := int64(format.RecordHeaderSize) + format.RecordHeader{Kind: h.Kind}.BodySize()
				fit := ((s.set.cap - s.next - sealRoom(s.count+1)) &^ 7) - overhead
				if hasPayload && fit >= 1 {
					// The whole record does not fit, so the payload
					// cannot either: the record's own padded size is a
					// multiple of 8 no smaller than the room the fit was
					// drawn from, so a fit that reached the payload would
					// mean the record fitted and the check above
					// misjudged it.
					if fit >= placed {
						panic("segment: the split found the whole record fitting after the reserve check refused it")
					}
					h.Extent.Len = fit
					size, placed = h.Size(), fit
				} else if s.count > 0 {
					// Not even one payload byte places, or the kind carries
					// no payload and does not fit: the record has no piece
					// here, and the segment holds records already, so the
					// roll takes the next one for it.
					roll = true
				} else {
					// A segment that holds no record yet cannot hold this
					// one at all, and no roll would serve it — an empty
					// segment is the largest room a stream hands out:
					// the record is larger than the cap allows, and the
					// refusal reserves nothing, so no roll ever loops on
					// it.
					s.appendMu.Unlock()
					return Placed{}, ErrSegmentFull
				}
			}
		}

		if roll {
			if s.rolling != nil {
				// Another caller's roll runs: park on its gate with no
				// lock and retry when it settles, so a failed roll costs
				// its runner alone, and every parked caller finds either
				// the new segment or the same refusal under the same lock.
				gate := s.rolling
				s.appendMu.Unlock()
				<-gate
				s.appendMu.Lock()
				continue
			}
			s.rolling = make(chan struct{})
			s.appendMu.Unlock()
			rerr := s.roll(hasPayload, r)
			s.appendMu.Lock()
			s.rollSettle()
			if rerr != nil {
				// A failed roll left the stream exactly where it was — no
				// segment, or the full open one, or the sealed one — so the
				// refusal names its own cause and the next append rolls
				// again.
				s.appendMu.Unlock()
				return Placed{}, rerr
			}
			continue
		}

		// The reservation. The header carries the segment, its own offset
		// and the synced-through the tracker holds (R10).
		h.Segment = s.id
		h.SyncedThrough = s.tracker.synced
		h.Offset = s.next

		slot := s.tracker.reserve(h.Offset, h.Offset+size)
		s.next += size
		s.count++
		// pending names this append to a seal that may start while it is
		// in flight, and leaves at the append's final exit — after its
		// record is charged, or after its failed write returned — so a
		// drained segment holds every entry and every share-charge its
		// appends made (R85). The one short lock section past the
		// completion's costs nothing the completion did not already pay.
		// Every exit from here on is this append's final one: the loop
		// never re-enters once a reservation is made, so the deferred
		// leave runs exactly once per reservation.
		s.enterDrain()
		defer s.leaveDrain()
		id, ref, off := s.id, s.ref, h.Offset
		s.appendMu.Unlock()

		// The encode and the write, outside the lock. A split piece keeps
		// the write's file offset and names the bytes it stages in its
		// length.
		if placed < int64(len(body.Payload)) {
			body.Payload = body.Payload[:placed]
		}
		// ponytail: a buffer per append is allocated, whole-record sized.
		// Upgrade to a pool when Append's allocations show in a profile.
		buf := make([]byte, 0, size)
		buf = format.AppendRecord(buf, s.identity, h, body)
		n, werr := ref.WriteAt(buf, off)
		if werr == nil && n != len(buf) {
			werr = io.ErrShortWrite
		}

		s.appendMu.Lock()
		s.tracker.complete(slot, werr == nil)
		if werr == nil {
			s.entries = append(s.entries, format.CatalogEntry{
				File:         h.File,
				Extent:       h.Extent,
				RecordOffset: off,
				Seq:          h.Seq,
				Version:      h.Version,
				Kind:         h.Kind,
				Tag:          h.Tag,
			})
		}
		s.appendMu.Unlock()

		if werr != nil {
			// ponytail: a failed write does not yet end its segment, and
			// later appends go on landing in it; the slot it reserved stays
			// as it is — never rewritten, never zeroed (R87) — and the
			// tracker's frozen prefix keeps later records' synced-through at
			// or below the failed slot's offset. RFC 28 §2.2 ends the
			// segment and moves the stream to a new one, which is the
			// durability layer's call of the roll.
			return Placed{}, werr
		}

		// The bytes are the share's once written (R55). The lock order puts
		// capacity before segment append (RFC 1 §8.3), so this runs with no
		// lock of the stream's held — never under appendMu.
		if err := s.set.AddRecord(id, h.Tag, size); err != nil {
			return Placed{}, err
		}
		return Placed{Loc: Location{Segment: id, Off: off, Len: size}, N: placed}, nil
	}
}

// roll moves the stream onto its next segment: it takes the spare the roll
// options name, seals the full segment the stream leaves — a full seal, for
// the segment is closed because the next record did not fit — and writes the
// new segment's 48-byte header naming the sealed segment as its predecessor,
// none after a segment a failure ended (R8, R24, R33). A roll the pool cannot
// serve is refused at once, before any seam call, so the active segment is
// untouched and the refusal costs no I/O (R15, R20).
//
// It is run by the one caller that armed the roll's gate, with no lock held,
// for its steps take the ledger's seal and storage's write and sync. A
// failure at any step past the take unwinds it through endTaken — the taken
// segment is ended in the ledger at its cap, for a retirement to unlink, and
// the ref released when one was held — and the stream stays on its old
// segment, so the next append rolls again. The first segment is a roll from
// nothing, with predecessor 0.
func (s *Stream) roll(hasPayload bool, r Roll) error {
	// The take, before any seam call: a refusal names the spare pool and
	// nothing moved.
	id, err := s.takeSpare(hasPayload, r)
	if err != nil {
		return err
	}

	// The predecessor: the segment the stream leaves, sealed here when it
	// was open, for the successor may be taken only once the seal's marker,
	// footer and trailer are durable (R24). A seal someone else already
	// finished, or one whose truncation failed (R34), leaves the segment
	// Sealed and names it; a segment a failed seal's syncs ended names none
	// (R33), and a stream that held no segment names none.
	s.appendMu.Lock()
	old, oldRef := s.id, s.ref
	state := s.sealState
	s.appendMu.Unlock()

	var pred core.SegmentID

	if state == sealSealed {
		// A seal someone else finished — explicit, idle, or one whose
		// truncation failed and left the segment sealed at its cap (R34) —
		// has already made the segment durable; the successor names it.
		pred = old
	} else if state == sealOpen {
		_, _ = s.Seal(false)
		// The seal the roll ran may have refused for a seal someone else
		// was already running — the racing state, which settles on the
		// drain's cond — so the predecessor is read only once no seal is in
		// flight, never under one: a successor taken while a seal still
		// parks the segment would name a segment whose footer the seal was
		// still writing.
		s.appendMu.Lock()
		for s.sealState == sealSealing {
			s.drained.Wait()
		}
		// The seal's own word decides the predecessor: a seal that sealed
		// the segment names it, and one that ended it — a failed sync of
		// its records, of its marker, or a write that failed — names none
		// (R33), and so does one that found the stream closed under it,
		// where the successor is unwanted and unwound below.
		pred = 0
		if s.sealState == sealSealed {
			pred = old
		}
		closed := s.closed
		s.appendMu.Unlock()
		if closed {
			return s.endTaken(id, Ref{}, ErrStreamClosed)
		}
	}
	// state == sealEnded, or a stream that held no segment, names none: a
	// segment a failure ended leaves no successor of its own, and the
	// first segment of a stream has nothing before it.

	// The new segment, opened and named: the header is written once, at
	// offset 0, and a failure past the take ends the taken segment in the
	// ledger and releases the ref, leaving the stream on its old segment.
	ref, err := s.cache.Acquire(id)
	if err != nil {
		return s.endTaken(id, Ref{}, err)
	}
	buf := format.AppendSegmentHeader(nil, format.SegmentHeader{
		Identity:    s.identity,
		ID:          id,
		Predecessor: pred,
		Stream:      s.no,
	})
	if _, err := ref.WriteAt(buf, 0); err != nil {
		return s.endTaken(id, ref, err)
	}

	// Install under the lock. A close that raced the roll unwinds the taken
	// spare itself, for the stream publishes no further location. The
	// successor was named only after its predecessor's seal was durable, so
	// the stream takes it never before R24 allows.
	s.appendMu.Lock()
	if s.closed {
		s.appendMu.Unlock()
		return s.endTaken(id, ref, ErrStreamClosed)
	}
	old, oldRef = s.id, s.ref
	s.id, s.ref = id, ref
	s.next = format.SegmentHeaderSize
	s.count = 0
	s.entries = nil
	s.tracker.reset(format.SegmentHeaderSize)
	s.sealState = sealOpen
	s.appendMu.Unlock()

	// The old ref is released only after its seal's syncs, outside the lock:
	// a descriptor that wrote is never closed before a sync covered it.
	if oldRef != (Ref{}) {
		oldRef.Release()
	}
	return nil
}

// enterDrain names the calling operation — an append between its
// reservation and its final exit, a stream sync between its window's read
// and its recording — to the seal's drain. The caller holds appendMu.
func (s *Stream) enterDrain() {
	s.pending++
}

// leaveDrain ends what enterDrain named: the drain count drops, and the
// seal waiting on it is woken when it reaches zero. It runs under the
// caller's deferred return, taking the lock itself because the exits it
// leaves from run with none held.
func (s *Stream) leaveDrain() {
	s.appendMu.Lock()
	s.pending--
	if s.pending == 0 {
		s.drained.Broadcast()
	}
	s.appendMu.Unlock()
}

// appendParkHook is the test hook at Append's loop top; see the comment at
// its call site. A check that parks there must nil it before it returns,
// or every later append of the process parks with it.
var appendParkHook func()

// rollSettle settles the roll whose gate the caller armed: the gate is
// lifted and the parked appends woken. The caller holds appendMu; the
// waiters hold no lock while they park, so the channel's close under it is
// what wakes them.
func (s *Stream) rollSettle() {
	if s.rolling != nil {
		close(s.rolling)
		s.rolling = nil
	}
}

// Sync makes the writes of the active segment durable and advances the
// synced-through value the next records carry: the completed prefix is read
// under the lock, the descriptor fsynced outside it, and what the fsync
// therefore covered is recorded on the way back. The first Sync covers the
// segment header (R8), which completes before any reservation exists.
//
// It is the primitive the durability layer's Sync drives. Sync is an
// in-flight operation of the stream's own, counted as an append is, so Close
// waits it out before releasing the active segment's ref — an fsync running
// on a descriptor nobody holds would race the cache's reclaim for the very
// error the sync exists to surface.
//
// decision: Sync answers ErrStreamClosed after Close rather than a soothing
// nil, because Close fsyncs nothing of the appends that landed before it, so
// a nil here would read as "everything acknowledged is durable" to a caller
// that synced after closing. Withdraw it if the durability layer ever needs a
// benign sync past the close; until then the refusal is the true answer.
func (s *Stream) Sync() error {
	s.appendMu.Lock()
	if s.closed {
		s.appendMu.Unlock()
		return ErrStreamClosed
	}
	if s.id == 0 {
		s.appendMu.Unlock()
		return nil // no segment is active: nothing to make durable
	}
	// A sealing segment is parked on the drain's cond until the seal
	// settles, for the platform reports a writeback error once per file and
	// an fsync racing the seal's own could consume the error the seal's
	// steps or their checks needed. A sealed segment's every record is
	// durable by the seal's first step, so the Sync then answers nil
	// without an fsync of its own; an ended segment takes no sync that
	// could claim its bytes durable. sealOpen alone keeps the ordinary
	// path, and a state past the wait is always one of the three the
	// switch answers — the seal never returns the stream to sealOpen.
	for s.sealState == sealSealing {
		s.drained.Wait()
	}
	switch s.sealState {
	case sealSealed:
		s.appendMu.Unlock()
		return nil
	case sealEnded:
		s.appendMu.Unlock()
		return ErrSegmentEnded
	}
	// Counted under the lock Close sets closed under, like Append's, and
	// counted in the seal's drain: a sync in flight when the seal starts,
	// or one arriving before the drain has emptied, is part of what the
	// seal waits out, so its failure reaches the seal's ended-check before
	// it writes (RFC 1 §5.3).
	s.inflight.Add(1)
	defer s.inflight.Done()
	s.enterDrain()
	defer s.leaveDrain()

	through := s.tracker.syncStart()
	ref := s.ref
	s.appendMu.Unlock()

	if err := ref.Sync(); err != nil {
		// The failure belongs to the durability layer's failed window: the
		// caller owns the records the window holds, and no later sync of
		// this segment claims what the failure left — the value stands where
		// the failure found it.
		s.appendMu.Lock()
		s.tracker.syncFail()
		s.appendMu.Unlock()
		return err
	}
	s.appendMu.Lock()
	s.tracker.syncDone(through)
	s.appendMu.Unlock()
	return nil
}

// Seal seals the stream's active segment: early truncates the free tail and
// returns it, full keeps the cap and writes the trailer at its last 32 bytes.
// A seal closes the segment to new reservations, waits for every one
// reserved before it — so the marker lands just past the last completed
// record, never over an in-flight slot (R85) — makes the records durable,
// writes the marker, the footer and the trailer, makes them durable, and, for
// an early seal, truncates and makes that durable (R26).
//
// After a seal the stream stays on its segment and the next append rolls:
// the seal closed the segment, so the roll takes the pool's next spare and
// names the sealed one as the new segment's predecessor.
//
// A second seal is refused with ErrWrongState or ErrSegmentEnded and writes
// nothing: a sealed segment keeps the footer it already carries, and an ended
// one takes no seal (R32, R33).
func (s *Stream) Seal(early bool) (SealResult, error) {
	s.appendMu.Lock()
	if s.closed {
		s.appendMu.Unlock()
		return SealResult{}, ErrStreamClosed
	}
	if s.id == 0 {
		s.appendMu.Unlock()
		return SealResult{}, ErrNoSegment
	}
	switch s.sealState {
	case sealSealing, sealSealed:
		s.appendMu.Unlock()
		return SealResult{}, fmt.Errorf("%w: the stream's seal already ran", ErrWrongState)
	case sealEnded:
		s.appendMu.Unlock()
		return SealResult{}, ErrSegmentEnded
	case sealOpen:
		// The one state a seal closes; the count below takes it.
	}
	// Counted with the appends and the syncs, so Close waits a seal out
	// before it releases the segment's ref.
	s.inflight.Add(1)
	defer s.inflight.Done()
	s.sealState = sealSealing

	// The drain: every append reserved below the seal has left, and every
	// sync that started before it or alongside it too, so every record the
	// segment holds is complete and charged and every failure its writes
	// and syncs left is already recorded in the tracker. The wait holds no
	// lock, so the appends and syncs it waits for can finish. A Sync that
	// arrives while the seal is sealing parks here on the same cond rather
	// than racing its fsync against the seal's.
	for s.pending > 0 {
		s.drained.Wait()
	}

	// A write or a sync of this segment already failed — its own, or one
	// of the syncs the drain waited out: the segment is ended, not sealed,
	// and the seal writes nothing. The reason names which failure ended
	// it, the way WriteSeal's own ended paths do.
	if s.tracker.freeze || s.tracker.syncFailed {
		reason := "a sync of the segment failed"
		if s.tracker.freeze {
			reason = "a write of the segment failed"
		}
		s.sealState = sealEnded
		id := s.id
		s.appendMu.Unlock()
		s.drained.Broadcast() // the Syncs parked on the seal leave with the end
		if err := s.set.End(id); err != nil {
			// The end of a segment the ledger may not know is the
			// stream's own and the seal's refusal above names it; a
			// ledger that cannot end it carries the failure here.
			return SealResult{}, errors.Join(ErrSegmentEnded, err)
		}
		return SealResult{}, fmt.Errorf("%w: %s", ErrSegmentEnded, reason)
	}

	// The marker sits at the stream's next free slot: every record below it
	// has completed. The entries are cloned and sorted, for the footer's
	// reader takes them ascending by offset while the stream kept them in
	// completion order.
	marker := s.next
	entries := slices.Clone(s.entries)
	sortSealEntries(entries)
	id, ref := s.id, s.ref
	s.appendMu.Unlock()

	res, err := WriteSeal(s.set, ref, s.identity, id, marker, entries, early)

	s.appendMu.Lock()
	if err != nil {
		if errors.Is(err, ErrLedgerDisagrees) {
			// The marker, footer and trailer are durable and the file is
			// cut where the seal named it; only a ledger call refused.
			// The disk and the caller's contract say sealed, so the
			// stream reports exactly that — the durable facts in res
			// reach the caller beside the refusal — never "ended",
			// which the durable bytes contradict.
			s.sealState = sealSealed
			s.drained.Broadcast()
			s.appendMu.Unlock()
			return res, err
		}
		// The seal failed: the segment takes no further append and no
		// second seal. WriteSeal has ended it in the ledger for the
		// failures it owns — a failed record sync, a failed write, a
		// failed step-2 sync — so nothing lands in it again, and its
		// successor names none (R33). A refusal the writer does not own,
		// ErrBadCatalog or a ledger's, is a bug in this stream's own
		// catalog: the segment is ended here too, so nothing appends
		// past a refused seal either.
		s.sealState = sealEnded
		s.drained.Broadcast() // the Syncs parked on the seal leave with the end
		if !errors.Is(err, ErrSegmentEnded) {
			s.appendMu.Unlock()
			if unerr := s.set.End(id); unerr != nil {
				err = errors.Join(err, unerr)
			}
			return SealResult{}, fmt.Errorf("%w: %w", ErrSegmentEnded, err)
		}
		s.appendMu.Unlock()
		return SealResult{}, err
	}
	s.sealState = sealSealed
	s.drained.Broadcast() // the Syncs parked on the seal leave with the seal
	s.appendMu.Unlock()
	return res, nil
}

// ReadAt reads len(p) bytes at offset off of segment seg through the
// descriptor cache: the reference is taken, the read made and the reference
// dropped, with nothing else held across the read.
//
// decision: ReadAt makes one attempt and passes ErrSegmentGone and I/O
// errors through unchanged, because the bounded retry of an I/O error, and
// the unread and suspect states around it, belong to the caller — which
// knows whether the record is already unread, in which case each access
// makes one attempt, never three. Withdraw this if a second caller ever
// needs the same retry loop the consumer runs.
func (s *Stream) ReadAt(seg core.SegmentID, p []byte, off int64) (int, error) {
	ref, err := s.cache.Acquire(seg)
	if err != nil {
		return 0, err
	}
	n, rerr := ref.ReadAt(p, off)
	ref.Release()
	return n, rerr
}

// Close closes the stream: further appends answer ErrStreamClosed, and the
// in-flight ones — a roll counted with them, for it runs inside an append and
// its seal is a Close of the segment in its own right, and an in-flight Sync,
// whose fsync holds the descriptor the released ref must not drop — are
// waited for, so on return no append is still writing, no roll is still
// sealing and no sync is still running. The active segment's ref is released,
// so the cache's Shutdown is never blocked by a ref the stream forgot. A
// second Close is a no-op.
func (s *Stream) Close() error {
	s.appendMu.Lock()
	if s.closed {
		s.appendMu.Unlock()
		return nil
	}
	s.closed = true
	ref := s.ref
	s.ref = Ref{}
	s.appendMu.Unlock()

	// The wait runs outside the lock: an append completing right now takes
	// it, and the take's runner settles its gate under it.
	s.inflight.Wait()

	if ref != (Ref{}) {
		ref.Release()
	}
	return nil
}
