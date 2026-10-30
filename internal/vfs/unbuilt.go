package vfs

import (
	"context"

	"github.com/marmos91/dittofs/pkg/vfs"
)

// The calls below are not built yet: each is refused while the filesystem is
// not serving, as every call is, and otherwise answered errUnbuilt. Each moves
// to its own file as it is built.

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
