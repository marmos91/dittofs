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

		assert.Equal(t, "file not found: /path/to/file", err.Error())
	})

	t.Run("error without path returns message only", func(t *testing.T) {
		t.Parallel()
		err := &StoreError{
			Code:    ErrInvalidHandle,
			Message: "invalid file handle",
			Path:    "",
		}

		assert.Equal(t, "invalid file handle", err.Error())
	})

	t.Run("error with empty message and path", func(t *testing.T) {
		t.Parallel()
		err := &StoreError{
			Code:    ErrIOError,
			Message: "",
			Path:    "",
		}

		assert.Equal(t, "", err.Error())
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
		wantMsg    string
	}{
		{"file not found", "/path/to/file.txt", "file", "file not found: /path/to/file.txt"},
		{"directory not found", "/path/to/dir", "directory", "directory not found: /path/to/dir"},
		{"share not found", "/export", "share", "share not found: /export"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := NewNotFoundError(tt.path, tt.entityType)

			assert.Equal(t, ErrNotFound, err.Code)
			assert.Equal(t, tt.path, err.Path)
			assert.Equal(t, tt.wantMsg, err.Error())
		})
	}
}

func TestNewPermissionDeniedError(t *testing.T) {
	t.Parallel()

	err := NewPermissionDeniedError("/protected/file.txt")

	assert.Equal(t, ErrPermissionDenied, err.Code)
	assert.Equal(t, "/protected/file.txt", err.Path)
	assert.Equal(t, "permission denied: /protected/file.txt", err.Error())
}

func TestNewIsDirectoryError(t *testing.T) {
	t.Parallel()

	err := NewIsDirectoryError("/path/to/directory")

	assert.Equal(t, ErrIsDirectory, err.Code)
	assert.Equal(t, "/path/to/directory", err.Path)
	assert.Equal(t, "is a directory: /path/to/directory", err.Error())
}

func TestNewNotDirectoryError(t *testing.T) {
	t.Parallel()

	err := NewNotDirectoryError("/path/to/file.txt")

	assert.Equal(t, ErrNotDirectory, err.Code)
	assert.Equal(t, "/path/to/file.txt", err.Path)
	assert.Equal(t, "not a directory: /path/to/file.txt", err.Error())
}

func TestNewInvalidHandleError(t *testing.T) {
	t.Parallel()

	err := NewInvalidHandleError()

	assert.Equal(t, ErrInvalidHandle, err.Code)
	assert.Empty(t, err.Path)
	assert.Equal(t, "invalid file handle", err.Error())
}

func TestNewNotEmptyError(t *testing.T) {
	t.Parallel()

	err := NewNotEmptyError("/path/to/directory")

	assert.Equal(t, ErrNotEmpty, err.Code)
	assert.Equal(t, "/path/to/directory", err.Path)
	assert.Equal(t, "directory not empty: /path/to/directory", err.Error())
}

func TestNewAlreadyExistsError(t *testing.T) {
	t.Parallel()

	err := NewAlreadyExistsError("/path/to/existing")

	assert.Equal(t, ErrAlreadyExists, err.Code)
	assert.Equal(t, "/path/to/existing", err.Path)
	assert.Equal(t, "already exists: /path/to/existing", err.Error())
}

func TestNewInvalidArgumentError(t *testing.T) {
	t.Parallel()

	err := NewInvalidArgumentError("invalid mode value")

	assert.Equal(t, ErrInvalidArgument, err.Code)
	assert.Empty(t, err.Path)
	assert.Equal(t, "invalid mode value", err.Error())
}

func TestNewAccessDeniedError(t *testing.T) {
	t.Parallel()

	err := NewAccessDeniedError("client IP not in allowed list")

	assert.Equal(t, ErrAccessDenied, err.Code)
	assert.Empty(t, err.Path)
	assert.Equal(t, "client IP not in allowed list", err.Error())
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
		assert.Contains(t, err.Error(), "session 12345")
		assert.Contains(t, err.Error(), "offset=100")
		assert.Contains(t, err.Error(), "length=50")
		assert.Contains(t, err.Error(), "exclusive=true")
	})

	t.Run("without conflict details", func(t *testing.T) {
		t.Parallel()
		err := NewLockedError("/path/to/locked/file", nil)

		assert.Equal(t, ErrLocked, err.Code)
		assert.Equal(t, "/path/to/locked/file", err.Path)
		assert.Equal(t, "file is locked: /path/to/locked/file", err.Error())
	})
}

func TestNewLockNotFoundError(t *testing.T) {
	t.Parallel()

	err := NewLockNotFoundError("/path/to/file")

	assert.Equal(t, ErrLockNotFound, err.Code)
	assert.Equal(t, "/path/to/file", err.Path)
	assert.Equal(t, "lock not found: /path/to/file", err.Error())
}

func TestNewQuotaExceededError(t *testing.T) {
	t.Parallel()

	err := NewQuotaExceededError("/user/home/largefile")

	assert.Equal(t, ErrQuotaExceeded, err.Code)
	assert.Equal(t, "/user/home/largefile", err.Path)
	assert.Equal(t, "disk quota exceeded: /user/home/largefile", err.Error())
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
	assert.Equal(t, "name too long: "+longPath, err.Error())
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
