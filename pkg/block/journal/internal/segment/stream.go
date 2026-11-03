package segment

import (
	"errors"
	"io"
	"math"
	"sync"

	"github.com/marmos91/dittofs/pkg/block/journal/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journal/internal/format"
)

// ErrSegmentFull is returned by Append when the record does not fit in what
// remains of the active segment before the seal's reserve: nothing was
// reserved and nothing was written.
//
// ponytail: with no roll yet, a full segment refuses the append outright. The
// roll replaces the refusal with a seal and a take of the next segment, so
// the caller's remaining bytes go on there instead of coming back here.
var ErrSegmentFull = errors.New("segment: no room for the record before the seal's reserve")

// ErrStreamClosed is returned by Append after Close: the stream publishes no
// further location, so every caller stops writing.
var ErrStreamClosed = errors.New("segment: stream closed")

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
// write failed freezes it: nothing after the failure moves it.
//
// decision: every field is guarded by the owning stream's append mutex, the
// segment append domain. Nothing else may touch the tracker.
//
// ponytail: the in-flight slots sit in a ring buffer that grows by doubling
// only when full, so a steady stream of appends allocates nothing on the
// sorted path. The growth copies the ring into order, so the slot numbers
// stop naming their indices; headSlot below is what still maps a slot to its
// place. Upgrade to a freelist of slot records if the doubling copy of a very
// deep in-flight run ever shows in a profile.
type tracker struct {
	ring []tslot // the in-flight slots, physically laid out from head on
	live int     // slots held in the ring

	next uint64 // the next slot number to hand out

	// headSlot is the slot number that sits at the ring's start. A slot's
	// place in the ring is its distance from headSlot, which doubling does
	// not move.
	headSlot uint64

	completed int64 // the end of the well-completed prefix
	synced    int64 // the highest completed value a returned Sync covered
	freeze    bool  // a slot's write failed: the prefix never moves again
	failedAt  int64 // the failed slot's own offset, the cap of every later sync
}

// reset starts the tracker for a segment whose first record slot is first:
// the header write is not a tracker slot — it completes before any record is
// reserved — so the prefix begins at it while no sync has covered anything.
func (t *tracker) reset(first int64) {
	t.ring = t.ring[:0]
	t.live, t.next, t.headSlot = 0, 0, 0
	t.completed = first
	t.synced = 0
	t.freeze = false
	t.failedAt = 0
}

// reserve takes the slot for the record at [off, end) and returns its
// number. It allocates only when the ring is full, growing it by doubling,
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
	if !t.freeze && t.live > 0 && t.ring[0].done && !t.ring[0].ok {
		// The failed slot caps every value a later sync may carry: the
		// prefix it bounds is its own offset, so what a sync records above
		// it claims bytes no fsync covered.
		t.freeze = true
		t.failedAt = t.ring[0].off
	}
}

// syncStart returns the offset a Sync that starts now will cover once it
// returns: the well-completed prefix it found.
func (t *tracker) syncStart() int64 {
	return t.completed
}

// syncDone records that a returned Sync covered up to through.
func (t *tracker) syncDone(through int64) {
	// A freeze caps what any sync may claim: the failed slot's own offset
	// is the farthest the prefix names, so a through past it is a word for
	// bytes no fsync covered.
	if t.freeze && through > t.failedAt {
		through = t.failedAt
	}
	if through > t.synced {
		t.synced = through
	}
}

// Stream is one append stream of a journal: it takes segments from the Maker,
// appends records to one active segment at a time and reads back positioned
// records through the descriptor cache. It is the writing half of the walking
// skeleton: one stream, one active segment, no roll and no seal — a record
// that does not fit answers ErrSegmentFull, and the segment it appends to is
// the first the Maker returns.
type Stream struct {
	no       uint32
	identity core.JournalID
	set      *Set
	maker    *Maker
	cache    *Descriptors

	// decision: every field below is guarded by appendMu, the segment append
	// domain of RFC 1 §8.2 — next, count, entries, the tracker, the active
	// segment and its ref, the take's single-flight gate and the closed
	// flag. The lock is held for a record's reservation and its completion
	// only — never across I/O, and nothing else is acquired under it, so no
	// set call and no storage call sit inside it.
	appendMu sync.Mutex
	next     int64 // the first free record slot of the active segment
	count    int64 // records reserved in the active segment
	// entries are the catalog entries of the segment's records, kept in
	// completion order for the seal, which sorts them by offset.
	entries  []format.CatalogEntry
	tracker  tracker
	id       core.SegmentID // 0 until a take installs its segment
	ref      Ref
	taking   chan struct{} // armed while one caller runs the take for all
	closed   bool
	inflight sync.WaitGroup // the appends Close waits for, added to under the lock
}

// NewStream returns the append stream no of the journal identity over set,
// taking its segments with maker and reading and writing them through cache.
// The cache must name segments as format.SegmentName does; the segment cap is
// the set's own, so the stream and the ledger can never disagree about it.
// It panics on a nil dependency and on a set whose cap is above MaxUint32,
// because a record's own offset is a 32-bit column.
func NewStream(no uint32, identity core.JournalID, set *Set, maker *Maker, cache *Descriptors) *Stream {
	if set == nil {
		panic("segment: a stream needs its ledger")
	}
	if maker == nil {
		panic("segment: a stream needs its maker")
	}
	if cache == nil {
		panic("segment: a stream needs its descriptor cache")
	}
	if set.cap > math.MaxUint32 {
		panic("segment: the segment cap is above MaxUint32, so an offset cannot be carried in a record header")
	}
	return &Stream{no: no, identity: identity, set: set, maker: maker, cache: cache}
}

// take makes the stream its next segment and returns its id and the ref held
// on it: the Maker's spare, carried into the ledger through Take, opened
// through the cache, and named by its 48-byte header written once at offset
// 0. It performs I/O, so it runs with no lock held (R12).
//
// If Make failed there is nothing to clean up — Make cleans up after itself.
// If Take itself failed, the id is still a spare in the ledger and its
// reservation is what DropSpare returns. A failure past Take ends the member
// segment in the ledger at its cap, for a retirement to unlink, and releases
// the ref when one was taken.
//
// ponytail: the next segment is made on the write path, because no spare
// pool exists yet. Upgrade to a roll that takes a pool spare and names its
// predecessor in the header rather than 0.
func (s *Stream) take() (core.SegmentID, Ref, error) {
	id, err := s.maker.Make()
	if err != nil {
		return 0, Ref{}, err
	}
	if err := s.set.Take(id); err != nil {
		if _, derr := s.set.DropSpare(id); derr != nil {
			return 0, Ref{}, errors.Join(err, derr)
		}
		return 0, Ref{}, err
	}
	ref, err := s.cache.Acquire(id)
	if err != nil {
		return 0, Ref{}, s.endTaken(id, Ref{}, err)
	}
	buf := format.AppendSegmentHeader(nil, format.SegmentHeader{
		Identity:    s.identity,
		ID:          id,
		Predecessor: 0,
		Stream:      s.no,
	})
	if _, err := ref.WriteAt(buf, 0); err != nil {
		return 0, Ref{}, s.endTaken(id, ref, err)
	}
	return id, ref, nil
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
// bytes it placed. The reservation — filling the header's segment and
// synced-through, sizing the record against the seal's reserve, advancing the
// byte position and taking the tracker's slot — happens under the append lock
// with no I/O; the encoding and the write run outside it; the completion — the
// tracker's slot and the record's catalog entry — runs back under it. The
// location is returned only once the bytes have been written, so a caller may
// publish it as its last step (R5).
//
// A write or fill whose whole record does not fit what remains of the segment
// before the seal's reserve is split at the cap: this Append returns the
// largest piece that fits and N below the payload's length, and the caller
// continues with the rest. A record that places not even one payload byte —
// or a kind with no payload that does not fit at all — answers ErrSegmentFull
// with nothing reserved and nothing written.
func (s *Stream) Append(h format.RecordHeader, body format.RecordBody) (Placed, error) {
	s.appendMu.Lock()
	s.inflight.Add(1) // under the lock, before anything is reserved
	for {
		if s.closed {
			s.inflight.Done()
			s.appendMu.Unlock()
			return Placed{}, ErrStreamClosed
		}

		// Taking the first segment is I/O, so one caller runs it for all and
		// the others park on its gate, holding no lock; each retries when
		// the gate settles, so a failed take costs its runner alone and the
		// next Append makes a fresh attempt.
		if s.id == 0 {
			if s.taking != nil {
				gate := s.taking
				s.appendMu.Unlock()
				<-gate
				s.appendMu.Lock()
				continue
			}
			s.taking = make(chan struct{})
			s.appendMu.Unlock()
			id, ref, terr := s.take()
			s.appendMu.Lock()
			s.takeSettle()
			if terr != nil || s.closed {
				// A failed take left no segment. A take that landed but
				// found the stream closed while it ran is unwanted too, so
				// it is unwound the same way a failed one is.
				if terr == nil {
					terr = s.endTaken(id, ref, ErrStreamClosed)
				}
				s.inflight.Done()
				s.appendMu.Unlock()
				if s.closed {
					return Placed{}, ErrStreamClosed
				}
				return Placed{}, terr
			}
			s.id, s.ref = id, ref
			s.next = format.SegmentHeaderSize
			s.count = 0
			s.tracker.reset(format.SegmentHeaderSize)
		}

		// The reservation. The header carries the segment, its own offset
		// and the synced-through the tracker holds (R10).
		h.Segment = s.id
		h.SyncedThrough = s.tracker.synced
		h.Offset = s.next

		size := h.Size()
		payload := int64(len(body.Payload))
		hasPayload := h.Kind == format.KindWrite || h.Kind == format.KindFill

		// The seal's reserve bounds the room (R11): a record fits when what
		// remains after it still holds the marker, a footer entry for every
		// record in the segment — this one included — and the trailer.
		fits := s.next+size+sealRoom(s.count+1) <= s.set.cap

		// "A write that does not fit in what remains of the current segment
		// is split at the cap into records, one per segment, each carrying
		// its own file offset and the write's version." — RFC 28 §2.2. The
		// whole record is one piece when it fits: no empty piece, no early
		// split.
		placed := payload
		if !fits && hasPayload && payload > 0 {
			for piece := payload - 1; piece >= 1; piece-- {
				h.Extent.Len = piece
				if s.next+h.Size()+sealRoom(s.count+1) <= s.set.cap {
					placed, fits = piece, true
					break
				}
			}
			size = h.Size()
		}
		if !fits {
			// Nothing was reserved and nothing written: the position did not
			// move, so the segment's remaining room is exactly what it was.
			s.inflight.Done()
			s.appendMu.Unlock()
			return Placed{}, ErrSegmentFull
		}

		slot := s.tracker.reserve(h.Offset, h.Offset+size)
		s.next += size
		s.count++
		id, ref, off := s.id, s.ref, h.Offset
		s.appendMu.Unlock()

		// The encode and the write, outside the lock. A split piece keeps
		// the write's file offset and names the bytes it stages in its
		// length.
		if placed < payload {
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
		s.inflight.Done()

		if werr != nil {
			// ponytail: a failed write does not yet end its segment, and
			// later appends go on landing in it; the slot it reserved stays
			// as it is — never rewritten, never zeroed (R87) — and the
			// tracker's frozen prefix keeps later records' synced-through at
			// or below the failed slot's offset. RFC 28 §2.2 ends the
			// segment and moves the stream to a new one, which is the roll's
			// work.
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

// takeSettle settles the take whose gate the caller armed: the gate is
// lifted and the parked appends woken. The caller holds appendMu; the
// waiters hold no lock while they park, so the channel's close under it is
// what wakes them.
func (s *Stream) takeSettle() {
	if s.taking != nil {
		close(s.taking)
		s.taking = nil
	}
}

// Sync makes the writes of the active segment durable and advances the
// synced-through value the next records carry: the completed prefix is read
// under the lock, the descriptor fsynced outside it, and what the fsync
// therefore covered is recorded on the way back. The first Sync covers the
// segment header (R8), which completes before any reservation exists.
//
// It is the primitive the durability layer's Sync drives.
func (s *Stream) Sync() error {
	s.appendMu.Lock()
	if s.closed || s.id == 0 || s.ref == (Ref{}) {
		s.appendMu.Unlock()
		// No segment is active — or the stream has closed and released the
		// one it held: nothing to make durable and no ref to make it with.
		return nil
	}
	through := s.tracker.syncStart()
	ref := s.ref
	s.appendMu.Unlock()

	if err := ref.Sync(); err != nil {
		// The failure belongs to the durability layer's failed window: the
		// tracker keeps the prefix the failed fsync reached, and the caller
		// owns the records the window holds.
		return err
	}
	s.appendMu.Lock()
	s.tracker.syncDone(through)
	s.appendMu.Unlock()
	return nil
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
// in-flight ones — an in-flight take counted with them, for it runs inside an
// append — are waited for, so on return no append is still writing. The
// active segment's ref is released, so the cache's Shutdown is never blocked
// by a ref the stream forgot. A second Close is a no-op; the full close
// rules come with the seal and the roll.
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
