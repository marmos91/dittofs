package content

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// FSContentRepository implements ContentRepository using the local filesystem
type FSContentRepository struct {
	basePath string
}

// NewFSContentRepository creates a new filesystem-based content repository
func NewFSContentRepository(basePath string) (*FSContentRepository, error) {
	// Create the base directory if it doesn't exist
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}

	return &FSContentRepository{
		basePath: basePath,
	}, nil
}

// getFilePath returns the full path for a given content ID
func (r *FSContentRepository) getFilePath(id ContentID) string {
	return filepath.Join(r.basePath, string(id))
}

// ReadContent returns a reader for the content identified by the given ID
func (r *FSContentRepository) ReadContent(id ContentID) (io.ReadCloser, error) {
	filePath := r.getFilePath(id)

	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("content not found: %s", id)
		}
		return nil, fmt.Errorf("failed to open content: %w", err)
	}

	return file, nil
}

// GetContentSize returns the size of the content in bytes
func (r *FSContentRepository) GetContentSize(id ContentID) (uint64, error) {
	filePath := r.getFilePath(id)

	info, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("content not found: %s", id)
		}
		return 0, fmt.Errorf("failed to stat content: %w", err)
	}

	return uint64(info.Size()), nil
}

// ContentExists checks if content with the given ID exists
func (r *FSContentRepository) ContentExists(id ContentID) (bool, error) {
	filePath := r.getFilePath(id)

	_, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check content existence: %w", err)
	}

	return true, nil
}

// WriteContent writes content to the repository and returns the content ID
// This is a helper method for testing and initial setup
func (r *FSContentRepository) WriteContent(id ContentID, content []byte) error {
	filePath := r.getFilePath(id)

	if err := os.WriteFile(filePath, content, 0644); err != nil {
		return fmt.Errorf("failed to write content: %w", err)
	}

	return nil
}

// WriteAt writes data at the specified offset
// Implements WriteRepository interface
func (r *FSContentRepository) WriteAt(id ContentID, data []byte, offset int64) error {
	filePath := r.getFilePath(id)

	// Open file for read/write, create if doesn't exist
	file, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("failed to open file for writing: %w", err)
	}
	defer file.Close()

	// Seek to offset
	_, err = file.Seek(offset, io.SeekStart)
	if err != nil {
		return fmt.Errorf("failed to seek to offset: %w", err)
	}

	// Write data
	_, err = file.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write data: %w", err)
	}

	return nil
}
