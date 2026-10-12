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
func (n Namespace) Unlink(ctx context.Context, id entity.Identity, dir entity.Handle, name []byte) (Orphaned, error) {
	var out Orphaned
	err := n.s.kv.Update(ctx, func(tx kv.Txn) error {
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
		out, err = dropEntry(tx, key, child)
		if err != nil {
			return err
		}
		return touchDir(tx, share, d)
	})
	return out, err
}

// dropEntry deletes the entry at key naming child and takes one from
// child's Nlink, refusing a non-empty directory. A last entry writes the
// pending release.
func dropEntry(tx kv.Txn, key []byte, child entity.File) (Orphaned, error) {
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
	if err := putFile(tx, was, child); err != nil {
		return Orphaned{}, err
	}
	if child.Nlink > 0 {
		return Orphaned{}, nil
	}
	return Orphaned{File: child.ID}, tx.Set(releaseKey(child.Share, child.ID), encodeRelease())
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
func (n Namespace) Link(ctx context.Context, id entity.Identity, dir entity.Handle, name []byte, target entity.Handle) error {
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
		if err := authorize(tx, id, d, entity.AddFile); err != nil {
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
		return touchDir(tx, share, d)
	})
}

// PendingReleases lists the files of a share that lost their last entry and
// are not yet released, for recovery after a crash.
//
// ponytail: scans every file record of the share to find the release
// records among them, so recovery costs the share's whole file count. Index
// the pending releases under their own share-level prefix, a store format
// addition, once a recovery of a large share shows it.
func (n Namespace) PendingReleases(ctx context.Context, share entity.ShareID) iter.Seq2[entity.FileID, error] {
	prefix := filesPrefix(share)
	want := len(prefix) + keys.IDLen + 1
	return func(yield func(entity.FileID, error) bool) {
		var ids []entity.FileID
		err := n.s.kv.View(ctx, func(r kv.Reader) error {
			for row, err := range r.Scan(prefix, nil) {
				if err != nil {
					return err
				}
				if len(row.Key) == want && row.Key[want-1] == keys.FRel {
					ids = append(ids, entity.FileID(row.Key[len(prefix):want-1]))
				}
			}
			return nil
		})
		if err != nil {
			yield(entity.FileID{}, err)
			return
		}
		for _, id := range ids {
			if !yield(id, nil) {
				return
			}
		}
	}
}
