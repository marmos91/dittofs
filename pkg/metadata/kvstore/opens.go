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
// and reports whether it left the file orphaned: no entry and no durable
// open of it or of its named streams, so the filesystem service runs its
// release. An open of a named stream is judged on its base file, which holds
// the entries. A share's root has no entry from its creation and is never
// orphaned.
func (o Opens) Delete(ctx context.Context, share entity.ShareID, file entity.FileID, id entity.OpenID) (orphan bool, err error) {
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
			orphan = false
			return nil
		}
		held, err := heldOpen(tx, share, f.ID)
		orphan = !held
		return err
	})
	return orphan, err
}

// heldOpen reports whether file or one of its named streams has a durable
// open.
func heldOpen(r kv.Reader, share entity.ShareID, file entity.FileID) (bool, error) {
	streams, err := streamsOf(r, share, file)
	if err != nil {
		return false, err
	}
	for _, id := range append(streams, file) {
		for _, err := range r.Scan(opensPrefix(share, id), nil) {
			return err == nil, err
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

// PutLock writes a durable lock beneath its open, which must be a
// persistent open of the file: no other open's locks are stored, and
// closing the open drops them with it.
func (o Opens) PutLock(ctx context.Context, share entity.ShareID, file entity.FileID, l entity.Lock) error {
	if l.ID == 0 || l.Offset < 0 || l.Length <= 0 {
		return merrs.NewInvalidArgumentError("a lock needs an ID, an offset and a length")
	}
	return o.s.kv.Update(ctx, func(tx kv.Txn) error {
		op, err := persistentOpen(tx, share, file, l.Open)
		if err != nil {
			return err
		}
		l.Open = op.ID
		return tx.Set(lockKey(share, file, l.Open, l.Offset, l.ID), encodeLock(l))
	})
}

// DeleteLock removes one durable lock.
func (o Opens) DeleteLock(ctx context.Context, share entity.ShareID, file entity.FileID, l entity.Lock) error {
	return o.s.kv.Update(ctx, func(tx kv.Txn) error {
		return tx.Delete(lockKey(share, file, l.Open, l.Offset, l.ID))
	})
}

// Locks lists the durable locks beneath one open, in offset order.
func (o Opens) Locks(ctx context.Context, share entity.ShareID, file entity.FileID, open entity.OpenID) (out []entity.Lock, err error) {
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
			l, err := decodeLock(open, int64(offset), entity.LockID(id), row.Value)
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
func (o Opens) SetDeletePending(ctx context.Context, share entity.ShareID, file entity.FileID, by entity.OpenID, d entity.DeletePending) error {
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
func (o Opens) DeletePending(ctx context.Context, share entity.ShareID, file entity.FileID) (d entity.DeletePending, ok bool, err error) {
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
