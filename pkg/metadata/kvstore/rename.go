package kvstore

import (
	"bytes"
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// Rename moves an entry, in one transaction: the source entry goes, the
// destination entry is installed over any entry there, the replaced file's
// Nlink falls with a pending release on its last name, and a moved
// directory's parent follows it. Nothing but entries and the two files'
// records changes: no handle, lock or byte moves.
//
// The loop check walks the destination's ancestors inside the transaction,
// reading each, so two renames that would each put a directory under the
// other conflict and the second is refused, in either commit order.
//
// open is the opens of the file a rename replaces, as for Unlink: made
// durable when the rename takes that file's last entry.
func (n Namespace) Rename(ctx context.Context, id entity.Identity, from entity.Handle, fromName []byte, to entity.Handle, toName []byte, open ...entity.Open) (Orphaned, error) {
	var out Orphaned
	err := n.s.kv.Update(ctx, func(tx kv.Txn) (err error) {
		out, err = n.rename(tx, id, from, fromName, to, toName, open)
		return err
	})
	return out, err
}

func (n Namespace) rename(tx kv.Txn, id entity.Identity, from entity.Handle, fromName []byte, to entity.Handle, toName []byte, open []entity.Open) (Orphaned, error) {
	share, sd, err := dirIn(tx, from)
	if err != nil {
		return Orphaned{}, err
	}
	toShare, dd, err := dirIn(tx, to)
	if err != nil {
		return Orphaned{}, err
	}
	if toShare != share {
		return Orphaned{}, entity.ErrWrongShare
	}
	b, err := n.s.bound(tx, share)
	if err != nil {
		return Orphaned{}, err
	}
	srcKey, err := b.entryKeyFor(share, sd.ID, fromName)
	if err != nil {
		return Orphaned{}, err
	}
	dstKey, err := b.entryKeyFor(share, dd.ID, toName)
	if err != nil {
		return Orphaned{}, err
	}
	v, ok, err := get(tx, srcKey)
	if err != nil {
		return Orphaned{}, err
	}
	if !ok {
		return Orphaned{}, merrs.NewNotFoundError(string(fromName), "entry")
	}
	src, err := decodeEntry(sd.ID, v)
	if err != nil {
		return Orphaned{}, err
	}
	child, err := fileIn(tx, share, src.Child)
	if err != nil {
		return Orphaned{}, err
	}
	if err := mayRemove(tx, id, sd, child); err != nil {
		return Orphaned{}, err
	}

	// The destination: the same entry under a new spelling, another name of
	// the same file, or an entry to replace.
	var replaced *entity.File
	if bytes.Equal(srcKey, dstKey) {
		if bytes.Equal(src.Name, toName) {
			return Orphaned{}, nil
		}
	} else if v, ok, err := get(tx, dstKey); err != nil {
		return Orphaned{}, err
	} else if ok {
		dst, err := decodeEntry(dd.ID, v)
		if err != nil {
			return Orphaned{}, err
		}
		if dst.Child == child.ID {
			return Orphaned{}, nil // two names of one file: POSIX changes nothing
		}
		f, err := fileIn(tx, share, dst.Child)
		if err != nil {
			return Orphaned{}, err
		}
		if err := replaceable(child, f); err != nil {
			return Orphaned{}, err
		}
		if err := mayRemove(tx, id, dd, f); err != nil {
			return Orphaned{}, err
		}
		replaced = &f
	}

	want := entity.AddFile
	if child.IsDir() {
		want = entity.AddSubdirectory
	}
	if err := authorize(tx, id, dd, want); err != nil {
		return Orphaned{}, err
	}
	if child.Project != dd.Project {
		return Orphaned{}, entity.ErrCrossProject
	}
	moved := child.IsDir() && sd.ID != dd.ID
	if moved {
		// Its ".." changes, which is a write of the directory itself.
		if err := authorize(tx, id, child, entity.WriteData); err != nil {
			return Orphaned{}, err
		}
		if err := notUnder(tx, share, dd, child.ID); err != nil {
			return Orphaned{}, err
		}
	}

	var out Orphaned
	if replaced != nil {
		if out, err = dropEntry(tx, b, dstKey, *replaced, open); err != nil {
			return Orphaned{}, err
		}
	}
	if !bytes.Equal(srcKey, dstKey) {
		if err := tx.Delete(srcKey); err != nil {
			return Orphaned{}, err
		}
	}
	if err := tx.Set(dstKey, encodeEntry(entity.Entry{Parent: dd.ID, Name: toName, Child: child.ID, Type: child.Type})); err != nil {
		return Orphaned{}, err
	}
	if moved {
		child.Parent = dd.ID
	}
	was := child.Version
	child.Change = tx.Now()
	child.Version = nextFileVersion(tx, child.Version)
	if err := putFile(tx, was, child); err != nil {
		return Orphaned{}, err
	}
	// The moved entry leaves sd and joins dd, and an empty directory it
	// replaces leaves dd: in one directory the two touches are one.
	left, joined := -dirCount(child), dirCount(child)
	if replaced != nil {
		joined -= dirCount(*replaced)
	}
	if dd.ID == sd.ID {
		return out, touchDir(tx, share, sd, left+joined)
	}
	if err := touchDir(tx, share, sd, left); err != nil {
		return Orphaned{}, err
	}
	if err := touchDir(tx, share, dd, joined); err != nil {
		return Orphaned{}, err
	}
	return out, nil
}

// replaceable is whether child may replace f at a name: a directory only an
// empty directory, which dropEntry proves, and a non-directory only a
// non-directory.
func replaceable(child, f entity.File) error {
	switch {
	case child.IsDir() && !f.IsDir():
		return merrs.NewNotDirectoryError(f.ID.String())
	case !child.IsDir() && f.IsDir():
		return merrs.NewIsDirectoryError(f.ID.String())
	}
	return nil
}

// notUnder refuses a move of directory moving into dir when dir is moving
// itself or one of its descendants: it walks dir's parents to the root,
// reading each, so a concurrent rename that changes one of them conflicts
// with this one. The walk ends because a directory has exactly one entry.
func notUnder(r kv.Reader, share entity.ShareID, dir entity.File, moving entity.FileID) error {
	for cur := dir; ; {
		if cur.ID == moving {
			return merrs.NewInvalidArgumentError("a directory cannot move under itself")
		}
		if cur.IsRoot() {
			return nil
		}
		next, err := fileIn(r, share, cur.Parent)
		if err != nil {
			return err
		}
		cur = next
	}
}
