package remote

import (
	"context"
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
// transform stays confined to SealChunk / ReadChunk. RemoteStore embeds
// RemoteBlockStore, so inner always carries them.
//
// Close, HealthCheck, Healthcheck and Durable forward because a
// transform changes the shape of the bytes, not where they land or
// whether the backend is reachable.
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

// PutBlock stores the assembled block verbatim (already-sealed bodies).
func (p Passthrough) PutBlock(ctx context.Context, blockID string, r io.Reader) error {
	return p.inner.PutBlock(ctx, blockID, r)
}

// GetBlock returns the raw block object verbatim.
func (p Passthrough) GetBlock(ctx context.Context, blockID string) ([]byte, error) {
	return p.inner.GetBlock(ctx, blockID)
}

// GetBlockRange returns raw block bytes verbatim; the per-chunk inverse
// transform is ReadChunk.
func (p Passthrough) GetBlockRange(ctx context.Context, blockID string, offset, length int64) ([]byte, error) {
	return p.inner.GetBlockRange(ctx, blockID, offset, length)
}

// DeleteBlock removes the block object.
func (p Passthrough) DeleteBlock(ctx context.Context, blockID string) error {
	return p.inner.DeleteBlock(ctx, blockID)
}

// WalkBlocks enumerates block objects.
func (p Passthrough) WalkBlocks(ctx context.Context, fn func(blockID string, meta block.Meta) error) error {
	return p.inner.WalkBlocks(ctx, fn)
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
