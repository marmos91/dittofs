package content

// ============================================================================
// Supporting Types
// ============================================================================

// StorageStats contains statistics about content storage.
//
// This provides information about storage capacity, usage, and health.
// Different backends may support different fields (unsupported fields
// should be set to 0).
type StorageStats struct {
	// TotalSize is the total storage capacity in bytes.
	// For cloud storage (S3), this may be unlimited (set to MaxUint64).
	// For filesystem, this is the total disk size.
	TotalSize uint64

	// UsedSize is the actual space consumed by content in bytes.
	// This is the sum of all content sizes.
	UsedSize uint64

	// AvailableSize is the remaining available space in bytes.
	// For cloud storage, this may be unlimited (set to MaxUint64).
	// For filesystem: AvailableSize = TotalSize - UsedSize (approximately)
	AvailableSize uint64

	// ContentCount is the total number of content items stored.
	ContentCount uint64

	// AverageSize is the average size of content items in bytes.
	// Calculated as: UsedSize / ContentCount
	// Set to 0 if ContentCount is 0.
	AverageSize uint64
}

// IncrementalWriteState tracks the state of an incremental write session.
type IncrementalWriteState struct {
	// UploadID is the S3 multipart upload ID (empty if not yet started)
	UploadID string

	// PartsWritten is the count of successfully uploaded parts
	PartsWritten int

	// PartsWriting is the count of parts currently being uploaded
	// Used by flusher to avoid finalizing while uploads in progress
	PartsWriting int

	// TotalFlushed is the total bytes uploaded so far
	TotalFlushed int64
}

// FlushResult contains information about a flush operation.
type FlushResult struct {
	// BytesFlushed is the number of bytes written to the content store.
	BytesFlushed uint64

	// Incremental indicates whether incremental flush was used (S3 multipart).
	Incremental bool

	// AlreadyFlushed indicates all data was already flushed (no-op).
	AlreadyFlushed bool

	// Finalized indicates whether the content was finalized (complete and durable).
	Finalized bool
}
