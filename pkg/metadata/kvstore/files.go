package kvstore

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Files is the filesystem service's view of files by handle.
type Files struct{ s *Store }

func (s *Store) Files() Files { return Files{s} }

// rootNumber is the numeric file id of every share's root; the allocator
// starts above it.
const rootNumber = 1

// Resolve finds the file a handle names in share, with one point read and
// no entry read: entity.ErrStale when the file is gone or the handle has no
// valid form, entity.ErrWrongShare when it names another share.
func (f Files) Resolve(ctx context.Context, share entity.ShareID, h entity.Handle) (entity.FileID, error) {
	var id entity.FileID
	err := f.s.kv.View(ctx, func(r kv.Reader) (err error) {
		id, _, err = resolveIn(r, share, h)
		return err
	})
	return id, err
}

func resolveIn(r kv.Reader, share entity.ShareID, h entity.Handle) (entity.FileID, entity.File, error) {
	hs, id, err := h.Parse()
	if err != nil {
		return id, entity.File{}, err
	}
	if hs != share {
		return id, entity.File{}, entity.ErrWrongShare
	}
	file, err := fileIn(r, share, id)
	return id, file, err
}

// fileIn reads a file's record, reporting a missing one as stale.
func fileIn(r kv.Reader, share entity.ShareID, id entity.FileID) (entity.File, error) {
	b, ok, err := get(r, fileKey(share, id))
	if err != nil {
		return entity.File{}, err
	}
	if !ok {
		return entity.File{}, entity.ErrStale
	}
	return decodeFile(share, id, b)
}

// Root is the share's root and its handle, for a mount or tree connect,
// refused unless the share grant admits the caller.
func (f Files) Root(ctx context.Context, id entity.Identity, share entity.ShareID) (entity.Handle, entity.File, error) {
	var root entity.File
	err := f.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, shareKey(share, keys.SInfo))
		if err != nil {
			return err
		}
		if !ok {
			return merrs.NewNotFoundError(share.String(), "share")
		}
		info, err := decodeShareInfo(share, b)
		if err != nil {
			return err
		}
		access, err := grantFor(r, share, id)
		if err != nil {
			return err
		}
		if access < entity.AccessRead {
			return merrs.NewAccessDeniedError("no grant on share " + info.Name)
		}
		root, err = fileIn(r, share, info.Root)
		return err
	})
	if err != nil {
		return nil, entity.File{}, err
	}
	return entity.NewHandle(share, root.ID), root, nil
}

// grantFor is a resolved identity's access to share: its own grant decides,
// even one of none; without one, the widest grant naming any group the
// identity carries does.
func grantFor(r kv.Reader, share entity.ShareID, id entity.Identity) (entity.ShareAccess, error) {
	g, ok, err := grantIn(r, share, id.Principal.ID)
	if err != nil || ok {
		return g.Access, err
	}
	var access entity.ShareAccess
	for _, grp := range append([]entity.Principal{id.Group}, id.Groups...) {
		if grp.IsZero() {
			continue
		}
		g, ok, err := grantIn(r, share, grp.ID)
		if err != nil {
			return 0, err
		}
		if ok && g.Access > access {
			access = g.Access
		}
	}
	return access, nil
}

// newRoot is a share's root directory: no entry names it, its parent is
// itself so ".." at the root stays there, and it is owned by nobody until a
// privileged caller gives it an owner.
func newRoot(tx kv.Txn, s entity.Share) entity.File {
	now := tx.Now()
	return entity.File{
		ID: s.Root, Share: s.ID, Type: entity.Directory, Parent: s.Root,
		Mode: 0o755, Birth: now, Modify: now, Change: now,
		Version: nextVersion(tx, 0), Number: rootNumber,
	}
}
