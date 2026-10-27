package index

import (
	"encoding/binary"
	"slices"
	"sync"

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
// that over more lines than a node has cores to contend for them. Revisit if
// a profile shows shard locks contended under many concurrent readers.
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
}

// file is one FileID's entry in the placement index.
//
// It has two lifetimes. The change-version entry, the file's change version
// and with it the held set, lasts while the file holds anything; when it
// holds nothing, the change version is dropped. The tag lasts longer, while
// any segment holds a record of the file, so a file whose every extent was
// released keeps its owner, as recovery would rebuild it from the records on
// disk. The entry leaves the map only when both have ended.
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
	// change is the file's change version, the zero Version when the file
	// has no change-version entry.
	change core.Version
	// segs are the segments holding a record of the file, in no order.
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
// so it never creates one. fn must not keep the entry past its return.
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
		if f.gone {
			f.mu.RUnlock()
			continue
		}
		fn(f)
		f.mu.RUnlock()
		return true
	}
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
		err := fn(f)
		f.settle(s, id)
		f.mu.Unlock()
		return err
	}
}

// settle applies the two lifetimes. The shard lock is taken after the file's,
// the one order in which both are ever held, and the entry is marked gone
// before it is removed, so a caller that found it before the removal sees
// the mark once it gets the lock.
//
// decision: only held extents keep the change-version entry for now. Removal
// markers and retained extents keep it too once the index carries them; until
// then a file holds nothing exactly when its held set is empty.
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
// version.
func (f *file) write(p Placed) {
	f.held.insert(p)
	if p.Version.Compare(f.change) > 0 {
		f.change = p.Version
	}
}

// changeVersion returns the file's change version, and false when the file
// has no change-version entry.
func (f *file) changeVersion() (core.Version, bool) {
	return f.change, f.change != core.Version{}
}

// recorded notes that seg holds a record of the file. It is called for every
// record appended for the file, and once a segment is noted again it changes
// nothing.
func (f *file) recorded(seg core.SegmentID) {
	if !slices.Contains(f.segs, seg) {
		f.segs = append(f.segs, seg)
	}
}

// segmentGone notes that seg, unlinked or recycled, no longer holds a record
// of the file.
func (f *file) segmentGone(seg core.SegmentID) {
	if i := slices.Index(f.segs, seg); i >= 0 {
		f.segs[i] = f.segs[len(f.segs)-1]
		f.segs = f.segs[:len(f.segs)-1]
	}
}
