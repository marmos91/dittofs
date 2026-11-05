package vfs

import (
	"context"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// The calls below are not built yet: each is refused while the filesystem is
// not serving, as every call is, and otherwise answered errUnbuilt. Each moves
// to its own file as it is built.

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
