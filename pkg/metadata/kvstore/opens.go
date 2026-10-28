package kvstore

import (
	"bytes"
	"context"
	"fmt"
	"iter"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Opens is open state's view of its durable open records. Most opens are
// never written: an open is stored only when it keeps a file with no entry
// alive, when it is SMB-persistent, or when a clone holds its source. Each
// record has an entry in its share's durable-open index, under the file's
// shard, written and deleted with it, so a new primary finds its shards'
// records without reading every file.
type Opens struct{ s *Store }

func (s *Store) Opens() Opens { return Opens{s} }

// Put writes a durable open of a file of share, with its index entry. The
// File is read tracked, so an open of a file with no entry conflicts with
// the release that would delete it, which in turn range-guards the file's
// open records: whichever commits second loses. An open of a released file
// is stale.
func (o Opens) Put(ctx context.Context, share entity.ShareID, op entity.OpenRecord) error {
	if op.ID == 0 {
		return merrs.NewInvalidArgumentError("an open needs an ID")
	}
	return o.s.kv.Update(ctx, func(tx kv.Txn) error {
		f, err := fileIn(tx, share, op.File)
		if err != nil {
			return err
		}
		return putOpen(tx, share, f.Shard, op)
	})
}

// Get reads one durable open.
func (o Opens) Get(ctx context.Context, share entity.ShareID, file entity.FileID, id entity.OpenID) (op entity.OpenRecord, err error) {
	err = o.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, openKey(share, file, id))
		if err != nil {
			return err
		}
		if !ok {
			return merrs.NewNotFoundError(fmt.Sprintf("open %d", id), "open")
		}
		op, err = decodeOpen(file, id, b)
		return err
	})
	return op, err
}

// Of lists a file's durable opens in ID order.
func (o Opens) Of(ctx context.Context, share entity.ShareID, file entity.FileID) (out []entity.OpenRecord, err error) {
	err = o.s.kv.View(ctx, func(r kv.Reader) (err error) {
		out, err = opensOf(r, share, file)
		return err
	})
	return out, err
}

// Delete removes a durable open with its index entry and its durable locks,
// and names the file it left orphaned: no entry and no durable open of it or
// of its named streams, so the filesystem service runs its release. An open
// of a named stream is judged on its base file, which holds the entries, and
// the base is the file named. A share's root has no entry from its creation
// and is never orphaned.
func (o Opens) Delete(ctx context.Context, share entity.ShareID, file entity.FileID, id entity.OpenID) (out Orphaned, err error) {
	err = o.s.kv.Update(ctx, func(tx kv.Txn) error {
		f, err := fileIn(tx, share, file)
		if err != nil {
			return err
		}
		if _, ok, err := get(tx, openKey(share, file, id)); err != nil {
			return err
		} else if !ok {
			return merrs.NewNotFoundError(fmt.Sprintf("open %d", id), "open")
		}
		if err := dropOpen(tx, share, f.Shard, file, id); err != nil {
			return err
		}
		if f.IsStream() {
			if f, err = fileIn(tx, share, f.StreamOf); err != nil {
				return err
			}
		}
		if f.Nlink > 0 || f.IsRoot() {
			return nil
		}
		held, err := heldOpen(tx, share, f.ID)
		if !held {
			out = Orphaned{File: f.ID}
		}
		return err
	})
	return out, err
}

// heldOpen reports whether file or one of its named streams has a durable
// open. The scan is not tracked, so it guards the open it found: two last
// closes, each seeing the other's open, would otherwise both commit and
// neither name the file orphaned. The other close deletes that key, so
// whichever commits second retries and finds no open.
func heldOpen(tx kv.Txn, share entity.ShareID, file entity.FileID) (bool, error) {
	streams, err := streamsOf(tx, share, file)
	if err != nil {
		return false, err
	}
	for _, id := range append(streams, file) {
		for row, err := range tx.Scan(opensPrefix(share, id), nil) {
			if err != nil {
				return false, err
			}
			return true, tx.Guard(row.Key)
		}
	}
	return false, nil
}

// DurableOpen is one entry of a share's durable-open index.
type DurableOpen struct {
	File entity.FileID
	Open entity.OpenID
}

// Durable lists the durable opens in one shard of a share, from that
// shard's range of the index: what a new primary reads at grace end to
// close the opens nobody reclaimed, and to rebuild a clone's hold on its
// source.
func (o Opens) Durable(ctx context.Context, share entity.ShareID, shard uint64) iter.Seq2[DurableOpen, error] {
	prefix := keys.Uint64(shareKey(share, keys.SDo), shard)
	return pages(ctx, o.s.kv, prefix, nil, func(_ kv.Reader, row kv.KeyValue) (DurableOpen, error) {
		file, rest, err := keys.ReadID(row.Key[len(prefix):], keys.IDLen)
		if err != nil {
			return DurableOpen{}, err
		}
		id, _, err := keys.ReadUint64(rest)
		return DurableOpen{File: entity.FileID(file), Open: entity.OpenID(id)}, err
	})
}

func putOpen(tx kv.Txn, share entity.ShareID, shard uint64, op entity.OpenRecord) error {
	if err := tx.Set(openKey(share, op.File, op.ID), encodeOpen(op)); err != nil {
		return err
	}
	return tx.Set(durableOpenIndexKey(share, shard, op.File, op.ID), encodeEmpty())
}

// dropOpen deletes a durable open, the locks beneath it and its index entry.
func dropOpen(tx kv.Txn, share entity.ShareID, shard uint64, file entity.FileID, id entity.OpenID) error {
	if err := deleteUnder(tx, openKey(share, file, id)); err != nil {
		return err
	}
	return tx.Delete(durableOpenIndexKey(share, shard, file, id))
}

// opensOf reads a file's durable open records, skipping the locks beneath
// them.
func opensOf(r kv.Reader, share entity.ShareID, file entity.FileID) ([]entity.OpenRecord, error) {
	pre := opensPrefix(share, file)
	var out []entity.OpenRecord
	for row, err := range r.Scan(pre, nil) {
		if err != nil {
			return nil, err
		}
		id, rest, err := keys.ReadUint64(row.Key[len(pre):])
		if err != nil {
			return nil, err
		}
		if len(rest) != 0 {
			continue // a lock beneath the open
		}
		op, err := decodeOpen(file, entity.OpenID(id), row.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, nil
}

// SetLocks changes the durable locks beneath open, which must be a
// persistent open of the file, in one transaction: del are deleted, then
// put written, so a change lands whole or not at all, and a lock in both is
// written. Each is keyed by open, its offset and its ID; a record's Open is
// not read. A delete must carry the stored offset; a missing one is ignored.
// rec, when non-nil, is the open's new record, its LockSeq or ChannelSeq
// changed, written in the same transaction. A change past the backend's
// limits fails with kv.ErrTooLarge and writes nothing; it is never split, as
// a part written alone would store locks held with others not.
//
// decision: the store does not check that a range ends below 2^64: it stores
// what the table holds, and the table reads a range that carries past the
// last byte as reaching it. Refuse one here if a caller is seen storing a
// range its protocol forbids.
func (o Opens) SetLocks(ctx context.Context, share entity.ShareID, file entity.FileID, open entity.OpenID, rec *entity.OpenRecord, put, del []entity.LockRecord) error {
	for _, l := range put {
		if l.ID == 0 {
			return merrs.NewInvalidArgumentError("a lock needs an ID")
		}
	}
	return o.s.kv.Update(ctx, func(tx kv.Txn) error {
		cur, err := persistentOpen(tx, share, file, open)
		if err != nil {
			return err
		}
		if rec != nil {
			if rec.ID != open || rec.Durability != cur.Durability {
				return merrs.NewInvalidArgumentError("rec is not this open")
			}
			if err := tx.Set(openKey(share, file, open), encodeOpen(*rec)); err != nil {
				return err
			}
		}
		for _, l := range del {
			if err := tx.Delete(lockKey(share, file, open, l.Offset, l.ID)); err != nil {
				return err
			}
		}
		for _, l := range put {
			if err := tx.Set(lockKey(share, file, open, l.Offset, l.ID), encodeLock(l)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Locks lists the durable locks beneath one open, in offset order.
func (o Opens) Locks(ctx context.Context, share entity.ShareID, file entity.FileID, open entity.OpenID) (out []entity.LockRecord, err error) {
	err = o.s.kv.View(ctx, func(r kv.Reader) error {
		out = nil
		pre := locksPrefix(share, file, open)
		for row, err := range r.Scan(pre, nil) {
			if err != nil {
				return err
			}
			offset, rest, err := keys.ReadUint64(row.Key[len(pre):])
			if err != nil {
				return err
			}
			id, _, err := keys.ReadUint64(rest)
			if err != nil {
				return err
			}
			l, err := decodeLock(open, offset, entity.LockID(id), row.Value)
			if err != nil {
				return err
			}
			out = append(out, l)
		}
		return nil
	})
	return out, err
}

// SetDeletePending records that a file is to be deleted at its last close:
// durably only while the file has a persistent open, which carries the
// promise across a failover; otherwise it is held with the opens, in
// memory, and this refuses it. The open named is the marking one.
func (o Opens) SetDeletePending(ctx context.Context, share entity.ShareID, file entity.FileID, by entity.OpenID, d entity.DeleteRecord) error {
	return o.s.kv.Update(ctx, func(tx kv.Txn) error {
		if _, err := persistentOpen(tx, share, file, by); err != nil {
			return err
		}
		return tx.Set(deletePendingKey(share, file), encodeDeletePending(d))
	})
}

// ClearDeletePending removes a file's delete-pending record.
func (o Opens) ClearDeletePending(ctx context.Context, share entity.ShareID, file entity.FileID) error {
	return o.s.kv.Update(ctx, func(tx kv.Txn) error { return tx.Delete(deletePendingKey(share, file)) })
}

// DeletePending reads a file's delete-pending record; ok is false when it
// has none.
func (o Opens) DeletePending(ctx context.Context, share entity.ShareID, file entity.FileID) (d entity.DeleteRecord, ok bool, err error) {
	err = o.s.kv.View(ctx, func(r kv.Reader) error {
		b, found, err := get(r, deletePendingKey(share, file))
		if err != nil || !found {
			return err
		}
		ok = true
		d, err = decodeDeletePending(b)
		return err
	})
	return d, ok, err
}

// persistentOpen reads a durable open, tracked, refusing one that is not
// persistent.
func persistentOpen(tx kv.Txn, share entity.ShareID, file entity.FileID, id entity.OpenID) (entity.OpenRecord, error) {
	b, ok, err := get(tx, openKey(share, file, id))
	if err != nil {
		return entity.OpenRecord{}, err
	}
	if !ok {
		return entity.OpenRecord{}, merrs.NewNotFoundError(fmt.Sprintf("open %d", id), "open")
	}
	op, err := decodeOpen(file, id, b)
	if err != nil {
		return entity.OpenRecord{}, err
	}
	if op.Durability != entity.DurabilityPersistent {
		return entity.OpenRecord{}, merrs.NewInvalidArgumentError("only a persistent open's state is stored")
	}
	return op, nil
}

// dropDeletePendingAt deletes child's delete-pending record when it names
// the entry at key, which the caller is removing.
func dropDeletePendingAt(tx kv.Txn, b bound, child entity.File, key []byte) error {
	v, ok, err := get(tx, deletePendingKey(child.Share, child.ID))
	if err != nil || !ok {
		return err
	}
	d, err := decodeDeletePending(v)
	if err != nil {
		return err
	}
	at, err := b.entryKeyFor(child.Share, d.Parent, d.Name)
	if err != nil || !bytes.Equal(at, key) {
		return err
	}
	return tx.Delete(deletePendingKey(child.Share, child.ID))
}
