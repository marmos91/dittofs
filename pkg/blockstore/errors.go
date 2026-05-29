package blockstore

import (
	"errors"
)

// Standard block store errors. Protocol handlers should check for these errors
// and map them to appropriate protocol-specific error codes.
var (
	// ErrContentNotFound indicates the requested content does not exist.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrNoEnt (2)
	//   - SMB: STATUS_OBJECT_NAME_NOT_FOUND
	//   - HTTP: 404 Not Found
	ErrContentNotFound = errors.New("content not found")

	// ErrContentExists indicates content with this ID already exists.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrExist (17)
	//   - SMB: STATUS_OBJECT_NAME_COLLISION
	//   - HTTP: 409 Conflict
	ErrContentExists = errors.New("content already exists")

	// ErrInvalidOffset indicates the offset is invalid for the operation.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrInval (22)
	//   - SMB: STATUS_INVALID_PARAMETER
	//   - HTTP: 416 Range Not Satisfiable
	ErrInvalidOffset = errors.New("invalid offset")

	// ErrInvalidSize indicates the size parameter is invalid.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrInval (22)
	//   - SMB: STATUS_INVALID_PARAMETER
	ErrInvalidSize = errors.New("invalid size")

	// ErrStorageFull indicates the storage backend has no available space.
	//
	// This is a transient error - it may succeed after cleanup.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrNoSpc (28)
	//   - SMB: STATUS_DISK_FULL
	//   - HTTP: 507 Insufficient Storage
	ErrStorageFull = errors.New("storage full")

	// ErrQuotaExceeded indicates a storage quota has been exceeded.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrDQuot (69)
	//   - SMB: STATUS_QUOTA_EXCEEDED
	//   - HTTP: 507 Insufficient Storage
	ErrQuotaExceeded = errors.New("quota exceeded")

	// ErrIntegrityCheckFailed indicates content integrity verification failed.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrIO (5)
	//   - SMB: STATUS_DATA_CHECKSUM_ERROR
	//   - HTTP: 500 Internal Server Error
	ErrIntegrityCheckFailed = errors.New("integrity check failed")

	// ErrReadOnly indicates the content store is read-only.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrRoFs (30)
	//   - SMB: STATUS_MEDIA_WRITE_PROTECTED
	//   - HTTP: 403 Forbidden
	ErrReadOnly = errors.New("content store is read-only")

	// ErrNotSupported indicates the operation is not supported.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrNotSupp (10004)
	//   - SMB: STATUS_NOT_SUPPORTED
	//   - HTTP: 501 Not Implemented
	ErrNotSupported = errors.New("operation not supported")

	// ErrConcurrentModification indicates content was modified concurrently.
	//
	// Callers should retry with fresh data.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrStale (70) or NFS3ErrJukebox (10008)
	//   - SMB: STATUS_FILE_LOCK_CONFLICT
	//   - HTTP: 409 Conflict or 412 Precondition Failed
	ErrConcurrentModification = errors.New("concurrent modification detected")

	// ErrInvalidPayloadID indicates the PayloadID format is invalid.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrBadHandle (10001)
	//   - SMB: STATUS_INVALID_PARAMETER
	//   - HTTP: 400 Bad Request
	ErrInvalidPayloadID = errors.New("invalid content ID")

	// ErrTooLarge indicates the content or operation is too large.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrFBig (27)
	//   - SMB: STATUS_FILE_TOO_LARGE
	//   - HTTP: 413 Payload Too Large
	ErrTooLarge = errors.New("content too large")

	// ErrUnavailable indicates the storage backend is temporarily unavailable.
	//
	// This is a transient error - retrying may succeed.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrJukebox (10008)
	//   - SMB: STATUS_DEVICE_NOT_READY
	//   - HTTP: 503 Service Unavailable
	ErrUnavailable = errors.New("storage unavailable")

	// ErrChunkNotFound indicates the requested content-addressed chunk
	// does not exist in the store (local or remote).
	ErrChunkNotFound = errors.New("chunk not found")

	// ErrStoreClosed is returned when operations are attempted on a closed store.
	ErrStoreClosed = errors.New("store is closed")

	// ErrInvalidHash is returned when a hash string is malformed.
	ErrInvalidHash = errors.New("invalid content hash format")

	// ErrFileBlockNotFound is returned when a file block is not found.
	ErrFileBlockNotFound = errors.New("file block not found")

	// ErrUnknownHash is returned by FileBlockStore.AddRef when no
	// FileBlock row exists for the given hash. The LRU hit path
	// (Opt 1 — see pkg/blockstore/local/fs/rollup.go) MUST
	// fall back to the full Put path on this sentinel; the LRU may
	// be ahead of the metadata store after a crash (RAM-only LRU
	// see), or the hash may not be present yet.
	ErrUnknownHash = errors.New("metadata: hash not yet present in FileBlockStore (AddRef called before Put)")

	// ErrRemoteUnavailable is returned when a remote store operation is needed
	// but the remote store is currently unreachable. Protocol handlers should
	// map this to appropriate I/O error codes (NFS3ERR_IO, NFS4ERR_IO
	// STATUS_UNEXPECTED_IO_ERROR).
	//
	// The error is intentionally returned early (before attempting network I/O)
	// when the health monitor reports the remote as unhealthy, avoiding network
	// timeouts.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrIO (5) / NFS4ERR_IO (5)
	//   - SMB: STATUS_UNEXPECTED_IO_ERROR (0xC00000E9)
	//   - HTTP: 503 Service Unavailable
	ErrRemoteUnavailable = errors.New("remote store unavailable")

	// ErrCASContentMismatch is returned by the streaming BLAKE3 verifier on
	// S3 GET when the recomputed hash (or the x-amz-meta-content-hash header)
	// does not match the expected ContentHash. On mismatch, the buffer is
	// discarded and this error surfaces — bad bytes never reach the caller.
	ErrCASContentMismatch = errors.New("blockstore: CAS content hash mismatch")

	// ErrCASKeyMalformed is returned by ParseCASKey for any input that does
	// not match the cas/{hh}/{hh}/{hex} shape.
	ErrCASKeyMalformed = errors.New("blockstore: malformed CAS key")

	// ErrBlockRefMissing is returned by engine.ReadAt when a BlockRef.Hash
	// refers to a FileBlock that has been GC'd or never existed. The
	// adapter layer (internal/adapter/common/errmap.go) maps this to
	// NFS3ERR_IO / STATUS_DATA_ERROR consistently across protocols.
	ErrBlockRefMissing = errors.New("blockstore: block ref hash missing in store")

	// ErrStopWalk is the sentinel a Walk callback returns to request a
	// clean early exit (e.g., GC found its target). Walk returns nil to
	// the outer caller. Any non-ErrStopWalk error halts and propagates
	// wrapped with file/offset context. Mirrors filepath.SkipDir /
	// fs.SkipAll.
	//
	// Detection pattern (callback side, when wrapping is required)
	//
	//   return fmt.Errorf("gc target %s: %w", id, blockstore.ErrStopWalk)
	//
	// Walk implementations match via errors.Is(err, ErrStopWalk) and
	// return nil to the outer caller. Any other non-nil callback error
	// halts and is wrapped as fmt.Errorf("walk halted at %s: %w", hash, err).
	//
	// See BlockStore.Walk.
	ErrStopWalk = errors.New("blockstore: stop walk")

	// ErrLegacyLayoutDetected is returned by fs.NewWithOptions when
	// the share directory contains legacy `.blk` files but no
	// `.cas-migrated-v1` sentinel marker file. The wrapped target
	// carries the offending share path
	//
	//   return nil, fmt.Errorf("%w: share path %s", ErrLegacyLayoutDetected, baseDir)
	//
	// Detection at boot is via errors.Is, not errors.As — the sentinel
	// is an errors.New value (not a typed struct), so errors.Is is the
	// idiomatic match
	//
	//   if errors.Is(err, blockstore.ErrLegacyLayoutDetected) { ... }
	//
	// cmd/dfs/start.go unwraps via errors.Is, prints an operator
	// directive ("Detected legacy `.blk` layout at <path>. v0.16+
	// requires CAS migration. Run `dfs migrate-to-cas` before
	// starting."), and exits 78 (EX_CONFIG from sysexits(3)). Per-share
	// fail-fast: the first un-migrated share halts boot.
	//
	// Operator action: run `dfs migrate-to-cas --share <name>` (or
	// `dfs migrate-to-cas` for all shares) and retry. See
	// docs/CONFIGURATION.md §migration..
	ErrLegacyLayoutDetected = errors.New("blockstore: legacy .blk layout detected (run `dfs migrate-to-cas`)")
)
