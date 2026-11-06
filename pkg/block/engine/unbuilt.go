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

// Commit makes [off, off+n) stable, n 0 the whole file, and answers the
// verifier and the file's loss sequence.
func (e *Engine) Commit(ctx context.Context, s entity.ShareID, f entity.FileID, off, n int64) (v vfserr.Verifier, lossSeq uint64, err error) {
	return vfserr.Verifier{}, 0, errUnbuilt
}

// LossSeq is the file's loss sequence now, with no sync; 0 for a share with
// no binding.
//
// decision: the stub answers 0 because the signature has no error to carry
// errUnbuilt. Zero reads as "no loss since any open", so no caller may depend
// on it until the loss sequence is built.
func (e *Engine) LossSeq(s entity.ShareID, f entity.FileID) uint64 { return 0 }

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
