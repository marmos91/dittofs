package index

import (
	"encoding/binary"
	"slices"
	"sync"
	"unsafe"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// fileShards is the number of shards the file map is split into.
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
}

func (fs *files) shard(id core.FileID) *fileShard {
	b := id.Bytes()
	h := binary.LittleEndian.Uint64(b[:8]) ^ binary.LittleEndian.Uint64(b[8:])
	return &fs.shards[h%fileShards]
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
		return f.apply(s, id, fn)
	}
}

// apply runs fn and settles the entry's lifetimes, then releases the file's
// lock, which the caller took; both happen even if fn panics.
func (f *file) apply(s *fileShard, id core.FileID, fn func(*file) error) error {
	defer f.mu.Unlock()
	defer f.settle(s, id)
	return fn(f)
}

// settle applies the two lifetimes. The shard lock is taken after the file's,
// the one order in which both are ever held, and the entry is marked gone
// before it is removed, so a caller that found it before the removal sees
// the mark once it gets the lock.
//
// decision: only held extents keep the change-version entry for now. Removal
// markers and retained extents keep it too once the index carries them; until
// then a file holds nothing exactly when its held set is empty.
//
// The dropped change version is not recorded anywhere yet, so nothing here
// refuses a stale fill once the entry has gone: no check of a fill may be
// built on changeVersion until dropping the version raises the file's floor
// to it, which belongs at the line that zeroes it.
func (f *file) settle(s *fileShard, id core.FileID) {
	if f.held.len() > 0 {
		return
	}
	f.change = core.Version{}
	if len(f.segs) > 0 {
		return
	}
	f.gone = true
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

// claim checks the file's tag against tag and claims the file for it when it
// has none. A file owned by another tag is refused, naming both, and keeps its
// owner. The caller passes the FileID the refusal names.
//
// decision: a claim alone does not keep the entry; only a record on disk or
// held content does. The check that refuses a call is this one, made before
// the record's I/O, so a refused call appends nothing. It holds across the
// I/O only because the caller keeps the file's appends serialised from the
// claim until recorded notes the record, and recorded claims again, since a
// claim left alone is dropped when its update returns. Revisit if a call must
// hold a claim across I/O without that serialisation.
func (f *file) claim(tag core.ShareTag, id core.FileID) error {
	if !f.tagged {
		f.tagged, f.tag = true, tag
		return nil
	}
	if f.tag != tag {
		return &core.OtherShareError{File: id, Tag: tag, Owner: f.tag}
	}
	return nil
}

// owner returns the tag that owns the file, and false when none does.
func (f *file) owner() (core.ShareTag, bool) { return f.tag, f.tagged }

// write holds p as a client write and raises the change version to its
// version. A file's writes take their versions in order, so the raise is a
// plain maximum and never lowers the change version.
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
// OtherShareError and leaves the owner in place. The claim made before the
// I/O makes that unreachable, so reaching it means the file now has records
// of two tags on disk, which only a fault leaves.
func (f *file) recorded(seg core.SegmentID, tag core.ShareTag, id core.FileID) error {
	if !slices.Contains(f.segs, seg) {
		f.segs = append(f.segs, seg)
	}
	return f.claim(tag, id)
}

// segmentGone notes that seg, unlinked or recycled, no longer holds a record
// of the file.
func (f *file) segmentGone(seg core.SegmentID) {
	if i := slices.Index(f.segs, seg); i >= 0 {
		f.segs[i] = f.segs[len(f.segs)-1]
		f.segs = f.segs[:len(f.segs)-1]
	}
}
