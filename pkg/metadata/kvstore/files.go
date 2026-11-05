package kvstore

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
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

// decision: resolving a handle does not read the share's state, so a handle
// into a share being removed or retired still resolves while its records
// stay. The state gates every call where the grant is evaluated, in the
// filesystem service, which holds both; reading it here too would add a
// read to every operation, GETATTR's single one included. Gate here if a
// caller ever reaches the store without passing that service.
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

// Get is a file's attributes, GETATTR's answer from the store: one read for
// a file, and for a directory its record with the time deltas not yet folded
// into it applied.
func (f Files) Get(ctx context.Context, id entity.Identity, h entity.Handle) (entity.File, error) {
	var file entity.File
	err := f.s.kv.View(ctx, func(r kv.Reader) error {
		share, _, err := h.Parse()
		if err != nil {
			return err
		}
		if _, file, err = resolveIn(r, share, h); err != nil {
			return err
		}
		if err := authorize(r, id, file, entity.ReadAttributes); err != nil {
			return err
		}
		if file.IsDir() {
			file, err = withDeltas(r, file)
		}
		return err
	})
	return file, err
}

// withDeltas applies a directory's unfolded time deltas to its record: the
// latest times and the highest version win.
func withDeltas(r kv.Reader, d entity.File) (entity.File, error) {
	for row, err := range r.Scan(deltasPrefix(d.Share, d.ID), nil) {
		if err != nil {
			return d, err
		}
		x, err := decodeDelta(row.Value)
		if err != nil {
			return d, err
		}
		d = applyDelta(d, x)
	}
	return d, nil
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
		if err := liveShare(r, share); err != nil {
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
// identity carries does, even when every one found is none; without one, a
// grant naming authenticated users
// does for an authenticated identity, then one naming everyone. A caller
// carrying its own Access is held to that. A read-only narrowing is not
// applied here: narrowed refuses it on its own.
func grantFor(r kv.Reader, share entity.ShareID, id entity.Identity) (entity.ShareAccess, error) {
	if id.Access != entity.AccessNone {
		return id.Access, nil
	}
	g, ok, err := grantIn(r, share, id.Principal.ID)
	if err != nil || ok {
		return g.Access, err
	}
	var access entity.ShareAccess
	found := false
	for _, grp := range append([]entity.Principal{id.Group}, id.Groups...) {
		if grp.IsZero() {
			continue
		}
		g, ok, err := grantIn(r, share, grp.ID)
		if err != nil {
			return 0, err
		}
		if ok {
			found, access = true, max(access, g.Access)
		}
	}
	if found {
		return access, nil
	}
	defaults := []entity.Principal{entity.Everyone}
	if id.Authenticated {
		defaults = []entity.Principal{entity.AuthenticatedUsers, entity.Everyone}
	}
	for _, d := range defaults {
		if g, ok, err := grantIn(r, share, d.ID); err != nil || ok {
			return g.Access, err
		}
	}
	return entity.AccessNone, nil
}

// newRoot is a share's root directory: no entry names it, its parent is
// itself so ".." at the root stays there, and it is owned by nobody until a
// privileged caller gives it an owner.
func newRoot(tx kv.Txn, s entity.Share) entity.File {
	now := tx.Now()
	return entity.File{
		ID: s.Root, Share: s.ID, Type: entity.Directory, Parent: s.Root,
		Mode: 0o755, Birth: now, Modify: now, Change: now,
		Version: nextFileVersion(tx, entity.JournalVersion{}), Number: rootNumber, Shard: uint64(s.Shard),
	}
}

// SetProject makes the empty directory h the root of a new project, a tree
// quota, and returns its number; every file created below it inherits it. On
// a project's root it returns that project and writes nothing, so a lost
// reply can be retried. Only a privileged caller makes one. A directory with
// an entry is refused not empty, its entries guarded so a create racing this
// either lands first and refuses it or lands after and inherits the project;
// a directory inside another project is refused invalid, as projects do not
// nest.
//
// decision: a project is numbered by its root's file number, which is unique
// in the share and never reused, so it needs no allocator of its own. A
// project outlives its root and its number stays taken with it. Overturned if
// a project must be numbered apart from any file, as one made without a root
// would be.
func (f Files) SetProject(ctx context.Context, id entity.Identity, h entity.Handle) (project uint64, err error) {
	if !id.Privileged {
		return 0, merrs.NewPrivilegeRequiredError("set project")
	}
	err = f.s.kv.Update(ctx, func(tx kv.Txn) error {
		share, _, err := h.Parse()
		if err != nil {
			return err
		}
		_, d, err := resolveIn(tx, share, h)
		switch {
		case err != nil:
			return err
		case !d.IsDir():
			return merrs.NewInvalidArgumentError("a project's root is a directory")
		case d.Project != 0 && d.Project == d.Number:
			project = d.Project
			return nil
		case d.Project != 0:
			return merrs.NewInvalidArgumentError("projects do not nest")
		}
		if err := provedEmpty(tx, d); err != nil {
			return err
		}
		stored := d.Version
		if _, _, err := foldDeltas(tx, &d, -1); err != nil {
			return err
		}
		next := d
		next.Project = d.Number
		changed(tx, &next)
		if err := chargeFile(tx, share, &d, &next); err != nil {
			return err
		}
		project = next.Project
		return putFile(tx, stored, next)
	})
	return project, err
}
