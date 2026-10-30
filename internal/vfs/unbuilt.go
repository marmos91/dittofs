package vfs

import (
	"context"

	"github.com/marmos91/dittofs/internal/vfs/pseudofs"

	"github.com/marmos91/dittofs/pkg/vfs"
)

// The calls below are not built yet: each is refused while the filesystem is
// not serving, as every call is, and otherwise answered errUnbuilt. Each moves
// to its own file as it is built.

// Lookup, ReadDir and GetAttr answer a pseudo directory; a file of a share
// is not built yet.

func (fs *Filesystem) Lookup(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte) (vfs.Handle, vfs.File, error) {
	if !pseudofs.IsHandle(dir) {
		return nil, vfs.File{}, fs.unbuilt(ctx)
	}
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return nil, vfs.File{}, err
	}
	defer done()
	return fs.pseudoLookup(ctx, c, dir, name)
}

func (fs *Filesystem) ReadDir(ctx context.Context, c vfs.Call, dir vfs.Handle, after vfs.Cursor, max int, want vfs.EntryFields) (vfs.Page, error) {
	if !pseudofs.IsHandle(dir) {
		return vfs.Page{}, fs.unbuilt(ctx)
	}
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.Page{}, err
	}
	defer done()
	return fs.pseudoReadDir(ctx, c, dir, after, max, want)
}

func (fs *Filesystem) Create(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, a vfs.CreateArgs) (vfs.CreateResult, error) {
	return vfs.CreateResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Link(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, target vfs.Handle) (vfs.CreateResult, error) {
	return vfs.CreateResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Unlink(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte) (vfs.UnlinkResult, error) {
	return vfs.UnlinkResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Rmdir(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte) (vfs.UnlinkResult, error) {
	return vfs.UnlinkResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Rename(ctx context.Context, c vfs.Call, from vfs.Target, fromName []byte, to vfs.Handle, toName []byte, noReplace bool) (vfs.RenameResult, error) {
	return vfs.RenameResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) GetAttr(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.File, error) {
	if !pseudofs.IsHandle(h) {
		return vfs.File{}, fs.unbuilt(ctx)
	}
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.File{}, err
	}
	defer done()
	t, err := fs.tree(ctx)
	if err != nil {
		return vfs.File{}, err
	}
	n, ok := t.Node(h)
	if !ok {
		return vfs.File{}, vfs.ErrStale
	}
	return pseudoFile(t, n), nil
}

func (fs *Filesystem) SetAttr(ctx context.Context, c vfs.Call, t vfs.Target, a vfs.AttrChange) (vfs.WCC, error) {
	return vfs.WCC{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) GetACL(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.ACL, error) {
	return vfs.ACL{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) SetACL(ctx context.Context, c vfs.Call, t vfs.Target, acl vfs.ACL) (vfs.WCC, error) {
	return vfs.WCC{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Xattrs(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.Xattr, error) {
	return nil, fs.unbuilt(ctx)
}

func (fs *Filesystem) SetXattr(ctx context.Context, c vfs.Call, t vfs.Target, x vfs.Xattr, m vfs.XattrMode) (vfs.WCC, error) {
	return vfs.WCC{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) RemoveXattr(ctx context.Context, c vfs.Call, t vfs.Target, name []byte) (vfs.WCC, error) {
	return vfs.WCC{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Streams(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.File, error) {
	return nil, fs.unbuilt(ctx)
}

func (fs *Filesystem) Access(ctx context.Context, c vfs.Call, h vfs.Handle, parent vfs.Handle, want vfs.Access) (vfs.Access, error) {
	return 0, fs.unbuilt(ctx)
}

func (fs *Filesystem) Open(ctx context.Context, c vfs.Call, req vfs.OpenRequest) (vfs.OpenResult, error) {
	return vfs.OpenResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Downgrade(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs, access vfs.Access, deny vfs.Access) (uint32, error) {
	return 0, fs.unbuilt(ctx)
}

func (fs *Filesystem) ConfirmOpen(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs) (uint32, error) {
	return 0, fs.unbuilt(ctx)
}

func (fs *Filesystem) Close(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs) (*vfs.WCC, error) {
	return nil, fs.unbuilt(ctx)
}

func (fs *Filesystem) Read(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, dst []byte) (int, bool, error) {
	return 0, false, fs.unbuilt(ctx)
}

func (fs *Filesystem) Write(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, src []byte, st vfs.Stability) (vfs.WriteResult, error) {
	return vfs.WriteResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Commit(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, length int64) (vfs.Verifier, error) {
	return vfs.Verifier{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Deallocate(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, length int64) (vfs.WCC, error) {
	return vfs.WCC{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Seek(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, what vfs.SeekWhat) (int64, error) {
	return 0, fs.unbuilt(ctx)
}

func (fs *Filesystem) Copy(ctx context.Context, c vfs.Call, a vfs.CopyArgs) (vfs.CopyResult, error) {
	return vfs.CopyResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Clone(ctx context.Context, c vfs.Call, a vfs.CopyArgs) (vfs.WCC, error) {
	return vfs.WCC{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Lock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, r vfs.LockReq) (vfs.StateRef, error) {
	return vfs.StateRef{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) CancelLock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, r vfs.ByteRange) (bool, error) {
	return false, fs.unbuilt(ctx)
}

func (fs *Filesystem) TestLock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, r vfs.ByteRange, exclusive bool) (*vfs.Lock, error) {
	return nil, fs.unbuilt(ctx)
}

func (fs *Filesystem) Unlock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, rs []vfs.ByteRange, q vfs.Seqs) (uint32, error) {
	return 0, fs.unbuilt(ctx)
}

func (fs *Filesystem) Watch(ctx context.Context, c vfs.Call, w vfs.WatchSpec) (vfs.WatchID, error) {
	return vfs.WatchID{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Unwatch(ctx context.Context, c vfs.Call, w vfs.WatchID) error {
	return fs.unbuilt(ctx)
}
