package block

import (
	"errors"
)

// Standard block store errors. Protocol handlers should check for these errors
// and map them to appropriate protocol-specific error codes.
var (
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

	// ErrIntegrityCheckFailed indicates content integrity verification failed.
	//
	// Protocol Mapping
	//   - NFS: NFS3ErrIO (5)
	//   - SMB: STATUS_DATA_CHECKSUM_ERROR
	//   - HTTP: 500 Internal Server Error
	ErrIntegrityCheckFailed = errors.New("integrity check failed")

	// ErrChunkNotFound indicates the requested content-addressed chunk
	// does not exist in the store (local or remote).
	ErrChunkNotFound = errors.New("chunk not found")

	// ErrStoreClosed is returned when operations are attempted on a closed store.
	ErrStoreClosed = errors.New("store is closed")

	// ErrInvalidHash is returned when a hash string is malformed.
	ErrInvalidHash = errors.New("invalid content hash format")

	// ErrFileChunkNotFound is returned when a file chunk is not found.
	ErrFileChunkNotFound = errors.New("file chunk not found")

	// ErrUnknownHash is returned by FileChunkStore.AddRef when no
	// FileChunk row exists for the given hash. The read-through hit path
	// (pkg/block/engine fetch) MUST
	// fall back to the full Put path on this sentinel; the LRU may
	// be ahead of the metadata store after a crash (RAM-only LRU
	// see), or the hash may not be present yet.
	ErrUnknownHash = errors.New("metadata: hash not yet present in FileChunkStore (AddRef called before Put)")

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

	// ErrChunkContentMismatch is returned by the streaming BLAKE3 verifier on
	// S3 GET when the recomputed hash (or the x-amz-meta-content-hash header)
	// does not match the expected ContentHash. On mismatch, the buffer is
	// discarded and this error surfaces — bad bytes never reach the caller.
	ErrChunkContentMismatch = errors.New("blockstore: chunk content hash mismatch")

	// ErrManifestInconsistent is returned when a payload's manifest holds a row
	// that cannot be placed in the file — today, a FileChunk ID that does not
	// carry a parseable "<payloadID>/<offset>". It is deliberately not treated as
	// a hole: a hole is the *absence* of a row and reads back as zeros by design,
	// so passing over an unplaceable row would serve zeros for a range the store
	// still holds bytes for, with nothing logged and no error returned. Reads
	// refuse instead, which turns silent corruption into a diagnosable one.
	ErrManifestInconsistent = errors.New("blockstore: file manifest inconsistent")

	// ErrStopWalk is the sentinel a Walk callback returns to request a
	// clean early exit (e.g., GC found its target). Walk returns nil to
	// the outer caller. Any non-ErrStopWalk error halts and propagates
	// wrapped with file/offset context. Mirrors filepath.SkipDir /
	// fs.SkipAll.
	//
	// Detection pattern (callback side, when wrapping is required)
	//
	//   return fmt.Errorf("gc target %s: %w", id, block.ErrStopWalk)
	//
	// Walk implementations match via errors.Is(err, ErrStopWalk) and
	// return nil to the outer caller. Any other non-nil callback error
	// halts and is wrapped as fmt.Errorf("walk halted at %s: %w", hash, err).
	//
	// See BlockStore.Walk.
	ErrStopWalk = errors.New("blockstore: stop walk")

	// ErrFutureFormat is returned when a store opens on-disk state that a
	// NEWER release wrote and this binary cannot read. It exists because the
	// alternative is silence: a record whose layout moved to a sibling key, or
	// a side-log whose name this binary does not know, decodes "successfully"
	// into a file with the right size and no content — the store serves zeros
	// and logs nothing.
	//
	// Wrap it with the versions the operator needs to act
	//
	//   return fmt.Errorf("%w: %s is at format version %d, this build reads up to %d",
	//       block.ErrFutureFormat, dir, onDisk, supported)
	//
	// Boot matches via errors.Is and exits 78 (EX_CONFIG per sysexits(3)).
	// Detection is per-share but the exit is fatal: a share that cannot be
	// opened safely must not leave the daemon looking healthy.
	//
	// The local and remote block tiers return it, and so does the metadata
	// store (whose migrations in sqlite/postgres/badger wrap it for their
	// side-log names), so the message carries no "blockstore:" prefix — unlike
	// the sentinels above, it is not about blocks.
	ErrFutureFormat = errors.New("store: on-disk format is newer than this build")
)
