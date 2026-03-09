package metadata

import (
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// ============================================================================
// Content-Addressed Types (re-exported from blockstore for backward compatibility)
// ============================================================================

// HashSize is the size of content hashes (SHA-256 = 32 bytes).
const HashSize = blockstore.HashSize

// ContentHash represents a SHA-256 hash of content.
// Type alias to blockstore.ContentHash -- all definitions live in pkg/blockstore.
type ContentHash = blockstore.ContentHash

// ParseContentHash parses a hex-encoded hash string.
var ParseContentHash = blockstore.ParseContentHash

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
type BlockState = blockstore.BlockState

// BlockState constants re-exported from blockstore.
const (
	BlockStateDirty   = blockstore.BlockStateDirty
	BlockStateLocal   = blockstore.BlockStateLocal
	BlockStateSyncing = blockstore.BlockStateSyncing
	BlockStateRemote  = blockstore.BlockStateRemote
)

// ============================================================================
// FileBlock (re-exported from blockstore for backward compatibility)
// ============================================================================

// FileBlock is the single block entity in DittoFS.
// Type alias to blockstore.FileBlock -- all definitions live in pkg/blockstore.
type FileBlock = blockstore.FileBlock

// NewFileBlock creates a new pending FileBlock with the given ID and cache path.
var NewFileBlock = blockstore.NewFileBlock

// ============================================================================
// Errors (re-exported from blockstore for backward compatibility)
// ============================================================================

// ErrInvalidHash is returned when a hash string is malformed.
var ErrInvalidHash = blockstore.ErrInvalidHash

// ErrFileBlockNotFound is returned when a file block is not found.
var ErrFileBlockNotFound = blockstore.ErrFileBlockNotFound
