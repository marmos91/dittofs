package engine

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// The content methods, called only by the filesystem service. Each names the
// file's share beside the file, as a FileID carries no shard and the engine
// keys a file's binding by its share. A share with no binding is to answer
// a Delay with DelayFrozen, as its binding is still to come: a share being
// removed never reaches the engine unbound, since admission answers ErrStale
// from the removal mark. None is built yet: each answers errUnbuilt, and moves
// to its own file as it is built.

// Truncate sets the file's size.
func (e *Engine) Truncate(ctx context.Context, s entity.ShareID, f entity.FileID, size int64) error {
	return errUnbuilt
}

// Deallocate makes [off, off+n) a hole.
func (e *Engine) Deallocate(ctx context.Context, s entity.ShareID, f entity.FileID, off, n int64) error {
	return errUnbuilt
}

// Delete is the file's release; never the journal's eviction.
func (e *Engine) Delete(ctx context.Context, s entity.ShareID, f entity.FileID) error {
	return errUnbuilt
}

// Clone makes x of dst the content of x of src, atomically, answered when
// done. Each file is named with its own share: on one node a clone between
// two shares of one namespace reaches the engine.
func (e *Engine) Clone(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, x vfserr.Extent) error {
	return errUnbuilt
}

// Copy copies xs in order as bounded clones, and may answer a short count.
func (e *Engine) Copy(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, xs []vfserr.Extent) (int64, error) {
	return 0, errUnbuilt
}
