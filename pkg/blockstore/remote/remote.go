package remote

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/health"
)

// ObjectInfo describes a single remote object listed by
// [RemoteStore.ListByPrefixWithMeta]. Used by the GC sweep phase to apply
// the LastModified > snapshot - GracePeriod TTL filter (D-05) before a
// candidate object is considered for deletion.
type ObjectInfo struct {
	// Key is the object key as the backend reports it (the same value
	// passed to ReadBlock / DeleteBlock). For S3 callers, this includes
	// any keyPrefix the store is configured with stripped — symmetric to
	// ListByPrefix.
	Key string
	// Size is the object size in bytes.
	Size int64
	// LastModified is the backend's last-modified timestamp. Required
	// for the GC sweep grace TTL check (D-05).
	//
	// MUST be non-zero for every object the backend lists. The GC sweep
	// fails closed on a zero LastModified — Phase 11 WR-4-02 — to
	// guarantee INV-04 (orphan-not-deleted is preferred over
	// live-data-deleted). Backends that cannot natively report a
	// timestamp MUST stamp time.Now() at WriteBlock /
	// WriteBlockWithHash time and surface that value here. The
	// remotetest conformance suite asserts a non-zero LastModified after
	// WriteBlockWithHash.
	LastModified time.Time
}

// RemoteStore defines the interface for remote block storage backends.
// Blocks are immutable chunks of data stored with a string key.
//
// Key format (legacy): "{payloadID}/block-{blockIdx}"
// Key format (CAS, Phase 11+): "cas/{hh}/{hh}/{hex}" — see blockstore.FormatCASKey.
// Example: "export/file.txt/block-0" or "cas/af/13/af1349b9..."
type RemoteStore interface {
	// WriteBlock writes a single block to storage.
	WriteBlock(ctx context.Context, blockKey string, data []byte) error

	// WriteBlockWithHash uploads block data and stamps the content hash in
	// backend-native object metadata (S3: x-amz-meta-content-hash). The
	// header value is "blake3:" + hex(h). Used by the CAS write path so
	// external tooling can verify object integrity without DittoFS metadata.
	// See BSCAS-06.
	//
	// Implementations MUST set the metadata atomically with the PUT (no
	// follow-up call). For legacy non-CAS keys, callers continue to use
	// WriteBlock.
	WriteBlockWithHash(ctx context.Context, blockKey string, hash blockstore.ContentHash, data []byte) error

	// ReadBlock reads a complete block. Returns error if missing.
	ReadBlock(ctx context.Context, blockKey string) ([]byte, error)

	// ReadBlockVerified GETs the object at key and verifies that the body's
	// BLAKE3 hash matches expected before returning bytes. Implementations
	// SHOULD also pre-check any backend-native content-hash metadata
	// (e.g., S3 x-amz-meta-content-hash) and fail fast on mismatch.
	// Returns blockstore.ErrCASContentMismatch wrapped with diagnostic
	// context on any verification failure. Per INV-06, the buffer is
	// discarded on mismatch — bad bytes never reach the caller.
	//
	// Used by the CAS dual-read engine resolver (D-21) for blocks whose
	// FileBlock metadata carries a non-zero ContentHash. Legacy
	// (zero-hash) blocks continue to use ReadBlock for the dual-read
	// window.
	ReadBlockVerified(ctx context.Context, blockKey string, expected blockstore.ContentHash) ([]byte, error)

	// ReadBlockRange reads a byte range from a block. Returns error if missing.
	ReadBlockRange(ctx context.Context, blockKey string, offset, length int64) ([]byte, error)

	// DeleteBlock removes a single block. Returns nil if missing.
	DeleteBlock(ctx context.Context, blockKey string) error

	// DeleteByPrefix removes all blocks matching the prefix.
	DeleteByPrefix(ctx context.Context, prefix string) error

	// ListByPrefix lists all block keys matching the prefix.
	ListByPrefix(ctx context.Context, prefix string) ([]string, error)

	// ListByPrefixWithMeta lists all objects under prefix and returns the
	// per-object metadata (Key, Size, LastModified) needed by the GC sweep
	// phase to apply the snapshot - GracePeriod TTL filter (D-05). The
	// returned Key is symmetric to ListByPrefix (any keyPrefix the store is
	// configured with is stripped). Order is unspecified.
	ListByPrefixWithMeta(ctx context.Context, prefix string) ([]ObjectInfo, error)

	// CopyBlock copies a block from srcKey to dstKey using server-side copy
	// when the backend supports it (e.g., S3 CopyObject). Falls back to
	// read-then-write for backends without native copy.
	// Returns blockstore.ErrBlockNotFound if the source block does not exist.
	CopyBlock(ctx context.Context, srcKey, dstKey string) error

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
