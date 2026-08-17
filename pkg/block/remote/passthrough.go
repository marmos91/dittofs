package remote

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/health"
)

// Passthrough forwards the parts of the RemoteStore surface that a
// per-chunk transform decorator does not touch straight through to the
// store it wraps. Decorators embed it and override only the operations
// their transform actually changes.
//
// The block-keyed operations forward verbatim by design: a packed block
// object carries per-chunk wire bodies that were already transformed by
// SealChunk, so the assembled block must NOT be transformed again at the
// block level. Forwarding them here is what lets a decorated store
// satisfy RemoteBlockStore (the carve path asserts it) while the
// transform stays confined to SealChunk / ReadChunk.
//
// Close, HealthCheck, Healthcheck and Durable forward because a
// transform changes the shape of the bytes, not where they land or
// whether the backend is reachable. Has and Delete forward because both
// key off the content hash, which a transform leaves unchanged.
type Passthrough struct {
	// inner is the wrapped store. It stays unexported so that embedding
	// Passthrough cannot promote a handle to the untransformed store into a
	// decorator's public surface: a caller holding that handle could read and
	// write bytes straight past the transform. Decorators that need the store
	// to reach a capability their transform participates in (ChunkReader,
	// ChunkSealer) keep their own reference.
	inner RemoteStore
}

// NewPassthrough builds a Passthrough forwarding to inner.
func NewPassthrough(inner RemoteStore) Passthrough { return Passthrough{inner: inner} }

// blockInner returns the inner store as a RemoteBlockStore, or an error
// when the wrapped store does not support block-keyed objects.
func (p Passthrough) blockInner() (RemoteBlockStore, error) {
	rbs, ok := p.inner.(RemoteBlockStore)
	if !ok {
		return nil, ErrChunkReadUnsupported
	}
	return rbs, nil
}

// CASInner exposes s's hash-keyed CAS surface (block.Store), used only by
// the legacy standalone-CAS read path (block.Store methods +
// ReadBlockVerified in each decorator's legacy_cas_migration.go). Every
// shipped remote backend and decorator implements block.Store, so the
// assertion succeeds in practice; it returns an error rather than
// panicking for defense in depth.
//
// A plain function rather than a method on Passthrough: promoting it
// would put an exported handle on the untransformed CAS surface into
// every decorator's public method set.
func CASInner(s RemoteStore) (block.Store, error) {
	cs, ok := s.(block.Store)
	if !ok {
		return nil, ErrChunkReadUnsupported
	}
	return cs, nil
}

// SliceRange returns [offset, offset+length) of full, clamped to its end.
// Used by decorators whose transform has no random access into the stored
// wire bytes: they materialise the whole plaintext, then slice it here so
// the bounds contract cannot drift between them. Callers reject a
// non-positive length before fetching, so reaching that branch here means
// the caller skipped its own guard.
func SliceRange(full []byte, offset, length int64) ([]byte, error) {
	if length <= 0 {
		return nil, fmt.Errorf("%w: length %d", block.ErrInvalidSize, length)
	}
	if offset < 0 || offset > int64(len(full)) {
		return nil, fmt.Errorf("%w: offset %d out of bounds (size %d)", block.ErrInvalidOffset, offset, len(full))
	}
	end := min(offset+length, int64(len(full)))
	out := make([]byte, end-offset)
	copy(out, full[offset:end])
	return out, nil
}

// PutBlock stores the assembled block verbatim (already-sealed bodies).
func (p Passthrough) PutBlock(ctx context.Context, blockID string, r io.Reader) error {
	rbs, err := p.blockInner()
	if err != nil {
		return err
	}
	return rbs.PutBlock(ctx, blockID, r)
}

// GetBlock returns the raw block object verbatim.
func (p Passthrough) GetBlock(ctx context.Context, blockID string) ([]byte, error) {
	rbs, err := p.blockInner()
	if err != nil {
		return nil, err
	}
	return rbs.GetBlock(ctx, blockID)
}

// GetBlockRange returns raw block bytes verbatim; the per-chunk inverse
// transform is ReadChunk.
func (p Passthrough) GetBlockRange(ctx context.Context, blockID string, offset, length int64) ([]byte, error) {
	rbs, err := p.blockInner()
	if err != nil {
		return nil, err
	}
	return rbs.GetBlockRange(ctx, blockID, offset, length)
}

// DeleteBlock removes the block object.
func (p Passthrough) DeleteBlock(ctx context.Context, blockID string) error {
	rbs, err := p.blockInner()
	if err != nil {
		return err
	}
	return rbs.DeleteBlock(ctx, blockID)
}

// WalkBlocks enumerates block objects.
func (p Passthrough) WalkBlocks(ctx context.Context, fn func(blockID string, meta block.Meta) error) error {
	rbs, err := p.blockInner()
	if err != nil {
		return err
	}
	return rbs.WalkBlocks(ctx, fn)
}

// Has reports presence by probing inner.Head. NotFound errors map to
// (false, nil); any other backend error propagates.
func (p Passthrough) Has(ctx context.Context, hash block.ContentHash) (bool, error) {
	cs, err := CASInner(p.inner)
	if err != nil {
		return false, err
	}
	if _, err := cs.Head(ctx, hash); err != nil {
		if errors.Is(err, block.ErrChunkNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Delete removes the standalone object keyed by hash.
func (p Passthrough) Delete(ctx context.Context, hash block.ContentHash) error {
	cs, err := CASInner(p.inner)
	if err != nil {
		return err
	}
	return cs.Delete(ctx, hash)
}

// DeleteLegacyChunk implements LegacyCASStore. The legacy standalone
// object is keyed by the plaintext hash, which no transform changes, so
// this is the same removal as Delete.
func (p Passthrough) DeleteLegacyChunk(ctx context.Context, hash block.ContentHash) error {
	return p.Delete(ctx, hash)
}

// Close releases inner resources. A decorator holding resources of its
// own overrides this and closes both.
func (p Passthrough) Close() error { return p.inner.Close() }

// HealthCheck delegates to inner.
func (p Passthrough) HealthCheck(ctx context.Context) error { return p.inner.HealthCheck(ctx) }

// Healthcheck delegates to inner.
func (p Passthrough) Healthcheck(ctx context.Context) health.Report {
	return p.inner.Healthcheck(ctx)
}

// Durable delegates to the wrapped store via block.IsDurable. Transforming
// block bodies does not change where the bytes ultimately land, so a durable
// inner store stays durable through a decorator; a wrapped store that does not
// report durability falls back to the conservative default (false).
func (p Passthrough) Durable() bool { return block.IsDurable(p.inner) }
