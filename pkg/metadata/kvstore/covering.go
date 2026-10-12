package kvstore

import (
	"cmp"
	"context"
	"iter"
	"slices"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Content is the engine's view of a file's content map: what each offset
// holds, and the offload commit that records it.
type Content struct{ s *Store }

func (s *Store) Content() Content { return Content{s} }

// startedRecord is one record of a per-file kind keyed by its start offset.
type startedRecord[T any] struct {
	start int64
	value T
}

// recordsOver returns the records under prefix that overlap [off, end), in
// start order. With span zero the records of the kind never overlap, so the
// one starting at or before off is found with one seek, then those starting
// inside the extent. A positive span bounds every record's length: records
// of the kind may overlap, as masked ones do while a removal is not done,
// so the scan starts span below off, where any record over off starts.
// Each is decoded by decode, which also reports its end.
func recordsOver[T any](r kv.Reader, prefix []byte, off, end, span int64, decode func(start int64, v []byte) (T, int64, error)) ([]startedRecord[T], error) {
	var out []startedRecord[T]
	at := func(start int64) []byte { return keys.Uint64(slices.Clip(prefix), uint64(start)) }
	after := at(off) // Scan resumes after its key: records starting past off
	if span > 0 {
		after = nil
		if off >= span {
			after = at(off - span)
		}
	} else if prev, ok, err := r.Before(prefix, at(off+1)); err != nil {
		return nil, err
	} else if ok {
		start, err := startOf(prefix, prev.Key)
		if err != nil {
			return nil, err
		}
		v, recEnd, err := decode(start, prev.Value)
		if err != nil {
			return nil, err
		}
		if recEnd > off {
			out = append(out, startedRecord[T]{start, v})
		}
	}
	for row, err := range r.Scan(prefix, after) {
		if err != nil {
			return nil, err
		}
		start, err := startOf(prefix, row.Key)
		if err != nil {
			return nil, err
		}
		if start >= end {
			break
		}
		v, recEnd, err := decode(start, row.Value)
		if err != nil {
			return nil, err
		}
		if recEnd > off {
			out = append(out, startedRecord[T]{start, v})
		}
	}
	return out, nil
}

func startOf(prefix, key []byte) (int64, error) {
	n, _, err := keys.ReadUint64(key[len(prefix):])
	return int64(n), err
}

func refsOver(r kv.Reader, share entity.ShareID, file entity.FileID, e entity.Extent) ([]entity.ChunkRef, error) {
	recs, err := recordsOver(r, refsPrefix(share, file), e.Offset, e.End(), 0, func(start int64, v []byte) (entity.ChunkRef, int64, error) {
		c, err := decodeRef(file, start, v)
		return c, c.End(), err
	})
	out := make([]entity.ChunkRef, len(recs))
	for i, rec := range recs {
		out[i] = rec.value
	}
	return out, err
}

func holesOver(r kv.Reader, share entity.ShareID, file entity.FileID, e entity.Extent, span int64) ([]hole, error) {
	recs, err := recordsOver(r, holesPrefix(share, file), e.Offset, e.End(), span, func(start int64, v []byte) (hole, int64, error) {
		h, err := decodeHole(start, v)
		return h, h.End(), err
	})
	out := make([]hole, len(recs))
	for i, rec := range recs {
		out[i] = rec.value
	}
	return out, err
}

func overwritesOver(r kv.Reader, share entity.ShareID, file entity.FileID, e entity.Extent, span int64) ([]overwrite, error) {
	recs, err := recordsOver(r, overwritesPrefix(share, file), e.Offset, e.End(), span, func(start int64, v []byte) (overwrite, int64, error) {
		o, err := decodeOverwrite(start, v)
		return o, o.End(), err
	})
	out := make([]overwrite, len(recs))
	for i, rec := range recs {
		out[i] = rec.value
	}
	return out, err
}

// Covering answers what [off, off+n) of a file holds, in order, below its
// size: holes, uncarved runs and carved runs, each carved run with the ref
// over it clipped to the run. An overwrite record above a ref's newest
// version makes what it covers uncarved, carrying that record's version. Every
// span carries the file's applied version as this read saw it. The cost is
// one seek per record kind plus the records over the extent, whatever the
// file's size.
//
// The removals not yet done mask what they will drop: a ref, hole or
// overwrite record in a removal's extent older than it reads as absent, and
// the offset resolves by what else is there.
func (c Content) Covering(ctx context.Context, share entity.ShareID, file entity.FileID, off, n int64) iter.Seq2[entity.Span, error] {
	return func(yield func(entity.Span, error) bool) {
		var spans []entity.Span
		err := c.s.kv.View(ctx, func(r kv.Reader) (err error) {
			spans, err = covering(r, share, file, entity.Extent{Offset: off, Length: n})
			return err
		})
		if err != nil {
			yield(entity.Span{}, err)
			return
		}
		for _, s := range spans {
			if !yield(s, nil) {
				return
			}
		}
	}
}

func covering(r kv.Reader, share entity.ShareID, file entity.FileID, want entity.Extent) ([]entity.Span, error) {
	f, err := fileIn(r, share, file)
	if err != nil {
		return nil, err
	}
	e := want.Intersect(entity.Extent{Length: f.Size})
	if e.IsEmpty() {
		return nil, nil
	}
	ms, err := masksOf(r, share, file)
	if err != nil {
		return nil, err
	}
	holes, err := holesOver(r, share, file, e, ms.span(f))
	if err != nil {
		return nil, err
	}
	refs, err := refsOver(r, share, file, e)
	if err != nil {
		return nil, err
	}
	ovs, err := overwritesOver(r, share, file, e, ms.span(f))
	if err != nil {
		return nil, err
	}
	if len(ms) > 0 {
		holes, refs, ovs = ms.apply(holes, refs, ovs)
	}
	return classify(e, f.Applied, holes, refs, ovs), nil
}

// apply drops what the removals hide, clipping a record that crosses a
// removal's edge to the part outside it, and returns each kind in start
// order.
func (ms masks) apply(holes []hole, refs []entity.ChunkRef, ovs []overwrite) ([]hole, []entity.ChunkRef, []overwrite) {
	var hs []hole
	for _, h := range holes {
		for _, x := range ms.visible(h.Version, h.Extent) {
			hs = append(hs, hole{Extent: x, Version: h.Version})
		}
	}
	var rs []entity.ChunkRef
	for _, c := range refs {
		for _, x := range ms.visible(c.Newest, c.Extent()) {
			rs = append(rs, c.Clip(x))
		}
	}
	var os []overwrite
	for _, o := range ovs {
		for _, x := range ms.visible(o.Version, o.Extent) {
			o.Extent = x
			os = append(os, o)
		}
	}
	byStart := func(a, b entity.Extent) int { return cmp.Compare(a.Offset, b.Offset) }
	slices.SortFunc(hs, func(a, b hole) int { return byStart(a.Extent, b.Extent) })
	slices.SortFunc(os, func(a, b overwrite) int { return byStart(a.Extent, b.Extent) })
	return hs, rs, os
}

// classify cuts e at every record edge and classes each piece: a hole, else
// a carved ref no newer overwrite covers, else uncarved. Adjacent pieces of
// one class, ref and overwrite version merge.
func classify(e entity.Extent, applied entity.JournalVersion, holes []hole, refs []entity.ChunkRef, ovs []overwrite) []entity.Span {
	cuts := []int64{e.Offset, e.End()}
	add := func(x entity.Extent) {
		for _, p := range []int64{x.Offset, x.End()} {
			if p > e.Offset && p < e.End() {
				cuts = append(cuts, p)
			}
		}
	}
	for _, h := range holes {
		add(h.Extent)
	}
	for _, c := range refs {
		add(c.Extent())
	}
	for _, o := range ovs {
		add(o.Extent)
	}
	slices.Sort(cuts)
	cuts = slices.Compact(cuts)

	var out []entity.Span
	hi, ri, oi := 0, 0, 0
	for i := 0; i+1 < len(cuts); i++ {
		piece := entity.ExtentOf(cuts[i], cuts[i+1])
		for hi < len(holes) && holes[hi].End() <= piece.Offset {
			hi++
		}
		for ri < len(refs) && refs[ri].End() <= piece.Offset {
			ri++
		}
		for oi < len(ovs) && ovs[oi].End() <= piece.Offset {
			oi++
		}
		s := entity.Span{Extent: piece, Applied: applied, Class: entity.Uncarved}
		switch {
		case hi < len(holes) && holes[hi].Contains(piece):
			s.Class = entity.Hole
		case ri < len(refs) && refs[ri].Extent().Contains(piece):
			ref := refs[ri]
			if oi < len(ovs) && ovs[oi].Contains(piece) && ref.Newest.Less(ovs[oi].Version) {
				s.Overwrite = ovs[oi].Version
			} else {
				s.Class, s.Ref = entity.Carved, ref.Clip(piece)
			}
		case oi < len(ovs) && ovs[oi].Contains(piece):
			s.Overwrite = ovs[oi].Version
		}
		out = mergeSpan(out, s)
	}
	return out
}

func mergeSpan(out []entity.Span, s entity.Span) []entity.Span {
	if n := len(out); n > 0 {
		last := &out[n-1]
		same := last.Class == s.Class && last.Overwrite == s.Overwrite && last.End() == s.Offset
		if same && s.Class == entity.Carved {
			same = last.Ref.Chunk == s.Ref.Chunk && last.Ref.Offset+last.Ref.Length == s.Ref.Offset &&
				last.Ref.Skip+last.Ref.Length == s.Ref.Skip && last.Ref.Newest == s.Ref.Newest && last.Ref.Oldest == s.Ref.Oldest
		}
		if same {
			last.Length += s.Length
			if s.Class == entity.Carved {
				last.Ref.Length += s.Ref.Length
			}
			return out
		}
	}
	return append(out, s)
}
