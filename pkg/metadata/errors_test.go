package metadata

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// StoreError.Error() Tests
// ============================================================================

func TestStoreError_Error(t *testing.T) {
	t.Parallel()

	t.Run("error with path includes path in message", func(t *testing.T) {
		t.Parallel()
		err := &StoreError{
			Code:    ErrNotFound,
			Message: "file not found",
			Path:    "/path/to/file",
		}

		// New format: "Code: message (path: /path)"
		assert.Contains(t, err.Error(), "NotFound")
		assert.Contains(t, err.Error(), "file not found")
		assert.Contains(t, err.Error(), "/path/to/file")
	})

	t.Run("error without path returns message only", func(t *testing.T) {
		t.Parallel()
		err := &StoreError{
			Code:    ErrInvalidHandle,
			Message: "invalid file handle",
			Path:    "",
		}

		assert.Contains(t, err.Error(), "InvalidHandle")
		assert.Contains(t, err.Error(), "invalid file handle")
	})

	t.Run("error with empty message and path", func(t *testing.T) {
		t.Parallel()
		err := &StoreError{
			Code:    ErrIOError,
			Message: "",
			Path:    "",
		}

		assert.Contains(t, err.Error(), "IOError")
	})
}

// ============================================================================
// Error Factory Function Tests
// ============================================================================

func TestNewPermissionDeniedError(t *testing.T) {
	t.Parallel()

	err := NewPermissionDeniedError("/protected/file.txt")

	assert.Equal(t, ErrPermissionDenied, err.Code)
	assert.Equal(t, "/protected/file.txt", err.Path)
	assert.Contains(t, err.Error(), "permission denied")
	assert.Contains(t, err.Error(), "/protected/file.txt")
}

func TestNewIsDirectoryError(t *testing.T) {
	t.Parallel()

	err := NewIsDirectoryError("/path/to/directory")

	assert.Equal(t, ErrIsDirectory, err.Code)
	assert.Equal(t, "/path/to/directory", err.Path)
	assert.Contains(t, err.Error(), "is a directory")
	assert.Contains(t, err.Error(), "/path/to/directory")
}

func TestNewInvalidHandleError(t *testing.T) {
	t.Parallel()

	err := NewInvalidHandleError()

	assert.Equal(t, ErrInvalidHandle, err.Code)
	assert.Empty(t, err.Path)
	assert.Contains(t, err.Error(), "invalid file handle")
}

func TestNewInvalidArgumentError(t *testing.T) {
	t.Parallel()

	err := NewInvalidArgumentError("invalid mode value")

	assert.Equal(t, ErrInvalidArgument, err.Code)
	assert.Empty(t, err.Path)
	assert.Contains(t, err.Error(), "invalid mode value")
}

func TestNewLockedError(t *testing.T) {
	t.Parallel()

	t.Run("with conflict details", func(t *testing.T) {
		t.Parallel()
		conflict := &LockConflict{
			OwnerSessionID: 12345,
			Offset:         100,
			Length:         50,
			Exclusive:      true,
		}

		err := NewLockedError("/path/to/locked/file", conflict)

		assert.Equal(t, ErrLocked, err.Code)
		assert.Equal(t, "/path/to/locked/file", err.Path)
		// New simplified error format - just check for lock indication
		assert.Contains(t, err.Error(), "locked")
	})

	t.Run("without conflict details", func(t *testing.T) {
		t.Parallel()
		err := NewLockedError("/path/to/locked/file", nil)

		assert.Equal(t, ErrLocked, err.Code)
		assert.Equal(t, "/path/to/locked/file", err.Path)
		assert.Contains(t, err.Error(), "locked")
		assert.Contains(t, err.Error(), "/path/to/locked/file")
	})
}

// ============================================================================
// IsNotFoundError Tests
// ============================================================================

func TestIsNotFoundError(t *testing.T) {
	t.Parallel()

	t.Run("nil error returns false", func(t *testing.T) {
		t.Parallel()
		assert.False(t, IsNotFoundError(nil))
	})

	t.Run("StoreError with ErrNotFound returns true", func(t *testing.T) {
		t.Parallel()
		err := &StoreError{Code: ErrNotFound, Message: "not found"}
		assert.True(t, IsNotFoundError(err))
	})

	t.Run("StoreError with different code returns false", func(t *testing.T) {
		t.Parallel()
		err := &StoreError{Code: ErrPermissionDenied, Message: "denied"}
		assert.False(t, IsNotFoundError(err))
	})

	t.Run("non-StoreError returns false", func(t *testing.T) {
		t.Parallel()
		err := errors.New("some other error")
		assert.False(t, IsNotFoundError(err))
	})

	t.Run("StoreError carrying ErrNotFound returns true", func(t *testing.T) {
		t.Parallel()
		err := &StoreError{Code: ErrNotFound, Path: "/path", Message: "file"}
		assert.True(t, IsNotFoundError(err))
	})
}

// ============================================================================
// ErrorCode Tests
// ============================================================================

func TestErrorCodes(t *testing.T) {
	t.Parallel()

	// Verify all error codes have distinct values
	codes := []ErrorCode{
		ErrNotFound,
		ErrAccessDenied,
		ErrAuthRequired,
		ErrPermissionDenied,
		ErrAlreadyExists,
		ErrNotEmpty,
		ErrIsDirectory,
		ErrNotDirectory,
		ErrInvalidArgument,
		ErrIOError,
		ErrNoSpace,
		ErrQuotaExceeded,
		ErrReadOnly,
		ErrNotSupported,
		ErrInvalidHandle,
		ErrStaleHandle,
		ErrLocked,
		ErrLockNotFound,
		ErrPrivilegeRequired,
		ErrNameTooLong,
	}

	seen := make(map[ErrorCode]bool)
	for _, code := range codes {
		require.False(t, seen[code], "duplicate error code: %d", code)
		seen[code] = true
	}
}

// ============================================================================
// Error Interface Compliance Tests
// ============================================================================

func TestStoreError_ImplementsError(t *testing.T) {
	t.Parallel()

	// Verify StoreError implements error interface
	var _ error = &StoreError{}

	// Verify it can be used with errors.As
	err := &StoreError{Code: ErrNotFound, Path: "/path", Message: "file"}
	var storeErr *StoreError
	require.True(t, errors.As(err, &storeErr))
	assert.Equal(t, ErrNotFound, storeErr.Code)
}
