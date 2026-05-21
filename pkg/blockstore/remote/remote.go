// Package remote declares the unified CAS-keyed remote-store contract.
//
// RemoteStore is the unified remote-store interface for CAS-keyed block
// storage. All operations are keyed by blockstore.ContentHash. The CAS
// object key shape (cas/{hh}/{hh}/{hex}) is derived from the hash via
// blockstore.FormatCASKey and is an implementation detail backends may
// not expose. The interface is structurally compatible with
// blockstore.BlockStore (Phase 17 Plan 01) — Plan 05 retargets engine
// consumers onto that unified type and this package becomes the s3 /
// memory backend home; the methods on this interface match the unified
// BlockStore method set verbatim, with two backend-specific additions
// (ReadBlockVerified for production CAS reads, Close + HealthCheck +
// Healthcheck for backend lifecycle / health).
package remote

import (
	"context"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/health"
)

// RemoteStore is the unified content-addressed remote block storage
// interface. Implemented by:
//
//   - pkg/blockstore/remote/s3.Store
//   - pkg/blockstore/remote/memory.Store
//
// Every method is keyed by blockstore.ContentHash; no opaque "block key"
// strings appear on this surface. Backends derive their on-disk / on-wire
// key shape via blockstore.FormatCASKey internally.
//
// The Put/Get/GetRange/Delete/Head/Walk method set matches the unified
// blockstore.BlockStore contract from Plan 01 byte-for-byte; ReadBlockVerified
// is a backend-specific extension (NOT part of BlockStore) used by the
// engine's verified-read path (BSCAS-06). Plan 05 type-asserts to access
// it on the s3 backend; the memory backend implements it as the trivial
// body-recompute case so test fixtures can exercise the same code path.
type RemoteStore interface {
	// Put writes data under the CAS-shaped key derived from hash. Backends
	// MUST stamp the content hash atomically with the PUT (S3:
	// x-amz-meta-content-hash header) — BSCAS-06 defense-in-depth.
	// Idempotent on the same (hash, data) pair; a Put with the same hash
	// but different bytes is undefined behavior — callers MUST NOT rely
	// on either outcome.
	//
	// Returns an error if the backend is closed, the I/O fails, or the
	// hash is zero (callers must compute the hash before calling).
	Put(ctx context.Context, hash blockstore.ContentHash, data []byte) error

	// Get returns the chunk bytes addressed by the given content hash.
	// The returned []byte is freshly allocated and owned by the caller —
	// implementations MUST NOT return a slice that aliases internal
	// storage.
	//
	// Returns blockstore.ErrBlockNotFound (or blockstore.ErrChunkNotFound
	// for fs-style backends — callers match via errors.Is on either) when
	// the chunk is absent.
	//
	// For S3, prefer ReadBlockVerified on the production read path — Get
	// returns raw bytes WITHOUT BLAKE3 verification.
	Get(ctx context.Context, hash blockstore.ContentHash) ([]byte, error)

	// GetRange returns a byte sub-range [offset, offset+length) of the
	// chunk addressed by hash. The returned slice is freshly allocated
	// (same no-aliasing rule as Get). Returns blockstore.ErrBlockNotFound
	// if the chunk is absent.
	GetRange(ctx context.Context, hash blockstore.ContentHash, offset, length int64) ([]byte, error)

	// Delete removes the object addressed by hash. Returns nil if the
	// object does not exist (Delete is idempotent).
	Delete(ctx context.Context, hash blockstore.ContentHash) error

	// Head returns blockstore.Meta for the object addressed by hash
	// without transferring the body. Returns blockstore.ErrBlockNotFound
	// when the object is absent.
	//
	// The backend's defense-in-depth content-hash header (S3:
	// x-amz-meta-content-hash) is verified internally during
	// ReadBlockVerified but is NOT echoed via Meta — per Phase 17 D-08,
	// the lookup key (ContentHash) is the input, not output, and Meta
	// stays minimal {Size, LastModified}.
	//
	// Meta.LastModified MUST be non-zero (Phase 11 WR-4-02 / INV-04: GC
	// sweep fails closed otherwise). Backends that cannot natively report
	// a timestamp MUST stamp time.Now() at Put time and surface that
	// value here.
	Head(ctx context.Context, hash blockstore.ContentHash) (blockstore.Meta, error)

	// Walk enumerates every CAS object in the store. The callback
	// receives the content hash and Meta for each object; ordering is
	// unspecified (backends MAY parallelize internally; the conformance
	// suite does not pin a traversal order).
	//
	// Returning blockstore.ErrStopWalk from the callback exits cleanly
	// — Walk returns nil to the outer caller. Any other non-nil
	// callback error halts the walk and Walk returns it wrapped with
	//
	//   fmt.Errorf("walk halted at %s: %w", hash, err)
	//
	// Context cancellation aborts immediately; the callback is NOT
	// re-invoked after ctx.Err() != nil (Walk MUST surface ctx.Err()
	// without one final spurious callback). Contract mirrors
	// filepath.SkipDir / fs.SkipAll.
	//
	// See blockstore.ErrStopWalk for the sentinel doc.
	Walk(ctx context.Context, fn func(hash blockstore.ContentHash, meta blockstore.Meta) error) error

	// ReadBlockVerified GETs the object addressed by hash and verifies
	// that the body's BLAKE3 hash matches expected before returning
	// bytes. Implementations SHOULD also pre-check any backend-native
	// content-hash metadata (S3: x-amz-meta-content-hash) and fail fast
	// on header mismatch. Returns blockstore.ErrCASContentMismatch
	// wrapped with diagnostic context on any verification failure. Per
	// INV-06, the buffer is discarded on mismatch — bad bytes never
	// reach the caller.
	//
	// Both hash arguments are intentional: hash derives the canonical
	// CAS key, while expected is the body BLAKE3 the caller is
	// asserting. Verification proves byte-on-disk == hash-in-key ==
	// expected; the redundancy is BSCAS-06 defense-in-depth and guards
	// against key-vs-content mismatch on backends where the two might
	// drift (e.g., during external mutation).
	//
	// ReadBlockVerified is NOT part of the unified blockstore.BlockStore
	// contract — it is a backend-specific extension on RemoteStore. The
	// engine accesses it via type-assertion on the unified BlockStore in
	// Plan 05; backends that do not need verification (in-memory test
	// fixtures) implement it as a trivial body-recompute wrapper.
	ReadBlockVerified(ctx context.Context, hash blockstore.ContentHash, expected blockstore.ContentHash) ([]byte, error)

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
