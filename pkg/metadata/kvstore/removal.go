package kvstore

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// removals reads a file's removal records, in version order. A file has a
// handful at most, so the scan is not bounded.
func removals(r kv.Reader, share entity.ShareID, file entity.FileID) ([]entity.Removal, error) {
	pre := removalsPrefix(share, file)
	var out []entity.Removal
	for row, err := range r.Scan(pre, nil) {
		if err != nil {
			return nil, err
		}
		v, _, err := keys.ReadVersion(row.Key[len(pre):])
		if err != nil {
			return nil, fmt.Errorf("%w: removal key: %v", errMalformed, err)
		}
		m, err := decodeRemoval(v, row.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// masks is a set of a file's removals, each hiding the refs, holes and
// overwrite records in its extent older than itself: those not yet done
// for a read or a write, every one not yet pruned for an offload commit.
type masks []entity.Removal

func masksOf(r kv.Reader, share entity.ShareID, file entity.FileID) (masks, error) {
	rs, err := removals(r, share, file)
	return slices.DeleteFunc(rs, func(m entity.Removal) bool { return m.Done }), err
}

// visible is the part of e that content at version v still holds: e less
// the extent of every removal not done that is newer than v.
func (ms masks) visible(v entity.JournalVersion, e entity.Extent) []entity.Extent {
	var cut []entity.Extent
	for _, m := range ms {
		if v.Less(m.Version) && m.Extent.Overlaps(e) {
			cut = append(cut, m.Extent)
		}
	}
	if len(cut) == 0 {
		return []entity.Extent{e}
	}
	return subtract(e, cut)
}

// masked reports a record wholly hidden: a hole or overwrite record of
// version v inside a newer removal's extent.
func (ms masks) masked(v entity.JournalVersion, e entity.Extent) bool {
	return len(ms.visible(v, e)) == 0
}

// span is how far below an offset a hole or overwrite record over it may
// start: records of a kind overlap only while a removal masks some of them,
// and none is longer than the file's MaxSpan.
func (ms masks) span(f entity.File) int64 {
	if len(ms) == 0 {
		return 0
	}
	return max(1, f.MaxSpan)
}

// RemoveOp is a removal the write path makes: a truncate down to Extent's
// offset, a deallocate of Extent, or a release of the whole file.
type RemoveOp struct {
	Version entity.JournalVersion
	Kind    entity.RemovalKind // RemoveTruncate or RemoveDeallocate
	Extent  entity.Extent      // a truncate's runs to the end of file
	Time    time.Time
}

// Remove applies phase 1 of a removal, after the file's pending existence
// below it; releasePhase1 says what a release's does. For a truncate or a
// deallocate, in one transaction it sets size for a truncate, cuts the at most two holes and two overwrite records crossing
// the extent's edges there,
// makes a deallocated extent one hole at the removal's version, lowers
// Charged, advances applied, and writes the removal record, which masks the
// refs, holes and overwrite records inside the extent until Resume has
// dropped them. It writes no ref. A removal at or below applied is a no-op,
// so a replay is harmless. Both fences are read and written, so the removal
// conflicts with an offload commit and with an existence commit of the file.
//
// decision: the pending existence commits in its own transactions before
// phase 1 rather than in phase 1's. The engine holds the file's guard across
// both, so nothing lands between them, and a crash between them leaves the
// removal above applied for recovery to apply, as any uncommitted operation.
//
// decision: a deallocate's extent becomes a hole record at once, at the
// removal's version, rather than when phase 2 finishes: a write after the
// removal then splits that hole as it would any other, so the bytes it wrote
// are never covered by the merged hole phase 2 leaves.
//
// ponytail: lowering Charged reads every hole in the extent, untracked,
// which a truncate of a TRIM-fragmented file makes O(holes) reads though it
// still writes O(1) keys. Carry the bytes holes cover on the File when that
// read shows in the truncate profile.
func (e Existence) Remove(ctx context.Context, share entity.ShareID, file entity.FileID, epoch uint64, op RemoveOp, pending []PendingOp) error {
	if op.Kind != entity.RemoveTruncate && op.Kind != entity.RemoveDeallocate && op.Kind != entity.RemoveRelease {
		return merrs.NewInvalidArgumentError("Remove takes a truncate, a deallocate or a release")
	}
	if len(pending) > 0 {
		failed, err := e.Commit(ctx, []PendingExistence{{Share: share, File: file, Epoch: epoch, Ops: pending}})
		if err != nil {
			return err
		}
		if err := failed[file]; err != nil {
			return err
		}
	}
	return e.s.kv.Update(ctx, func(tx kv.Txn) error {
		if op.Kind == entity.RemoveRelease {
			return releasePhase1(tx, share, file, epoch, op)
		}
		return removePhase1(tx, share, file, epoch, op)
	})
}

func removePhase1(tx kv.Txn, share entity.ShareID, file entity.FileID, epoch uint64, op RemoveOp) error {
	if err := rewriteFences(tx, share, file, epoch); err != nil {
		return err
	}
	f, err := fileIn(tx, share, file)
	if err != nil {
		return err
	}
	if !f.Applied.Less(op.Version) {
		return nil // already applied
	}
	before := f
	ms, err := masksOf(tx, share, file)
	if err != nil {
		return err
	}
	ext := op.Extent
	if op.Kind == entity.RemoveTruncate {
		if ext.Offset > f.Size {
			return merrs.NewInvalidArgumentError("a truncate up is a grow")
		}
		ext = entity.ExtentOf(ext.Offset, entity.EndOfFile)
	}
	live := ext.Intersect(entity.Extent{Length: f.Size}) // the part below size
	if !live.IsEmpty() {
		charged, err := chargedIn(tx, share, file, f, ms, live)
		if err != nil {
			return err
		}
		f.Charged -= charged
		if err := narrowStraddlers(tx, share, file, f, ms, live); err != nil {
			return err
		}
	}
	switch {
	case op.Kind == entity.RemoveTruncate:
		f.Size = ext.Offset
	case !live.IsEmpty():
		ext = live
		if err := tx.Set(holeKey(share, file, ext.Offset), encodeHole(hole{Extent: ext, Version: op.Version})); err != nil {
			return err
		}
		f.MaxSpan = max(f.MaxSpan, ext.Length)
	}
	was := f.Version
	f.Applied = op.Version
	f.Modify, f.Change = later(f.Modify, op.Time), later(f.Change, op.Time)
	f.Version = nextFileVersion(tx, entity.MaxVersion(f.Version, f.Applied))
	if err := putFile(tx, was, f); err != nil {
		return err
	}
	if err := chargeFile(tx, share, &before, &f); err != nil {
		return err
	}
	if live.IsEmpty() {
		return nil // a deallocate past the end of file removes nothing
	}
	m := entity.Removal{Version: op.Version, Extent: ext, Kind: op.Kind, Cursor: ext.Offset}
	return tx.Set(removalKey(share, file, op.Version), encodeRemoval(m))
}

// rewriteFences reads both of a file's fences, tracked, and writes them
// back, at epoch when the file has none: a removal conflicts with every
// commit of the file on either path.
func rewriteFences(tx kv.Txn, share entity.ShareID, file entity.FileID, epoch uint64) error {
	for _, key := range [][]byte{existenceFenceKey(share, file), offloadFenceKey(share, file)} {
		b, ok, err := get(tx, key)
		if err != nil {
			return err
		}
		if !ok {
			b = encodeFence(fence{Epoch: epoch})
		}
		if err := tx.Set(key, b); err != nil {
			return err
		}
	}
	return nil
}

// chargedIn is the bytes of e, below size, that the file is charged for:
// e less its holes and the extents of deallocations not yet done, which
// read as holes. A masked hole lies inside one of those, or past size.
func chargedIn(r kv.Reader, share entity.ShareID, file entity.FileID, f entity.File, ms masks, e entity.Extent) (int64, error) {
	hs, err := holesOver(r, share, file, e, ms.span(f))
	if err != nil {
		return 0, err
	}
	var free []entity.Extent
	for _, h := range hs {
		free = append(free, h.Extent)
	}
	for _, m := range ms {
		if m.Kind == entity.RemoveDeallocate {
			free = append(free, m.Extent)
		}
	}
	n := e.Length
	for _, x := range mergeExtents(free) {
		n -= x.Intersect(e).Length
	}
	return n, nil
}

// narrowStraddlers cuts each visible hole and overwrite record that
// crosses an edge of e in two at that edge, so every one is either inside
// e, and masked, or outside it: no later write or commit can then leave a
// piece of a masked record where phase 2 has already passed. That is at
// most two records of each kind.
func narrowStraddlers(tx kv.Txn, share entity.ShareID, file entity.FileID, f entity.File, ms masks, e entity.Extent) error {
	for _, edge := range []int64{e.Offset, e.End()} {
		if edge >= f.Size {
			continue
		}
		at := entity.Extent{Offset: edge, Length: 1}
		hs, err := holesOver(tx, share, file, at, ms.span(f))
		if err != nil {
			return err
		}
		for _, h := range hs {
			if h.Offset >= edge || ms.masked(h.Version, h.Extent) {
				continue
			}
			for _, x := range []entity.Extent{entity.ExtentOf(h.Offset, edge), entity.ExtentOf(edge, h.End())} {
				if err := tx.Set(holeKey(share, file, x.Offset), encodeHole(hole{Extent: x, Version: h.Version})); err != nil {
					return err
				}
			}
		}
		os, err := overwritesOver(tx, share, file, at, ms.span(f))
		if err != nil {
			return err
		}
		for _, o := range os {
			if o.Offset >= edge || ms.masked(o.Version, o.Extent) {
				continue
			}
			for _, x := range []entity.Extent{entity.ExtentOf(o.Offset, edge), entity.ExtentOf(edge, o.End())} {
				piece := o
				piece.Extent = x
				if err := tx.Set(overwriteKey(share, file, x.Offset), encodeOverwrite(piece)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// batchItemKeys is at most the keys one item a phase-2 batch reaches
// writes: a ref split in two is its drop, two puts and their reverse keys,
// its chunk record, and its block record with its trash entry when the
// chunk's count crosses zero; a hole or an overwrite record costs less.
const batchItemKeys = 10

// Resume runs phase 2 of every removal of the file not done, in version
// order, each in sub-transactions within the key budget from its cursor. A
// batch drops each ref inside the extent with newest below the removal's
// version and narrows one crossing an edge, splitting a ref that spans a
// deallocated extent; drops the holes and overwrite records inside the
// extent below that version; and advances the cursor. Phase 1 left no hole
// or overwrite record crossing an edge, so those it drops lie wholly inside.
// A clone's batches also adopt the source's carved refs over what they
// reached. The last batch of a deallocate or a clone joins the extent's
// holes with those they touch, and a clone's deletes its open of the
// source. Content at or above the removal's version is never touched, so a
// re-run is harmless.
//
// ponytail: nothing moves to history, because no snapshot cut is recorded
// yet, so a dropped ref is dated by nothing and an overwrite record is
// dropped whole with the batch that reaches its start, not narrowed for the
// next batch to date by. Date and move what a live cut sees here when cuts
// land.
func (c Content) Resume(ctx context.Context, share entity.ShareID, file entity.FileID) error {
	for {
		var more bool
		err := c.s.kv.Update(ctx, func(tx kv.Txn) (err error) {
			more, err = c.s.resumeBatch(tx, share, file)
			return err
		})
		if err != nil || !more {
			return err
		}
	}
}

// resumeBatch runs one batch of the oldest removal not done, reporting
// whether any removal is left.
func (s *Store) resumeBatch(tx kv.Txn, share entity.ShareID, file entity.FileID) (bool, error) {
	ms, err := masksOf(tx, share, file)
	if err != nil || len(ms) == 0 {
		return false, err
	}
	m := ms[0]
	if _, err := tx.Get(removalKey(share, file, m.Version)); err != nil {
		return false, err // tracked: two runners of one removal serialise
	}
	f, _, err := fileOrGone(tx, share, file)
	if err != nil {
		return false, err
	}
	kinds := 3 // refs, holes and overwrite records, up to n of each
	if m.Kind == entity.RemoveClone {
		kinds++ // and the source's refs a batch adopts
	}
	n := max(1, (s.k-4)/(kinds*batchItemKeys))
	from, end := m.Cursor, m.Extent.End()

	refs, refEnd, err := firstStarting(tx, refsPrefix(share, file), from, end, n, func(start int64, v []byte) (entity.ChunkRef, int64, error) {
		c, err := decodeRef(file, start, v)
		return c, c.End(), err
	})
	if err != nil {
		return false, err
	}
	holes, holeEnd, err := firstStarting(tx, holesPrefix(share, file), from, end, n, func(start int64, v []byte) (hole, int64, error) {
		h, err := decodeHole(start, v)
		return h, h.End(), err
	})
	if err != nil {
		return false, err
	}
	ovs, ovEnd, err := firstStarting(tx, overwritesPrefix(share, file), from, end, n, func(start int64, v []byte) (overwrite, int64, error) {
		o, err := decodeOverwrite(start, v)
		return o, o.End(), err
	})
	if err != nil {
		return false, err
	}
	batchEnd := min(refEnd, holeEnd, ovEnd)
	if sp := m.Clone; sp != nil {
		srcFrom := from - sp.DstOff + sp.SrcOff
		_, srcEnd, err := firstStarting(tx, refsPrefix(sp.SrcShare, sp.Src), srcFrom, sp.SrcOff+sp.Len, n, func(start int64, v []byte) (entity.ChunkRef, int64, error) {
			c, err := decodeRef(sp.Src, start, v)
			return c, c.End(), err
		})
		if err != nil {
			return false, err
		}
		batchEnd = min(batchEnd, srcEnd-sp.SrcOff+sp.DstOff)
	}
	batch := entity.ExtentOf(from, batchEnd)
	reaches := func(start int64, e entity.Extent) bool {
		return e.Overlaps(m.Extent) && (start >= from && start < batchEnd || start < from && from == m.Extent.Offset)
	}

	nss, err := shareNamespacesOf(tx, share)
	if err != nil {
		return false, err
	}
	deltas := map[uint32]counts{}
	for _, rec := range refs {
		r := rec.value
		if !reaches(r.Offset, r.Extent()) || !r.Newest.Less(m.Version) {
			continue
		}
		ns, err := nss.namespace(r.NSGen)
		if err != nil {
			return false, err
		}
		if deltas[r.NSGen] == nil {
			deltas[r.NSGen] = counts{}
		}
		if err := cutRef(tx, ns, share, r, r.Extent().Intersect(m.Extent), deltas[r.NSGen], ms); err != nil {
			return false, err
		}
	}
	for _, rec := range holes {
		h := rec.value
		if reaches(h.Offset, h.Extent) && h.Version.Less(m.Version) && m.Extent.Contains(h.Extent) {
			if err := tx.Delete(holeKey(share, file, h.Offset)); err != nil {
				return false, err
			}
		}
	}
	for _, rec := range ovs {
		o := rec.value
		if reaches(o.Offset, o.Extent) && o.Version.Less(m.Version) && m.Extent.Contains(o.Extent) {
			if err := tx.Delete(overwriteKey(share, file, o.Offset)); err != nil {
				return false, err
			}
		}
	}
	if m.Kind == entity.RemoveClone {
		if err := adopt(tx, share, file, m, batch, ms, nss, deltas); err != nil {
			return false, err
		}
	}
	for gen, d := range deltas {
		ns, _ := nss.namespace(gen)
		blocks := map[entity.BlockName]*entity.Block{}
		if err := applyCounts(tx, ns, d, blocks); err != nil {
			return false, err
		}
		if err := writeBlocks(tx, ns, blocks); err != nil {
			return false, err
		}
	}

	m.Cursor = batch.End()
	if m.Cursor >= end {
		m.Done = true
		if m.Kind == entity.RemoveDeallocate || m.Kind == entity.RemoveClone {
			if err := joinHoles(tx, share, file, f, ms[1:], m.Extent); err != nil {
				return false, err
			}
		}
		if m.Kind == entity.RemoveClone {
			if err := tx.Delete(cloneOpenKey(share, file, m.Version, *m.Clone)); err != nil {
				return false, err
			}
		}
	}
	if err := tx.Set(removalKey(share, file, m.Version), encodeRemoval(m)); err != nil {
		return false, err
	}
	return true, nil
}

// fileOrGone reads a file's record, a released file reading as the zero
// File with ok false: its removal still runs to done.
func fileOrGone(r kv.Reader, share entity.ShareID, file entity.FileID) (entity.File, bool, error) {
	b, ok, err := get(r, fileKey(share, file))
	if err != nil || !ok {
		return entity.File{}, false, err
	}
	f, err := decodeFile(share, file, b)
	return f, err == nil, err
}

// firstStarting reads, in start order, the records under prefix that
// overlap [from, end) and start before the n+1th record starting at or
// after from, and reports where that one starts, end when there is none: a
// batch reaching up to there holds at most n records starting in it. The
// record crossing from is found with one seek. That is exact for phase 2:
// refs never overlap, and phase 1 cut every hole and overwrite record
// crossing the removal's edges, so none that a batch drops crosses its
// start, whatever masked records of other removals overlap.
func firstStarting[T any](r kv.Reader, prefix []byte, from, end int64, n int, decode func(start int64, v []byte) (T, int64, error)) ([]startedRecord[T], int64, error) {
	at := func(start int64) []byte { return keys.Uint64(slices.Clip(prefix), uint64(start)) }
	var out []startedRecord[T]
	if prev, ok, err := r.Before(prefix, at(from)); err != nil {
		return nil, 0, err
	} else if ok {
		start, err := startOf(prefix, prev.Key)
		if err != nil {
			return nil, 0, err
		}
		x, recEnd, err := decode(start, prev.Value)
		if err != nil {
			return nil, 0, err
		}
		if recEnd > from {
			out = append(out, startedRecord[T]{start, x})
		}
	}
	var after []byte // Scan resumes after its key
	if from > 0 {
		after = at(from - 1)
	}
	startingIn := 0
	for row, err := range r.Scan(prefix, after) {
		if err != nil {
			return nil, 0, err
		}
		start, err := startOf(prefix, row.Key)
		if err != nil {
			return nil, 0, err
		}
		if start >= end {
			break
		}
		if startingIn == n {
			return out, start, nil
		}
		startingIn++
		x, _, err := decode(start, row.Value)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, startedRecord[T]{start, x})
	}
	return out, end, nil
}

// joinHoles joins the holes over e and those touching it that no other
// removal not done masks, so a deallocate leaves holes that never touch.
func joinHoles(tx kv.Txn, share entity.ShareID, file entity.FileID, f entity.File, others masks, e entity.Extent) error {
	w := entity.ExtentOf(max(0, e.Offset-1), e.End()+1)
	hs, err := holesOver(tx, share, file, w, others.span(f))
	if err != nil {
		return err
	}
	hs = slices.DeleteFunc(hs, func(h hole) bool { return others.masked(h.Version, h.Extent) })
	for i := 0; i < len(hs); {
		j, joined := i+1, hs[i]
		for j < len(hs) && hs[j].Offset == joined.End() {
			joined.Extent = entity.ExtentOf(joined.Offset, hs[j].End())
			joined.Version = entity.MaxVersion(joined.Version, hs[j].Version)
			if err := tx.Delete(holeKey(share, file, hs[j].Offset)); err != nil {
				return err
			}
			j++
		}
		if j > i+1 {
			if err := tx.Set(holeKey(share, file, joined.Offset), encodeHole(joined)); err != nil {
				return err
			}
		}
		i = j
	}
	return nil
}

// shareNamespaces maps a share's namespace generations to namespaces.
func shareNamespacesOf(r kv.Reader, share entity.ShareID) (shareNamespaces, error) {
	b, ok, err := get(r, namespacesKey(share))
	if err != nil || !ok {
		return shareNamespaces{}, err
	}
	return decodeNamespaces(b)
}

func (n shareNamespaces) namespace(gen uint32) (entity.NamespaceID, error) {
	for _, g := range n.Namespaces {
		if g.Generation == gen {
			return g.Namespace, nil
		}
	}
	return entity.NamespaceID{}, fmt.Errorf("%w: no namespace at generation %d", ErrInconsistent, gen)
}

// PruneRemovals deletes the file's removals that are done and at or below
// atOrBelow, the lowest newest version of the file's offload passes in
// flight, or every done one when none is. It reads the offload fence,
// tracked, so a pass committing meanwhile orders against it. Pruning a
// released file's removal deletes the rest of its keys, its history apart.
func (c Content) PruneRemovals(ctx context.Context, share entity.ShareID, file entity.FileID, atOrBelow entity.JournalVersion) error {
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		if _, _, err := get(tx, offloadFenceKey(share, file)); err != nil {
			return err
		}
		rs, err := removals(tx, share, file)
		if err != nil {
			return err
		}
		for _, m := range rs {
			if !m.Done || atOrBelow.Less(m.Version) {
				continue
			}
			if m.Kind == entity.RemoveRelease {
				return dropReleased(tx, share, file, m.Version)
			}
			if err := tx.Delete(removalKey(share, file, m.Version)); err != nil {
				return err
			}
		}
		return nil
	})
}
