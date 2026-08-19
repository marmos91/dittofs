// Package remote declares the production remote-store contract.
//
// RemoteStore is the interface every remote backend (and decorator) exposes to
// the engine and control plane. Its production surface is entirely block-keyed:
// packed block objects stored under the "blocks/" prefix, read and written
// through RemoteBlockStore + ChunkReader + ChunkSealer, plus
// Close/HealthCheck/Healthcheck for backend lifecycle and health. Objects are
// keyed by an opaque blockID string; the on-wire key is
// block.FormatBlockKey(blockID).
//
// No hash-keyed CAS read/write/enumerate operation is exposed here. The
// pre-blocks standalone-CAS layout is reachable only by type-asserting a store
// to LegacyCASStore (see legacy_cas.go), which the one-shot cas→blocks startup
// migration does.
package remote

import (
	"context"
	"errors"
	"io"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/health"
)

// ErrChunkReadUnsupported is returned by a decorator's ReadChunk when the
// wrapped store does not implement ChunkReader, so a block read cannot be
// composed through the transform stack. It guards the capability boundary for
// callers that hold a store only as RemoteBlockStore.
var ErrChunkReadUnsupported = errors.New("remote: wrapped store does not support block reads")

// RemoteStore is the production remote block storage interface. Implemented by
//
//   - pkg/block/remote/s3.Store
//   - pkg/block/remote/memory.Store
//   - the compression / encryption decorators
//
// The production surface is block-keyed: RemoteBlockStore (PutBlock / GetBlock /
// GetBlockRange / DeleteBlock / WalkBlocks) for whole packed block objects,
// ChunkReader.ReadChunk / ChunkSealer.SealChunk for the per-chunk transform, and
// Close / HealthCheck / Healthcheck for lifecycle and health. No hash-keyed CAS
// operation is exposed here — the legacy standalone-CAS layout is reachable only
// by type-asserting a store to LegacyCASStore, which only the cas→blocks
// migration does.
type RemoteStore interface {
	RemoteBlockStore
	ChunkReader
	ChunkSealer

	// Close releases resources held by the store.
	Close() error

	// HealthCheck verifies the store is accessible. This is the legacy
	// error-returning probe used internally by the syncer's HealthMonitor.
	// New callers should prefer Healthcheck (lowercase 'c') which returns
	// a structured [health.Report] and satisfies [health.Checker].
	HealthCheck(ctx context.Context) error

	// Healthcheck returns a structured health report and satisfies
	// [health.Checker]. Implementations typically delegate to HealthCheck
	// and wrap the result via [health.ReportFromError].
	Healthcheck(ctx context.Context) health.Report
}

// RemoteBlockStore is the block-keyed (non-CAS) remote store contract for
// objects stored under the "blocks/" prefix. Implemented
// by pkg/block/remote/s3.Store and pkg/block/remote/memory.Store.
//
// Objects are keyed by an opaque blockID string; the on-disk/on-wire key shape
// is block.FormatBlockKey(blockID) = "blocks/<blockID>". This is the production
// remote surface; the migration-only legacy standalone layout lives under a
// separate prefix reachable via LegacyCASStore (see legacy_cas.go).
type RemoteBlockStore interface {
	// PutBlock writes the content of r under blocks/<blockID>. Idempotent:
	// a second call with the same blockID overwrites silently. Implementations
	// MUST stream r without buffering the whole body; callers may provide an
	// unbounded reader (e.g., a packing file).
	PutBlock(ctx context.Context, blockID string, r io.Reader) error

	// GetBlock returns the full bytes of the block object identified by
	// blockID. Returns block.ErrChunkNotFound when the block is absent.
	// The returned slice is freshly allocated and owned by the caller.
	GetBlock(ctx context.Context, blockID string) ([]byte, error)

	// GetBlockRange returns [offset, offset+length) bytes of the block
	// object identified by blockID. Bounds semantics mirror
	// block.Store.GetRange: ErrInvalidOffset for a negative offset; a
	// past-EOF offset cannot be detected here without a HEAD, so
	// backends surface a native error (S3: 416) instead — the contract
	// only guarantees some error for offset >= EOF. ErrInvalidSize for a
	// non-positive length; past-EOF length is clamped to the object's
	// remaining bytes on backends that support it (S3 partial-content).
	// Returns block.ErrChunkNotFound when the block is absent.
	GetBlockRange(ctx context.Context, blockID string, offset, length int64) ([]byte, error)

	// DeleteBlock removes the block object keyed by blockID. Idempotent:
	// deleting an absent blockID returns nil.
	DeleteBlock(ctx context.Context, blockID string) error

	// WalkBlocks enumerates every block object in the store. The callback
	// receives the blockID (the key with the "blocks/" prefix stripped) and
	// block.Meta. Ordering is unspecified. Honors block.ErrStopWalk for
	// clean early exit (WalkBlocks returns nil); any other callback error
	// halts enumeration and is returned wrapped as
	//
	//   fmt.Errorf("walk halted at %s: %w", blockID, err)
	//
	// Context cancellation aborts immediately.
	WalkBlocks(ctx context.Context, fn func(blockID string, meta block.Meta) error) error
}

// ChunkReader reads a chunk that lives inside a block object. It is a mandatory
// member of RemoteStore; the s3 + memory backends and the encryption/compression
// decorators implement it. Callers that hold only the narrower RemoteBlockStore
// type-assert to it when a block.ChunkLocator resolves to a block (BlockID != "")
// and return ErrChunkReadUnsupported when the assertion fails.
//
// ReadChunk reads the chunk whose stored wire bytes occupy
// [offset, offset+length) within block object blocks/<blockID> (see
// block.FormatBlockKey) and returns the chunk PLAINTEXT, inverting the store's
// transform chain on the way up — each decorator decompresses/decrypts its own
// layer, threading hash as the AEAD AAD. It does NOT verify the BLAKE3 (no
// single layer holds both the wire bytes and the plaintext hash domain); the
// engine read path verifies blake3(result) == hash after the top-level call.
// hash is consulted only by the encryption layer (AAD) and ignored by the base
// stores and the compression layer.
type ChunkReader interface {
	ReadChunk(ctx context.Context, blockID string, offset, length int64, hash block.ContentHash) ([]byte, error)
}

// ChunkSealer is the write-side counterpart to ChunkReader, and likewise a
// mandatory member of RemoteStore. The block carver calls SealChunk on the (possibly decorated) remote
// store to transform one chunk's raw plaintext bytes into the wire bytes that
// land inside a packed block object — applying exactly the same per-chunk
// compression/encryption transforms the standalone CAS Put path applies, in the
// same order. The carver then frames the returned wire bytes into the block via
// blockcodec and uploads the assembled block with RemoteBlockStore.PutBlock.
//
// SealChunk MUST be byte-for-byte symmetric with ChunkReader.ReadChunk:
// ReadChunk(GetBlockRange(SealChunk(hash, plaintext))) == plaintext. The base
// stores (s3, memory) implement it as the identity transform; the compression
// and encryption decorators seal their own layer and delegate inward, so a
// decorated chain produces encrypt(compress(plaintext)) — never plaintext at
// rest on an encrypted share.
//
// hash is threaded through so the encryption layer can bind it as AEAD AAD,
// matching the per-chunk scheme of the standalone Put path. Base stores and the
// compression layer ignore it.
type ChunkSealer interface {
	SealChunk(ctx context.Context, hash block.ContentHash, plaintext []byte) ([]byte, error)
}
