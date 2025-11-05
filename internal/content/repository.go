package content

import "io"

// ContentID represents a unique identifier for blob content
type ContentID string

// Repository defines the interface for reading file content
type Repository interface {
	// ReadContent returns a reader for the content identified by the given ID
	// Returns an error if the content doesn't exist or can't be read
	ReadContent(id ContentID) (io.ReadCloser, error)

	// GetContentSize returns the size of the content in bytes
	// This is useful for NFS operations that need to know file size without reading
	GetContentSize(id ContentID) (uint64, error)

	// ContentExists checks if content with the given ID exists
	ContentExists(id ContentID) (bool, error)
}

type WriteRepository interface {
	Repository

	// WriteAt writes data at the specified offset
	// If the file doesn't exist, it will be created
	WriteAt(id ContentID, data []byte, offset int64) error
}

// SeekableContentRepository is an optional extended interface
// for repositories that support random access reads
type SeekableContentRepository interface {
	Repository

	// ReadContentSeekable returns a seeker for random access
	// This is useful for efficient partial reads
	ReadContentSeekable(id ContentID) (io.ReadSeekCloser, error)
}
