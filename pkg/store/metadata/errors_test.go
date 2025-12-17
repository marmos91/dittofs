package metadata

import (
	"testing"
)

func TestNewNotFoundError(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		entityType string
		wantMsg    string
	}{
		{"file not found", "/path/to/file", "file", "file not found"},
		{"directory not found", "/path/to/dir", "directory", "directory not found"},
		{"share not found", "/export", "share", "share not found"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewNotFoundError(tt.path, tt.entityType)

			if err.Code != ErrNotFound {
				t.Errorf("Code = %v, want %v", err.Code, ErrNotFound)
			}
			if err.Message != tt.wantMsg {
				t.Errorf("Message = %q, want %q", err.Message, tt.wantMsg)
			}
			if err.Path != tt.path {
				t.Errorf("Path = %q, want %q", err.Path, tt.path)
			}
		})
	}
}

func TestNewPermissionDeniedError(t *testing.T) {
	err := NewPermissionDeniedError("/path/to/file")

	if err.Code != ErrPermissionDenied {
		t.Errorf("Code = %v, want %v", err.Code, ErrPermissionDenied)
	}
	if err.Message != "permission denied" {
		t.Errorf("Message = %q, want %q", err.Message, "permission denied")
	}
	if err.Path != "/path/to/file" {
		t.Errorf("Path = %q, want %q", err.Path, "/path/to/file")
	}
}

func TestNewIsDirectoryError(t *testing.T) {
	err := NewIsDirectoryError("/path/to/dir")

	if err.Code != ErrIsDirectory {
		t.Errorf("Code = %v, want %v", err.Code, ErrIsDirectory)
	}
	if err.Message != "is a directory" {
		t.Errorf("Message = %q, want %q", err.Message, "is a directory")
	}
	if err.Path != "/path/to/dir" {
		t.Errorf("Path = %q, want %q", err.Path, "/path/to/dir")
	}
}

func TestNewNotDirectoryError(t *testing.T) {
	err := NewNotDirectoryError("/path/to/file")

	if err.Code != ErrNotDirectory {
		t.Errorf("Code = %v, want %v", err.Code, ErrNotDirectory)
	}
	if err.Message != "not a directory" {
		t.Errorf("Message = %q, want %q", err.Message, "not a directory")
	}
	if err.Path != "/path/to/file" {
		t.Errorf("Path = %q, want %q", err.Path, "/path/to/file")
	}
}

func TestNewInvalidHandleError(t *testing.T) {
	err := NewInvalidHandleError()

	if err.Code != ErrInvalidHandle {
		t.Errorf("Code = %v, want %v", err.Code, ErrInvalidHandle)
	}
	if err.Message != "invalid file handle" {
		t.Errorf("Message = %q, want %q", err.Message, "invalid file handle")
	}
	if err.Path != "" {
		t.Errorf("Path = %q, want empty", err.Path)
	}
}

func TestNewNotEmptyError(t *testing.T) {
	err := NewNotEmptyError("/path/to/dir")

	if err.Code != ErrNotEmpty {
		t.Errorf("Code = %v, want %v", err.Code, ErrNotEmpty)
	}
	if err.Message != "directory not empty" {
		t.Errorf("Message = %q, want %q", err.Message, "directory not empty")
	}
	if err.Path != "/path/to/dir" {
		t.Errorf("Path = %q, want %q", err.Path, "/path/to/dir")
	}
}

func TestNewAlreadyExistsError(t *testing.T) {
	err := NewAlreadyExistsError("/path/to/file")

	if err.Code != ErrAlreadyExists {
		t.Errorf("Code = %v, want %v", err.Code, ErrAlreadyExists)
	}
	if err.Message != "already exists" {
		t.Errorf("Message = %q, want %q", err.Message, "already exists")
	}
	if err.Path != "/path/to/file" {
		t.Errorf("Path = %q, want %q", err.Path, "/path/to/file")
	}
}

func TestNewInvalidArgumentError(t *testing.T) {
	err := NewInvalidArgumentError("invalid file name")

	if err.Code != ErrInvalidArgument {
		t.Errorf("Code = %v, want %v", err.Code, ErrInvalidArgument)
	}
	if err.Message != "invalid file name" {
		t.Errorf("Message = %q, want %q", err.Message, "invalid file name")
	}
}

func TestNewAccessDeniedError(t *testing.T) {
	err := NewAccessDeniedError("client not in allow list")

	if err.Code != ErrAccessDenied {
		t.Errorf("Code = %v, want %v", err.Code, ErrAccessDenied)
	}
	if err.Message != "client not in allow list" {
		t.Errorf("Message = %q, want %q", err.Message, "client not in allow list")
	}
}

func TestStoreError_Error(t *testing.T) {
	tests := []struct {
		name    string
		err     *StoreError
		wantMsg string
	}{
		{
			name:    "with path",
			err:     &StoreError{Code: ErrNotFound, Message: "file not found", Path: "/test/path"},
			wantMsg: "file not found: /test/path",
		},
		{
			name:    "without path",
			err:     &StoreError{Code: ErrInvalidHandle, Message: "invalid file handle"},
			wantMsg: "invalid file handle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Error()
			if got != tt.wantMsg {
				t.Errorf("Error() = %q, want %q", got, tt.wantMsg)
			}
		})
	}
}
