package index

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// fileShards is the number of shards the file map is split into: 1<<6, the
// top six bits of a FileID's hash.
const fileShards = 64

// floorBuckets is the number of floor buckets: 1<<12, the top twelve bits of
// a FileID's hash.
//
// decision: floor buckets, not one journal-wide floor. With one floor, every
// delete in the journal refuses every in-flight fill of every file that holds
// nothing, so a delete-heavy share — a build tree's temporary files — keeps
// every other share's cold reads from filling. A bucket confines that to
// about one file in 4,096. Overturned by a measured refused-fill rate under
// a delete-heavy load that 4,096 buckets do not bring below one refusal per
// thousand fills.
const floorBuckets = 4096

// files maps each FileID to its file entry: the outer level of the placement
// index. It needs no ordering, so it is a hash map, split into shards by a
// hash of the FileID.
//
// A shard's lock guards only which entries the shard holds. It is taken to
// find, add or remove an entry and released before anything else: it is
// never held while waiting for a file's lock, nor across any work on an
// entry. Everything inside an entry is guarded by that entry's own lock, the
// file index domain for one file. So no lock spans the store, and a read of
// one file never waits on a write to another.
//
// decision: the map is split into 64 shards, each with a plain mutex, rather
// than kept behind one lock. One lock, even a read-write one held only for
// the map access, is a store-wide lock taken by every read of every file, and
// its shared counter is a cache line every reader writes. 64 shards spread
// that over 64 locks spaced 64 bytes apart. Revisit if a profile shows shard
// locks contended under many concurrent readers.
//
// decision: the file map and the file entry are unexported, like the rest of
// this package. Their only consumer until the journal's operations are wired
// is this package's tests, and the API is exported with the consumer that
// needs it.
type files struct {
	shards [fileShards]fileShard
	// buckets are the floor buckets, the change-version floor a file with no
	// entry of its own is checked against, one per 1/4096 of the FileIDs.
	// They grow only, by openBuckets and by the drops below, both of which
	// raise them.
	//
	// decision: a bucket keeps only a version's counter half, as Counters
	// does: in this format the epoch half is zero, and the counters refuse a
	// record, floor or draw with any other, so none reaches a bucket. That
	// holds while the format's epoch half stays zero: a format version that
	// raises the epoch must widen a bucket to a full version, or this table
	// needs a second word beside it.
	//
	// decision: retained extents do not keep the change-version entry:
	// retained content is no longer held, and the drop raises the bucket to
	// the entry's version, so a stale fill is still refused; the cost is that
	// the files sharing the bucket are checked against a higher version.
	// Overturned if a measured needless-refusal rate traces to drops of
	// files whose superseded content is still waiting for its offload.
	//
	// decision: an entry not changed since open is checked against its
	// bucket, not its own version — the top of the range openBuckets'
	// contract allows — so a read begun before the open cannot fill a file
	// that has an entry either. The cost is a needless refusal for a fill of
	// such a file racing a delete of another file in its bucket. Overturned
	// by a measured needless-refusal rate for exactly those fills, ones that
	// would otherwise be accepted.
	//
	// A bucket's top six bits are its shard, since both take the hash's top
	// bits: a bucket's files all share one shard lock. That is harmless,
	// since a bucket holds many files and no lock spans them longer than one
	// entry's update.
	buckets [floorBuckets]atomic.Uint64
	// opened is the bucket start set at open: the counter half of the higher
	// of the highest content version on disk and the floor. Zero marks a
	// fresh index, where no version has been opened above and the buckets
	// also stand at zero.
	opened uint64

	// dropRanges is the number of drop ranges over every file and segment.
	// It is maintained by apply, under each file's lock, so it stays exact
	// beside whatever an entry's ranges hold.
	dropRanges atomic.Int64
}

type fileShard struct {
	mu sync.Mutex
	m  map[core.FileID]*file
	_  [64 - unsafe.Sizeof(sync.Mutex{}) - unsafe.Sizeof(map[core.FileID]*file(nil))]byte
}

// file is one FileID's entry in the placement index.
//
// It has two lifetimes. The change-version entry, the file's change version
// and with it the held set, lasts while the file holds anything; when it
// holds nothing, the change version is dropped. The tag lasts longer, while
// any segment holds a record of the file, so a file whose every extent was
// released keeps its owner, as recovery would rebuild it from the records on
// disk. The entry leaves the map only when both have ended. Forgetting a
// tag, which drops it whole while its records are still on disk, is the one
// other way a tag ends, and clears it directly.
//
// decision: a file's state is guarded by a read-write lock, not kept in a
// copy-on-write tree that would give readers a stable view without one.
// Readers of other files never touch this lock, and a reader of this file
// waits only for one in-memory change, since no I/O is done holding it.
// Revisit if reads of a file under a heavy stream of writes to that same file
// show lock waits in a latency profile.
type file struct {
	mu sync.RWMutex
	// gone is set, under mu, when the entry leaves the map. A caller that
	// finds a gone entry after taking its lock looks the file up again.
	gone bool

	tagged bool
	tag    core.ShareTag
	// rival is a second tag the file has a record under, when rivalled. A
	// rivalled file refuses every call through either tag.
	rivalled bool
	rival    core.ShareTag

	held heldSet
	// nranges is the number of drop ranges over all of the file's segments,
	// maintained by recorded and segmentGone.
	nranges int
	// retained is the superseded content the file still keeps. It is never
	// consulted to answer a read.
	retained retainedSet
	// change is the file's change version. The zero Version means the file
	// has no change version of its own, which is also the case for a file
	// holding only filled content, since a fill changes no version: either
	// way a check against it uses the file's floor bucket.
	change core.Version
	// segs are the segments holding a record of the file, in no order, each
	// with its drop ranges.
	//
	// ponytail: a slice scanned on every record appended and on every drop
	// query, O(s) for the s segments holding the file. Upgrade to a map keyed
	// by segment when a profile shows the scan on the append path.
	segs []segRecords
	// markers are the file's unsettled removal markers, in version order.
	markers []marker
}

// hashID mixes a FileID into the hash the shard and the floor bucket are
// both taken from. Every byte of the FileID reaches the hash's top bits, so
// identifiers that differ only in a few bytes, such as time-ordered ones,
// still spread.
func hashID(id core.FileID) uint64 {
	b := id.Bytes()
	h := binary.LittleEndian.Uint64(b[:8])*0x9e3779b97f4a7c15 ^ binary.LittleEndian.Uint64(b[8:])
	h *= 0xbf58476d1ce4e5b9
	return h
}

// shard returns the shard holding id.
func (fs *files) shard(id core.FileID) *fileShard {
	return &fs.shards[hashID(id)>>(64-6)]
}

// bucketOf returns the index of id's floor bucket.
func bucketOf(id core.FileID) int { return int(hashID(id) >> (64 - 12)) }

// len returns the number of files with an entry.
func (fs *files) len() int {
	n := 0
	for i := range fs.shards {
		s := &fs.shards[i]
		s.mu.Lock()
		n += len(s.m)
		s.mu.Unlock()
	}
	return n
}

// openBuckets starts every bucket at start, the higher of the highest content
// version on disk and the floor, and records it as the version every entry
// not changed since is checked against. It is called once, at open, before
// any reader or writer runs. A start with a non-zero epoch never reaches it:
// the counters refuse one first.
//
// This is the only thing that seeds the buckets. Attaching a share later
// raises the counter above that share's floor but touches no bucket, so a
// share attached after open must seed the buckets itself if an asOf stored
// in one of its earlier incarnations is to stay refused here.
func (fs *files) openBuckets(start core.Version) {
	for i := range fs.buckets {
		fs.buckets[i].Store(start.Counter)
	}
	fs.opened = start.Counter
}

// checkedAgainst returns the version f, id's entry, is checked against: its
// own change version when it has changed since open, and its floor bucket
// otherwise — the top of the range the bucket-start rule allows, which keeps
// a read that began before the open from filling a file that has an entry
// too. The caller holds f's lock.
func (fs *files) checkedAgainst(f *file, id core.FileID) core.Version {
	if c := f.change.Counter; c > fs.opened {
		return f.change
	}
	return core.Version{Counter: fs.buckets[bucketOf(id)].Load()}
}

// asOf returns the change version a read of id reports: its entry's, or its
// bucket's when it has none or has not changed since open. It claims nothing
// and creates no entry, and it allocates nothing, so it may run on a read's
// hot path.
//
// decision: a bucket-based asOf can name a version drawn but not yet applied
// to the index. Two files share a bucket; the first's write draws its
// version and while the append is in flight the second draws a higher one,
// is deleted and settled, raising the bucket to it. A read of the first
// file, missing now, reports the raised bucket, and a change landed after
// that read at the lower version no longer refuses a fill of it, since the
// lower version is not above the asOf the read reported. The index cannot
// close this on its own: the caller must keep drawing a version and applying
// it to the index atomic with respect to a read taking its asOf, for
// example by taking the asOf under the append lock. Overturned if the index
// gains a view of drawn-but-unapplied versions.
func (fs *files) asOf(id core.FileID) core.Version {
	var v core.Version
	if !fs.view(id, func(f *file) { v = fs.checkedAgainst(f, id) }) {
		v = core.Version{Counter: fs.buckets[bucketOf(id)].Load()}
	}
	return v
}

// view runs fn on id's entry holding the file's lock for reading, so it runs
// beside other readers of the file and sees no write half made. It reports
// false, and runs nothing, when the file has no entry: a read claims nothing,
// so it never creates one. It can find an empty entry an update has just
// created and may yet drop, which fn sees holding nothing and owned by no
// tag. fn must not keep the entry past its return.
func (fs *files) view(id core.FileID, fn func(*file)) bool {
	s := fs.shard(id)
	for {
		s.mu.Lock()
		f := s.m[id]
		s.mu.Unlock()
		if f == nil {
			return false
		}
		f.mu.RLock()
		// A gone entry is no longer the file's: look again.
		if f.gone {
			f.mu.RUnlock()
			continue
		}
		f.read(fn)
		return true
	}
}

// read runs fn and releases the file's read lock, which the caller took, even
// if fn panics.
func (f *file) read(fn func(*file)) {
	defer f.mu.RUnlock()
	fn(f)
}

// update runs fn on id's entry holding the file's lock exclusively, creating
// the entry when the file has none. Once fn returns, whatever it returned,
// the entry's lifetimes are applied under the same lock: a file holding
// nothing loses its change version, and one that also has no record on disk
// leaves the map. So an update that leaves nothing behind leaves no entry.
func (fs *files) update(id core.FileID, fn func(*file) error) error {
	s := fs.shard(id)
	for {
		s.mu.Lock()
		f := s.m[id]
		if f == nil {
			if s.m == nil {
				s.m = make(map[core.FileID]*file)
			}
			f = &file{}
			s.m[id] = f
		}
		s.mu.Unlock()
		f.mu.Lock()
		if f.gone {
			f.mu.Unlock()
			continue
		}
		return f.apply(fs, s, id, fn)
	}
}

// apply runs fn and settles the entry's lifetimes, then releases the file's
// lock, which the caller took; all happen even if fn panics. The drop-range
// gauge is moved by what fn's ranges ended at, beside settle, so a command
// that panics still counts what it noted or dropped. The unlock is deferred
// first, so it runs last: even a panic in settle cannot leave the file
// locked.
func (f *file) apply(fs *files, s *fileShard, id core.FileID, fn func(*file) error) error {
	before := f.nranges
	defer f.mu.Unlock()
	defer func() {
		f.settle(fs, s, id)
		fs.dropRanges.Add(int64(f.nranges - before))
	}()
	return fn(f)
}

// settle applies the two lifetimes. The shard lock is taken after the file's,
// the one order in which both are ever held, and the entry is marked gone
// before it is removed, so a caller that found it before the removal sees
// the mark once it gets the lock.
//
// A held extent or an unsettled removal marker keeps the change-version
// entry: a file whose content was all removed still has a removal Since must
// yield until the caller settles it.
//
// decision: retained extents do not keep the change-version entry; see the
// decision at the bucket table. Retained extents keep the entry itself in
// the map, with its tag. That guard is defensive only: a retained piece's
// record keeps its segment noted, so the segments alone already keep the
// entry, unless a caller notes that segment gone too early. Then this guard
// keeps superseded content an offload has still to offer from being dropped
// with the entry.
//
// When the change-version entry drops — while retained content, a segment or
// nothing at all keeps the map entry — the file's bucket is raised to the
// entry's change version before the entry can be found missing and before
// any early return, so a read that began below that version is refused by
// the bucket rather than by the entry. The change version is zeroed only
// after the raise, and a reader that finds the entry gone — or was never
// given one — reads the raised bucket.
func (f *file) settle(fs *files, s *fileShard, id core.FileID) {
	if f.held.len() > 0 || len(f.markers) > 0 {
		return
	}
	raise(&fs.buckets[bucketOf(id)], f.change.Counter)
	f.change = core.Version{}
	f.held = heldSet{} // free the empty tree and its scratch for the tag-only life
	if len(f.segs) > 0 || f.retained.len() > 0 {
		return
	}
	f.gone = true
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

// check refuses a call on the file through tag, naming both tags, when
// another tag owns the file, and refuses a call through either tag of a file
// with records under two. It claims nothing, so a read uses it. The caller
// passes the FileID the refusal names.
func (f *file) check(tag core.ShareTag, id core.FileID) error {
	switch {
	case f.rivalled && tag == f.tag:
		return &core.OtherShareError{File: id, Tag: tag, Owner: f.rival}
	case f.tagged && tag != f.tag:
		return &core.OtherShareError{File: id, Tag: tag, Owner: f.tag}
	}
	return nil
}

// claim checks the file's tag against tag, as check does, and claims the
// file for tag when it has none.
//
// decision: a claim alone does not keep the entry; only a record on disk,
// held content or an unsettled marker does. The check that refuses a call is
// this one, made before the record's I/O, so a refused call appends nothing.
// It holds across the I/O only because the caller keeps the file's appends
// serialised from the claim until recorded notes the record, and recorded
// claims again, since a claim left alone is dropped when its update returns.
// So the lock that serialises a file's appends is not this entry's and does not live in it:
// an entry holding only a claim can be dropped and made again mid-append,
// and a lock inside it would go with it, letting two shares claim one new
// file. Revisit if a call must hold a claim across I/O without that
// serialisation.
func (f *file) claim(tag core.ShareTag, id core.FileID) error {
	if err := f.check(tag, id); err != nil {
		return err
	}
	if !f.tagged {
		f.tagged, f.tag = true, tag
	}
	return nil
}

// owner returns the tag that owns the file, and false when none does.
func (f *file) owner() (core.ShareTag, bool) { return f.tag, f.tagged }

// write holds p as a client write and raises the change version to its
// version. A file's writes take their versions in order, so the raise is a
// plain maximum and never lowers the change version.
//
// decision: a write consults no removal marker. A write draws its version
// inside the file's serialised append, so it is above every marker the file
// has, and no marker excludes it. A rebuild, which meets records in any
// order, must decide precedence before applying a write below a marker.
func (f *file) write(p Placed) {
	f.held.insert(p)
	if p.Version.Compare(f.change) > 0 {
		f.change = p.Version
	}
}

// changeVersion returns the file's change version, and false when the file
// has no change version of its own. It tells only what the entry holds; a
// check of a file goes through checkedAgainst or asOf, which fall to the
// bucket.
func (f *file) changeVersion() (core.Version, bool) {
	return f.change, f.change != core.Version{}
}

// recorded notes that seg holds a record of the file, covering e with pair n,
// appended through tag. It is called for every record appended for the file,
// and notes the record's extent and pair among the segment's drop ranges, so
// a record can never be on disk unnoted. It claims the file for tag, so no
// record is noted without an owner.
//
// The record is on disk by the time it is noted, so the segment is noted
// whatever the tag: a segment dropped here would end the file's tag while a
// record of it remains. A tag that does not own the file is reported with
// OtherShareError. The claim made before the I/O makes that unreachable, so
// reaching it means the file now has records of two tags on disk, which only
// a fault leaves: the file is marked rivalled, and from then on refuses every
// call through either tag, as recovery from those records would.
//
// decision: a rivalled file stays rivalled while its entry lasts. Forgetting
// one of the tags is what clears it, and the index cannot tell which tag's
// records a segment holds, so the last record of one tag going does not.
// Revisit if a fault is ever seen to leave a rivalled file whose second tag
// is never forgotten.
func (f *file) recorded(seg core.SegmentID, e core.Extent, n Numbers, tag core.ShareTag, id core.FileID) error {
	var s *segRecords
	for i := range f.segs {
		if f.segs[i].seg == seg {
			s = &f.segs[i]
			break
		}
	}
	if s == nil {
		f.segs = append(f.segs, segRecords{seg: seg})
		s = &f.segs[len(f.segs)-1]
	}
	f.nranges += s.note(e, n)
	err := f.claim(tag, id)
	if err != nil && !f.rivalled {
		f.rivalled, f.rival = true, tag
	}
	return err
}

// segmentGone notes that seg, unlinked or recycled, no longer holds a record
// of the file — and, with them, forgets the segment's drop ranges, so from
// here on no drop query is answered against seg's records. A segment the file
// does not know is a no-op.
//
// Call it only once the unlink or recycle is durable. A removal the drop
// query keeps only by a record of seg may be dropped the moment seg's ranges
// are gone, so a call made while the unlink could still be lost to a crash
// lets the removal drop while the records it covered can still come back with
// the segment, held again where zeros belong.
func (f *file) segmentGone(seg core.SegmentID) {
	for i := range f.segs {
		if f.segs[i].seg == seg {
			f.nranges -= len(f.segs[i].ranges)
			f.segs[i] = f.segs[len(f.segs)-1]
			f.segs = f.segs[:len(f.segs)-1]
			return
		}
	}
}

// mayOutrank reports whether a removal, release or loss record of the file in
// seg, covering e with pair n, may still outrank a record of the file in
// another segment: whether a range of another segment overlaps e with a
// lowest pair below n. False means the record outranks nothing on disk.
// Whether its marker is settled, and whether the unlinks it counted on are
// durable, are the caller's to check. O(s) for the s segments holding the
// file, at most maxDropRanges ranges each.
func (f *file) mayOutrank(seg core.SegmentID, e core.Extent, n Numbers) bool {
	if e.Len == 0 {
		// A zero-length extent covers no byte, so no record overlaps it.
		return false
	}
	lo, hi := e.Off, e.Off+e.Len
	for _, s := range f.segs {
		if s.seg == seg {
			continue
		}
		for _, r := range s.ranges {
			// A strictly lower pair only: an equal one is the record itself
			// or a carried copy of it, never a record it outranks.
			if r.overlaps(lo, hi) && r.low.Compare(n) < 0 {
				return true
			}
		}
	}
	return false
}

// mayOutrank answers the drop query for id on any entry it has. A file with
// no entry has no record on disk, so nothing outranks it and the answer is
// false. This is also how the query stays within one file.
func (fs *files) mayOutrank(id core.FileID, seg core.SegmentID, e core.Extent, n Numbers) bool {
	keep := false
	fs.view(id, func(f *file) { keep = f.mayOutrank(seg, e, n) })
	return keep
}

// dropRangeCount returns the number of drop ranges over every file and
// segment.
func (fs *files) dropRangeCount() int64 { return fs.dropRanges.Load() }
