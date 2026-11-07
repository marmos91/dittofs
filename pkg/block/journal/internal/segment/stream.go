package segment

import (
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

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
var ErrStreamClosed = fmt.Errorf("segment: stream closed: %w", core.ErrClosed)

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
//
// It reports whether the sync counts: false when an earlier sync failed, for
// then the sync covers nothing the tracker may claim and nothing is reported
// to the durability sink.
func (t *tracker) syncDone(through int64) bool {
	if t.syncFailed {
		return false
	}
	if through > t.synced {
		t.synced = through
	}
	return true
}

// syncFail freezes the value a failed sync left behind: no later sync of the
// segment claims anything. The value that stood when the failure returned is
// the last truth this segment's records carry.
func (t *tracker) syncFail() {
	t.syncFailed = true
}

// clockBox holds the clock the idle sealer installs on a stream. idleClock
// below is an atomic pointer, so the interface value it stores needs a box
// to point at: the box is the unit the pointer's loads and stores trade in,
// published whole by one Store.
type clockBox struct{ c core.Clock }

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
	// sink is the durability sink the stream reports to. It is written only
	// by NewStream and never changes, so it needs no lock domain.
	sink Sink

	// decision: every field below is guarded by appendMu, the segment append
	// domain of RFC 1 §8.2 — next, count, entries, the tracker, the active
	// segment and its ref, the roll's single-flight gate, the closed flag,
	// the last append's idle stamp, the seal state with its pending-drain,
	// the in-flight reservations the end's event waits on, the failed
	// write's own offset, and the end the stream holds for its consumer
	// with the consumer that reads it. The lock is held for a record's
	// reservation and its completion only — never across I/O, and nothing
	// else is acquired under it, so no set call and no storage call sit
	// inside it.
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

	// readClosed is set by Close and read by ReadAt without a lock, so a read
	// never takes appendMu: the flag lives outside the append domain on
	// purpose, and a read that loaded it clear just before Close set it
	// finishes on the reference it holds.
	readClosed atomic.Bool

	// idleClock is the clock the idle sealer installed, boxed so the
	// pointer's loads and stores are lock-free reads before a sealer exists.
	// It is read before appendMu is taken — the decision comment there says
	// nothing else is acquired under that lock, while Clock.Elapsed takes
	// the clock's own mutex — so it cannot sit among the fields the lock
	// guards. Nil until a sealer installs one; a stream with no sealer
	// stamps nothing and is never idle-sealed.
	idleClock atomic.Pointer[clockBox]
	// lastAppend is the Elapsed of the clock above at the stream's last
	// append that reserved — the stamp the idle sealer ages. Guarded by
	// appendMu, like every field of its block.
	lastAppend time.Duration

	// sealState names where the stream's seal stands. The zero value is
	// sealOpen: the stream is appending.
	sealState sealState
	// endedLeft holds the segments the stream rolled away from while a
	// failure had ended them: none of their records is known durable.
	//
	// ponytail: one entry per ended segment, never pruned, and a failed seal
	// is rare. Upgrade to a bounded set when the ledger can name a retired
	// segment, which makes the entry unreachable.
	endedLeft map[core.SegmentID]struct{}
	// pending is the appends and the stream syncs of the active segment
	// between their reservation and their final exit, so a seal can wait
	// for every one that started before it or alongside it (R85, RFC 1
	// §5.3): an append, so the marker lands past every record and every
	// entry is charged, and a sync, so a failure of its window reaches the
	// seal's ended-check before the seal writes anything.
	pending int
	// drained wakes the seal when pending reaches zero, and wakes the
	// stream syncs that parked while the seal ran when it settles, and
	// wakes the end's notifier when the last in-flight reservation lands
	// and the roll it parked on the end's record.
	drained *sync.Cond

	// ending holds the record of the segment the stream ended while no
	// successor is installed over it, from the end itself until its notifier
	// cleared it: the roll the next append makes waits out that record, so a
	// late completion of the ended segment never touches the successor's
	// tracker or catalog, and the end's own word reaches its consumer before
	// the segment it names is left behind (R48, R49).
	ending *endRec
	// apending is the appends reserved in the active segment whose
	// completion section has not run. It rises at the reservation and falls
	// in the completion, so the end's notifier waits out every slot the
	// segment holds before its event hands the segment over.
	apending int
	// failedAt is the lowest offset a failed write took in the active
	// segment, -1 when none has failed. The completion sets it with a min,
	// and a roll resets it at install; the end's event reads it while it
	// waits, where a roll cannot move it.
	failedAt int64
	// failErr is the failure the active segment's writes and syncs left, kept
	// for the seal that finds the segment already failed and must name it in
	// its end: the tracker records that a failure happened, not which. The
	// first failure stands, except that an EROFS replaces any other, for it
	// alone decides the stream's stop. Reset by a roll.
	failErr error
	// onEnd is the consumer of the stream's end events; started says the
	// stream has made a reservation, a roll or an end, so the consumer
	// cannot be registered anymore.
	onEnd   func(EndEvent)
	started bool

	// leftFailed says the active segment was taken to leave an ended one:
	// a failure in it before any sync of it has succeeded is the second
	// segment in a row (RFC 1 §5.4). The roll sets it at install.
	leftFailed bool
	// syncedOnce says a Sync of the active segment has succeeded: the
	// second-refusal rule reads it beside leftFailed, for a stream that
	// synced its new segment once no longer appends into a segment nothing
	// proved durable. Set in Sync's success branch, reset at roll install.
	syncedOnce bool
	// stopped is non-nil once the stream stopped: a second segment in a
	// row failed, or a failure carried EROFS. Every later append fails at
	// the top of its loop with it, so no spare is taken and nothing is reserved; the
	// failure it wraps is the one that stopped the stream.
	stopped error
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
// 32-bit column, and on a sink missing either of its two functions.
func NewStream(no uint32, identity core.JournalID, set *Set, pool *Pool, cache *Descriptors, sink Sink) *Stream {
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
	if sink.Track == nil || sink.Synced == nil {
		panic("segment: a stream needs its durability sink")
	}
	s := &Stream{no: no, identity: identity, set: set, pool: pool, cache: cache, sink: sink, failedAt: -1}
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

// takeSpare takes the spare the roll needs and, when no class can serve it,
// classifies the refusal the roll answers with — the one place the roll
// knows its answer is final.
//
// When the pool reports making a spare failed with ENOSPC and no full-size
// retired segment is queued to be recycled, a refusal naming `spare` is
// answered with the platform's own ENOSPC, wrapped so errors.Is holds for
// syscall.ENOSPC and not for core.ErrNoSpace: the journal's limits are the
// caller's to drain and a full device is the operator's, and the two must not
// be answered alike (R21). It is not counted. When such a segment is queued
// the recycle is what clears the shortage, so the refusal is the counted
// `spare` one below. Otherwise the refusal naming `spare` is counted once and
// returned unchanged (R20, R22). Every other answer passes through unchanged
// and uncounted: a stopped pool's core.ErrClosed, when it carries no
// NoSpaceError, and a header-only roll's headroom-named refusal.
//
// It runs with no lock held and makes no I/O: the takes go to the pool and
// the hold, which reach the ledger alone, so a roll costs only the take
// (R15, R17).
func (s *Stream) takeSpare(hasPayload bool, r Roll) (core.SegmentID, error) {
	id, err := s.drawSpare(hasPayload, r)
	if err == nil {
		return id, nil
	}

	// The full device, answered as itself rather than as a limit, unless a
	// full-size retired segment is to hand: that recycle is the spare.
	var spare *core.NoSpaceError
	if errors.As(err, &spare) && spare.Limit == core.LimitSpare && s.pool.DeviceFull() && !s.pool.retiredToHand() {
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
//
// decision: the class names the spare a roll takes and nothing past it. A
// repack copy rolled onto a reserve spare hands the stream an ordinary
// segment, so the content records that follow land in it and spend repack-
// reserve space, and a repack copy that follows a content roll lands in the
// segment that content made. The classes keep the takes apart, not the
// records after them. The exemption holds while streams serve files of every
// kind; withdraw it when repack copies run on a stream of their own, or when
// the stream remembers the class of its active segment and rolls a record
// of the other kind out of it.
func (s *Stream) drawSpare(hasPayload bool, r Roll) (core.SegmentID, error) {
	switch {
	case r.Repack:
		return s.pool.Take(Reserve)
	case r.Hold != nil:
		return r.Hold.Take()
	case hasPayload:
		return s.pool.Take(Content)
	}
	// The header-only record, the only kind the switch above passes by: a
	// content spare by the unreserved take, so holds keep theirs, and only
	// when no content spare is ready a headroom one (D12). Both refusing —
	// the content miss naming `spare`, the headroom miss the same — the
	// roll is answered naming the headroom rather than the spare: the
	// record goes on drawing on the allocated headroom, and the spare pool
	// never held what it asked for, so the refusal is neither a spare's
	// nor counted (R23). Only the pool's own `spare` miss names the
	// headroom: a headroom take answered something else — a stopped pool's
	// ErrClosed, a ledger that refused the spare it popped — carries its own
	// word to the caller, never a capacity label that reads as drainable.
	id, err := s.pool.Take(Content)
	if err == nil {
		return id, nil
	}
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) || ns.Limit != core.LimitSpare {
		return 0, err
	}
	hid, herr := s.pool.Take(Headroom)
	if herr == nil {
		return hid, nil
	}
	if errors.As(herr, &ns) && ns.Limit == core.LimitSpare {
		return 0, &core.NoSpaceError{Limit: core.LimitHeadroom, Missing: 1, Unit: core.UnitSegments}
	}
	return 0, herr
}

// endTaken unwinds a take that failed past Take: the member segment is ended
// in the ledger — held at its cap until a retirement unlinks it — and the
// ref, when one was taken, is released. The failing error comes back first in
// the join, so its class still reads.
//
// decision: the segment ends with no event, for no record was ever reserved
// in it — a spare's failure carries no window, and the stream never handed
// its consumer a segment it had not taken. Withdrawn if a take ever unwinds
// a segment the stream had already appended to. The appends' own ends, the
// seal's and the requested End are the ends that send one.
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
// rolls. A record that fits nowhere at all — one whose payload it cannot
// place a single byte of, or whose own size exceeds an empty segment's room,
// the largest a stream hands out — answers ErrSegmentFull with nothing
// reserved and nothing written.
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

	// The idle stamp's time is read here, before the lock, so appendMu is
	// never held across Clock.Elapsed — the decision comment at appendMu
	// holds for the clock's own mutex too. The stamp taken below therefore
	// names the call's arrival, not its reservation: the lock's wait, a
	// drain park while a seal runs, and the roll's own segment-sealing I/O
	// can each sit between the reading here and the reservation below, and
	// only shorten the measured idle time by the span, never lengthen it —
	// harmless, for it errs toward sealing sooner, never toward stranding a
	// segment past its age. A seal's pass that reads the stream while this
	// append is in flight can therefore see a stamp below its own now by
	// that same span; the pass's own wait bound comment covers that side.
	var now time.Duration
	stamp := false
	if b := s.idleClock.Load(); b != nil {
		now, stamp = b.c.Elapsed(), true
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
		// decision: the stop is checked inside the loop, re-read under the
		// lock every turn holds at this top, and not only once before it,
		// because the appends that entered concurrent with a stream-ending
		// failure roll past a check the loop never re-makes: a roll-gate
		// wake, a drain cond wake and a lock hand-off all resume at this
		// top, and an append parked on the roll gate while another's failure
		// stopped the stream would otherwise arm a roll of its own and take
		// a third spare, which RFC 1 §5.4's stopped stream must not — the
		// pool's spares stand exactly where the second refusal left them.
		// Withdrawn only by a stopped stream that is ever allowed to serve
		// again, which the journal-wide read-only state of the durability
		// code owns instead.
		if s.stopped != nil {
			s.appendMu.Unlock()
			return Placed{}, s.stopped
		}
		// decision: no reservation is placed here without re-reading the
		// seal state under this same hold (R1, R85: once a seal has taken
		// the segment, a marker chosen over bytes its footer does not name
		// is corruption, so the reservation must never land past a seal).
		// What is deliberately NOT here is the past-seal refusal the take
		// gate's resume once ran: the roll replaced the take, and every
		// park this loop makes — the roll gate's, the drain cond's, this
		// hook's — resumes at this top, where the state read below is the
		// one the next turn re-reads too. A seal that settled while this
		// append parked leaves sealSealed or sealEnded, which arm the roll
		// below rather than reserving under the marker; a seal still
		// running parks the append on the drain's cond, whose wake lands
		// here again. So a reservation runs only with sealState == sealOpen
		// read under appendMu, whatever parks came before it. What would
		// still need the refusal is a park that resumes without this top —
		// a gate bypassed or an inline reserve — which the loop's shape
		// alone forbids; a reservation found running under a settled seal
		// that this top did not gate is the bug to withdraw this over.

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

		// The one refusal the roll cannot answer, checked before anything
		// is taken: a record that cannot place even one payload byte —
		// of the ones it carries: a no-payload kind, or a write or fill of
		// no payload bytes, has no piece to size but its whole record,
		// which fits exactly when its size equals the room — anywhere in
		// an empty segment, the largest room a stream hands out. A write or
		// fill whose payload alone exceeds a segment is not refused but
		// split, piece by piece, segment after segment (RFC 28 §2.2); the
		// refusal is for the record whose own overhead leaves the room no
		// piece at all, and reserve-side discovery, after a take, would
		// consume one spare after another for a record that fits nowhere.
		// The refusal reserves nothing, so it leaves the stream exactly
		// where it was.
		emptyRoom := ((s.set.cap - format.SegmentHeaderSize - sealRoom(1)) &^ 7) -
			(int64(format.RecordHeaderSize) + format.RecordHeader{Kind: h.Kind}.BodySize())
		if emptyRoom < 0 || len(body.Payload) > 0 && emptyRoom < 1 {
			s.appendMu.Unlock()
			return Placed{}, ErrSegmentFull
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
				} else {
					// Not even one payload byte places, or the kind carries
					// no payload and does not fit: the record has no piece
					// here, and the roll takes the next segment for it. A
					// record no segment could hold at all was refused by
					// the empty-room guard above, before anything rolled,
					// so the roll never loops on the record.
					roll = true
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
			// The first roll starts the stream no less than the first
			// reservation does, for OnEnd's gate reads its history
			// through the one flag: a consumer registered while a roll
			// is in flight would miss the segment the roll's seal ends.
			s.started = true
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
		// The arrival time read before the lock becomes the stream's last
		// append. Only a reservation stamps: a refused append — a closed
		// stream, a segment with no room, a failed roll — wrote nothing, so
		// it does not count as one.
		if stamp && now > s.lastAppend {
			s.lastAppend = now
		}
		s.count++
		// The reservation names its segment to the end's notifier: the
		// completion below is the one that clears the name, and the event
		// waits for it. The stream is already started by this point — the
		// reservation runs only past the roll that armed the gate, and the
		// gate's arming is where the first append marked it — so the
		// reservation needs no flag of its own.
		s.apending++
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

		var written Written
		if werr == nil {
			written = Written{
				Loc:        Location{Segment: id, Off: off, Len: size},
				Seq:        h.Seq,
				Version:    h.Version,
				File:       h.File,
				Kind:       h.Kind,
				PayloadCRC: format.RecordPayloadCRC(buf),
			}
			if !hasPayload {
				// decision: the sink keeps buf without a copy, because
				// the append allocates its buffer per call and never reuses
				// it. Withdraw this and copy here if the ponytail buffer-pool
				// upgrade at the encode is ever made.
				written.Bytes = buf
			}
		}

		s.appendMu.Lock()
		if werr == nil {
			// The record is registered before its slot completes: the bytes
			// are written, and no sync's start can cover the slot until
			// complete runs under this same lock, so every Synced that
			// covers the record is reported after its Track.
			//
			// decision: the record is registered even if the ledger
			// refuses it below, for it was written all the same, and a
			// record the sink lacks could be reissued by a failed window
			// while its bytes sit in the segment. The ledger's refusal is
			// unreachable today, because the drain holds the segment Active
			// until the append leaves; overturned by any path that can end
			// or retire the segment between the write and the ledger's
			// add, which would leave a registered record with no location
			// returned to its caller.
			s.sink.Track(written)
		}
		s.tracker.complete(slot, werr == nil)
		// The completion ends a reservation the end's event waits on, on the
		// failed path included — the early return below leaves through the
		// notifier's branch, whose unlock the count's fall must precede.
		s.apending--
		if s.apending == 0 {
			s.drained.Broadcast()
		}
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
		} else {
			// A failed write ends its segment (R48, RFC 28 §2.2): its slot
			// may hold bytes the zero-slot rule cannot step over, or zeros
			// no later synced-through offset may pass, so nothing could
			// prove a record appended after it synced. Only the active
			// segment takes the end here: a seal already running owns the
			// segment it drained — it sees the tracker's freeze and ends
			// the segment itself on the paths the seal failure took — and
			// a segment the end already covered keeps the first end's
			// word. The id check is a guard, for a roll cannot install a
			// successor while the segment holds any reservation in flight:
			// the roll waits out the ended segment first.
			if s.failedAt < 0 || off < s.failedAt {
				s.failedAt = off
			}
			s.noteFailure(werr)
			// An EROFS stops the stream wherever it lands, a segment already
			// ended included: the device is read-only whichever write met
			// it first, and endLocked below reads only the failure that
			// ends the segment. A straggler's other failure is no stop of
			// its own — the segment is the one the first failure ended,
			// not a second refusal.
			if errors.Is(werr, syscall.EROFS) {
				s.stopLocked(werr)
			}
			if s.id == id && s.sealState == sealOpen {
				if endID, notify := s.endLocked(EndAppend, werr); notify {
					s.appendMu.Unlock()
					if unerr := s.set.End(endID); unerr != nil {
						// The end of a segment the ledger may not know is
						// the stream's own and the write's refusal above
						// names it; a ledger that cannot end it carries its
						// failure here, after the write's own, so the
						// platform's error still reads first.
						werr = errors.Join(werr, unerr)
					}
					go s.notifyEnd()
					return Placed{}, werr
				}
			}
		}
		s.appendMu.Unlock()

		if werr != nil {
			// The failed slot stays as it is — never rewritten, never
			// zeroed (R87) — and the tracker's frozen prefix keeps the
			// records around it at or below its offset (R10).
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
// none after a segment a failure ended (R8, R24, R33). The first segment is
// a roll from nothing: nothing is sealed, and the header names none. A roll
// the pool cannot serve is refused at once, before any seam call, so the
// active segment is untouched and the refusal costs no I/O (R15, R20).
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
	//
	// A seal that is still running — one the caller asked for, the idle
	// sealer's, or a race's — is waited out here on the drain's cond before
	// any word of the predecessor is read: a successor named under a
	// sealing segment would name a segment whose footer the seal was still
	// writing, and one installed beside it would be overwritten by the
	// seal's settled state the moment the seal returned.
	//
	// decision: this wait is defense in depth over the loop's own park on
	// a sealing state, and no test can force it. The window it closes is
	// the length of the pool's take, between the roll's caller releasing
	// appendMu and this read, and it holds no seam call a hold could park;
	// every seal the loop can see is parked there, so a seal this read
	// still finds must have started inside those microseconds, and this
	// wait alone keeps the predecessor off a footer still being written.
	// Withdrawn only by a hold the roll can be parked on, which the seam
	// would have to grow.
	s.appendMu.Lock()
	old, oldRef := s.id, s.ref
	state := s.sealState
	for state == sealSealing {
		s.drained.Wait()
		state = s.sealState
	}
	s.appendMu.Unlock()

	var pred core.SegmentID
	// leftEnded says the roll is leaving a segment the stream ended (or its
	// own seal just did), the one fact both the successor-header stop below
	// and the install's leftFailed need: every ordinary roll leaves a sealed
	// one behind, and a refused header write back onto the ended one would
	// be the second refusal in a row.
	leftEnded := old != 0 && state == sealEnded

	switch {
	case old == 0:
		// A stream that held no segment names none: the first segment of a
		// stream has nothing before it, and there is nothing to seal.
	case state == sealSealed:
		// A seal someone else finished — explicit, idle, or one whose
		// truncation failed and left the segment sealed at its cap (R34) —
		// has already made the segment durable; the successor names it.
		pred = old
	case state == sealOpen:
		_, serr := s.Seal(false)
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
		ended := s.sealState == sealEnded
		leftEnded = ended
		s.appendMu.Unlock()
		if closed {
			return s.endTaken(id, Ref{}, ErrStreamClosed)
		}
		if ended {
			// The seal ended the segment: a failed sync of its records,
			// of its marker, or a write that failed. That failure is the
			// only word the durability layer ever gets that the segment's
			// acknowledged records were not made durable, so the roll
			// hands it to its caller and stays on the ended segment —
			// moving on would answer the next Sync with a nil over a
			// window no fsync reached. The taken spare is unwound, and the
			// next append rolls again, naming none (R33). The seal's
			// refusal already names the sentinel on every ended path,
			// so it is carried as itself — wrapping it would name the
			// end twice.
			if errors.Is(serr, ErrSegmentEnded) {
				return s.endTaken(id, Ref{}, serr)
			}
			return s.endTaken(id, Ref{}, fmt.Errorf("%w: %w", ErrSegmentEnded, serr))
		}
	}
	// state == sealEnded names none: a segment a failure ended leaves no
	// successor of its own.
	if old != 0 && state == sealEnded {
		s.appendMu.Lock()
		if s.endedLeft == nil {
			s.endedLeft = make(map[core.SegmentID]struct{})
		}
		s.endedLeft[old] = struct{}{}
		s.appendMu.Unlock()
	}

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
		// decision: a failed successor header write stops the stream when
		// the segment the stream stays on is ended — the refusal it just
		// met leaving it is the second in a row, and the next append would
		// only take a further spare to meet a third — or when the failure
		// itself carries EROFS, wherever the stream stands. The taken spare
		// ends below with no event, for no record was ever reserved in it.
		// The old segment's own event may not have gone yet — the header
		// write runs before the roll waits out the end's record, so an
		// append still writing there holds it back — and then it carries
		// Stopped, for the notifier reads the stream's stop when it
		// snapshots; an event already handed over said Stopped false, and
		// the stop reaches the consumer through every later append's
		// ErrStreamStopped instead. The stream keeps its stopped error for
		// the append loop's top to answer every later append with.
		// Withdrawn together with the endTaken exemption the day a take
		// unwinds a segment the stream had already appended to.
		s.appendMu.Lock()
		if errors.Is(err, syscall.EROFS) || leftEnded {
			s.stopLocked(err)
		}
		s.appendMu.Unlock()
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
	// decision: the wait below runs only when leaving a segment the stream
	// ended, for a seal to a sealed segment has already drained every
	// appender before the roll runs, while an append failure ends its
	// segment with writes still in flight. Waiting for the end's record to
	// clear keeps a late completion of the ended segment from touching the
	// successor's tracker or catalog — the completion would complete and
	// charge a record the successor never reserved — and the wait costs the
	// in-flight writes of the ended segment alone, on the failure path
	// only. The record is all the roll waits on: the notifier that clears it
	// first waits out every reservation and every sync in flight, and an
	// ended segment takes no new one, so a cleared record means the segment
	// holds none. Withdrawn the day completions
	// stop touching the stream's shared tracker and catalog — per-segment
	// trackers, for instance — for a late completion of an ended segment
	// would then be harmless to a successor installed at once.
	for s.ending != nil && !s.closed {
		s.drained.Wait()
	}
	// A close that landed during the wait itself unwinds the taken spare: a
	// successor installed now would stay Active in the ledger, never ended
	// and never unlinked, and its ref would never be released while the
	// close released the old segment's own — the same wait that carries the
	// ended segment's late completions also carries a close racing them.
	if s.closed {
		s.appendMu.Unlock()
		return s.endTaken(id, ref, ErrStreamClosed)
	}
	s.id, s.ref = id, ref
	s.next = format.SegmentHeaderSize
	s.count = 0
	s.failedAt = -1
	s.failErr = nil
	s.entries = nil
	s.tracker.reset(format.SegmentHeaderSize)
	s.sealState = sealOpen
	// The new segment's own second-refusal state: the roll left a segment
	// the stream ended — the one this segment replaces, whatever ended it,
	// its own failure or its own seal's — so a failure in the new one
	// before any sync of it succeeds is the second segment in a row (RFC 1
	// §5.4), which endLocked reads through this pair.
	s.leftFailed = leftEnded
	s.syncedOnce = false
	s.appendMu.Unlock()

	// The old ref is released only after its seal's syncs, outside the lock:
	// a descriptor that wrote is never closed before a sync covered it.
	if oldRef != (Ref{}) {
		oldRef.Release()
	}
	return nil
}

// noteFailure keeps the failure a write or a sync of the active segment met
// for the seal that may find it later. The caller holds appendMu.
func (s *Stream) noteFailure(err error) {
	if s.failErr == nil || errors.Is(err, syscall.EROFS) {
		s.failErr = err
	}
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
	_, _, err := s.SyncSegment()
	return err
}

// SyncSegment is Sync that also names the segment the call was of, read under
// the same append domain as the sync's own state, so the durability layer
// attributes a failure to the segment that failed and not to whichever one is
// active by the time it looks. The id is zero when no segment was active or
// the stream was closed. issued is true only when the call reached an fsync of
// the segment's file: a closed stream, no active segment, a sealed segment and
// an ended one all answer without one, so a caller that counts syncs issued
// and failed counts only the calls that report issued.
func (s *Stream) SyncSegment() (id core.SegmentID, issued bool, err error) {
	s.appendMu.Lock()
	if s.closed {
		s.appendMu.Unlock()
		return 0, false, ErrStreamClosed
	}
	if s.id == 0 {
		s.appendMu.Unlock()
		return 0, false, nil // no segment is active: nothing to make durable
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
		id := s.id
		s.appendMu.Unlock()
		return id, false, nil
	case sealEnded:
		id := s.id
		s.appendMu.Unlock()
		return id, false, ErrSegmentEnded
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
	// The stream cannot move while the fsync runs: the sync is counted in the
	// drain, a seal waits for it before it seals, and the end's notifier —
	// which the roll over an ended segment waits out — waits for it too, so
	// the tracker and the second-refusal word below are still the segment's
	// own when the fsync returns (R10).
	ref, id := s.ref, s.id
	s.appendMu.Unlock()

	if err := ref.Sync(); err != nil {
		// The failure belongs to the durability layer's failed window: the
		// caller owns the records the window holds, and no later sync of
		// this segment claims what the failure left — the value stands where
		// the failure found it.
		s.appendMu.Lock()
		s.tracker.syncFail()
		s.noteFailure(err)
		s.appendMu.Unlock()
		return id, true, err
	}
	s.appendMu.Lock()
	// decision: a sync that completes after an earlier sync of the segment
	// failed is not reported, because it covers nothing the tracker may claim
	// (RFC 1 §5.3's failed window: "a second sync can succeed without having
	// written them"); reporting it would let the ledger drop the failed
	// window's entries. Overturned if the durability layer ever needs to see
	// such syncs, which would then need a flag of their own in the event.
	//
	// The report is made under the append domain, after the Track of every
	// record the through offset covers: a slot is complete, and so covered,
	// only after its Track ran under this same lock.
	if s.tracker.syncDone(through) {
		s.sink.Synced(id, through)
	}
	// A sync of the active segment has succeeded: the second-refusal rule
	// reads this beside leftFailed, for a stream that proved this segment
	// durable once is past the window where a failure in it counts as the
	// second in a row (RFC 1 §5.4).
	s.syncedOnce = true
	s.appendMu.Unlock()
	return id, true, nil
}

// syncAtGapHook is the test hook in SyncAt between its checks and its sync,
// where a seal and a roll can still end and leave the segment. A check that
// sets it must nil it before it returns.
var syncAtGapHook func()

// SyncAt makes the record at loc durable, not merely the active segment's: a
// segment the stream has left was either sealed, which made its every record
// durable, or ended by a failure, which made none of them known durable, and
// the answer is ErrSegmentEnded. The segment still active takes Sync.
func (s *Stream) SyncAt(loc Location) error {
	s.appendMu.Lock()
	if s.closed {
		s.appendMu.Unlock()
		return ErrStreamClosed
	}
	if _, ended := s.endedLeft[loc.Segment]; ended {
		s.appendMu.Unlock()
		return ErrSegmentEnded
	}
	if s.id != loc.Segment {
		s.appendMu.Unlock()
		return nil
	}
	s.appendMu.Unlock()
	if syncAtGapHook != nil {
		syncAtGapHook()
	}
	if err := s.Sync(); err != nil {
		return err
	}
	// The stream may have rolled away between the checks and the sync; an
	// ended segment it left says the sync covered a successor.
	s.appendMu.Lock()
	_, ended := s.endedLeft[loc.Segment]
	s.appendMu.Unlock()
	if ended {
		return ErrSegmentEnded
	}
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
	return s.sealLocked(early)
}

// sealLocked is Seal under the append lock: the caller enters holding
// appendMu and this body releases it on every path, exactly as Seal does,
// so a caller that has the lock — the idle seal's check-and-take below —
// can run the whole seal without releasing it between deciding and acting.
// Behaviour, error values and comments are Seal's own.
func (s *Stream) sealLocked(early bool) (SealResult, error) {
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
	// before it releases the segment's ref. The ref and the id are read
	// under this same hold, before the drain's park: the park drops the
	// lock — the drain it waits must run — and a Close racing the park
	// clears the stream's ref and id in that window, so a woken seal that
	// read them after the park would drive the zero ref the close left.
	// The pair read here is the pair the seal owns and drives; whatever a
	// close does after it, the seal is Close's in-flight operation and the
	// pair's release is Close's own wait.
	s.inflight.Add(1)
	defer s.inflight.Done()
	sealedID, sealedRef := s.id, s.ref
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
	// it, the way WriteSeal's own ended paths do, and the seal owns the
	// end's event, for the appends whose failures it found have already
	// returned: no completion of the segment holds the event back.
	if s.tracker.freeze || s.tracker.syncFailed {
		reason := fmt.Errorf("%w: a sync of the segment failed", ErrSegmentEnded)
		if s.tracker.freeze {
			reason = fmt.Errorf("%w: a write of the segment failed", ErrSegmentEnded)
		}
		if s.failErr != nil {
			reason = fmt.Errorf("%w: %w", reason, s.failErr)
		}
		endID, notify := s.endLocked(EndSeal, reason)
		s.appendMu.Unlock()
		s.drained.Broadcast() // the Syncs parked on the seal leave with the end
		// The notifier starts whatever the ledger said, for the end already
		// named it to the in-flight group Close waits on: a refusal here is
		// a ledger and a stream that disagree, and it must not turn into a
		// hang of Close and of every later roll on the end's record, which
		// nothing else would ever clear.
		if err := s.set.End(endID); err != nil {
			// The end of a segment the ledger may not know is the
			// stream's own and the seal's refusal above names it; a
			// ledger that cannot end it carries the failure here.
			if notify {
				go s.notifyEnd()
			}
			return SealResult{}, errors.Join(ErrSegmentEnded, err)
		}
		if notify {
			go s.notifyEnd()
		}
		return SealResult{}, reason
	}

	// A Close may have run while the seal parked on the drain's cond: it
	// cleared the active segment's ref — this seal among the in-flight
	// operations it waits, for it counted itself before the park — so the
	// ref below must never be re-read from the stream. The seal drives the
	// pair it read while it owned the segment, and a close that beat the
	// drain refuses the seal whole: the stream already left the segment
	// (its id is 0 under the close), publishes no further location, and
	// the segment is ended so nothing lands in it again.
	//
	// decision: this end sends no event, for it is the close's own doing —
	// the stream is shutting down and its consumer's contract ends with the
	// stream, not with a farewell the close owes it. The segment still ends
	// in the ledger, held at its cap for a retirement to unlink. Withdrawn
	// when a close needs to hand its segment over, which must then carry
	// its own event and its own cause.
	if s.closed {
		s.sealState = sealEnded
		s.appendMu.Unlock()
		s.drained.Broadcast() // the Syncs parked on the seal leave with the end
		if err := s.set.End(sealedID); err != nil {
			return SealResult{}, errors.Join(ErrStreamClosed, err)
		}
		return SealResult{}, ErrStreamClosed
	}

	// The marker sits at the stream's next free slot: every record below it
	// has completed. The entries are cloned and sorted, for the footer's
	// reader takes them ascending by offset while the stream kept them in
	// completion order. The id and the ref are the pair the seal read
	// before the drain's park — the same segment it took, whose ref the
	// close racing the park may already have cleared from the stream.
	marker := s.next
	entries := slices.Clone(s.entries)
	sortSealEntries(entries)
	id, ref := sealedID, sealedRef
	s.appendMu.Unlock()

	res, err := WriteSeal(s.set, ref, s.identity, id, marker, entries, early, s.sink.Synced)

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
		// past a refused seal either. Both paths call the seal's own
		// event to their consumer: the drain has emptied, so no slot the
		// segment holds still writes.
		// A failure past the seal's first step came after its record sync
		// succeeded: that is a successful sync of the segment, so the
		// second-refusal rule (RFC 1 §5.4) must not count the failure as one
		// before any sync of the spare.
		var durable recordsDurable
		if errors.As(err, &durable) {
			s.syncedOnce = true
		}
		endID, notify := s.endLocked(EndSeal, err)
		s.drained.Broadcast() // the Syncs parked on the seal leave with the end
		if !errors.Is(err, ErrSegmentEnded) {
			s.appendMu.Unlock()
			if unerr := s.set.End(endID); unerr != nil {
				err = errors.Join(err, unerr)
			}
			if notify {
				go s.notifyEnd()
			}
			return SealResult{}, fmt.Errorf("%w: %w", ErrSegmentEnded, err)
		}
		s.appendMu.Unlock()
		if notify {
			go s.notifyEnd()
		}
		return SealResult{}, err
	}
	s.sealState = sealSealed
	s.drained.Broadcast() // the Syncs parked on the seal leave with the seal
	s.appendMu.Unlock()
	return res, nil
}

// watchIdle installs the clock the stream's idle time is read from: the
// stream counts as just active from the reading the sealer makes here, so a
// segment that idled before the sealer existed is never sealed the moment
// the sealer appears. The clock is stored before the stamp is taken, so an
// append that lands between the two stamps the clock's own now; the store is
// published by the lock below for any reader that loads the pointer first.
func (s *Stream) watchIdle(c core.Clock) {
	now := c.Elapsed()
	s.idleClock.Store(&clockBox{c: c})
	s.appendMu.Lock()
	if now > s.lastAppend {
		s.lastAppend = now
	}
	s.appendMu.Unlock()
}

// idleDueLocked reports, under the append lock the caller holds, whether the
// stream's active segment is a candidate the idle seal may close at now, and
// the wait until it becomes due. The bytes beyond the segment's last
// catalogued point are next past the segment header: an active segment
// carries no catalog of its own — RFC 28 writes a catalog only in a seal's
// footer — so its last catalogued point is the end of its 48-byte header, and
// every reserved record slot lies beyond it.
//
// The bounds follow the rule's own words: "more than" the threshold is a
// strict comparison — a segment holding exactly the threshold stays open —
// and "has taken no append for 30 s" means an idle time at least the age.
// A stream that is not a candidate answers (false, -1), the wait that tells
// the sealer to ignore it rather than arm one.
func (s *Stream) idleDueLocked(now time.Duration, threshold int64) (due bool, wait time.Duration) {
	// The candidate: a live stream with an open segment holding more than
	// the threshold beyond its last catalogued point.
	if s.closed || s.id == 0 || s.sealState != sealOpen ||
		s.next-format.SegmentHeaderSize <= threshold {
		return false, -1
	}
	idle := now - s.lastAppend
	if idle >= core.IdleSealAge {
		return true, 0
	}
	return false, core.IdleSealAge - idle
}

// idleDue is idleDueLocked under the append lock, taken and released here,
// with no I/O: the peek the idle sealer makes on each of its passes. It is
// a peek alone — the state it reads can change the moment it returns — and
// every caller that acts on a due verdict runs sealIdle, which re-checks
// under the same hold the seal takes.
func (s *Stream) idleDue(now time.Duration, threshold int64) (due bool, wait time.Duration) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	return s.idleDueLocked(now, threshold)
}

// sealIdle seals the stream's active segment early when it is still due at
// now, re-checked under the same hold the seal takes: an append that
// reserved since the sealer's peek wins here and the segment stays open, for
// the reservation and the seal's move to sealSealing happen under one hold
// of appendMu, so no check ever acts on a state a later append moved. An
// append arriving after that move parks on the seal's drain cond and then
// rolls, exactly as for an explicit seal (R85). Not due any more — an append
// landed, another seal ran, the stream closed — the stream is left exactly
// as the peek's caller found it and nothing is written.
func (s *Stream) sealIdle(now time.Duration, threshold int64) (sealed bool, res SealResult, err error) {
	s.appendMu.Lock()
	if due, _ := s.idleDueLocked(now, threshold); !due {
		s.appendMu.Unlock()
		return false, SealResult{}, nil
	}
	res, err = s.sealLocked(true)
	return true, res, err
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
//
// decision: an in-flight read is not counted in inflight, because it holds a
// cache reference and Descriptors.Shutdown waits for every reference, so the
// storage a reader holds is never freed under it. A read that passed the flag
// check but acquires after Shutdown began fails cleanly with ErrCacheClosed.
func (s *Stream) ReadAt(seg core.SegmentID, p []byte, off int64) (int, error) {
	if s.readClosed.Load() {
		return 0, ErrStreamClosed
	}
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
	ref, began := s.beginClose()
	if !began {
		return nil
	}
	s.finishClose(ref)
	return nil
}

// beginClose marks the stream closed and takes its active ref, so no new
// append or read is admitted from here on; began is false when it was
// already closed. The caller owes one finishClose with the ref.
func (s *Stream) beginClose() (ref Ref, began bool) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.closed {
		return Ref{}, false
	}
	s.closed = true
	s.readClosed.Store(true)
	ref = s.ref
	s.ref = Ref{}
	return ref, true
}

// finishClose waits for the appends in flight and releases the ref beginClose
// took. The wait runs outside the lock: an append completing right now takes
// it, and the take's runner settles its gate under it.
func (s *Stream) finishClose(ref Ref) {
	s.inflight.Wait()
	if ref != (Ref{}) {
		ref.Release()
	}
}
