package metadata

import (
	"github.com/marmos91/dittofs/pkg/block"
)

// ============================================================================
// Content-Addressed Types (re-exported from blockstore for backward compatibility)
// ============================================================================

// HashSize is the size of content hashes (BLAKE3 = 32 bytes).
const HashSize = block.HashSize

// ContentHash represents a BLAKE3-256 hash of content.
// Type alias to block.ContentHash -- all definitions live in pkg/block.
type ContentHash = block.ContentHash

// ParseContentHash parses a hex-encoded hash string.
var ParseContentHash = block.ParseContentHash

// ============================================================================
// ObjectID Type for FileAttr
// ============================================================================

// ObjectID is a reference to a content-addressed Object.
// It's the ContentHash stored as a fixed-size array for embedding in FileAttr.
type ObjectID = ContentHash

// ============================================================================
// BlockState (re-exported from blockstore for backward compatibility)
// ============================================================================

// BlockState represents the lifecycle state of a FileChunk.
type BlockState = block.BlockState

// BlockState constants re-exported from blockstore.
//
// collapsed the previous 4-state machine
// (Dirty/Local/Syncing/Remote) to 3 states (Pending/Syncing/Remote);
// Pending(0) replaces both Dirty(0) and Local(1).
const (
	BlockStatePending = block.BlockStatePending
	BlockStateSyncing = block.BlockStateSyncing
	BlockStateRemote  = block.BlockStateRemote
)

// ============================================================================
// FileChunk (re-exported from blockstore for backward compatibility)
// ============================================================================

// FileChunk is the per-file content-chunk entity in DittoFS.
// Type alias to block.FileChunk -- all definitions live in pkg/block.
type FileChunk = block.FileChunk

// NewFileChunk creates a new pending FileChunk with the given ID and cache path.
var NewFileChunk = block.NewFileChunk

// ============================================================================
// Errors (re-exported from blockstore for backward compatibility)
// ============================================================================

// ErrInvalidHash is returned when a hash string is malformed.
var ErrInvalidHash = block.ErrInvalidHash

// ErrFileChunkNotFound is returned when a file chunk is not found.
var ErrFileChunkNotFound = block.ErrFileChunkNotFound

// ErrUnknownHash is returned by FileChunkStore.AddRef when the hash is
// not yet present in the metadata store. Re-exported from blockstore
// for backward compatibility — see block.ErrUnknownHash for the
// canonical declaration and full contract..
var ErrUnknownHash = block.ErrUnknownHash
