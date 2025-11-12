package metadata

import "time"

// WriteOperation represents a validated intent to write to a file.
//
// This is returned by PrepareWrite and contains everything needed to:
//  1. Write content to the content repository
//  2. Commit metadata changes after successful write
//
// The metadata repository does NOT modify any metadata during PrepareWrite.
// This ensures consistency - metadata only changes after content is safely written.
//
// Lifecycle:
//   - PrepareWrite validates and creates intent (no metadata changes)
//   - Protocol handler writes content using ContentID from intent
//   - CommitWrite updates metadata after successful content write
//   - If content write fails, no rollback needed (metadata unchanged)
type WriteOperation struct {
	// Handle is the file being written to
	Handle FileHandle

	// NewSize is the file size after the write
	NewSize uint64

	// NewMtime is the modification time to set after write
	NewMtime time.Time

	// ContentID is the identifier for writing to content repository
	ContentID ContentID

	// PreWriteAttr contains the file attributes before the write
	// Used for protocol responses (e.g., NFS WCC data)
	PreWriteAttr *FileAttr
}

// ReadMetadata contains metadata returned by PrepareRead.
//
// This provides the protocol handler with the information needed to read
// file content from the content repository.
type ReadMetadata struct {
	// Attr contains the file attributes including the ContentID
	// The protocol handler uses ContentID to read from the content repository
	Attr *FileAttr
}
