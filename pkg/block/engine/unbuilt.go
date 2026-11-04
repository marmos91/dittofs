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

// Copy copies xs in order as bounded clones, and may answer a short count.
func (e *Engine) Copy(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, xs []vfserr.Extent) (int64, error) {
	return 0, errUnbuilt
}
