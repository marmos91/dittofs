package kvstore

import (
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

// NextOpenID issues an open ID never issued before in this installation:
// every open needs one, a volatile one too.
func (s *Store) NextOpenID(ctx context.Context) (entity.OpenID, error) {
	id, err := s.nextID(ctx, idOpen)
	return entity.OpenID(id), err
}

// Put writes a durable open of a file of share, with its index entry. The
// File is read tracked, so an open of a file with no entry conflicts with
// the release that would delete it, which in turn range-guards the file's
// open records: whichever commits second loses. An open of a released file
// is stale.
func (o Opens) Put(ctx context.Context, share entity.ShareID, op entity.Open) error {
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
func (o Opens) Get(ctx context.Context, share entity.ShareID, file entity.FileID, id entity.OpenID) (op entity.Open, err error) {
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
func (o Opens) Of(ctx context.Context, share entity.ShareID, file entity.FileID) (out []entity.Open, err error) {
	err = o.s.kv.View(ctx, func(r kv.Reader) (err error) {
		out, err = opensOf(r, share, file)
		return err
	})
	return out, err
}

// Delete removes a durable open with its index entry and its durable locks,
// and reports whether it left the file orphaned: no entry and no durable
// open, so the filesystem service runs its release. A share's root has no
// entry from its creation and is never orphaned.
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
		if f.Nlink > 0 || f.IsRoot() {
			orphan = false
			return nil
		}
		left, err := opensOf(tx, share, file)
		orphan = len(left) == 0
		return err
	})
	return orphan, err
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

func putOpen(tx kv.Txn, share entity.ShareID, shard uint64, op entity.Open) error {
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
func opensOf(r kv.Reader, share entity.ShareID, file entity.FileID) ([]entity.Open, error) {
	pre := opensPrefix(share, file)
	var out []entity.Open
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
