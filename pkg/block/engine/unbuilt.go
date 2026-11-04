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

// Delete is the file's release; never the journal's eviction. retry is
// non-nil only when the release is pending, and yields its outcome once
// (RFC 8 §8.1). The send on it never blocks: it has capacity 1, as its
// receiver may stop listening, abandoned at the filesystem's Stop, which
// precedes the engine's Close (RFC 17 §2.5).
func (e *Engine) Delete(ctx context.Context, s entity.ShareID, f entity.FileID) (retry <-chan error, err error) {
	return nil, errUnbuilt
}

// Copy copies xs in order as bounded clones, and may answer a short count.
func (e *Engine) Copy(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, xs []vfserr.Extent) (int64, error) {
	return 0, errUnbuilt
}
