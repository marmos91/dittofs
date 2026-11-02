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
