package kvstore

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

var (
	// ErrNoIntent is a commit of a block name with no put intent: it was
	// abandoned, or never recorded.
	ErrNoIntent = errors.New("kvstore: put intent absent")
	// ErrInconsistent is a store that contradicts itself: a block record
	// already present for a freshly minted name, or a count about to go
	// negative.
	ErrInconsistent = errors.New("kvstore: store inconsistent")
	// ErrNamespace is a commit for a file whose share does not count its
	// content in the block's namespace.
	ErrNamespace = errors.New("kvstore: file not in the block's namespace")
)

// trashRetention is how long a retired block stays recoverable before the
// deleter may delete it.
//
// ponytail: fixed at the default; it is the setting gc.trash_retention once
// settings reach the content path.
const trashRetention = 48 * time.Hour

// ChunkAt is one chunk a block carries: its hash, and its body's position
// and encoded length in the block.
type ChunkAt struct {
	Hash             entity.ChunkHash
	Position, Length int64
}

// FileCommit is one file's share of a block commit: the refs the pass
// carved for it, with the content versions it was offered at.
type FileCommit struct {
	Share entity.ShareID
	File  entity.FileID
	Epoch uint64 // the primary's epoch the pass ran under
	Refs  []entity.ChunkRef
}

// BlockCommit is everything one stored block's commit records.
type BlockCommit struct {
	Namespace entity.NamespaceID
	Name      entity.BlockName
	Size      int64 // the block's encoded bytes
	Chunks    []ChunkAt
	Files     []FileCommit
}

// CommitResult is, per file, the extents now offloaded: refs applied, and
// refs found already committed. Refs dropped or refused are absent, and the
// journal offers them again.
type CommitResult map[entity.FileID][]entity.Extent

// Intend durably records a put intent for a freshly minted block name,
// before any put of it.
func (c Content) Intend(ctx context.Context, ns entity.NamespaceID, name entity.BlockName, i Intent) error {
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		if _, ok, err := get(tx, intentKey(ns, name)); err != nil {
			return err
		} else if ok {
			return fmt.Errorf("%w: intent for %s already recorded", ErrInconsistent, name)
		}
		return tx.Set(intentKey(ns, name), encodeIntent(i))
	})
}

// Commit records a stored block, in one transaction: it consumes the block
// name's put intent, creates the block record and a chunk record for every
// carried chunk that has none, and replaces each file's refs with the
// pass's, counting every chunk as its refs change. The first block to
// commit a chunk owns its record; a later carrier leaves it alone and its
// copy is dead weight here, unless the record names a retired or deleted
// block, which it then repoints to itself.
//
// A file's new ref replaces an existing one only with newer content: one
// whose newest version falls inside the existing ref's range is already
// committed and reported offloaded, and one older than the existing ref is
// refused. Each file's offload fence is read and written, so two commits of
// one file serialise whatever process runs them. A file that no longer has
// a record has been released, and its refs are dropped. The commit never
// touches size, times or an overwrite record: those are the write path's.
//
// ponytail: removals are not consulted, because none is recorded yet. Drop
// a ref below an overlapping removal's version here when removals are
// written.
func (c Content) Commit(ctx context.Context, bc BlockCommit) (CommitResult, error) {
	var out CommitResult
	err := c.s.kv.Update(ctx, func(tx kv.Txn) (err error) {
		out, err = commitBlock(tx, bc)
		return err
	})
	return out, err
}

// counts is the refcount change a transaction makes per chunk.
type counts map[entity.ChunkHash]int64

func commitBlock(tx kv.Txn, bc BlockCommit) (CommitResult, error) {
	ns, now := bc.Namespace, tx.Now()
	ikey := intentKey(ns, bc.Name)
	if _, ok, err := get(tx, ikey); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNoIntent
	}
	if err := tx.Delete(ikey); err != nil {
		return nil, err
	}
	if _, ok, err := get(tx, blockKey(ns, bc.Name)); err != nil {
		return nil, err
	} else if ok {
		return nil, fmt.Errorf("%w: block %s already recorded", ErrInconsistent, bc.Name)
	}

	// Chunk records: create the missing, repoint those naming a block that
	// is gone or going.
	owned := map[entity.ChunkHash]bool{}
	var preLive int64
	for _, ch := range bc.Chunks {
		b, ok, err := get(tx, chunkKey(ns, ch.Hash))
		if err != nil {
			return nil, err
		}
		repoint := !ok
		var rec chunkRecord
		if ok {
			if rec, err = decodeChunk(ch.Hash, b); err != nil {
				return nil, err
			}
			st, err := blockState(tx, ns, rec.Block)
			if err != nil {
				return nil, err
			}
			repoint = st != entity.BlockLive
		}
		if repoint {
			// A repointed record keeps its count: refs naming the chunk read
			// this block's copy from now on, so they count toward its live.
			rec.Hash, rec.Block, rec.Position, rec.Length = ch.Hash, bc.Name, ch.Position, ch.Length
			rec.Stamp = newStamp()
			if err := tx.Set(chunkKey(ns, ch.Hash), encodeChunk(rec)); err != nil {
				return nil, err
			}
			owned[ch.Hash] = true
			if rec.Refcount > 0 {
				preLive++
			}
		}
	}

	delta := counts{}
	out := CommitResult{}
	for _, fc := range bc.Files {
		done, err := commitFile(tx, ns, fc, delta)
		if err != nil {
			return nil, err
		}
		if len(done) > 0 {
			out[fc.File] = done
		}
	}

	// The block record, with live counting the chunk records it owns that
	// any ref names.
	block := entity.Block{Name: bc.Name, Size: bc.Size, Live: preLive, Carried: make([]entity.Carried, len(bc.Chunks))}
	for i, ch := range bc.Chunks {
		block.Carried[i] = entity.Carried{Hash: ch.Hash, Length: ch.Length}
	}
	blocks := map[entity.BlockName]*entity.Block{bc.Name: &block}
	if err := applyCounts(tx, ns, delta, blocks); err != nil {
		return nil, err
	}
	if block.Live == 0 {
		// Born dead: every chunk adopted or every ref dropped. No later
		// count can reach it, so it is retired now; at once when no chunk
		// record names it, after the trash otherwise.
		block.State, block.NotBefore = entity.BlockRetired, now
		if len(owned) > 0 {
			block.NotBefore = now.Add(trashRetention)
		}
	}
	return out, writeBlocks(tx, ns, blocks)
}

func blockState(r kv.Reader, ns entity.NamespaceID, name entity.BlockName) (entity.BlockState, error) {
	b, ok, err := get(r, blockKey(ns, name))
	if err != nil {
		return 0, err
	}
	if !ok {
		return entity.BlockDeleted, nil // pruned: gone
	}
	blk, err := decodeBlock(name, b)
	return blk.State, err
}

// commitFile applies one file's refs and returns the extents now offloaded.
func commitFile(tx kv.Txn, ns entity.NamespaceID, fc FileCommit, delta counts) ([]entity.Extent, error) {
	share, file := fc.Share, fc.File
	gen, err := generationOf(tx, share, ns)
	if err != nil {
		return nil, err
	}
	// The offload fence: read and rewritten, so commits of one file
	// serialise on it. On a single node its epoch is not compared.
	fkey := offloadFenceKey(share, file)
	fb, ok, err := get(tx, fkey)
	if err != nil {
		return nil, err
	}
	fv := encodeFence(fence{Epoch: fc.Epoch})
	if ok {
		fv = fb
	}
	if err := tx.Set(fkey, fv); err != nil {
		return nil, err
	}
	if _, ok, err := get(tx, fileKey(share, file)); err != nil {
		return nil, err
	} else if !ok {
		return nil, nil // released: its refs are dropped
	}

	refs := slices.Clone(fc.Refs)
	slices.SortFunc(refs, func(a, b entity.ChunkRef) int { return cmp.Compare(a.Offset, b.Offset) })
	var done []entity.Extent
	for i, nr := range refs {
		if nr.Length <= 0 || (i > 0 && refs[i-1].End() > nr.Offset) {
			return nil, fmt.Errorf("%w: a commit's refs overlap or are empty", ErrInconsistent)
		}
		nr.File, nr.NSGen, nr.Died = file, gen, 0
		applied, err := replaceRefs(tx, ns, share, nr, delta)
		if err != nil {
			return nil, err
		}
		done = append(done, applied...)
	}
	return mergeExtents(done), nil
}

// generationOf is the generation the share counts its content in ns under.
func generationOf(r kv.Reader, share entity.ShareID, ns entity.NamespaceID) (uint32, error) {
	b, ok, err := get(r, namespacesKey(share))
	if err != nil {
		return 0, err
	}
	if ok {
		nss, err := decodeNamespaces(b)
		if err != nil {
			return 0, err
		}
		for _, g := range nss.Namespaces {
			if g.Namespace == ns {
				return g.Generation, nil
			}
		}
	}
	return 0, ErrNamespace
}

// replaceRefs writes nr over the file's refs by the version table: over an
// existing ref with older content it replaces the overlapped part; where the
// existing ref's versions include nr's newest, that part is already
// committed; where the existing ref is newer, that part of nr is refused.
// It returns the extents of nr now offloaded.
func replaceRefs(tx kv.Txn, ns entity.NamespaceID, share entity.ShareID, nr entity.ChunkRef, delta counts) ([]entity.Extent, error) {
	existing, err := refsOver(tx, share, nr.File, nr.Extent())
	if err != nil {
		return nil, err
	}
	var write, committed []entity.Extent // parts of nr
	cursor := nr.Offset
	for _, e := range existing {
		over := e.Extent().Intersect(nr.Extent())
		if over.Offset > cursor {
			write = append(write, entity.ExtentOf(cursor, over.Offset))
		}
		switch {
		case e.Newest.Less(nr.Newest):
			write = append(write, over)
			if err := cutRef(tx, ns, share, e, over, delta); err != nil {
				return nil, err
			}
		case !nr.Newest.Less(e.Oldest):
			committed = append(committed, over)
		default:
			// Older than what is there: refused as live.
		}
		cursor = over.End()
	}
	if cursor < nr.End() {
		write = append(write, entity.ExtentOf(cursor, nr.End()))
	}
	for _, w := range mergeExtents(write) {
		if err := putRef(tx, ns, share, nr.Clip(w), delta); err != nil {
			return nil, err
		}
	}
	return mergeExtents(append(write, committed...)), nil
}

// cutRef removes the part over of an existing ref, keeping what lies on
// either side as refs of their own.
func cutRef(tx kv.Txn, ns entity.NamespaceID, share entity.ShareID, e entity.ChunkRef, over entity.Extent, delta counts) error {
	if err := dropRef(tx, ns, share, e, delta); err != nil {
		return err
	}
	if left := e.Clip(entity.ExtentOf(e.Offset, over.Offset)); left.Length > 0 {
		if err := putRef(tx, ns, share, left, delta); err != nil {
			return err
		}
	}
	if right := e.Clip(entity.ExtentOf(over.End(), e.End())); right.Length > 0 {
		if err := putRef(tx, ns, share, right, delta); err != nil {
			return err
		}
	}
	return nil
}

// putRef writes a ref with its reverse key and counts its chunk. A zero ref
// names no chunk, so it has neither.
func putRef(tx kv.Txn, ns entity.NamespaceID, share entity.ShareID, c entity.ChunkRef, delta counts) error {
	if err := tx.Set(refKey(share, c.File, c.Offset), encodeRef(c)); err != nil {
		return err
	}
	if c.IsZeroRef() {
		return nil
	}
	delta[c.Chunk]++
	return tx.Set(reverseKey(ns, c.Chunk, share, c.File, c.Offset, 0), encodeEmpty())
}

func dropRef(tx kv.Txn, ns entity.NamespaceID, share entity.ShareID, c entity.ChunkRef, delta counts) error {
	if err := tx.Delete(refKey(share, c.File, c.Offset)); err != nil {
		return err
	}
	if c.IsZeroRef() {
		return nil
	}
	delta[c.Chunk]--
	return tx.Delete(reverseKey(ns, c.Chunk, share, c.File, c.Offset, 0))
}

// applyCounts applies refcount changes to their chunk records, stamping
// each, and moves the live count of the block a chunk names whenever its
// count crosses zero. blocks holds the block records already loaded, the
// committing one included; it gains any other it touches.
func applyCounts(tx kv.Txn, ns entity.NamespaceID, delta counts, blocks map[entity.BlockName]*entity.Block) error {
	for h, d := range delta {
		b, ok, err := get(tx, chunkKey(ns, h))
		if err != nil {
			return err
		}
		if !ok {
			if d > 0 {
				return fmt.Errorf("%w: a ref names chunk %s, which has no record", ErrInconsistent, h)
			}
			continue // dropping a ref whose chunk is gone: the content was already lost
		}
		rec, err := decodeChunk(h, b)
		if err != nil {
			return err
		}
		before := rec.Refcount
		rec.Refcount += d
		if rec.Refcount < 0 {
			return fmt.Errorf("%w: refcount of %s below zero", ErrInconsistent, h)
		}
		rec.Stamp = newStamp()
		if err := tx.Set(chunkKey(ns, h), encodeChunk(rec)); err != nil {
			return err
		}
		if (before == 0) == (rec.Refcount == 0) {
			continue
		}
		blk, err := loadBlock(tx, ns, rec.Block, blocks)
		if err != nil {
			return err
		}
		if rec.Refcount == 0 {
			blk.Live--
		} else {
			blk.Live++
		}
		if blk.Live < 0 {
			return fmt.Errorf("%w: live of block %s below zero", ErrInconsistent, rec.Block)
		}
	}
	return nil
}

func loadBlock(r kv.Reader, ns entity.NamespaceID, name entity.BlockName, blocks map[entity.BlockName]*entity.Block) (*entity.Block, error) {
	if b, ok := blocks[name]; ok {
		return b, nil
	}
	v, err := r.Get(blockKey(ns, name))
	if err != nil {
		return nil, fmt.Errorf("%w: chunk names block %s: %v", ErrInconsistent, name, err)
	}
	b, err := decodeBlock(name, v)
	if err != nil {
		return nil, err
	}
	blocks[name] = &b
	return &b, nil
}

// writeBlocks writes the block records a transaction changed. A block whose
// live count reached zero is retired, with its trash entry; one that left
// zero while retired is resurrected.
func writeBlocks(tx kv.Txn, ns entity.NamespaceID, blocks map[entity.BlockName]*entity.Block) error {
	now := tx.Now()
	for name, b := range blocks {
		v, existed, err := get(tx, blockKey(ns, name))
		if err != nil {
			return err
		}
		var was entity.Block
		if existed {
			if was, err = decodeBlock(name, v); err != nil {
				return err
			}
		}
		switch {
		case b.Live == 0 && b.State == entity.BlockLive && existed:
			b.State, b.NotBefore = entity.BlockRetired, now.Add(trashRetention)
		case b.Live > 0 && b.State == entity.BlockRetired:
			b.State, b.NotBefore = entity.BlockLive, time.Time{}
		}
		if was.State == entity.BlockRetired && existed {
			if err := tx.Delete(retiredKey(ns, was.NotBefore, name)); err != nil {
				return err
			}
		}
		if b.State == entity.BlockRetired {
			if err := tx.Set(retiredKey(ns, b.NotBefore, name), encodeEmpty()); err != nil {
				return err
			}
		}
		if err := tx.Set(blockKey(ns, name), encodeBlock(*b)); err != nil {
			return err
		}
	}
	return nil
}

func newStamp() uint64 {
	var b [8]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint64(b[:])
}

// mergeExtents sorts extents and joins those that touch or overlap.
func mergeExtents(es []entity.Extent) []entity.Extent {
	es = slices.DeleteFunc(slices.Clone(es), func(e entity.Extent) bool { return e.IsEmpty() })
	slices.SortFunc(es, func(a, b entity.Extent) int { return cmp.Compare(a.Offset, b.Offset) })
	var out []entity.Extent
	for _, e := range es {
		if n := len(out); n > 0 && out[n-1].End() >= e.Offset {
			out[n-1] = entity.ExtentOf(out[n-1].Offset, max(out[n-1].End(), e.End()))
			continue
		}
		out = append(out, e)
	}
	return out
}
