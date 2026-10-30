package index

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// RecordKind is the kind of an on-disk record the rebuild interprets. The
// values are the builder's own, in no on-disk order.
//
// decision: loss, forget, hold, unhold and stamp records are not kinds the
// rebuild interprets, so they have no value here and need not be fed to the
// builder. Those kinds change what earlier records mean by rules of their
// own — a loss record's reasons, a forget record's tag, a stamp's committed
// versions — and no rule of the index covers them. The recovery code that
// reads them decides and feeds what its decision needs: retained pieces
// through Retain, nothing for a record whose meaning ends on the disk.
// Overturned when those rules land in the index and must hold at the
// rebuild too.
type RecordKind uint8

const (
	RecWrite RecordKind = iota + 1
	RecFill
	RecRelease
	RecTruncate
	RecDeallocate
	RecDelete
	RecCloneTarget
	RecOffloaded
	RecUnmark
)

// Record is one record as recovery reads its header: what the rebuild needs
// of it, nothing more. Segment and At are where the record sits, for the
// drop ranges and the read-out; Off and Len are the header's file offset and
// length, which by kind are:
//
//	write, fill, release, offloaded, unmark: the extent the record covers
//	deallocate: the extent punched
//	truncate: Off is the new size, Len is 0
//	delete: both 0 — the removal runs to the end of the file
//	clone target: DstOff and Len of the spec
//
// A write or fill record's payload starts at its extent's first byte, so a
// rebuilt piece's Skip is 0.
type Record struct {
	Kind     RecordKind
	File     core.FileID
	Tag      core.ShareTag
	Segment  core.SegmentID
	At       uint32
	Off, Len int64
	Numbers  Numbers
	Clone    *core.CloneSpec // RecCloneTarget only
}

// errRebuilt is returned by Feed and Retain once Finish has begun: a shard
// it drained will never replay a buffer again, so a record or piece fed
// after it would be lost, and the call is refused rather than silently
// dropped. It is returned whether Finish succeeds or a replay errors.
var errRebuilt = errors.New("index: rebuild already finished")

// bufRec is one buffered record, compact beside Record: what the replay and
// the note pass need, with the tag the marks and notes read back.
type bufRec struct {
	kind    RecordKind
	tag     core.ShareTag
	seg     core.SegmentID
	at      uint32
	off, n  int64
	numbers Numbers
	clone   *core.CloneSpec
}

// keepBuf is one buffered retained piece, with its file.
type keepBuf struct {
	file core.FileID
	p    Retained
}

// rebuildShard is one shard of a rebuilder: the files hashing to it, each
// with its buffered records, and the retained pieces waiting for their
// files, behind one mutex held only to look up or append. Two shards never
// touch the same file, so their buffers replay independently.
type rebuildShard struct {
	mu sync.Mutex
	// drained marks the shard Finish has taken the buffers of, under mu. A
	// feeder arriving after that would append to a buffer nobody replays,
	// so Feed and Retain refuse it under the same lock instead of losing
	// the record with a nil.
	drained bool
	bufs    map[core.FileID][]bufRec
	keeps   []keepBuf
}

// rebuilder turns the records recovery finds on disk into the index,
// whatever order they were fed in.
//
// ponytail: every record is buffered, about 70 bytes each, until Finish, so
// a journal with 10^7 records holds about 700 MB while it rebuilds. Upgrade
// to per-segment partial indexes merged by precedence, or to a seq-aware
// insert, when a rebuild's peak memory shows in a profile of a large
// journal's open.
//
// decision: concurrent feeders, not per-segment partial results merged at
// Finish. The feeders appending to shard buffers is one lock per 1/64 of
// the FileIDs held for one append, and the replay is the runtime's own
// operations under each file's lock, so nothing new must hold at a rebuild.
// Merging partial indexes would trade the peak memory above for a second
// implementation of every runtime operation, one that takes precedence into
// itself. Revisit if a profile shows the shard lock contended by enough
// feeders on one shard.
type rebuilder struct {
	fs  *files
	ctr *Counters
	// shards are keyed like fs's: a file's buffers sit in the shard that
	// holds its entry, so Finish's workers and the file map agree on the
	// split.
	shards [fileShards]*rebuildShard
	// finishing claims the builder for Finish: one call wins the
	// CompareAndSwap and runs the rebuild — every other, from the moment
	// the claim lands to the end of the process, is refused with
	// errRebuilt before it touches anything, counters included. Only a
	// floor the check above already admitted reaches the claim, so the
	// claim is never released: nothing after it can leave the builder
	// open again.
	finishing atomic.Bool
}

// newRebuilder returns a builder that rebuilds into fs and ctr, the index
// and counters the caller built. The extent bound set on fs is the one the
// result respects — the rebuild itself never consults it, so a rebuild
// over the bound succeeds and reports the true count.
func newRebuilder(fs *files, ctr *Counters) *rebuilder {
	b := &rebuilder{fs: fs, ctr: ctr}
	for i := range b.shards {
		b.shards[i] = &rebuildShard{}
	}
	return b
}

// Feed buffers one record found on disk. A header that verified but names
// an impossible shape — an extent its kind cannot carry, an unknown kind, a
// spec that disagrees with its extent — is damage or another writer, so it
// is refused and nothing is buffered; the same is true of a record whose
// shard Finish has drained. The counters move only once the shard's buffer
// has taken the record, so a refused one raises neither. After Finish has
// begun it returns errRebuilt, whatever the record's shape.
func (b *rebuilder) Feed(r Record) error {
	// The claim runs before the validation, so a caller racing a Finish
	// hears the rebuild is over rather than a validation error the
	// record's shape did not earn — a record whose shard Finish is
	// currently replaying is refused here, not half-validated first.
	if b.finishing.Load() {
		return errRebuilt
	}
	switch r.Kind {
	case RecWrite, RecFill, RecRelease, RecOffloaded, RecUnmark:
		if r.Len == 0 {
			return fmt.Errorf("%w: a kind %d record with no extent", core.ErrUnknownFormat, r.Kind)
		}
	case RecDeallocate, RecCloneTarget:
		if r.Len == 0 {
			return core.ErrZeroLength
		}
		if r.Kind == RecCloneTarget && (r.Clone == nil || r.Clone.DstOff != r.Off || r.Clone.Len != r.Len) {
			return fmt.Errorf("%w: a clone target record whose spec disagrees with its extent", core.ErrUnknownFormat)
		}
	case RecTruncate:
		if r.Len != 0 {
			return fmt.Errorf("%w: a truncate record with a length", core.ErrUnknownFormat)
		}
	case RecDelete:
		if r.Off != 0 || r.Len != 0 {
			return fmt.Errorf("%w: a delete record with an extent", core.ErrUnknownFormat)
		}
	default:
		return fmt.Errorf("%w: record kind %d", core.ErrUnknownFormat, r.Kind)
	}
	if r.Kind != RecCloneTarget && r.Clone != nil {
		return fmt.Errorf("%w: a clone spec on a kind %d record", core.ErrUnknownFormat, r.Kind)
	}
	// A write or fill longer than a segment cannot sit in one: its payload
	// would run past the segment's end, and the piece's placement could
	// not name it.
	if (r.Kind == RecWrite || r.Kind == RecFill) && r.Len > core.SegmentCap {
		return fmt.Errorf("%w: a kind %d record longer than a segment", core.ErrUnknownFormat, r.Kind)
	}
	if r.Off < 0 || r.Len < 0 || r.Off > math.MaxInt64-r.Len {
		return fmt.Errorf("%w: record extent [%d, +%d)", core.ErrUnknownFormat, r.Off, r.Len)
	}
	s := b.shards[hashID(r.File)>>(64-6)]
	s.mu.Lock()
	if s.drained {
		s.mu.Unlock()
		return errRebuilt
	}
	// The record's numbers are observed only once its buffer has taken it:
	// a refused record raises no counter, so a caller draining after Finish
	// leaves the journal's next issue where the taken records left it.
	if err := b.ctr.Observe(r.Numbers); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.bufs == nil {
		s.bufs = make(map[core.FileID][]bufRec)
	}
	s.bufs[r.File] = append(s.bufs[r.File], bufRec{
		kind: r.Kind, tag: r.Tag, seg: r.Segment, at: r.At, off: r.Off, n: r.Len,
		numbers: r.Numbers, clone: r.Clone,
	})
	s.mu.Unlock()
	return nil
}

// Retain buffers a retained piece the caller computed, with the offloaded
// bit the caller's rules gave it: a mark of held content never sets or
// clears it, because the mark names the offered bytes, which are held or
// filled, and not the superseded ones under them. The caller owns the bits
// of the pieces it hands over, offloaded and unmark records included: it
// re-reads those records alongside the rest, since its rules — not the
// replay's — decide which of them name kept content.
//
// decision: retained extents enter the rebuild by injection, not by the
// builder's interpreting hold, unhold, stamp, offloaded or unmark records.
// Which cuts are live, which versions a stamp has committed, which
// supersedes keep content and which offloaded record covers kept content
// are the snapshot-hold rules, and no requirement of the index covers
// them, so the caller that has read those records decides what is retained
// and hands the pieces over with their bits. Overturned when the hold rules
// land and need the superseded pieces the replay cuts, in which case the
// replay grows a hook at the cut computing what a hold keeps.
//
// Once Finish has begun the piece is refused with errRebuilt: a piece the
// shard has already drained would never reach the file it names.
func (b *rebuilder) Retain(id core.FileID, r Retained) error {
	s := b.shards[hashID(id)>>(64-6)]
	s.mu.Lock()
	// Every shard is drained once Finish claims, so the drained check is
	// the whole lifetime: before the claim it admits, from the claim on it
	// refuses.
	if s.drained || b.finishing.Load() {
		s.mu.Unlock()
		return errRebuilt
	}
	s.keeps = append(s.keeps, keepBuf{file: id, p: r})
	s.mu.Unlock()
	return nil
}

// Finish applies everything fed, in the precedence of the records, and ends
// the builder. floor is the version floor the journal was opened with: the
// counters are raised above it, and every bucket starts at the higher of it
// and the highest version on disk, before any file is applied, so a file
// that settles mid-replay drops its change-version entry against a bucket
// already standing at the start. The shards replay in parallel; the first
// error a replay returns is kept and returned, and the shard that returned
// it stops while the others complete. A floor with a non-zero epoch is
// refused before the claim, so it changes nothing — not the counters, not
// the buckets, and no feeder is told the rebuild is over by a floor the
// builder went on to refuse — and the builder stays usable for a good
// floor, its buffers wherever the feeders left them. The loser of two good
// floors is refused before it raises anything.
func (b *rebuilder) Finish(floor core.Version) error {
	// The floor is checked before the claim: a refused floor never claims,
	// so a feeder racing a Finish that goes on to be refused is never
	// told the rebuild is over, and the builder stays open for a good one.
	if err := checkFloor(floor); err != nil {
		return err
	}
	// The claim resolves two racing Finishes on it alone: the loser is
	// refused before it raises the counters or stores anything, and an
	// openBuckets of its own could not lower the buckets the winner is
	// dropping entries against.
	if !b.finishing.CompareAndSwap(false, true) {
		return errRebuilt
	}
	// The floor was checked above, so this cannot refuse; the check is kept
	// rather than ignored with a blank, so a future reason it can refuses
	// loudly here instead of claiming a builder it cannot rebuild.
	if err := b.ctr.RaiseFloor(floor); err != nil {
		return err
	}
	// Every shard refuses feeds before the start is read, and each record
	// accepted before that was observed under its shard's lock — Observe
	// runs there — so the counters' high is the highest version on the
	// disk the rebuild replays, floor and feed together.
	for _, s := range b.shards {
		s.mu.Lock()
		s.drained = true
		s.mu.Unlock()
	}
	// RaiseFloor clamps the counter to the greatest version it ever
	// issues, so the start needs no clamp of its own.
	start := b.ctr.high.Load()
	b.fs.openBuckets(core.Version{Counter: start})

	workers := min(runtime.GOMAXPROCS(0), fileShards)
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	next := make(chan int)
	for range workers {
		wg.Go(func() {
			for i := range next {
				if err := b.replayShard(b.shards[i]); err != nil {
					once.Do(func() { first = err })
				}
			}
		})
	}
	for i := range b.shards {
		next <- i
	}
	close(next)
	wg.Wait()
	return first
}

// replayShard applies the buffers of every file in one shard. The shards
// were all marked drained by Finish before the workers started, so a feeder
// racing the replay is already refused by the drained check rather than
// left holding a record nobody replays.
func (b *rebuilder) replayShard(s *rebuildShard) error {
	s.mu.Lock()
	bufs := s.bufs
	keeps := s.keeps
	s.bufs, s.keeps = nil, nil
	s.mu.Unlock()

	byFile := make(map[core.FileID][]Retained)
	for _, k := range keeps {
		byFile[k.file] = append(byFile[k.file], k.p)
	}
	for id, recs := range bufs {
		// The buffer is freed as soon as the file is applied, so memory
		// falls while Finish runs.
		delete(bufs, id)
		if err := b.fs.update(id, func(f *file) error {
			return b.replayFile(f, id, recs, byFile[id])
		}); err != nil {
			return err
		}
		delete(byFile, id)
	}
	// A retained piece can name a file whose records are elsewhere — one
	// the caller holds pieces for but fed no record of. It lands as the
	// caller gave it: no mark of this replay reaches a retained piece.
	for id, pieces := range byFile {
		if err := b.fs.update(id, func(f *file) error {
			return b.replayFile(f, id, nil, pieces)
		}); err != nil {
			return err
		}
	}
	return nil
}

// extentOf is the extent a record's drop range is noted over: what its kind
// names, or the to-end removal a truncate or delete reaches.
func extentOf(r bufRec) core.Extent {
	switch r.kind {
	case RecTruncate:
		return toEnd(r.off)
	case RecDelete:
		return toEnd(0)
	default:
		return core.Extent{Off: r.off, Len: r.n}
	}
}

// replayFile rebuilds one file from its buffered records, under its lock.
func (b *rebuilder) replayFile(f *file, id core.FileID, recs []bufRec, retained []Retained) error {
	// A. Note every record's segment, tag and extent in the drop ranges,
	// in sequence order — the order the running journal notes records in
	// at append.
	//
	// decision: the drop ranges are noted in sequence order, as the running
	// journal notes them. Any two rebuilds of the same records give the
	// same ranges, whatever the feed order, because the sequence number
	// orders the notes alone. A rebuild against an incremental build is
	// compared for soundness only — every record covered, never under-
	// covering — because the ninth-range merge keeps the pair of the two
	// ranges with the smallest gap, which depends on the order records
	// arrive in, and the running journal notes a carried-forward record at
	// repack time while a rebuild notes it with its original numbers.
	// Overturned by a rule that keeps records rather than ranges, which
	// makes the merge's choice independent of arrival order.
	//
	// decision: every copy of a record is noted, its duplicates included.
	// Each copy sits in a segment that must keep the file's tag and its
	// ranges, so unnoting one would let that segment's unlink end the tag
	// while the copy is still on disk. Overturned by a rule that knows a
	// copy's segment by its id alone, which would not need the copy noted
	// to keep the unlink safe.
	//
	// decision: a rivalled file from a two-tag history is rebuilt rivalled,
	// not refused. The OtherShareError that noting a second tag returns
	// means two tags' records are on disk, and the file refusing every call
	// through either tag until one is forgotten is the correct rebuilt
	// state — the running journal's rival route reaches the same state only
	// through a fault, while a rebuild reaches it from records alone.
	// Overturned by a recovery rule that can tell which tag's records a
	// segment holds, which would let the rebuild keep the owner serving and
	// park the rival's records.
	notes := slices.Clone(recs)
	slices.SortStableFunc(notes, func(a, c bufRec) int {
		// The sequence number orders the notes; where two records carry
		// the same one — damage or another writer, the journal issues a
		// pair once — where they sit decides, so the noted order is a
		// total order and the owner among equal pairs does not depend on
		// the feed order.
		return cmp.Or(
			cmp.Compare(a.numbers.Seq, c.numbers.Seq),
			cmp.Compare(a.seg, c.seg),
			cmp.Compare(a.at, c.at),
		)
	})
	for _, r := range notes {
		_ = f.recorded(r.seg, extentOf(r), r.numbers, r.tag, id)
	}

	// B. Precedence order, a total order, so the replay below never
	// depends on the feed order.
	slices.SortFunc(recs, compareBufRec)

	// decision: records with equal numbers are one record, whatever its
	// feed count — a header-only record carried forward by repack, a
	// record re-appended after a failed sync whose original is still on
	// disk. The journal never reissues a pair of numbers, so two records
	// whose kind or extent differs can only be damage or another writer,
	// and they collapse too, into the greater (segment, offset); every
	// copy has been noted above, so the drop ranges lose nothing here.
	//
	// decision: of two copies of one record, the replay keeps the one at
	// the greater (segment, offset). Both carry the record's numbers, so
	// they decide every read the same way, and the key only makes the
	// rebuild deterministic. It is not always the newer home: a re-append
	// at open into an older repack-reserve spare can be the lower one.
	// Overturned if a read or a repack ever treats the two copies
	// differently, such as a copy in a segment marked untrusted on disk.
	kept := recs[:0]
	for i := 0; i < len(recs); {
		j := i + 1
		for j < len(recs) && sameRecord(recs[i], recs[j]) {
			j++
		}
		kept = append(kept, recs[j-1]) // the run's greatest (segment, offset)
		i = j
	}
	clear(recs[len(kept):])
	recs = kept

	// The retained pieces go in with the bits their caller computed: the
	// marks of this replay reach held pieces only, never them. Their By
	// and record location are the caller's too.
	for _, r := range retained {
		f.retained.add(r)
	}

	// C. Replay every record ascending. When one is applied, everything
	// already applied carries a smaller (version, seq), so a cut is exactly
	// "this record outranks it", and anything applied later outranks it:
	// last applied wins is exactly precedence.
	marks := make([]bufRec, 0, len(recs))
	for _, r := range recs {
		e := core.Extent{Off: r.off, Len: r.n}
		switch r.kind {
		case RecWrite:
			f.write(Placed{Extent: e, Record: RecordAt{Segment: r.seg, Offset: r.at}, Version: r.numbers.Version})
		case RecFill:
			// decision: a fill replays as a plain put. A fill record
			// covers exactly the extents that were absent when its journal
			// appended it, so where the runtime fill's only-where-absent and
			// marker masking decide the same bytes, the put is the simpler
			// replay: nothing lower in precedence sits inside the fill's
			// extent, and the markers above its version cut the put piece
			// by the same rule they masked the runtime fill with. Overturned
			// by a fill record whose extent names bytes its journal still
			// held when it appended the record, which the mask would have
			// left alone and a put supersedes.
			f.held.put(Piece{Extent: e, Record: RecordAt{Segment: r.seg, Offset: r.at}, Version: r.numbers.Version})
			// A fill's own mark, so an unmark's order against it matters
			// exactly as against a real offloaded record: filled content
			// enters the journal already offloaded, from the remote tier.
			marks = append(marks, r)
		case RecRelease:
			// decision: a release replays as an unconditional cut. The
			// record is a fact of what its journal released; the not-
			// offloaded refusal belongs to the call that appended it, which
			// checked the bits before it wrote. Precedence keeps the cut
			// right: content above the release's pair is applied after it
			// and lands over the cut span. Overturned by content the
			// running journal kept inside a released span at a pair the
			// release outranks, which the cut would wrongly drop.
			f.held.cut(e)
		case RecTruncate:
			f.truncate(r.off, r.numbers.Version)
		case RecDeallocate:
			if err := f.deallocate(e, r.numbers.Version); err != nil {
				return err
			}
		case RecDelete:
			f.delete(r.numbers.Version)
		case RecCloneTarget:
			if err := f.cloneTarget(*r.clone, r.numbers.Version); err != nil {
				return err
			}
		case RecOffloaded, RecUnmark:
			// D below: a mark holds no content, so it changes nothing but
			// bits.
			marks = append(marks, r)
		}
	}

	// D. The offloaded-bit pass. Applied in sequence order, the highest
	// sequence number among the marks whose version is at or above a
	// piece's version decides its bit, which is the fold of the format's
	// recovery rule. A mark never touches content newer than its version,
	// and a piece's own record seq plays no part: a repack copy keeps the
	// bit its content had.
	slices.SortStableFunc(marks, func(a, c bufRec) int {
		return cmp.Or(
			cmp.Compare(a.numbers.Seq, c.numbers.Seq),
			cmp.Compare(a.seg, c.seg),
			cmp.Compare(a.at, c.at),
		)
	})
	// decision: the marks reach the held set only, never retained pieces.
	// A mark names the bytes its record offered or unmarked, which are
	// held or filled, and a retained piece overlaps newer content by
	// construction: the v7 record says nothing about the v3 bytes it
	// superseded, and setting the older piece's bit would be the unsafe
	// direction — a later hold or offload reading the bit as a commit could
	// drop the piece before its snapshot's upload happens. At run time a
	// retained piece's bit is fixed at supersede time, when writeRetain
	// copies the bit of the content it keeps, and nothing marks one after;
	// the Retain caller sets the bits of the pieces it hands over.
	// Overturned by a journal rule that reports an offload commit for a
	// superseded version directly, which would reach retained pieces by
	// its own window and make the rebuild apply it here.
	for _, r := range marks {
		e := core.Extent{Off: r.off, Len: r.n}
		v := r.numbers.Version
		if r.kind == RecUnmark {
			f.held.unmark(e, v)
		} else {
			f.held.markOffloaded(e, core.Version{}, v)
		}
	}
	return nil
}

// compareBufRec orders two buffered records by precedence, then by where
// they sit: precedence names one record already — the journal issues a pair
// of numbers once — so the placement tie-break keeps the order total over
// distinct copies of one record, and two feeds of the same records replay
// identically.
func compareBufRec(a, b bufRec) int {
	return cmp.Or(
		a.numbers.Compare(b.numbers),
		cmp.Compare(a.seg, b.seg),
		cmp.Compare(a.at, b.at),
	)
}

// sameRecord reports whether a and b are one record carried in two places:
// the journal issues a pair of numbers to one record only, so equal numbers
// mean the same record, wherever each copy sits.
func sameRecord(a, b bufRec) bool { return a.numbers == b.numbers }
