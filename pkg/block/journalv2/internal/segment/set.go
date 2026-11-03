// Package segment holds the ledger of the segments the journal owns: the
// set of segments a stream has taken (active, sealed or ended), the spares
// made for the pool beside it, and every byte they occupy. Each set segment
// is accounted at its cap from the moment it is taken until its unlink,
// unless shortened at seal or by a torn-tail cut; each spare is accounted at
// the cap. The ledger is pure memory: no interface separates it from its
// callers, it performs no I/O, and the retirement executor, the seal and the
// spare pool hand it its facts.
//
// A segment's bytes leave the accounted footprint exactly once: out of it by
// an unlink (Retire), or into the spare pool by a recycle (Recycle, which
// moves it, not lowers it), in the step that removes the segment from the
// set, and only in the call that found it there. A spare's leave by
// DropSpare. A later call of any of these kinds is refused, counted as a
// double retire and logged at Warn.
package segment

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// State is where a segment of the set stands. Spares are not in the set:
// they are held beside it, because nothing appends to them and their bytes
// come back to the pool through the pool's own paths.
type State uint8

const (
	// Active is a segment a stream appends to.
	Active State = iota + 1
	// Sealed is a segment closed by a seal; never appended to again.
	Sealed
	// Ended is a segment ended by a failed append or sync, or by recovery;
	// never sealed.
	Ended
)

// The ledger's errors. Every operation validates its whole argument and the
// set's state before changing anything, so a refused call changes no counter
// and no map — with one exception the ledger's own rule writes down: a
// Retire or Recycle that finds its id out of the set counts a double retire
// and logs at Warn, which is the counter and the log the refusal exists to
// raise.
var (
	// ErrNotInSet is the refusal of an operation naming a segment the set
	// does not hold (or a Take of an id that is no spare): the double retire
	// this package counts, a release arrived too late, a claim on a segment
	// already retired.
	ErrNotInSet = errors.New("segment: not in the set")
	// ErrWrongState is the refusal of an operation naming a segment in a
	// state it does not serve: retiring a segment a stream still appends to,
	// sealing one already sealed, accounting a record into a sealed or ended
	// one.
	ErrWrongState = errors.New("segment: wrong state for the operation")
	// ErrDuplicateID is the refusal of an add naming a segment id the ledger
	// already owns, in the set or among the spares.
	ErrDuplicateID = errors.New("segment: id already owned")
	// ErrBadSize is the refusal of a size that accounts no storage or more
	// than the segment holds: a shrink that grows or empties, a record of
	// no bytes, a recycle of a seal that never said full, whatever its size.
	ErrBadSize = errors.New("segment: bad size")
	// ErrBadID is the refusal of the id that names no segment: the ledger
	// never reserves or moves storage under it, because no segment file can
	// carry it and an id that names nothing cannot be returned by a retire.
	ErrBadID = errors.New("segment: bad id")
	// ErrMismatch is what Check reports once the accounted footprint and the
	// sum the set's sizes make of it differ.
	ErrMismatch = errors.New("segment: accounted footprint does not match the set")
)

// Counters are the ledger's counters, read without a walk: the writing side
// places a call on its own path for every counter it needs, and none may
// walk the set to answer. DoubleRetires counts every retire, recycle or spare
// drop that found its id already gone — two reclamation paths selecting one
// segment or one spare, the race the idempotence rule counts.
type Counters struct {
	// UsedBytes is the accounted footprint: every segment of the set at its
	// accounted size, plus every spare at the segment cap. It is raised
	// before a spare is allocated and lowered only after an unlink has
	// happened, so it never runs below the storage actually in use.
	UsedBytes int64
	// Segments is the number of segments in the set.
	Segments int64
	// Sealed is the number of sealed segments in the set.
	Sealed int64
	// Spares is the number of spares the ledger holds.
	// decision: it counts every spare the ledger holds; RFC 1 §3.7's Spares
	// gauge excludes the headroom and repack-reserve spares, which the pool
	// separates into classes because only content spares answer a roll — and
	// the pool is not built yet, so the classes have nothing to hand over.
	// Withdrawn when the pool lands and reports the content class.
	Spares int64
	// DoubleRetires counts retires, recycles and spare drops that found the
	// segment or spare out of the set. A retire that instead finds an Active
	// segment is refused without a count: that is a different bug (see
	// Retire).
	DoubleRetires uint64
}

// Set is the ledger of the segments the journal owns and of every byte they
// occupy.
//
// decision: the set, the footprint and the spare counts sit under the
// capacity lock domain of RFC 1 §8.2 — store-wide counters, no I/O under the
// mutex, nothing acquired while holding it, and never held across anything
// but map and counter work. Overturning this splits the counting by domain,
// not the ledger itself, and fails only if the lock recorder of record ever
// finds a leaf held above it.
type Set struct {
	mu     sync.Mutex
	cap    int64
	log    *slog.Logger
	segs   map[core.SegmentID]*entry
	spares map[core.SegmentID]struct{}
	tags   map[core.ShareTag]int64

	// c holds the counters of Counters above, maintained at each event that
	// moves one and never rebuilt: every read is one lock and a copy.
	c Counters
}

// entry is one segment of the set: its state, the size it is accounted at,
// its claim flag, and the record bytes per share tag it holds. The claim
// lives here so it leaves with the segment the day it is retired. The record
// bytes and the size are one invariant: what the segment holds never exceeds
// what it is accounted at, so a record that would break the bound is refused
// where it is charged, and a cut that would break it is refused where the
// size is lowered. Its fields are guarded by the mu of the Set that holds it,
// and only that Set reaches an entry — the capacity lock domain its owner
// names covers it.
type entry struct {
	state   State
	size    int64
	claimed bool
	// full is what the seal named: true only for a segment closed at its cap
	// by a full seal, never for one the seal named early — whose truncation
	// may have failed and whose size cannot tell the two apart.
	full bool
	// held is the total record bytes the entries below hold for every share
	// together — the running bound against size, not a derived sum.
	held int64
	// tags is nil until the first record byte is accounted to the segment.
	tags map[core.ShareTag]int64
}

// absentMessage is the Warn of a retire or recycle that subtracted nothing.
const absentMessage = "segment: retire found the segment out of the set"

// NewSet returns an empty ledger that accounts every spare at segCap, the
// segment cap. A cap that is not positive names no storage a spare could be
// accounted at, so it is a caller bug and panics. A nil logger keeps the
// ledger silent — each call logs nothing, and every Warn line is suppressed.
func NewSet(segCap int64, log *slog.Logger) *Set {
	if segCap <= 0 {
		panic(fmt.Sprintf("segment: the cap %d is not a size a spare can be accounted at", segCap))
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Set{
		cap:    segCap,
		log:    log,
		segs:   make(map[core.SegmentID]*entry),
		spares: make(map[core.SegmentID]struct{}),
		tags:   make(map[core.ShareTag]int64),
	}
}

// ownsLocked reports whether the id is already in the set or among the
// spares. Only ids the ledger does not own may be added.
func (s *Set) ownsLocked(id core.SegmentID) bool {
	if _, ok := s.segs[id]; ok {
		return true
	}
	_, ok := s.spares[id]
	return ok
}

// AddSpare carries the step that reserves cap for a fresh spare against the
// journal's maximum, max, raising UsedBytes before the allocation it accounts
// for happens. The reservation and the check of the maximum are one critical
// section under the ledger's mutex, so two makes near the maximum never both
// pass — the answer is not a read of Counters followed by a decision outside
// the lock. A refusal returns the NoSpaceError the maximum owes — Limit
// LimitJournal, Missing what the reservation lacked, Unit bytes — and changes
// nothing: no counter, no spare, and it is not counted a double retire.
//
// AddSpare is called before the spare's creation; the allocation itself is
// the caller's, and a spare whose making fails is returned by DropSpare once
// its unlink has happened, so the raise always comes first. A leftover the
// unlink cannot remove is carried into the set with Take and ended instead —
// a spare cannot go back to being one — and a later retire takes it.
//
// decision: the maximum is checked against the footprint the ledger holds,
// and a reservation held against the maximum outside the ledger counts only
// once it moves into this critical section, never by being read beside it.
// A max <= 0 therefore refuses every spare, with no special case. Overturn
// this only if a caller must reserve against the maximum without a spare,
// which would need a call of its own rather than a read of UsedBytes.
func (s *Set) AddSpare(id core.SegmentID, max int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id == 0 {
		return ErrBadID
	}
	if s.ownsLocked(id) {
		return ErrDuplicateID
	}
	// The overflow-safe form of UsedBytes+cap > max: a max below the cap
	// refuses here without any wraparound, and Missing reads what the
	// reservation lacked whatever the figures.
	if s.cap > max || s.c.UsedBytes > max-s.cap {
		return &core.NoSpaceError{
			Limit:   core.LimitJournal,
			Missing: s.c.UsedBytes + s.cap - max,
			Unit:    core.UnitBytes,
		}
	}
	s.spares[id] = struct{}{}
	s.c.UsedBytes += s.cap
	s.c.Spares++
	return nil
}

// Take carries a spare into the set, as the segment a stream appends to. The
// storage is already accounted, so the footprint does not move: it moves only
// when a segment is created, unlinked or shortened.
func (s *Set) Take(id core.SegmentID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.spares[id]; !ok {
		return ErrNotInSet
	}
	delete(s.spares, id)
	s.c.Spares--
	s.c.Segments++
	s.segs[id] = &entry{state: Active, size: s.cap}
	return nil
}

// Seal closes a segment to further appends, at the size it is accounted at:
// a full seal keeps the cap, an early one is followed by Shrink to the
// truncated size. The full flag is the seal's own word for itself, and the
// only fact a later recycle can trust: an early seal whose truncation sync
// failed stays Sealed and accounted at its cap (the truncation never became
// durable), so a size at the cap does not name a full seal.
func (s *Set) Seal(id core.SegmentID, full bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.segs[id]
	if !ok {
		return ErrNotInSet
	}
	if e.state != Active {
		return ErrWrongState
	}
	e.state = Sealed
	e.full = full
	s.c.Sealed++
	return nil
}

// End closes a segment a failed append or sync left, keeping the cap it is
// accounted at until it is unlinked: an ended segment is never sealed, and
// its records leave with the segment when it is retired.
func (s *Set) End(id core.SegmentID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.segs[id]
	if !ok {
		return ErrNotInSet
	}
	if e.state != Active {
		return ErrWrongState
	}
	e.state = Ended
	return nil
}

// Shrink lowers the accounted size of a segment of the set that no stream
// appends to anymore: what a seal's truncation or a torn-tail cut of an
// ended segment takes from the file comes off the footprint here. A segment a
// stream still appends to is refused: its bytes sit at the cap from its
// takeover to its seal or its end, and a cut before either would sit the
// footprint below the storage the file keeps allocating, a break no check of
// the set's own fields can see. The deducted difference is the caller's to
// name; only the accounting is the ledger's.
//
// decision: Active is refused rather than served, because both cuts the
// format knows — the seal's truncation and the torn-tail cut of an ended
// segment — run after the stream left the segment, so a shrink naming an
// Active one is a caller bug, and serving it would let the footprint fall
// below the allocated storage with the ledger none the wiser. Withdrawn if a
// cut of an appended-to segment ever exists, which a marker from the stream
// side would have to keep above the append's bytes.
//
// decision: the Ended case is served, and recovery's torn-tail cut (RFC 28
// §3.3) is the only caller meant to reach it — an ended segment keeps its cap
// until its unlink, and a runtime failed-append ended segment has no cut of
// it coming. The ledger cannot tell the two callers apart, so the caller
// contract holds the ceiling: a Shrink of an ended segment that never came
// from recovery keeps the accounted size below the file, silent to every
// check. Withdrawn if a runtime caller ever needs its own cut, which must
// then carry a fact the ledger can gate on.
//
// decision: the Sealed case is served any number of times, because the
// ledger cannot see the file. The seal writer makes one cut by contract — it
// shrinks once, after the truncation is durable — but a caller that shrank an
// early-sealed segment again would keep the footprint below the file with
// Check still nil, and the ledger would not know; a cut flag cannot hold this
// either, because a legitimate path (recovery's torn-tail cut of an ended
// segment, later truncated past its trailer) can cut twice. Withdrawn if a
// caller ever needs its second cut: it must carry a fact the ledger can gate
// on, or the sequences both callers make become known and a flag can count
// cuts.
func (s *Set) Shrink(id core.SegmentID, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.segs[id]
	if !ok {
		return ErrNotInSet
	}
	if e.state == Active {
		return ErrWrongState
	}
	// A full seal has no cut of the file behind it: nothing was truncated,
	// so there is no cut to account, and only the seal the caller named early
	// ever serves its truncation here.
	if e.full && e.state == Sealed {
		return ErrWrongState
	}
	if size <= 0 || size > e.size {
		return ErrBadSize
	}
	// The cut may lower the size only as far as the record bytes the segment
	// still holds: below them the set would charge shares for storage the
	// segment no longer accounts for. Both cuts the format knows — a seal's
	// truncation and the torn-tail cut of an ended segment — run after the
	// segment took its last record, and each leaves the size at or above
	// those bytes, so a lower cut is a caller bug.
	if size < e.held {
		return ErrBadSize
	}
	s.c.UsedBytes -= e.size - size
	e.size = size
	return nil
}

// AddRecord accounts n record bytes — a record's header, payload and padding
// together — to the share whose tag the record carries, in an Active segment
// of the set. The footprint does not move: the bytes are inside storage the
// segment already accounts for, so they count against the share's record
// bytes, not against the journal's allocation.
//
// ponytail: one mutex serves every append's tag accounting, so a mesh of
// streams on many files funnels into one lock the hotter it gets. Ceiling:
// contention on it, visible as a flat profile with every append's owner
// waiting here. Upgrade to per-segment counters per tag once a profile shows
// it, and let the retire and recycle paths fold them back into each share's
// total before releasing its per-tag bytes.
func (s *Set) AddRecord(id core.SegmentID, tag core.ShareTag, n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.segs[id]
	if !ok {
		return ErrNotInSet
	}
	if e.state != Active {
		return ErrWrongState
	}
	if n <= 0 {
		return ErrBadSize
	}
	// The record's bytes sit inside storage the segment already accounts for,
	// so the running total of its record bytes cannot pass its size: a
	// record that would take it past charges a share for storage the segment
	// does not have, and is refused before anything moves.
	if e.held+n > e.size {
		return ErrBadSize
	}
	if e.tags == nil {
		e.tags = make(map[core.ShareTag]int64)
	}
	e.held += n
	e.tags[tag] += n
	s.tags[tag] += n
	return nil
}

// ShareBytes returns the record bytes of the share's tag still held in
// segments of the set — one map read, no walk of the set.
func (s *Set) ShareBytes(tag core.ShareTag) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tags[tag]
}

// Claim takes a retirement's claim on a segment of the set that no stream
// can still append to. It is held until Release clears it, or until the
// segment is retired, in which case the claim is gone: a release or a new
// claim on a retired segment finds nothing and takes nothing.
func (s *Set) Claim(id core.SegmentID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.segs[id]
	if !ok || e.state == Active || e.claimed {
		return false
	}
	e.claimed = true
	return true
}

// Release clears a claim. Releasing what is not claimed, or a segment already
// retired — whose claim left with it — changes nothing.
//
// decision: releasing a retired id reaches nothing and clears nothing, so the
// claim of a retired segment cannot be erased by a late release — and the
// same holds for the fresh id a recycle minted, because ids are never reused
// under a live set: a segment's id starts at 1 and ascends, and the id of a
// new spare or a recycle exceeds every id in the directory (RFC 28 §2.2). A
// reused id could make this release land on the new segment that owns it, so
// the holder's own claim checks — which read the flag and the state together —
// are the ceiling that holds until then. Withdrawn if ids ever become
// reusable, in which case the claim must carry the id of the retired segment
// rather than reaching by it.
func (s *Set) Release(id core.SegmentID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e, ok := s.segs[id]; ok {
		e.claimed = false
	}
}

// DropSpare returns a spare's cap after the spare's unlink has happened: like
// Retire, it is the ledger's half of a reclamation, called once the storage
// side is done, and only the call that finds the id among the spares
// subtracts. A spare is not a segment — no stream ever appended to it — so
// its removal touches the spare count and the footprint and nothing else.
//
// The id that names no spare counts itself a double retire and logs at Warn,
// exactly as Retire's does: the pool and a failed make's cleanup can both
// select one spare, and the second drop finds it gone. A segment of the set
// is refused uncounted, for the same reason Retire refuses a spare: the caller
// named live storage by the wrong door, and counting that would drown the
// race the count exists to watch.
func (s *Set) DropSpare(id core.SegmentID) (freed int64, err error) {
	s.mu.Lock()
	if _, ok := s.spares[id]; !ok {
		if _, seg := s.segs[id]; seg {
			s.mu.Unlock()
			return 0, ErrWrongState
		}
		s.c.DoubleRetires++
		s.mu.Unlock()
		s.log.Warn(absentMessage, "segment", uint64(id), "op", "drop")
		return 0, ErrNotInSet
	}
	delete(s.spares, id)
	s.c.Spares--
	s.c.UsedBytes -= s.cap
	s.mu.Unlock()
	return s.cap, nil
}

// Retire carries the unlink step of a retirement: it removes a segment of
// the set — Sealed or Ended — subtracts its accounted size from the footprint
// and releases each of its record bytes from the share it was accounted to.
// Only the call that finds the id in the set subtracts: the reclamation
// executor calls on the one hand and the retire queue on the other can both
// select one segment, and the second retire finds its bytes already gone,
// subtracts nothing, counts itself as a double retire and logs at Warn.
//
// decision: a segment still Active, and a spare of the pool, are refused
// rather than retired, and neither refusal is counted as a double retire.
// A retire of one means the caller's selection named a segment a stream
// still appends to, or arrived at a spare by the wrong door — a spare goes
// out through DropSpare — and both are a bug in the caller, not the race
// the count watches for, and counting either would drown that signal. The
// ledger is the only side that knows which ids are spares, so the refusal
// belongs here rather than in a caller's pre-check. Withdrawn if a caller
// ever needs to see either as a count.
func (s *Set) Retire(id core.SegmentID) (freed int64, err error) {
	s.mu.Lock()
	e, ok := s.segs[id]
	if !ok {
		if _, spare := s.spares[id]; spare {
			s.mu.Unlock()
			return 0, ErrWrongState
		}
		s.c.DoubleRetires++
		s.mu.Unlock()
		s.log.Warn(absentMessage, "segment", uint64(id), "op", "retire")
		return 0, ErrNotInSet
	}
	if e.state == Active {
		s.mu.Unlock()
		return 0, ErrWrongState
	}
	freed = e.size
	s.removeLocked(id, e)
	s.mu.Unlock()
	return freed, nil
}

// Recycle carries the recycle step of a retirement: it removes a Sealed
// segment accounted at its full cap from the set and moves its storage to
// the spare pool under a fresh id, so the footprint does not move at all —
// the segment's bytes stay in use, now a spare's — while each of its record
// bytes is released to the share it was accounted to, exactly once. Every
// refusal is checked before anything changes: a recycle that fails has taken
// nothing and given nothing.
//
// decision: a recycle is a retire of the old id plus a new spare in one
// step, so the accounted footprint does not move and only an unlink of
// either the segment or the spare ever lowers it, while the spare pool grows
// by one. Overturning this only when a spare can be created smaller than a
// cap, which the segment format today forbids. A spare of the pool is
// refused uncounted, for the same reason as Retire's.
func (s *Set) Recycle(old, fresh core.SegmentID) error {
	s.mu.Lock()
	e, ok := s.segs[old]
	if !ok {
		if _, spare := s.spares[old]; spare {
			s.mu.Unlock()
			return ErrWrongState
		}
		s.c.DoubleRetires++
		s.mu.Unlock()
		s.log.Warn(absentMessage, "segment", uint64(old), "op", "recycle")
		return ErrNotInSet
	}
	if e.state != Sealed {
		s.mu.Unlock()
		return ErrWrongState
	}
	// Only a seal that said full names a segment whose file still holds the
	// whole cap — an early seal whose truncation sync failed stays Sealed at
	// the cap too, and recycling either would hand a truncated file to the
	// pool as a full spare (RFC 28 §2.5). A full seal is also the only seal
	// Shrink leaves at the cap: it refuses a full-sealed segment outright, so
	// the seal's own word is the whole test and the size is read nowhere.
	if !e.full {
		s.mu.Unlock()
		return ErrBadSize
	}
	if fresh == 0 {
		s.mu.Unlock()
		return ErrBadID
	}
	if s.ownsLocked(fresh) { // ownsLocked(fresh) covers fresh == old: old was found in segs
		s.mu.Unlock()
		return ErrDuplicateID
	}
	s.removeLocked(old, e)
	s.spares[fresh] = struct{}{}
	s.c.UsedBytes += s.cap
	s.c.Spares++
	s.mu.Unlock()
	return nil
}

// removeLocked takes a segment out of the set and releases what it held:
// its accounted size from the footprint, its claim (which dies with the
// entry), and each of its per-tag record bytes from the share it was
// accounted to, deleting the shares that reach zero so the ledger stays
// bounded by live state, not by history. The caller holds mu and has already
// found the entry in the set.
func (s *Set) removeLocked(id core.SegmentID, e *entry) {
	delete(s.segs, id)
	if e.state == Sealed {
		s.c.Sealed--
	}
	s.c.Segments--
	s.c.UsedBytes -= e.size
	for tag, n := range e.tags {
		left := s.tags[tag] - n
		if left == 0 {
			delete(s.tags, tag)
		} else {
			// A negative leftover can only be a release the totals never
			// held: left in place for Check to report rather than deleted.
			s.tags[tag] = left
		}
	}
}

// Counters returns the ledger's maintained counters. One lock, one struct
// copy, no walk of the set and no allocation.
func (s *Set) Counters() Counters {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c
}

// Check walks the set and reports whether the accounted footprint is the sum
// of every segment's accounted size plus every spare at the segment cap,
// whether each segment's held record bytes match the bytes its own entries
// name and fit its accounted size, and whether each maintained count matches
// what it is counted from. It is the verification of last resort, called by
// recovery's checks and by tests, not on any write path, and reports a
// mismatch as an error naming both figures wrapped around ErrMismatch.
func (s *Set) Check() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		sum      int64
		segments int64
		sealed   int64
	)
	for id, e := range s.segs {
		// The total and its own per-tag map are two statements of one fact: a
		// drifted total admits a record that does not fit, so the check sums
		// the tags and takes neither statement's word. Above the size is the
		// same break from the other side.
		var tot int64
		for _, n := range e.tags {
			tot += n
		}
		if tot != e.held || e.held > e.size {
			return fmt.Errorf("%w: segment %d holds %d record bytes, its entries make %d and it is accounted at %d",
				ErrMismatch, uint64(id), e.held, tot, e.size)
		}
		sum += e.size
		segments++
		if e.state == Sealed {
			sealed++
		}
	}
	for range s.spares {
		sum += s.cap
	}

	// Every share's total is the sum of the bytes its segments' entries hold
	// for it: the walk sums each entry's own tags into a fresh map and
	// compares, so a release that dropped or doubled one — or a charge that
	// reached a share's total without its segment's entry — is a broken
	// ledger the check reports here. A share that reaches zero leaves the
	// ledger's map, so it also holds no entry to sum: the two maps agree in
	// length first, then byte for byte.
	held := make(map[core.ShareTag]int64)
	for _, e := range s.segs {
		for tag, n := range e.tags {
			held[tag] += n
		}
	}

	spares := int64(len(s.spares))
	tagsMatch := len(held) == len(s.tags)
	if tagsMatch {
		for tag, n := range held {
			if s.tags[tag] != n {
				tagsMatch = false
				break
			}
		}
	}
	if s.c.UsedBytes == sum &&
		s.c.Segments == segments &&
		s.c.Sealed == sealed &&
		s.c.Spares == spares &&
		tagsMatch {
		return nil
	}
	if s.c.UsedBytes != sum {
		return fmt.Errorf("%w: accounted %d, the set's sizes make %d",
			ErrMismatch, s.c.UsedBytes, sum)
	}
	if !tagsMatch {
		return fmt.Errorf("%w: the shares' record bytes do not match the segments' entries: %v vs %v",
			ErrMismatch, s.tags, held)
	}
	return fmt.Errorf("%w: the counts do not match the set: %d segments vs %d, %d sealed vs %d, %d spares vs %d",
		ErrMismatch, s.c.Segments, segments, s.c.Sealed, sealed, s.c.Spares, spares)
}
