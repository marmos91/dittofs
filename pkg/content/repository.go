package content

import (
	"context"
	"io"
)

// ContentID represents a unique identifier for blob content
type ContentID string

// Repository defines the interface for reading file content
type Repository interface {
	// ReadContent returns a reader for the content identified by the given ID
	// Returns an error if the content doesn't exist or can't be read
	ReadContent(ctx context.Context, id ContentID) (io.ReadCloser, error)

	// GetContentSize returns the size of the content in bytes
	// This is useful for NFS operations that need to know file size without reading
	GetContentSize(ctx context.Context, id ContentID) (uint64, error)

	// ContentExists checks if content with the given ID exists
	ContentExists(ctx context.Context, id ContentID) (bool, error)
}

type WriteRepository interface {
	Repository

	// WriteAt writes data at the specified offset
	// If the file doesn't exist, it will be created
	WriteAt(ctx context.Context, id ContentID, data []byte, offset int64) error

	// Truncate changes the size of the content to the specified size.
	// If size is less than current size, content is truncated.
	// If size is greater than current size, content is extended (usually with zeros).
	Truncate(ctx context.Context, id ContentID, size uint64) error
}

// SeekableContentRepository is an optional extended interface
// for repositories that support random access reads
type SeekableContentRepository interface {
	Repository

	// ReadContentSeekable returns a seeker for random access
	// This is useful for efficient partial reads
	ReadContentSeekable(ctx context.Context, id ContentID) (io.ReadSeekCloser, error)
}
