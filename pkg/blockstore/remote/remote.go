// Package remote declares the unified CAS-keyed remote-store contract.
//
// RemoteStore is the unified remote-store interface for CAS-keyed block
// storage. All operations are keyed by blockstore.ContentHash. The CAS
// object key shape (cas/{hh}/{hh}/{hex}) is derived from the hash via
// blockstore.FormatCASKey and is an implementation detail backends may
// not expose. The interface embeds blockstore.BlockStore and adds
// backend-specific extras (ReadBlockVerified for production CAS reads,
// Close + HealthCheck + Healthcheck for backend lifecycle / health).
package remote

import (
	"context"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/health"
)

// RemoteStore is the unified content-addressed remote block storage
// interface. Implemented by
//
//   - pkg/blockstore/remote/s3.Store
//   - pkg/blockstore/remote/memory.Store
//
// Every method is keyed by blockstore.ContentHash; no opaque "block key"
// strings appear on this surface. Backends derive their on-disk / on-wire
// key shape via blockstore.FormatCASKey internally.
//
// The Put / Get / GetRange / Has / Delete / Head / Walk method set comes
// from the embedded blockstore.BlockStore contract — byte-for-byte the
// same semantics as the unified type. ReadBlockVerified is a
// backend-specific extension (NOT part of BlockStore) used by the
// engine's verified-read path; callers type-assert to access it on the
// s3 backend, and the memory backend implements it as the trivial
// body-recompute case so test fixtures can exercise the same code path.
// Close / HealthCheck / Healthcheck cover backend lifecycle and health
// probes — also outside the BlockStore contract.
type RemoteStore interface {
	blockstore.BlockStore

	// ReadBlockVerified GETs the object addressed by hash and verifies
	// that the body's BLAKE3 hash matches expected before returning
	// bytes. Implementations SHOULD also pre-check any backend-native
	// content-hash metadata (S3: x-amz-meta-content-hash) and fail fast
	// on header mismatch. Returns blockstore.ErrCASContentMismatch
	// wrapped with diagnostic context on any verification failure. Per
	// the buffer is discarded on mismatch — bad bytes never
	// reach the caller.
	//
	// Both hash arguments are intentional: hash derives the canonical
	// CAS key, while expected is the body BLAKE3 the caller is
	// asserting. Verification proves byte-on-disk == hash-in-key ==
	// expected; the redundancy is defense-in-depth and guards
	// against key-vs-content mismatch on backends where the two might
	// drift (e.g., during external mutation).
	//
	// ReadBlockVerified is NOT part of the unified blockstore.BlockStore
	// contract — it is a backend-specific extension on RemoteStore. The
	// engine accesses it via type-assertion on the unified BlockStore in
	// backends that do not need verification (in-memory test
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
