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

func TestNewNotFoundError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		entityType string
	}{
		{"file not found", "/path/to/file.txt", "file"},
		{"directory not found", "/path/to/dir", "directory"},
		{"share not found", "/export", "share"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := NewNotFoundError(tt.path, tt.entityType)

			assert.Equal(t, ErrNotFound, err.Code)
			assert.Equal(t, tt.path, err.Path)
			assert.Contains(t, err.Error(), tt.entityType+" not found")
			assert.Contains(t, err.Error(), tt.path)
		})
	}
}

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

func TestNewNotDirectoryError(t *testing.T) {
	t.Parallel()

	err := NewNotDirectoryError("/path/to/file.txt")

	assert.Equal(t, ErrNotDirectory, err.Code)
	assert.Equal(t, "/path/to/file.txt", err.Path)
	assert.Contains(t, err.Error(), "not a directory")
	assert.Contains(t, err.Error(), "/path/to/file.txt")
}

func TestNewInvalidHandleError(t *testing.T) {
	t.Parallel()

	err := NewInvalidHandleError()

	assert.Equal(t, ErrInvalidHandle, err.Code)
	assert.Empty(t, err.Path)
	assert.Contains(t, err.Error(), "invalid file handle")
}

func TestNewNotEmptyError(t *testing.T) {
	t.Parallel()

	err := NewNotEmptyError("/path/to/directory")

	assert.Equal(t, ErrNotEmpty, err.Code)
	assert.Equal(t, "/path/to/directory", err.Path)
	assert.Contains(t, err.Error(), "directory not empty")
	assert.Contains(t, err.Error(), "/path/to/directory")
}

func TestNewAlreadyExistsError(t *testing.T) {
	t.Parallel()

	err := NewAlreadyExistsError("/path/to/existing")

	assert.Equal(t, ErrAlreadyExists, err.Code)
	assert.Equal(t, "/path/to/existing", err.Path)
	assert.Contains(t, err.Error(), "already exists")
	assert.Contains(t, err.Error(), "/path/to/existing")
}

func TestNewInvalidArgumentError(t *testing.T) {
	t.Parallel()

	err := NewInvalidArgumentError("invalid mode value")

	assert.Equal(t, ErrInvalidArgument, err.Code)
	assert.Empty(t, err.Path)
	assert.Contains(t, err.Error(), "invalid mode value")
}

func TestNewAccessDeniedError(t *testing.T) {
	t.Parallel()

	err := NewAccessDeniedError("client IP not in allowed list")

	assert.Equal(t, ErrAccessDenied, err.Code)
	assert.Empty(t, err.Path)
	assert.Contains(t, err.Error(), "client IP not in allowed list")
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

func TestNewLockNotFoundError(t *testing.T) {
	t.Parallel()

	err := NewLockNotFoundError("/path/to/file")

	assert.Equal(t, ErrLockNotFound, err.Code)
	assert.Equal(t, "/path/to/file", err.Path)
	assert.Contains(t, err.Error(), "lock not found")
	assert.Contains(t, err.Error(), "/path/to/file")
}

func TestNewQuotaExceededError(t *testing.T) {
	t.Parallel()

	err := NewQuotaExceededError("/user/home/largefile")

	assert.Equal(t, ErrQuotaExceeded, err.Code)
	assert.Equal(t, "/user/home/largefile", err.Path)
	assert.Contains(t, err.Error(), "quota exceeded")
	assert.Contains(t, err.Error(), "/user/home/largefile")
}

func TestNewPrivilegeRequiredError(t *testing.T) {
	t.Parallel()

	err := NewPrivilegeRequiredError("chown")

	assert.Equal(t, ErrPrivilegeRequired, err.Code)
	assert.Empty(t, err.Path)
	assert.Contains(t, err.Error(), "chown")
	assert.Contains(t, err.Error(), "root privileges")
}

func TestNewNameTooLongError(t *testing.T) {
	t.Parallel()

	longPath := "/very/long/path/that/exceeds/limits"
	err := NewNameTooLongError(longPath)

	assert.Equal(t, ErrNameTooLong, err.Code)
	assert.Equal(t, longPath, err.Path)
	assert.Contains(t, err.Error(), "name too long")
	assert.Contains(t, err.Error(), longPath)
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

	t.Run("NewNotFoundError result returns true", func(t *testing.T) {
		t.Parallel()
		err := NewNotFoundError("/path", "file")
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
	err := NewNotFoundError("/path", "file")
	var storeErr *StoreError
	require.True(t, errors.As(err, &storeErr))
	assert.Equal(t, ErrNotFound, storeErr.Code)
}
