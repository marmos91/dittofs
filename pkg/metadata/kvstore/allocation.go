package kvstore

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// allocationWindow is the first extent Allocation asks the covering lookup
// for; each further window doubles, so a run of any length costs
// O(log length) lookups.
const allocationWindow = 64 << 20

// Allocation answers SEEK_DATA, SEEK_HOLE and allocated-range queries: the
// longest run from off that is all hole or all data, and which. A hole is
// a hole record or a zero ref; a zero ref under a newer overwrite record is
// data, since its current bytes are the overwrite's. Past the end of file
// the run is empty.
func (e Existence) Allocation(ctx context.Context, share entity.ShareID, file entity.FileID, off int64) (run entity.Extent, hole bool, err error) {
	run = entity.Extent{Offset: off}
	err = e.s.kv.View(ctx, func(r kv.Reader) error {
		at, first := off, true
		for w := int64(allocationWindow); ; w = min(w*2, entity.EndOfFile/2) {
			spans, err := covering(r, share, file, entity.Extent{Offset: at, Length: w})
			if err != nil || len(spans) == 0 {
				return err
			}
			for _, s := range spans {
				h := s.Class == entity.Hole || s.Class == entity.Carved && s.Ref.IsZeroRef()
				if first {
					hole, first = h, false
				} else if h != hole {
					return nil
				}
				run.Length = s.End() - off
			}
			at = spans[len(spans)-1].End()
		}
	})
	return run, hole, err
}

// PruneOverwrites deletes the file's overwrite records that nothing needs
// any more: every part of the record's extent is covered by a ref at or
// above its version, or lies in a hole or past size. A part no ref covers
// keeps the record, since an offer in flight older than it may still
// commit there. It runs on the existence path, so it reads the existence
// fence, and reads the offload fence, tracked, so no offload commit lands
// between its read of the refs and its deletes. A record a removal not done
// masks is left for the removal to drop. It deletes at most the key budget
// per call and reports whether it stopped short.
//
// Pruning is housekeeping: a record never pruned costs lookup work, never
// a wrong answer.
func (e Existence) PruneOverwrites(ctx context.Context, share entity.ShareID, file entity.FileID) (more bool, err error) {
	err = e.s.kv.Update(ctx, func(tx kv.Txn) error {
		more = false
		for _, key := range [][]byte{existenceFenceKey(share, file), offloadFenceKey(share, file)} {
			if _, _, err := get(tx, key); err != nil {
				return err
			}
		}
		f, err := fileIn(tx, share, file)
		if err != nil {
			return err
		}
		ms, err := masksOf(tx, share, file)
		if err != nil {
			return err
		}
		ovs, err := overwritesOver(tx, share, file, entity.Extent{Length: entity.EndOfFile}, ms.span(f))
		if err != nil {
			return err
		}
		deleted := 0
		for _, o := range ovs {
			if ms.masked(o.Version, o.Extent) {
				continue
			}
			need, err := uncovered(tx, share, file, f, ms, o)
			if err != nil {
				return err
			}
			if len(need) > 0 {
				continue
			}
			if deleted == e.s.k {
				more = true
				return nil
			}
			if err := tx.Delete(overwriteKey(share, file, o.Offset)); err != nil {
				return err
			}
			deleted++
		}
		return nil
	})
	return more, err
}

// uncovered is the part of o's extent below size, outside every visible
// hole, that no visible ref at or above o's version covers.
func uncovered(r kv.Reader, share entity.ShareID, file entity.FileID, f entity.File, ms masks, o overwrite) ([]entity.Extent, error) {
	e := o.Intersect(entity.Extent{Length: f.Size})
	if e.IsEmpty() {
		return nil, nil
	}
	hs, err := holesOver(r, share, file, e, ms.span(f))
	if err != nil {
		return nil, err
	}
	refs, err := refsOver(r, share, file, e)
	if err != nil {
		return nil, err
	}
	var cut []entity.Extent
	for _, h := range hs {
		cut = append(cut, ms.visible(h.Version, h.Extent)...)
	}
	for _, c := range refs {
		if !c.Newest.Less(o.Version) {
			cut = append(cut, ms.visible(c.Newest, c.Extent())...)
		}
	}
	return subtract(e, cut), nil
}
