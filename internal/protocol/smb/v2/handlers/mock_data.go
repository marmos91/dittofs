package handlers

import (
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// MockFile represents a mock file for Phase 1
type MockFile struct {
	Name       string
	IsDir      bool
	Size       int64
	Content    []byte
	Created    time.Time
	Modified   time.Time
	Accessed   time.Time
	Attributes uint32
}

// GetMockFiles returns mock directory contents for Phase 1
func (h *Handler) GetMockFiles(shareName, path string) map[string]*MockFile {
	now := time.Now()

	// Normalize path
	path = strings.TrimPrefix(path, "\\")
	path = strings.TrimPrefix(path, "/")

	// Root directory mock files
	if path == "" {
		return map[string]*MockFile{
			"readme.txt": {
				Name:       "readme.txt",
				IsDir:      false,
				Size:       int64(len("Hello from DittoFS SMB2!\n")),
				Content:    []byte("Hello from DittoFS SMB2!\n"),
				Created:    now,
				Modified:   now,
				Accessed:   now,
				Attributes: uint32(types.FileAttributeNormal),
			},
			"subdir": {
				Name:       "subdir",
				IsDir:      true,
				Size:       0,
				Created:    now,
				Modified:   now,
				Accessed:   now,
				Attributes: uint32(types.FileAttributeDirectory),
			},
		}
	}

	return map[string]*MockFile{}
}

// GetMockFile returns a specific mock file
func (h *Handler) GetMockFile(shareName, path string) *MockFile {
	// Normalize path
	path = strings.TrimPrefix(path, "\\")
	path = strings.TrimPrefix(path, "/")

	if path == "" {
		// Root directory
		return &MockFile{
			Name:       "",
			IsDir:      true,
			Size:       0,
			Created:    time.Now(),
			Modified:   time.Now(),
			Accessed:   time.Now(),
			Attributes: uint32(types.FileAttributeDirectory),
		}
	}

	files := h.GetMockFiles(shareName, "")
	return files[path]
}

// MockShareExists checks if share exists (Phase 1 only)
func (h *Handler) MockShareExists(shareName string) bool {
	// Normalize share name
	shareName = strings.TrimPrefix(shareName, "/")
	shareName = strings.TrimPrefix(shareName, "\\")
	shareName = strings.ToLower(shareName)

	// Hardcoded for Phase 1
	return shareName == "export"
}
