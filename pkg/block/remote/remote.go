// Package remote declares the unified CAS-keyed remote-store contract.
//
// RemoteStore is the unified remote-store interface for CAS-keyed block
// storage. All operations are keyed by block.ContentHash. The CAS
// object key shape (cas/{hh}/{hh}/{hex}) is derived from the hash via
// block.FormatCASKey and is an implementation detail backends may
// not expose. The interface embeds block.BlockStore and adds
// backend-specific extras (ReadBlockVerified for production CAS reads,
// Close + HealthCheck + Healthcheck for backend lifecycle / health).
package remote

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/health"
)

// ErrPackReadUnsupported is returned by a decorator's GetPackChunk when the
// wrapped store does not implement PackChunkReader, so a pack read cannot be
// composed through the transform stack. It cannot occur on the live PR3a path
// (which never produces pack locators); it guards the capability boundary.
var ErrPackReadUnsupported = errors.New("remote: wrapped store does not support pack reads")

// RemoteStore is the unified content-addressed remote block storage
// interface. Implemented by
//
//   - pkg/block/remote/s3.Store
//   - pkg/block/remote/memory.Store
//
// Every method is keyed by block.ContentHash; no opaque "block key"
// strings appear on this surface. Backends derive their on-disk / on-wire
// key shape via block.FormatCASKey internally.
//
// The Put / Get / GetRange / Has / Delete / Head / Walk method set comes
// from the embedded block.BlockStore contract — byte-for-byte the
// same semantics as the unified type. ReadBlockVerified is a
// backend-specific extension (NOT part of BlockStore) used by the
// engine's verified-read path; callers type-assert to access it on the
// s3 backend, and the memory backend implements it as the trivial
// body-recompute case so test fixtures can exercise the same code path.
// Close / HealthCheck / Healthcheck cover backend lifecycle and health
// probes — also outside the BlockStore contract.
type RemoteStore interface {
	block.Store

	// ReadBlockVerified GETs the object addressed by hash and verifies
	// that the body's BLAKE3 hash matches expected before returning
	// bytes. Implementations SHOULD also pre-check any backend-native
	// content-hash metadata (S3: x-amz-meta-content-hash) and fail fast
	// on header mismatch. Returns block.ErrCASContentMismatch
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
	// ReadBlockVerified is NOT part of the unified block.BlockStore
	// contract — it is a backend-specific extension on RemoteStore. The
	// engine accesses it via type-assertion on the unified BlockStore in
	// backends that do not need verification (in-memory test
	// fixtures) implement it as a trivial body-recompute wrapper.
	ReadBlockVerified(ctx context.Context, hash block.ContentHash, expected block.ContentHash) ([]byte, error)

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

// PackChunkReader is an OPTIONAL RemoteStore capability for reading a chunk that
// lives inside a pack object (#1414 object packing). It is deliberately kept OFF
// the RemoteStore contract — only the locator read path needs it, so the many
// RemoteStore test fakes need not implement it (mirroring how EnumerateSynced is
// kept off metadata.SyncedHashStore). The engine type-asserts m.remoteStore to
// this interface when a block.ChunkLocator resolves to a pack (PackID != "");
// the s3 + memory backends and the encryption/compression decorators implement
// it.
//
// GetPackChunk reads the chunk whose stored wire bytes occupy
// [offset, offset+length) within pack object packs/<packID> (see
// block.FormatPackKey) and returns the chunk PLAINTEXT, inverting the store's
// transform chain on the way up — each decorator decompresses/decrypts its own
// layer, threading hash as the AEAD AAD. It does NOT verify the BLAKE3 (no
// single layer holds both the wire bytes and the plaintext hash domain); the
// engine read path verifies blake3(result) == hash after the top-level call,
// exactly as ReadBlockVerified guarantees for standalone objects. hash is
// consulted only by the encryption layer (AAD) and ignored by the base stores
// and the compression layer.
type PackChunkReader interface {
	GetPackChunk(ctx context.Context, packID string, offset, length int64, hash block.ContentHash) ([]byte, error)
}
