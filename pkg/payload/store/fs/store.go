// Package fs provides a filesystem-backed block store implementation.
package fs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/marmos91/dittofs/pkg/payload/store"
)

// Store is a filesystem-backed implementation of store.BlockStore.
// Blocks are stored as files with the block key as the path.
type Store struct {
	mu      sync.RWMutex
	basePath string
	closed   bool
}

// Config holds configuration for the filesystem block store.
type Config struct {
	// BasePath is the root directory for block storage.
	// Block keys are stored as paths relative to this directory.
	BasePath string

	// CreateDir creates the base directory if it doesn't exist.
	// Default: true
	CreateDir bool

	// DirMode is the permission mode for created directories.
	// Default: 0755
	DirMode os.FileMode

	// FileMode is the permission mode for created files.
	// Default: 0644
	FileMode os.FileMode
}

// DefaultConfig returns the default configuration.
func DefaultConfig(basePath string) Config {
	return Config{
		BasePath:  basePath,
		CreateDir: true,
		DirMode:   0755,
		FileMode:  0644,
	}
}

// New creates a new filesystem block store with the given configuration.
func New(cfg Config) (*Store, error) {
	if cfg.BasePath == "" {
		return nil, errors.New("base path is required")
	}

	if cfg.DirMode == 0 {
		cfg.DirMode = 0755
	}
	if cfg.FileMode == 0 {
		cfg.FileMode = 0644
	}

	// Create base directory if requested
	if cfg.CreateDir {
		if err := os.MkdirAll(cfg.BasePath, cfg.DirMode); err != nil {
			return nil, err
		}
	}

	// Verify the base path exists and is a directory
	info, err := os.Stat(cfg.BasePath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("base path is not a directory")
	}

	return &Store{
		basePath: cfg.BasePath,
	}, nil
}

// NewWithPath creates a new filesystem block store with default configuration.
func NewWithPath(basePath string) (*Store, error) {
	return New(DefaultConfig(basePath))
}

// blockPath returns the full filesystem path for a block key.
func (s *Store) blockPath(blockKey string) string {
	// Replace any potentially dangerous path components
	// Block keys use forward slashes as separators
	return filepath.Join(s.basePath, filepath.FromSlash(blockKey))
}

// WriteBlock writes a single block to the filesystem.
func (s *Store) WriteBlock(ctx context.Context, blockKey string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return store.ErrStoreClosed
	}

	path := s.blockPath(blockKey)

	// Create parent directories
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Write to a temporary file first, then rename for atomicity
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}

	// Atomic rename
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // Clean up on failure
		return err
	}

	return nil
}

// ReadBlock reads a complete block from the filesystem.
func (s *Store) ReadBlock(ctx context.Context, blockKey string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, store.ErrStoreClosed
	}

	path := s.blockPath(blockKey)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, store.ErrBlockNotFound
		}
		return nil, err
	}

	return data, nil
}

// ReadBlockRange reads a byte range from a block.
func (s *Store) ReadBlockRange(ctx context.Context, blockKey string, offset, length int64) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, store.ErrStoreClosed
	}

	path := s.blockPath(blockKey)

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, store.ErrBlockNotFound
		}
		return nil, err
	}
	defer f.Close()

	// Get file size for bounds checking
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	if offset < 0 || offset >= info.Size() {
		return nil, store.ErrBlockNotFound
	}

	// Calculate actual read length
	actualLength := length
	if offset+length > info.Size() {
		actualLength = info.Size() - offset
	}

	// Seek to offset
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}

	// Read the requested range
	data := make([]byte, actualLength)
	n, err := f.Read(data)
	if err != nil {
		return nil, err
	}

	return data[:n], nil
}

// DeleteBlock removes a single block from the filesystem.
func (s *Store) DeleteBlock(ctx context.Context, blockKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return store.ErrStoreClosed
	}

	path := s.blockPath(blockKey)
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Try to clean up empty parent directories
	s.cleanEmptyDirs(filepath.Dir(path))

	return nil
}

// cleanEmptyDirs removes empty directories up to the base path.
func (s *Store) cleanEmptyDirs(dir string) {
	for dir != s.basePath && strings.HasPrefix(dir, s.basePath) {
		err := os.Remove(dir)
		if err != nil {
			// Directory not empty or other error, stop
			break
		}
		dir = filepath.Dir(dir)
	}
}

// DeleteByPrefix removes all blocks with a given prefix.
func (s *Store) DeleteByPrefix(ctx context.Context, prefix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return store.ErrStoreClosed
	}

	prefixPath := s.blockPath(prefix)

	// Check if the prefix path exists
	info, err := os.Stat(prefixPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Nothing to delete
		}
		return err
	}

	// If it's a directory, remove the whole tree
	if info.IsDir() {
		if err := os.RemoveAll(prefixPath); err != nil {
			return err
		}
		// Clean up empty parent directories
		s.cleanEmptyDirs(filepath.Dir(prefixPath))
		return nil
	}

	// If it's a file, just remove it
	if err := os.Remove(prefixPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	s.cleanEmptyDirs(filepath.Dir(prefixPath))

	return nil
}

// ListByPrefix lists all block keys with a given prefix.
func (s *Store) ListByPrefix(ctx context.Context, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, store.ErrStoreClosed
	}

	prefixPath := s.blockPath(prefix)
	var keys []string

	// Check if the prefix path exists
	_, err := os.Stat(prefixPath)
	if err != nil {
		if os.IsNotExist(err) {
			return keys, nil // Empty list
		}
		return nil, err
	}

	// Walk the directory tree
	err = filepath.WalkDir(prefixPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil // Skip directories
		}

		// Skip temporary files
		if strings.HasSuffix(path, ".tmp") {
			return nil
		}

		// Convert path back to block key
		relPath, err := filepath.Rel(s.basePath, path)
		if err != nil {
			return err
		}
		// Convert to forward slashes for consistent key format
		key := filepath.ToSlash(relPath)
		keys = append(keys, key)
		return nil
	})

	if err != nil {
		return nil, err
	}

	// Sort for deterministic output
	sort.Strings(keys)
	return keys, nil
}

// Close marks the store as closed.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	return nil
}

// HealthCheck verifies the store is accessible and operational.
func (s *Store) HealthCheck(ctx context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return store.ErrStoreClosed
	}

	// Verify base path is accessible
	_, err := os.Stat(s.basePath)
	if err != nil {
		return err
	}

	return nil
}

// BasePath returns the base path of the store (for testing).
func (s *Store) BasePath() string {
	return s.basePath
}

// Ensure Store implements store.BlockStore.
var _ store.BlockStore = (*Store)(nil)
