package kvstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// ErrCrossNamespace is a clone whose destination's share does not count
// content in every namespace the source's share does: its refs could not
// adopt their chunks, and the client copies the bytes itself.
var ErrCrossNamespace = errors.New("kvstore: clone across namespaces")

// CloneOp is one clone of [SrcOff, SrcOff+Len) of the spec's source onto
// the destination at DstOff, at the destination journal's version Version.
// Copied is the newest version of the source's journal-held runs the engine
// staged and synced into the destination above Version, zero if none.
type CloneOp struct {
	Share   entity.ShareID // the destination's
	File    entity.FileID
	Epoch   uint64
	Spec    entity.CloneSpec
	Version entity.JournalVersion
	Copied  entity.JournalVersion
	Time    time.Time
}

// Clone applies phase 1 of a clone, after the destination's pending
// existence below it. In one transaction, O(1) keys whatever the source
// holds, it removes the destination extent as a deallocate does, so the
// content there older than the clone is masked, but writes no merged hole;
// records the whole extent as existing; grows the destination to cover it;
// writes the clone's removal record with its spec; and writes a durable open
// of the source, which keeps the source from release until the clone is
// done. Resume then adopts the source's carved refs, re-versioned at the
// clone's version, and writes its holes as the destination's. Every refusal —
// an overlapping clone within one file, a source extent past its size or
// under a clone of its own not done, a clone across namespaces — comes
// before anything changes. A clone at or below applied was applied before,
// and a replay changes nothing.
//
// Until the clone is done, a covering lookup over the destination extent
// answers ErrDelay rather than a half-copied extent, and the engine answers
// a write into it ErrDelay, so no destination write lands where a batch
// writes a hole.
//
// decision: phase 1 charges the whole extent and each batch gives back the
// bytes of the source holes it writes, so the destination's Charged and its
// usage read high, by the source's hole bytes in the extent, until the clone
// is done. Lowering them in phase 1 would read every source hole in one
// transaction, the O(holes) cost the batches exist to avoid. Revisit if a
// quota check against that transient excess refuses writes in practice.
func (e Existence) Clone(ctx context.Context, op CloneOp, pending []PendingOp) error {
	sp := op.Spec
	if sp.Len <= 0 || sp.SrcOff < 0 || sp.DstOff < 0 {
		return merrs.NewInvalidArgumentError("a clone of nothing")
	}
	if sp.SrcShare == op.Share && sp.Src == op.File &&
		entity.ExtentOf(sp.SrcOff, sp.SrcOff+sp.Len).Overlaps(entity.ExtentOf(sp.DstOff, sp.DstOff+sp.Len)) {
		return merrs.NewInvalidArgumentError("a clone overlapping itself")
	}
	if len(pending) > 0 {
		failed, err := e.Commit(ctx, []PendingExistence{{Share: op.Share, File: op.File, Epoch: op.Epoch, Ops: pending}})
		if err != nil {
			return err
		}
		if err := failed[op.File]; err != nil {
			return err
		}
	}
	return e.s.kv.Update(ctx, func(tx kv.Txn) error { return clonePhase1(tx, op, e.s.k) })
}

func clonePhase1(tx kv.Txn, op CloneOp, k int) error {
	sp, share, file := op.Spec, op.Share, op.File
	if err := rewriteFences(tx, share, file, op.Epoch); err != nil {
		return err
	}
	f, err := fileIn(tx, share, file)
	if err != nil {
		return err
	}
	if !f.Applied.Less(op.Version) {
		return nil // applied before, its source perhaps gone since
	}
	if err := destinationFree(tx, share, file); err != nil {
		return err
	}
	before := f
	src, err := fileIn(tx, sp.SrcShare, sp.Src)
	if err != nil {
		return err
	}
	if sp.SrcOff+sp.Len > src.Size {
		return merrs.NewInvalidArgumentError("a clone past the source's end of file")
	}
	if err := sameNamespaces(tx, sp.SrcShare, share); err != nil {
		return err
	}
	if err := noCloneOver(tx, sp.SrcShare, sp.Src, entity.ExtentOf(sp.SrcOff, sp.SrcOff+sp.Len)); err != nil {
		return err
	}
	ms, err := masksOf(tx, share, file)
	if err != nil {
		return err
	}
	ext := entity.ExtentOf(sp.DstOff, sp.DstOff+sp.Len)
	was := f.Version
	credit := false
	if live := ext.Intersect(entity.Extent{Length: f.Size}); !live.IsEmpty() {
		var charged int64
		if charged, credit, err = chargedIn(tx, share, file, f, ms, live, k, false); err != nil {
			return err
		}
		f.Charged -= charged
		if err := narrowStraddlers(tx, share, file, f, ms, live, true); err != nil {
			return err
		}
	} else if ext.Offset > f.Size {
		if err := gapHole(tx, share, &f, ms, ext.Offset, op.Version); err != nil {
			return err
		}
	}
	// The destination exists over the whole extent; phase 2 writes the
	// source's holes there and gives their bytes back.
	f.Charged += ext.Length
	f.Size = max(f.Size, ext.End())
	f.Applied = entity.MaxVersion(op.Version, op.Copied)
	f.Modify, f.Change = later(f.Modify, op.Time), later(f.Change, op.Time)
	f.Version = nextFileVersion(tx, entity.MaxVersion(f.Version, f.Applied))
	if err := putFile(tx, was, f); err != nil {
		return err
	}
	if err := chargeFile(tx, share, &before, &f); err != nil {
		return err
	}
	spec := sp
	m := entity.Removal{Version: op.Version, Extent: ext, Kind: entity.RemoveClone, Cursor: ext.Offset, OvSpan: f.MaxOverwrite, HoleCredit: credit, Clone: &spec}
	if err := tx.Set(removalKey(share, file, op.Version), encodeRemoval(m)); err != nil {
		return err
	}
	return tx.Set(cloneOpenKey(share, file, op.Version, sp), encodeEmpty())
}

// gapHole makes [size, at) a hole at version v, joined with a visible hole
// ending at size, as a write past the end of file does.
func gapHole(tx kv.Txn, share entity.ShareID, f *entity.File, ms masks, at int64, v entity.JournalVersion) error {
	h := hole{Extent: entity.ExtentOf(f.Size, at), Version: v}
	if f.Size > 0 {
		prev, err := holesOver(tx, share, f.ID, entity.ExtentOf(f.Size-1, f.Size), ms.holeSpan(*f))
		if err != nil {
			return err
		}
		for _, p := range prev {
			if p.End() == f.Size && !ms.masked(p.Version, p.Extent) {
				h.Extent = entity.ExtentOf(p.Offset, at)
				h.Version = entity.MaxVersion(v, p.Version)
			}
		}
	}
	f.MaxSpan = max(f.MaxSpan, h.Length)
	return tx.Set(holeKey(share, f.ID, h.Offset), encodeHole(h))
}

// noCloneOver refuses a source extent that a clone of its own not done
// still writes: it is not served until that clone is done, so a clone of it
// waits too. It reads the source's removals, a handful.
func noCloneOver(r kv.Reader, share entity.ShareID, file entity.FileID, e entity.Extent) error {
	ms, err := masksOf(r, share, file)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.Kind == entity.RemoveClone && m.Extent.Overlaps(e) {
			return entity.ErrDelay
		}
	}
	return nil
}

// cloneOpenKey is the durable open a clone holds on its source: its ID is
// derived from the clone, so the clone's last batch finds it again.
//
// decision: the record holds no open state, only its existence, since the
// open-state records of RFC 14 are not written yet; what the release check
// needs is that some durable open names the source, and this one does.
func cloneOpenKey(share entity.ShareID, file entity.FileID, v entity.JournalVersion, sp entity.CloneSpec) []byte {
	h := sha256.New()
	h.Write(share[:])
	h.Write(file[:])
	h.Write(v[:])
	return append(opensPrefix(sp.SrcShare, sp.Src), h.Sum(nil)[:16]...)
}

// sameNamespaces refuses a clone unless the destination's share counts
// content in every namespace the source's share does.
func sameNamespaces(r kv.Reader, src, dst entity.ShareID) error {
	sn, err := shareNamespacesOf(r, src)
	if err != nil {
		return err
	}
	dn, err := shareNamespacesOf(r, dst)
	if err != nil {
		return err
	}
	for _, g := range sn.Namespaces {
		if !slices.ContainsFunc(dn.Namespaces, func(d entity.NamespaceGen) bool { return d.Namespace == g.Namespace }) {
			return ErrCrossNamespace
		}
	}
	return nil
}

// adopt writes, over the destination batch extent batch, the source's
// carved refs there re-versioned at the clone's version and born at its
// cut, counting each chunk in the destination's generation of its
// namespace. A destination ref at or above the clone's version holds
// content written after it and is left alone. Each source hole run becomes a
// destination hole at the clone's version, joined with a hole it touches;
// its bytes come off f's Charged and f's MaxSpan is raised to the joined
// hole. A nil f is a released destination, which takes no holes. Uncarved source runs write nothing: a copied run reads from the
// destination's journal, and one not held reads Lost as the source's does.
// A ref whose chunk record is gone or names a deleted block is carried as
// Lost the same way.
func adopt(tx kv.Txn, share entity.ShareID, file entity.FileID, f *entity.File, m entity.Removal, batch entity.Extent, ms masks, nss shareNamespaces, deltas map[uint32]counts) error {
	sp := m.Clone
	srcBatch := entity.Extent{Offset: batch.Offset - sp.DstOff + sp.SrcOff, Length: batch.Length}
	spans, err := covering(tx, sp.SrcShare, sp.Src, srcBatch)
	if err != nil {
		return err
	}
	srcNss, err := shareNamespacesOf(tx, sp.SrcShare)
	if err != nil {
		return err
	}
	for _, s := range spans {
		if s.Class == entity.Hole {
			if f == nil {
				continue
			}
			e := entity.Extent{Offset: s.Offset - sp.SrcOff + sp.DstOff, Length: s.Length}
			h, err := mergedHole(tx, share, file, *f, ms, e, m.Version)
			if err != nil {
				return err
			}
			f.Charged -= e.Length
			f.MaxSpan = max(f.MaxSpan, h.Length)
			continue
		}
		if s.Class != entity.Carved {
			continue
		}
		c := s.Ref
		c.File, c.Offset = file, s.Offset-sp.SrcOff+sp.DstOff
		c.Oldest, c.Newest, c.Born, c.Died = m.Version, m.Version, m.Cut, 0
		var ns entity.NamespaceID
		if !c.IsZeroRef() {
			if ns, err = srcNss.namespace(c.NSGen); err != nil {
				return err
			}
			if gone, err := chunkGone(tx, ns, c.Chunk); err != nil {
				return err
			} else if gone {
				continue
			}
			if c.NSGen, err = nss.generation(ns); err != nil {
				return err
			}
		}
		if deltas[c.NSGen] == nil {
			deltas[c.NSGen] = counts{}
		}
		if _, err := replaceRefs(tx, ns, share, c, deltas[c.NSGen], ms); err != nil {
			return err
		}
	}
	return nil
}

// chunkGone reports a chunk with no record, or one naming a deleted block.
func chunkGone(r kv.Reader, ns entity.NamespaceID, h entity.ChunkHash) (bool, error) {
	b, ok, err := get(r, chunkKey(ns, h))
	if err != nil || !ok {
		return !ok, err
	}
	rec, err := decodeChunk(h, b)
	if err != nil {
		return false, err
	}
	bb, ok, err := get(r, blockKey(ns, rec.Block))
	if err != nil || !ok {
		return !ok, err
	}
	blk, err := decodeBlock(rec.Block, bb)
	return blk.State == entity.BlockDeleted, err
}

func (n shareNamespaces) generation(ns entity.NamespaceID) (uint32, error) {
	for _, g := range n.Namespaces {
		if g.Namespace == ns {
			return g.Generation, nil
		}
	}
	return 0, ErrCrossNamespace
}
