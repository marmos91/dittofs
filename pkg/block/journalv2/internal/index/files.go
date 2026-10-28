package index

import (
	"encoding/binary"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// fileShards is the number of shards the file map is split into: 1<<6, the
// top six bits of a FileID's hash.
const fileShards = 64

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
	// count is the journal's placement index entries: held pieces, retained
	// extents and removal markers of every file, plus the entries admitted to
	// content writes not yet applied. In-flight admissions count, so a gauge
	// read mid-write includes them; abandon takes one back.
	//
	// limit is the configured bound on count, set before the index is first
	// used from the journal's settings.
	//
	// decision: refusals counts every refusal naming LimitIndex, splitting
	// removals included, so index_refusals has one meaning: every NoSpaceError
	// that names the index. It counts refusals at admission only: a write the
	// caller never admits is not counted, so it is the engine's count of
	// refusals it was asked to make. Revisit if a refusal is found made
	// outside admission.
	count    atomic.Int64
	limit    atomic.Int64
	refusals atomic.Uint64
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
	// retained is the superseded content the file still keeps. It is never
	// consulted to answer a read.
	retained retainedSet
	// change is the file's change version. The zero Version means the file
	// has none of its own, which is also the case for a file holding only
	// filled content, since a fill changes no version: either way a check
	// against it must use the file's floor instead.
	change core.Version
	// segs are the segments holding a record of the file, in no order.
	//
	// ponytail: a slice scanned on every record appended, O(s) for the s
	// segments holding the file, and nothing more than which segments those
	// are. Upgrade to per-segment entries kept in a map or sorted, carrying
	// the drop ranges of each, when the index tracks drop ranges or a
	// profile shows the scan on the append path.
	segs []core.SegmentID
	// markers are the file's unsettled removal markers, in version order.
	markers []marker
}

// entries returns the number of entries of the file the count is of: held
// pieces, retained extents and removal markers.
func (f *file) entries() int64 {
	return int64(f.held.len()) + int64(f.retained.len()) + int64(len(f.markers))
}

// shard returns the shard holding id. Every byte of the FileID reaches the
// top bits of the hash, which pick the shard, so identifiers that differ
// only in a few bytes, such as time-ordered ones, still spread.
func (fs *files) shard(id core.FileID) *fileShard {
	b := id.Bytes()
	h := binary.LittleEndian.Uint64(b[:8])*0x9e3779b97f4a7c15 ^ binary.LittleEndian.Uint64(b[8:])
	h *= 0xbf58476d1ce4e5b9
	return &fs.shards[h>>(64-6)]
}

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

// setExtentLimit sets the bound to n. A limit of 0 refuses every content
// write that adds an entry, so the journal never reaches an index no bound
// checks. A negative n refuses every content write as well, by the checks
// that compare against it: the journal's settings must reject one before
// the index is built, since no caller of the index can tell it an unset
// bound from a refusing one. Defaults are the settings' job, before the
// index exists.
//
// decision: the bound has no getter yet. Reading it back is the journal
// stats' job, which reports it as ExtentLimit alongside the count and the
// refusals; add the getter with the reader that needs it.
func (fs *files) setExtentLimit(n int64) { fs.limit.Store(n) }

// extentCount returns the number of entries of the journal's placement index
// the count is of: held pieces, retained extents and removal markers of
// every file, plus the entries admitted to content writes not yet applied.
func (fs *files) extentCount() int64 { return fs.count.Load() }

// indexRefusals returns the number of calls refused by the index's bound.
func (fs *files) indexRefusals() uint64 { return fs.refusals.Load() }

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
	return fs.commit(id, 0, fn)
}

// commit is update for an admitted op: admitted is the amount its admission
// reserved in the count, which the commit's entry delta instead of adding
// on top of. Everything else is as update.
//
// commit consumes the admission whether or not fn returns an error: the
// admitted amount is already taken out of the delta. A caller whose I/O fails
// before commit abandons the admission; one that commits must not abandon it
// as well, or the count stands too low.
func (fs *files) commit(id core.FileID, admitted int64, fn func(*file) error) error {
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
		before := f.entries()
		return f.apply(fs, s, id, before, admitted, fn)
	}
}

// apply runs fn, adds the entry change it made to the count and settles the
// entry's lifetimes, then releases the file's lock, which the caller took;
// all three happen even if fn panics.
//
// decision: the count is kept by the delta, not by a walk of the entry after
// fn. A rebuild or an open builds a populated index through one update a
// record, each about a walk of everything so far; the delta is O(1).
func (f *file) apply(fs *files, s *fileShard, id core.FileID, before, admitted int64, fn func(*file) error) error {
	defer f.mu.Unlock()
	defer f.counted(fs, before, admitted)
	defer f.settle(s, id)
	return fn(f)
}

// counted adds to the count the change fn's entries made: the difference of
// the entry's counts, taken after settle, which may drop the change-version
// entry whole, minus the amount its admission had already reserved. The
// result is the true delta whatever the pre-check predicted.
func (f *file) counted(fs *files, before, admitted int64) {
	fs.count.Add(f.entries() - admitted - before)
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
// decision: retained extents do not keep it. Whether they should is decided
// with the floor that dropping it raises, so for now they do not: retained
// content is no longer held.
//
// Retained extents also keep the entry itself in the map, with its tag. The
// guard is defensive only: a retained piece's record keeps its segment
// noted, so the segments alone already keep the entry, unless a caller notes
// that segment gone too early. Then this guard keeps superseded content an
// offload has still to offer from being dropped with the entry.
//
// The dropped change version is not recorded anywhere yet, so nothing here
// refuses a stale fill once the entry has gone: no check of a fill may be
// built on changeVersion until dropping the version raises the file's floor
// to it, which belongs at the line that zeroes it.
func (f *file) settle(s *fileShard, id core.FileID) {
	if f.held.len() > 0 || len(f.markers) > 0 {
		return
	}
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
// has no change version of its own.
func (f *file) changeVersion() (core.Version, bool) {
	return f.change, f.change != core.Version{}
}

// recorded notes that seg holds a record of the file, appended through tag.
// It is called for every record appended for the file, and once a segment is
// noted again it changes nothing. It claims the file for tag, so no record is
// noted without an owner.
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
func (f *file) recorded(seg core.SegmentID, tag core.ShareTag, id core.FileID) error {
	if !slices.Contains(f.segs, seg) {
		f.segs = append(f.segs, seg)
	}
	err := f.claim(tag, id)
	if err != nil && !f.rivalled {
		f.rivalled, f.rival = true, tag
	}
	return err
}

// segmentGone notes that seg, unlinked or recycled, no longer holds a record
// of the file.
func (f *file) segmentGone(seg core.SegmentID) {
	if i := slices.Index(f.segs, seg); i >= 0 {
		f.segs[i] = f.segs[len(f.segs)-1]
		f.segs = f.segs[:len(f.segs)-1]
	}
}
