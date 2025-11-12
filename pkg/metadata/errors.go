package metadata

// RepositoryError represents a domain error from repository operations.
//
// These are business logic errors (file not found, permission denied, etc.)
// as opposed to infrastructure errors (network failure, disk error).
//
// Protocol handlers translate RepositoryError codes to protocol-specific
// error codes (e.g., NFS status codes, SMB error codes).
type RepositoryError struct {
	// Code is the error category
	Code ErrorCode

	// Message is a human-readable error description
	Message string

	// Path is the filesystem path related to the error (if applicable)
	// This helps with debugging and error reporting
	Path string
}

// Error implements the error interface.
func (e *RepositoryError) Error() string {
	if e.Path != "" {
		return e.Message + ": " + e.Path
	}
	return e.Message
}

// ErrorCode represents the category of a repository error.
//
// These are generic error categories that map to protocol-specific errors.
// Protocol handlers translate ErrorCode to appropriate protocol error codes.
type ErrorCode int

const (
	// ErrNotFound indicates the requested file/directory/share doesn't exist
	ErrNotFound ErrorCode = iota

	// ErrAccessDenied indicates share-level access was denied
	// Used for IP-based access control, authentication failures, etc.
	ErrAccessDenied

	// ErrAuthRequired indicates authentication is required but not provided
	ErrAuthRequired

	// ErrPermissionDenied indicates file-level permission was denied
	// Used for Unix permission checks (read/write/execute)
	ErrPermissionDenied

	// ErrAlreadyExists indicates a file/directory with the name already exists
	ErrAlreadyExists

	// ErrNotEmpty indicates a directory is not empty (cannot be removed)
	ErrNotEmpty

	// ErrIsDirectory indicates operation expected a file but got a directory
	ErrIsDirectory

	// ErrNotDirectory indicates operation expected a directory but got a file
	ErrNotDirectory

	// ErrInvalidArgument indicates invalid parameters were provided
	// Examples: empty name, invalid mode, negative size
	ErrInvalidArgument

	// ErrIOError indicates an I/O error occurred
	// Used for errors reading/writing metadata or content
	ErrIOError

	// ErrNoSpace indicates no space is available
	// Used when filesystem is full (bytes or inodes)
	ErrNoSpace

	// ErrReadOnly indicates operation failed because filesystem is read-only
	ErrReadOnly

	// ErrNotSupported indicates operation is not supported by implementation
	// Examples: hard links on implementations that don't support them
	ErrNotSupported

	// ErrInvalidHandle indicates the file handle is malformed
	// Different from ErrNotFound - the handle format itself is invalid
	ErrInvalidHandle

	// ErrStaleHandle indicates the file handle is valid but stale
	// Used when a file has been deleted but handle is still in use
	ErrStaleHandle
)
