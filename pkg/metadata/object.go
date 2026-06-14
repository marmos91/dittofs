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

// BlockState represents the lifecycle state of a FileBlock.
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
// FileBlock (re-exported from blockstore for backward compatibility)
// ============================================================================

// FileBlock is the single block entity in DittoFS.
// Type alias to block.FileBlock -- all definitions live in pkg/block.
type FileBlock = block.FileBlock

// NewFileBlock creates a new pending FileBlock with the given ID and cache path.
var NewFileBlock = block.NewFileBlock

// ============================================================================
// Errors (re-exported from blockstore for backward compatibility)
// ============================================================================

// ErrInvalidHash is returned when a hash string is malformed.
var ErrInvalidHash = block.ErrInvalidHash

// ErrFileBlockNotFound is returned when a file block is not found.
var ErrFileBlockNotFound = block.ErrFileBlockNotFound

// ErrUnknownHash is returned by FileBlockStore.AddRef when the hash is
// not yet present in the metadata store. Re-exported from blockstore
// for backward compatibility — see block.ErrUnknownHash for the
// canonical declaration and full contract..
var ErrUnknownHash = block.ErrUnknownHash
