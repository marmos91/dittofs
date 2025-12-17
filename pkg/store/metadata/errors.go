package metadata

// StoreError represents a domain error from repository operations.
//
// These are business logic errors (file not found, permission denied, etc.)
// as opposed to infrastructure errors (network failure, disk error).
//
// Protocol handlers translate StoreError codes to protocol-specific
// error codes (e.g., NFS status codes, SMB error codes).
type StoreError struct {
	// Code is the error category
	Code ErrorCode

	// Message is a human-readable error description
	Message string

	// Path is the filesystem path related to the error (if applicable)
	// This helps with debugging and error reporting
	Path string
}

// Error implements the error interface.
func (e *StoreError) Error() string {
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

// ============================================================================
// Error Factory Functions
// ============================================================================
// These factory functions provide a consistent way to create common errors
// across all metadata store implementations.

// NewNotFoundError creates a StoreError for when a file, directory, or share is not found.
//
// Parameters:
//   - path: The path that was not found
//   - entityType: The type of entity (e.g., "file", "directory", "share")
//
// Returns:
//   - *StoreError with ErrNotFound code
func NewNotFoundError(path string, entityType string) *StoreError {
	return &StoreError{
		Code:    ErrNotFound,
		Message: entityType + " not found",
		Path:    path,
	}
}

// NewPermissionDeniedError creates a StoreError for permission denied errors.
//
// Parameters:
//   - path: The path where permission was denied
//
// Returns:
//   - *StoreError with ErrPermissionDenied code
func NewPermissionDeniedError(path string) *StoreError {
	return &StoreError{
		Code:    ErrPermissionDenied,
		Message: "permission denied",
		Path:    path,
	}
}

// NewIsDirectoryError creates a StoreError for when a file operation is attempted on a directory.
//
// Parameters:
//   - path: The path that is a directory
//
// Returns:
//   - *StoreError with ErrIsDirectory code
func NewIsDirectoryError(path string) *StoreError {
	return &StoreError{
		Code:    ErrIsDirectory,
		Message: "is a directory",
		Path:    path,
	}
}

// NewNotDirectoryError creates a StoreError for when a directory operation is attempted on a non-directory.
//
// Parameters:
//   - path: The path that is not a directory
//
// Returns:
//   - *StoreError with ErrNotDirectory code
func NewNotDirectoryError(path string) *StoreError {
	return &StoreError{
		Code:    ErrNotDirectory,
		Message: "not a directory",
		Path:    path,
	}
}

// NewInvalidHandleError creates a StoreError for invalid file handles.
//
// Returns:
//   - *StoreError with ErrInvalidHandle code
func NewInvalidHandleError() *StoreError {
	return &StoreError{
		Code:    ErrInvalidHandle,
		Message: "invalid file handle",
	}
}

// NewNotEmptyError creates a StoreError for when a directory is not empty.
//
// Parameters:
//   - path: The directory path that is not empty
//
// Returns:
//   - *StoreError with ErrNotEmpty code
func NewNotEmptyError(path string) *StoreError {
	return &StoreError{
		Code:    ErrNotEmpty,
		Message: "directory not empty",
		Path:    path,
	}
}

// NewAlreadyExistsError creates a StoreError for when a file/directory already exists.
//
// Parameters:
//   - path: The path that already exists
//
// Returns:
//   - *StoreError with ErrAlreadyExists code
func NewAlreadyExistsError(path string) *StoreError {
	return &StoreError{
		Code:    ErrAlreadyExists,
		Message: "already exists",
		Path:    path,
	}
}

// NewInvalidArgumentError creates a StoreError for invalid arguments.
//
// Parameters:
//   - message: Description of the invalid argument
//
// Returns:
//   - *StoreError with ErrInvalidArgument code
func NewInvalidArgumentError(message string) *StoreError {
	return &StoreError{
		Code:    ErrInvalidArgument,
		Message: message,
	}
}

// NewAccessDeniedError creates a StoreError for share-level access denial.
//
// Parameters:
//   - reason: The reason for access denial
//
// Returns:
//   - *StoreError with ErrAccessDenied code
func NewAccessDeniedError(reason string) *StoreError {
	return &StoreError{
		Code:    ErrAccessDenied,
		Message: reason,
	}
}
