package kvstore

import (
	"context"
	"iter"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Orphaned names the file an operation left with no entry; zero when none.
// Its pending release is already written, and the filesystem service runs
// the release once no open holds it.
type Orphaned struct{ File entity.FileID }

// Unlink removes a name. Its file's Nlink falls by one in the same
// transaction, and when it reaches zero a pending release is written. A
// directory must be empty, which this transaction proves under a range
// guard of its entries while writing its record: a create racing into it
// conflicts in either commit order.
//
// open is the file's opens, held only in memory at its primary: when this
// unlink takes the file's last entry, each is written durable in the same
// transaction, since an open is the second holder of a file and the release
// finds its holders only in the store.
func (n Namespace) Unlink(ctx context.Context, id entity.Identity, dir entity.Handle, name []byte, open ...entity.Open) (_ Orphaned, err error) {
	defer opError(&err, "unlink", dir, name)
	var out Orphaned
	err = n.s.kv.Update(ctx, func(tx kv.Txn) error {
		share, d, err := dirIn(tx, dir)
		if err != nil {
			return err
		}
		b, err := n.s.bound(tx, share)
		if err != nil {
			return err
		}
		key, err := b.entryKeyFor(share, d.ID, name)
		if err != nil {
			return err
		}
		v, ok, err := get(tx, key)
		if err != nil {
			return err
		}
		if !ok {
			return merrs.NewNotFoundError(string(name), "entry")
		}
		e, err := decodeEntry(d.ID, v)
		if err != nil {
			return err
		}
		child, err := fileIn(tx, share, e.Child)
		if err != nil {
			return err
		}
		if err := mayRemove(tx, id, d, child); err != nil {
			return err
		}
		out, err = dropEntry(tx, b, key, child, open)
		if err != nil {
			return err
		}
		return touchDir(tx, share, d, -dirCount(child))
	})
	return out, err
}

// dropEntry deletes the entry at key naming child and takes one from
// child's Nlink, refusing a non-empty directory. A last entry writes the
// pending release, and makes durable each of open that opens child. A
// delete-pending record naming this entry goes with it.
func dropEntry(tx kv.Txn, b bound, key []byte, child entity.File, open []entity.Open) (Orphaned, error) {
	if child.IsDir() {
		if err := provedEmpty(tx, child); err != nil {
			return Orphaned{}, err
		}
	}
	if child.Nlink == 0 {
		return Orphaned{}, entity.ErrInconsistent
	}
	was := child.Version
	child.Nlink--
	child.Change = tx.Now()
	child.Version = nextFileVersion(tx, child.Version)
	if err := tx.Delete(key); err != nil {
		return Orphaned{}, err
	}
	if err := dropDeletePendingAt(tx, b, child, key); err != nil {
		return Orphaned{}, err
	}
	if err := putFile(tx, was, child); err != nil {
		return Orphaned{}, err
	}
	if child.Nlink > 0 {
		return Orphaned{}, nil
	}
	for _, op := range open {
		if op.File != child.ID {
			continue
		}
		if err := putOpen(tx, child.Share, child.Shard, op); err != nil {
			return Orphaned{}, err
		}
	}
	return Orphaned{File: child.ID}, putRelease(tx, child.Share, child.Shard, child.ID)
}

// provedEmpty refuses a directory that holds an entry, guarding its entries
// so an entry added after this snapshot aborts the removal.
func provedEmpty(tx kv.Txn, d entity.File) error {
	prefix := entriesPrefix(d.Share, d.ID)
	if err := tx.GuardRange(prefix); err != nil {
		return err
	}
	for _, err := range tx.Scan(prefix, nil) {
		if err != nil {
			return err
		}
		return merrs.NewNotEmptyError(d.ID.String())
	}
	return nil
}

// Link adds a name for an existing file. It refuses a directory, which has
// exactly one entry, and a file with no entry left, which is released or
// about to be: a link racing the release would name a deleted file.
func (n Namespace) Link(ctx context.Context, id entity.Identity, dir entity.Handle, name []byte, target entity.Handle) (err error) {
	defer opError(&err, "link", dir, name)
	return n.s.kv.Update(ctx, func(tx kv.Txn) error {
		share, d, err := dirIn(tx, dir)
		if err != nil {
			return err
		}
		_, f, err := resolveIn(tx, share, target)
		if err != nil {
			return err
		}
		switch {
		case f.IsDir():
			return merrs.NewIsDirectoryError(f.ID.String())
		case f.Nlink == 0:
			return entity.ErrStale
		case f.IsImmutable() || f.IsAppendOnly():
			return merrs.NewPermissionDeniedError("immutable or append-only " + f.ID.String())
		case f.Project != d.Project:
			return entity.ErrCrossProject
		}
		if err := authorize(tx, id, d, entity.AddFile|entity.Execute); err != nil {
			return err
		}
		b, err := n.s.bound(tx, share)
		if err != nil {
			return err
		}
		key, err := b.entryKeyFor(share, d.ID, name)
		if err != nil {
			return err
		}
		if _, ok, err := get(tx, key); err != nil {
			return err
		} else if ok {
			return merrs.NewAlreadyExistsError(string(name))
		}
		was := f.Version
		f.Nlink++
		f.Change = tx.Now()
		f.Version = nextFileVersion(tx, f.Version)
		if err := tx.Set(key, encodeEntry(entity.Entry{Parent: d.ID, Name: name, Child: f.ID, Type: f.Type})); err != nil {
			return err
		}
		if err := putFile(tx, was, f); err != nil {
			return err
		}
		return touchDir(tx, share, d, 0) // a link never names a directory
	})
}

// putRelease writes a file's pending release and its index entry; dropRelease
// deletes both. Every pending release is written and deleted through these,
// so the index never misses one.
func putRelease(tx kv.Txn, share entity.ShareID, shard uint64, id entity.FileID) error {
	if err := tx.Set(releaseKey(share, id), encodeRelease()); err != nil {
		return err
	}
	return tx.Set(releaseIndexKey(share, shard, id), encodeEmpty())
}

func dropRelease(tx kv.Txn, share entity.ShareID, shard uint64, id entity.FileID) error {
	if err := tx.Delete(releaseKey(share, id)); err != nil {
		return err
	}
	return tx.Delete(releaseIndexKey(share, shard, id))
}

// PendingReleases lists the files in one shard of a share that lost their
// last entry and are not yet released, for a primary's recovery after a
// crash, from that shard's range of the pending-release index: it reads one
// key per pending release, whatever the share's size, in pages.
func (n Namespace) PendingReleases(ctx context.Context, share entity.ShareID, shard uint64) iter.Seq2[entity.FileID, error] {
	prefix := keys.Uint64(shareKey(share, keys.SPr), shard)
	return pages(ctx, n.s.kv, prefix, nil, func(_ kv.Reader, row kv.KeyValue) (entity.FileID, error) {
		id, _, err := keys.ReadID(row.Key[len(prefix):], keys.IDLen)
		return entity.FileID(id), err
	})
}
