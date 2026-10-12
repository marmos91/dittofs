package kvstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"sync"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Namespace is the filesystem service's view of names: lookup, listings,
// and the operations that add and remove entries.
type Namespace struct{ s *Store }

func (s *Store) Namespace() Namespace { return Namespace{s} }

// maxNameBytes is the longest name any share accepts.
const maxNameBytes = 255

// bound is what a share fixes at create for the names in it. None of it ever
// changes, so it is cached once read.
type bound struct {
	fold  entity.FoldRule
	names entity.NameRule
	key   [entity.DigestKeyLen]byte
}

type boundCache struct {
	mu     sync.Mutex
	shares map[entity.ShareID]bound
}

func (s *Store) bound(r kv.Reader, share entity.ShareID) (bound, error) {
	s.bounds.mu.Lock()
	b, ok := s.bounds.shares[share]
	s.bounds.mu.Unlock()
	if ok {
		return b, nil
	}
	v, found, err := get(r, shareKey(share, keys.SInfo))
	if err != nil {
		return b, err
	}
	if !found {
		return b, entity.ErrStale
	}
	info, err := decodeShareInfo(share, v)
	if err != nil {
		return b, err
	}
	b = bound{fold: info.Fold, names: info.Names, key: info.DigestKey}
	s.bounds.mu.Lock()
	if s.bounds.shares == nil {
		s.bounds.shares = map[entity.ShareID]bound{}
	}
	s.bounds.shares[share] = b
	s.bounds.mu.Unlock()
	return b, nil
}

// entryKeyFor validates name under the share's rule and returns its entry
// key in dir.
func (b bound) entryKeyFor(share entity.ShareID, dir entity.FileID, name []byte) ([]byte, error) {
	if err := entity.ValidateName(name, b.names, maxNameBytes); err != nil {
		return nil, err
	}
	folded := b.fold.Fold(name)
	return entryKey(share, dir, entity.Digest(&b.key, folded), folded), nil
}

// dirIn resolves a handle that must name a directory.
func dirIn(r kv.Reader, h entity.Handle) (entity.ShareID, entity.File, error) {
	share, _, err := h.Parse()
	if err != nil {
		return share, entity.File{}, err
	}
	_, dir, err := resolveIn(r, share, h)
	if err != nil {
		return share, dir, err
	}
	if !dir.IsDir() {
		return share, dir, merrs.NewNotDirectoryError(dir.ID.String())
	}
	return share, dir, nil
}

// Lookup resolves one name in a directory: one entry read and one file read,
// after the caller's traverse right on the directory is checked.
func (n Namespace) Lookup(ctx context.Context, id entity.Identity, dir entity.Handle, name []byte) (entity.Handle, entity.File, error) {
	var child entity.File
	err := n.s.kv.View(ctx, func(r kv.Reader) error {
		share, d, err := dirIn(r, dir)
		if err != nil {
			return err
		}
		if err := authorize(r, id, d, entity.Execute); err != nil {
			return err
		}
		b, err := n.s.bound(r, share)
		if err != nil {
			return err
		}
		key, err := b.entryKeyFor(share, d.ID, name)
		if err != nil {
			return err
		}
		v, ok, err := get(r, key)
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
		child, err = fileIn(r, share, e.Child)
		return err
	})
	if err != nil {
		return nil, entity.File{}, err
	}
	return entity.NewHandle(child.Share, child.ID), child, nil
}

// NewFile is what a create makes: the type and its type-specific fields,
// the requested mode, and optionally an owner, a group and times the request
// names. Verifier makes the create exclusive.
type NewFile struct {
	Type     entity.FileType
	Mode     uint32
	Target   []byte
	Device   entity.DeviceID
	Attrs    entity.Attrs // owner, group, birth, access and modify, when the request names them
	Verifier []byte
}

// Create adds a name in dir for a new file and returns it.
//
// The create reads the directory's record, which conflicts with a write of
// it and with nothing else: two creates in one directory both commit, while
// a removal of the directory, which writes the record, aborts a create that
// read it first. The directory's times move by a delta record of the create's
// own, so the record itself is never rewritten here.
//
// An exclusive create that finds the name taken returns the file there when
// its verifier is the request's, the retry of a create whose reply was lost,
// and fails as existing otherwise.
func (n Namespace) Create(ctx context.Context, id entity.Identity, dir entity.Handle, name []byte, nf NewFile) (entity.Handle, entity.File, error) {
	if nf.Type == entity.Stream || nf.Type.String() == "" {
		return nil, entity.File{}, merrs.NewInvalidArgumentError("cannot create a " + nf.Type.String())
	}
	share, _, err := dir.Parse()
	if err != nil {
		return nil, entity.File{}, err
	}
	number, err := n.s.nextNumber(ctx, share)
	if err != nil {
		return nil, entity.File{}, err
	}
	fileID := entity.NewFileID()
	var made entity.File
	err = n.s.kv.Update(ctx, func(tx kv.Txn) error {
		share, d, err := dirIn(tx, dir)
		if err != nil {
			return err
		}
		want := entity.AddFile
		if nf.Type == entity.Directory {
			want = entity.AddSubdirectory
		}
		if err := authorize(tx, id, d, want); err != nil {
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
		if v, ok, err := get(tx, key); err != nil {
			return err
		} else if ok {
			return existing(tx, share, d.ID, v, name, nf.Verifier, &made)
		}
		f, err := newFile(tx, id, d, fileID, number, nf)
		if err != nil {
			return err
		}
		if err := tx.Set(fileKey(share, f.ID), encodeFile(f)); err != nil {
			return err
		}
		if err := tx.Set(key, encodeEntry(entity.Entry{Parent: d.ID, Name: name, Child: f.ID, Type: f.Type})); err != nil {
			return err
		}
		made = f
		return touchDir(tx, share, d)
	})
	if err != nil {
		return nil, entity.File{}, err
	}
	return entity.NewHandle(share, made.ID), made, nil
}

// existing answers a create whose name is taken.
func existing(r kv.Reader, share entity.ShareID, dir entity.FileID, v, name, verifier []byte, out *entity.File) error {
	if verifier == nil {
		return merrs.NewAlreadyExistsError(string(name))
	}
	e, err := decodeEntry(dir, v)
	if err != nil {
		return err
	}
	f, err := fileIn(r, share, e.Child)
	if err != nil {
		return err
	}
	if !bytes.Equal(f.CreateVerifier, verifier) {
		return merrs.NewAlreadyExistsError(string(name))
	}
	*out = f
	return nil
}

// newFile is the File a create makes in d. Its owner is the caller. Its
// group is d's when d has setgid; otherwise the credential's group, the
// owner's primary group when the credential names none, and the owner's own
// principal when it has neither, a group of one. A setgid bit the caller
// could not have set on a file of that group is cleared; a directory under
// a setgid directory is setgid itself.
func newFile(tx kv.Txn, id entity.Identity, d entity.File, fileID entity.FileID, number uint64, nf NewFile) (entity.File, error) {
	now := tx.Now()
	f := entity.File{
		ID: fileID, Share: d.Share, Type: nf.Type,
		Owner: id.Principal, Mode: nf.Mode & entity.ModePerm,
		Birth: now, Access: now, Modify: now, Change: now,
		Version: nextVersion(tx, 0), Nlink: 1,
		Shard: d.Shard, Project: d.Project, Number: number,
		CreateVerifier: cloneOrNil(nf.Verifier), Target: cloneOrNil(nf.Target), Device: nf.Device,
	}
	if f.IsDir() {
		f.Parent = d.ID
	}
	group, err := createGroup(tx, id, d)
	if err != nil {
		return f, err
	}
	f.Group = group
	if d.Mode&entity.ModeSetgid != 0 && f.IsDir() {
		f.Mode |= entity.ModeSetgid
	}
	if !f.IsDir() && f.Mode&entity.ModeSetgid != 0 && !id.Privileged && !id.InGroup(f.Group) {
		f.Mode &^= entity.ModeSetgid
	}
	f, err = applyRequested(id, f, nf.Attrs)
	if err != nil {
		return f, err
	}
	return f, f.Validate()
}

func createGroup(r kv.Reader, id entity.Identity, d entity.File) (entity.Principal, error) {
	if d.Mode&entity.ModeSetgid != 0 && !d.Group.IsZero() {
		return d.Group, nil
	}
	if !id.Group.IsZero() {
		return id.Group, nil
	}
	if id.Principal.Kind == entity.PrincipalUser {
		b, ok, err := get(r, userKey(id.Principal.ID))
		if err != nil {
			return entity.Principal{}, err
		}
		if ok {
			u, err := decodeUser(id.Principal.ID, b)
			if err != nil {
				return entity.Principal{}, err
			}
			if !u.Primary.IsZero() {
				return u.Primary, nil
			}
		}
	}
	return id.Principal, nil
}

// applyRequested applies the owner, group and times a create request names,
// as a change of them on the new file would be: a different owner only by a
// privileged caller, a group only one the caller is in. A refused change
// fails the create whole.
func applyRequested(id entity.Identity, f entity.File, a entity.Attrs) (entity.File, error) {
	a.Mask &= entity.AttrOwner | entity.AttrGroup | entity.AttrBirth | entity.AttrAccess | entity.AttrModify
	if a.Changes(entity.AttrOwner) && a.Owner != f.Owner && !id.Privileged {
		return f, merrs.NewPermissionDeniedError("owner of a new file")
	}
	if a.Changes(entity.AttrGroup) && a.Group != f.Group && !id.Privileged && !id.InGroup(a.Group) {
		return f, merrs.NewPermissionDeniedError("group of a new file")
	}
	verifier := f.CreateVerifier
	f = f.Apply(a)
	f.CreateVerifier = verifier // the create's own verifier is not a SetAttrs
	return f, nil
}

// touchDir records that d's entries changed: a delta under d with the new
// times and a version above d's, written without reading or rewriting d's
// record beyond the read the caller already made.
//
// ponytail: the delta's unique part is 32 random bytes rather than the
// writer's node ID, epoch and sequence, since the store has no node identity
// yet. Switch to those once a node is registered at start.
func touchDir(tx kv.Txn, share entity.ShareID, d entity.File) error {
	var unique [32]byte
	rand.Read(unique[:])
	now := tx.Now()
	return tx.Set(deltaKey(share, d.ID, unique), encodeDelta(delta{
		Modify: now, Change: now, Version: nextVersion(tx, d.Version),
	}))
}
