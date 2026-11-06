package index

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// Index is the placement index of one journal: the exported handle over the
// file table. Every method takes only the lock of the file it names, and only
// for the one change or read it makes, so no call waits on another file and
// none does I/O.
type Index struct{ fs files }

// New returns an empty index that refuses a change past extentLimit entries.
func New(extentLimit int64) *Index {
	ix := &Index{}
	ix.fs.setExtentLimit(extentLimit)
	return ix
}

// WriteRetain publishes a client write of p to id through tag: it checks the
// file's owner, admits the write against the extent bound, notes the record
// with p's version and seq, and holds p, retaining what it supersedes as
// superseded by p's version. It takes the file's lock exclusively. A refusal
// holds nothing.
func (ix *Index) WriteRetain(tag core.ShareTag, id core.FileID, p Placed, seq core.Seq) (HeldDelta, error) {
	var err error
	if ix.fs.view(id, func(f *file) { err = f.check(tag, id) }) && err != nil {
		return HeldDelta{}, err
	}
	adm, err := ix.fs.admitWrite(id, p.Extent, true)
	if err != nil {
		return HeldDelta{}, err
	}
	return ix.fs.commit(id, adm, func(f *file) error {
		if err := f.recorded(p.Record.Segment, p.Extent, Numbers{Version: p.Version, Seq: seq}, tag, id); err != nil {
			return err
		}
		f.writeRetain(p)
		return nil
	})
}

// Lookup returns the held piece of id that covers off, and false when none
// does or the file has no entry. It takes the file's lock shared and creates
// no entry.
func (ix *Index) Lookup(id core.FileID, off int64) (Placed, bool) {
	var p Placed
	var ok bool
	ix.fs.view(id, func(f *file) { p, ok = f.held.lookup(off) })
	return p, ok
}

// Retained returns a copy of id's retained pieces, in offset, version and
// superseder order. It takes the file's lock shared and creates no entry.
func (ix *Index) Retained(id core.FileID) []Retained {
	var out []Retained
	ix.fs.view(id, func(f *file) { out = f.retained.list(nil) })
	return out
}

// SegmentBytes returns the bytes seg backs as held and as retained extents. It
// takes no file's lock.
func (ix *Index) SegmentBytes(seg core.SegmentID) SegmentBytes { return ix.fs.segmentBytes(seg) }
