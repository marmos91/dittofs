package kvstore

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// ErrReferenced is a retired block the deleter found still referenced: it
// is resurrected instead of deleted.
var ErrReferenced = errors.New("kvstore: retired block referenced")

// putBound is the longest a put may still be in flight after its intent is
// abandoned, so an abandoned name's object is deleted only after it lands.
//
// ponytail: fixed here; it follows the remote store's put deadline once
// that is a setting.
const putBound = time.Hour

// Blocks is GC's view of one namespace's blocks: what is due for deletion,
// the verified move to deleted, pruning, abandoned intents, and recounts.
type Blocks struct {
	s  *Store
	ns entity.NamespaceID
}

func (s *Store) Blocks(ns entity.NamespaceID) Blocks { return Blocks{s, ns} }

// Due yields the retired blocks whose trash time has passed, oldest first.
func (b Blocks) Due(ctx context.Context, now time.Time) iter.Seq2[entity.BlockName, error] {
	prefix := retiredPrefix(b.ns)
	return func(yield func(entity.BlockName, error) bool) {
		var names []entity.BlockName
		err := b.s.kv.View(ctx, func(r kv.Reader) error {
			for row, err := range r.Scan(prefix, nil) {
				if err != nil {
					return err
				}
				at, rest, err := keys.ReadUint64(row.Key[len(prefix):])
				if err != nil {
					return err
				}
				if int64(at) > now.UnixNano() {
					break
				}
				names = append(names, entity.BlockName(rest))
			}
			return nil
		})
		if err != nil {
			yield(entity.BlockName{}, err)
			return
		}
		for _, n := range names {
			if !yield(n, nil) {
				return
			}
		}
	}
}

// MarkDeleted moves each retired block whose trash time has passed to
// deleted, the irreversible step, after checking inside its transaction that
// no ref names any chunk whose record names the block. It reads each such
// chunk record with tracking, and every transaction that adds a ref writes
// that record, so a ref committed concurrently conflicts with the move. A
// block found referenced is not deleted: its counts are raised from the
// reverse index, which resurrects it, and its error is ErrReferenced.
//
// ponytail: it does not read the namespace's GC pause, which a namespace
// move sets so no block it ships is deleted under it. No move exists yet;
// read the pause here when one does.
func (b Blocks) MarkDeleted(ctx context.Context, names []entity.BlockName) []error {
	errs := make([]error, len(names))
	for i, name := range names {
		referenced := false
		errs[i] = b.s.kv.Update(ctx, func(tx kv.Txn) (err error) {
			referenced, err = b.markDeleted(tx, name)
			return err
		})
		if errs[i] == nil && referenced {
			// The recount committed: the block is live again, not deleted.
			errs[i] = ErrReferenced
		}
	}
	return errs
}

// markDeleted moves one block to deleted, or, when a ref still names one of
// its chunks, raises the counts instead and reports it referenced. Either
// way the transaction commits.
func (b Blocks) markDeleted(tx kv.Txn, name entity.BlockName) (bool, error) {
	v, ok, err := get(tx, blockKey(b.ns, name))
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("%w: no block %s", ErrInconsistent, name)
	}
	blk, err := decodeBlock(name, v)
	if err != nil {
		return false, err
	}
	if blk.State != entity.BlockRetired || tx.Now().Before(blk.NotBefore) {
		return false, fmt.Errorf("kvstore: block %s is not due for deletion", name)
	}
	referenced := false
	for _, c := range blk.Carried {
		cb, ok, err := get(tx, chunkKey(b.ns, c.Hash))
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		rec, err := decodeChunk(c.Hash, cb)
		if err != nil {
			return false, err
		}
		if rec.Block != name {
			continue // carried as dead weight: another block owns it
		}
		n, err := countReverse(tx, b.ns, c.Hash)
		if err != nil {
			return false, err
		}
		if n > 0 {
			referenced = true
		}
	}
	if referenced {
		return true, b.recountBlock(tx, blk)
	}
	if err := tx.Delete(retiredKey(b.ns, blk.NotBefore, name)); err != nil {
		return false, err
	}
	blk.State, blk.MarkedAt = entity.BlockDeleted, tx.Now()
	if err := tx.Set(deletedKey(b.ns, name), encodeDeletedAt(time.Time{})); err != nil {
		return false, err
	}
	return false, tx.Set(blockKey(b.ns, name), encodeBlock(blk))
}

func countReverse(r kv.Reader, ns entity.NamespaceID, h entity.ChunkHash) (int64, error) {
	var n int64
	for _, err := range r.Scan(reversePrefix(ns, h), nil) {
		if err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// recountBlock raises the counts of the chunks a block owns to their reverse
// index, which resurrects the block when any is referenced.
func (b Blocks) recountBlock(tx kv.Txn, blk entity.Block) error {
	delta := counts{}
	for _, c := range blk.Carried {
		if err := b.recountInto(tx, c.Hash, blk.Name, delta); err != nil {
			return err
		}
	}
	blocks := map[entity.BlockName]*entity.Block{blk.Name: &blk}
	if err := applyCounts(tx, b.ns, delta, blocks); err != nil {
		return err
	}
	return writeBlocks(tx, b.ns, blocks)
}

// recountInto adds to delta what brings a chunk's count up to its reverse
// keys, for a chunk whose record names block (any block when block is zero).
// A correction only ever raises a count: lowering one needs the audit's
// unchanged stamp and agreeing walks.
func (b Blocks) recountInto(r kv.Reader, h entity.ChunkHash, block entity.BlockName, delta counts) error {
	cb, ok, err := get(r, chunkKey(b.ns, h))
	if err != nil || !ok {
		return err
	}
	rec, err := decodeChunk(h, cb)
	if err != nil {
		return err
	}
	if block != (entity.BlockName{}) && rec.Block != block {
		return nil
	}
	n, err := countReverse(r, b.ns, h)
	if err != nil {
		return err
	}
	if n > rec.Refcount {
		delta[h] += n - rec.Refcount
	}
	return nil
}

// Recount raises one chunk's count to the refs its reverse index holds, as a
// targeted repair after an underflow.
func (b Blocks) Recount(ctx context.Context, h entity.ChunkHash) error {
	return b.s.kv.Update(ctx, func(tx kv.Txn) error {
		delta := counts{}
		if err := b.recountInto(tx, h, entity.BlockName{}, delta); err != nil {
			return err
		}
		blocks := map[entity.BlockName]*entity.Block{}
		if err := applyCounts(tx, b.ns, delta, blocks); err != nil {
			return err
		}
		return writeBlocks(tx, b.ns, blocks)
	})
}

// Deleted records that the deleted blocks' objects are gone from the
// remote store, so they may be pruned.
func (b Blocks) Deleted(ctx context.Context, names []entity.BlockName) error {
	return b.s.kv.Update(ctx, func(tx kv.Txn) error {
		for _, name := range names {
			if _, ok, err := get(tx, deletedKey(b.ns, name)); err != nil {
				return err
			} else if !ok {
				return fmt.Errorf("kvstore: block %s is not deleted", name)
			}
			if err := tx.Set(deletedKey(b.ns, name), encodeDeletedAt(tx.Now())); err != nil {
				return err
			}
		}
		return nil
	})
}

// Prune removes deleted blocks whose objects are gone, with every chunk
// record that still names them. A chunk record another block owns is not
// touched: only the block's own records go.
func (b Blocks) Prune(ctx context.Context, names []entity.BlockName) error {
	return b.s.kv.Update(ctx, func(tx kv.Txn) error {
		for _, name := range names {
			v, ok, err := get(tx, deletedKey(b.ns, name))
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			r, err := open(v)
			if err != nil {
				return err
			}
			if readTime(r).IsZero() {
				return fmt.Errorf("kvstore: block %s's object is not deleted yet", name)
			}
			bv, err := tx.Get(blockKey(b.ns, name))
			if err != nil {
				return err
			}
			blk, err := decodeBlock(name, bv)
			if err != nil {
				return err
			}
			for _, c := range blk.Carried {
				cb, ok, err := get(tx, chunkKey(b.ns, c.Hash))
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				rec, err := decodeChunk(c.Hash, cb)
				if err != nil {
					return err
				}
				if rec.Block == name {
					if err := tx.Delete(chunkKey(b.ns, c.Hash)); err != nil {
						return err
					}
				}
			}
			if err := tx.Delete(deletedKey(b.ns, name)); err != nil {
				return err
			}
			if err := tx.Delete(blockKey(b.ns, name)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Abandon deletes put intents and writes each name's block record retired,
// carrying nothing, due once a put still in flight has landed, so its
// object is deleted. Reading and deleting the intent key conflicts with a
// commit consuming it: whichever is second fails.
func (b Blocks) Abandon(ctx context.Context, names []entity.BlockName) error {
	return b.s.kv.Update(ctx, func(tx kv.Txn) error {
		for _, name := range names {
			if _, ok, err := get(tx, intentKey(b.ns, name)); err != nil {
				return err
			} else if !ok {
				continue
			}
			if err := tx.Delete(intentKey(b.ns, name)); err != nil {
				return err
			}
			blk := entity.Block{Name: name, State: entity.BlockRetired, NotBefore: tx.Now().Add(putBound)}
			if err := tx.Set(retiredKey(b.ns, blk.NotBefore, name), encodeEmpty()); err != nil {
				return err
			}
			if err := tx.Set(blockKey(b.ns, name), encodeBlock(blk)); err != nil {
				return err
			}
		}
		return nil
	})
}

// AbandonShardIntents abandons every intent of this namespace whose domain
// is a shard. On a single node this runs at every start, before the first
// offload: no writer of the previous run survives it, so none of those
// intents can still be committed, and waiting for an epoch that never moves
// would leak their objects for good.
func (b Blocks) AbandonShardIntents(ctx context.Context) (int, error) {
	var names []entity.BlockName
	prefix := intentsPrefix(b.ns)
	if err := b.s.kv.View(ctx, func(r kv.Reader) error {
		for row, err := range r.Scan(prefix, nil) {
			if err != nil {
				return err
			}
			i, err := decodeIntent(row.Value)
			if err != nil {
				return err
			}
			if i.Domain == DomainShard {
				names = append(names, entity.BlockName(row.Key[len(prefix):]))
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return len(names), b.Abandon(ctx, names)
}

// Holder is one ref naming a chunk, from the reverse index.
type Holder struct {
	Share  entity.ShareID
	File   entity.FileID
	Offset int64
	Died   entity.SnapshotCut
}

// Holders yields the refs naming a chunk: its reverse index prefix.
func (b Blocks) Holders(ctx context.Context, h entity.ChunkHash) iter.Seq2[Holder, error] {
	prefix := reversePrefix(b.ns, h)
	return func(yield func(Holder, error) bool) {
		var hs []Holder
		err := b.s.kv.View(ctx, func(r kv.Reader) error {
			for row, err := range r.Scan(prefix, nil) {
				if err != nil {
					return err
				}
				rest := row.Key[len(prefix):]
				var x Holder
				var id []byte
				if id, rest, err = keys.ReadID(rest, keys.IDLen); err != nil {
					return err
				}
				x.Share = entity.ShareID(id)
				if id, rest, err = keys.ReadID(rest, keys.IDLen); err != nil {
					return err
				}
				x.File = entity.FileID(id)
				off, rest, err := keys.ReadUint64(rest)
				if err != nil {
					return err
				}
				died, _, err := keys.ReadUint64(rest)
				if err != nil {
					return err
				}
				x.Offset, x.Died = int64(off), entity.SnapshotCut(died)
				hs = append(hs, x)
			}
			return nil
		})
		if err != nil {
			yield(Holder{}, err)
			return
		}
		for _, x := range hs {
			if !yield(x, nil) {
				return
			}
		}
	}
}
