package remote

import (
	"context"

	"github.com/marmos91/dittofs/pkg/block"
)

// LegacyCASStore is the migration-only accessor for pre-blocks standalone
// chunk objects stored under the legacy "cas/" namespace (one sealed object
// per chunk, keyed by content hash). It exists solely for the one-shot
// cas→blocks startup migration (#1493 PR4) and is the ONLY surviving consumer
// of the legacy standalone-CAS layout; delete this interface (and every
// legacy_cas_migration.go implementation) when the migration is retired.
//
// Implemented by the shipped backends (s3, memory) and forwarded through the
// compression/encryption decorators: ReadLegacyChunkVerified applies exactly
// the per-chunk unseal transforms the old standalone read path applied, and
// verifies blake3(plaintext) == hash fail-closed before returning.
type LegacyCASStore interface {
	// WalkLegacyChunks calls fn once per object in the legacy cas/ namespace.
	// fn is invoked sequentially; a non-nil error aborts the walk and is
	// returned. An empty namespace returns nil without invoking fn.
	WalkLegacyChunks(ctx context.Context, fn func(hash block.ContentHash, size int64) error) error

	// ReadLegacyChunkVerified GETs the standalone object for hash, unseals it
	// through the transform stack, and verifies the plaintext BLAKE3 matches
	// hash before returning. Returns block.ErrChunkNotFound when the object
	// does not exist.
	ReadLegacyChunkVerified(ctx context.Context, hash block.ContentHash) ([]byte, error)

	// DeleteLegacyChunk removes the standalone object for hash. Idempotent:
	// deleting an absent object returns nil.
	DeleteLegacyChunk(ctx context.Context, hash block.ContentHash) error
}
