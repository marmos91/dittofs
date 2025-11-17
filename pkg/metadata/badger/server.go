package badger

import (
	"context"
	"fmt"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// SetServerConfig sets the server-wide configuration.
//
// This stores global server settings that apply across all shares and operations.
// The configuration is stored as a singleton in the database using the key "cfg:server".
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Context for cancellation
//   - config: The server configuration to apply
//
// Returns:
//   - error: Only context cancellation errors or database errors
func (s *BadgerMetadataStore) SetServerConfig(ctx context.Context, config metadata.MetadataServerConfig) error {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(txn *badger.Txn) error {
		configBytes, err := encodeServerConfig(&config)
		if err != nil {
			return err
		}

		if err := txn.Set(keyServerConfig(), configBytes); err != nil {
			return fmt.Errorf("failed to store server config: %w", err)
		}

		return nil
	})
}

// GetServerConfig returns the current server configuration.
//
// This retrieves the global server settings from the database. If no configuration
// has been set, it returns an empty configuration with initialized CustomSettings map.
//
// Thread Safety: Safe for concurrent use.
//
// Returns:
//   - MetadataServerConfig: Current server configuration
//   - error: Only context cancellation errors or database errors
func (s *BadgerMetadataStore) GetServerConfig(ctx context.Context) (metadata.MetadataServerConfig, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return metadata.MetadataServerConfig{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var config metadata.MetadataServerConfig

	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyServerConfig())
		if err == badger.ErrKeyNotFound {
			// Return default empty config if not found
			config = metadata.MetadataServerConfig{
				CustomSettings: make(map[string]any),
			}
			return nil
		}
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			cfg, err := decodeServerConfig(val)
			if err != nil {
				return err
			}
			config = *cfg
			return nil
		})
	})

	if err != nil {
		return metadata.MetadataServerConfig{}, fmt.Errorf("failed to get server config: %w", err)
	}

	return config, nil
}

// Healthcheck verifies the repository is operational.
//
// This performs a health check to ensure BadgerDB is accessible and can serve
// requests. The check:
//   - Attempts to start a read transaction
//   - Verifies the database is not closed
//   - Checks context cancellation
//
// For BadgerDB, this is a lightweight operation that doesn't perform extensive
// checks, as BadgerDB handles most error conditions internally.
//
// Use Cases:
//   - Liveness probes in container orchestration
//   - Load balancer health checks
//   - Monitoring and alerting systems
//   - Protocol NULL/ping procedures
//
// Thread Safety: Safe for concurrent use.
//
// Returns:
//   - error: Returns error if repository is unhealthy, nil if healthy
func (s *BadgerMetadataStore) Healthcheck(ctx context.Context) error {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Attempt a simple read transaction to verify database is accessible
	err := s.db.View(func(txn *badger.Txn) error {
		// Just verify we can start a transaction
		// BadgerDB will return an error if it's closed or corrupted
		return nil
	})

	if err != nil {
		return fmt.Errorf("healthcheck failed: %w", err)
	}

	return nil
}

// GetFilesystemCapabilities returns static filesystem capabilities and limits.
//
// This provides information about what the filesystem supports and its limits.
// The capabilities are stored in the database and initialized when the store
// is created.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: A file handle within the filesystem to query (currently unused,
//     as we have single global capabilities)
//
// Returns:
//   - *FilesystemCapabilities: Static filesystem capabilities and limits
//   - error: Only context cancellation errors or database errors
func (s *BadgerMetadataStore) GetFilesystemCapabilities(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemCapabilities, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var caps *metadata.FilesystemCapabilities

	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyFilesystemCapabilities())
		if err == badger.ErrKeyNotFound {
			// Return in-memory capabilities if not found in DB
			// (should not happen if initialization succeeded)
			caps = &s.capabilities
			return nil
		}
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			c, err := decodeFilesystemCapabilities(val)
			if err != nil {
				return err
			}
			caps = c
			return nil
		})
	})

	if err != nil {
		return nil, fmt.Errorf("failed to get filesystem capabilities: %w", err)
	}

	return caps, nil
}

// GetFilesystemStatistics returns dynamic filesystem statistics.
//
// This provides current information about filesystem usage and availability.
// For BadgerDB-backed storage, we calculate statistics based on:
//   - Total files: Count of entries in the file metadata
//   - Used space: Sum of file sizes from metadata
//   - Available space: Based on disk space (if maxStorageBytes is set)
//
// Note: This operation scans all file entries to calculate statistics, which
// may be slow for very large filesystems (>1M files). Consider caching results
// with a TTL for production use.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: A file handle within the filesystem to query
//
// Returns:
//   - *FilesystemStatistics: Dynamic filesystem usage statistics
//   - error: Context cancellation errors or database errors
func (s *BadgerMetadataStore) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ========================================================================
	// Step 1: Check cache first (fast path)
	// ========================================================================
	s.statsCache.mu.RLock()
	if s.statsCache.hasStats && time.Since(s.statsCache.timestamp) < s.statsCache.ttl {
		cached := s.statsCache.stats
		s.statsCache.mu.RUnlock()
		return &cached, nil
	}
	s.statsCache.mu.RUnlock()

	// ========================================================================
	// Step 2: Cache miss or expired - compute stats (slow path)
	// ========================================================================

	s.mu.RLock()
	defer s.mu.RUnlock()

	var stats metadata.FilesystemStatistics
	var fileCount uint64
	var usedSize uint64

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(prefixFile)
		opts.PrefetchValues = true

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			// Check context periodically (every 100 files)
			if fileCount%100 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}

			item := it.Item()
			err := item.Value(func(val []byte) error {
				fileData, err := decodeFileData(val)
				if err != nil {
					return err
				}

				fileCount++
				usedSize += fileData.Attr.Size
				return nil
			})
			if err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to calculate filesystem statistics: %w", err)
	}

	// Calculate statistics
	stats.UsedFiles = fileCount
	stats.UsedBytes = usedSize

	// Total and available depend on maxStorageBytes setting
	if s.maxStorageBytes > 0 {
		stats.TotalBytes = s.maxStorageBytes
		if usedSize < s.maxStorageBytes {
			stats.AvailableBytes = s.maxStorageBytes - usedSize
		} else {
			stats.AvailableBytes = 0
		}
	} else {
		// Unlimited - report large values
		// In production, you'd query actual disk space here
		const reportedSize = 1024 * 1024 * 1024 * 1024 // 1TB
		stats.TotalBytes = reportedSize
		stats.AvailableBytes = reportedSize - usedSize
	}

	// Available files
	if s.maxFiles > 0 {
		stats.TotalFiles = s.maxFiles
		if fileCount < s.maxFiles {
			stats.AvailableFiles = s.maxFiles - fileCount
		} else {
			stats.AvailableFiles = 0
		}
	} else {
		// Unlimited - report 1 million
		const reportedFiles = 1000000
		stats.TotalFiles = reportedFiles
		stats.AvailableFiles = reportedFiles - fileCount
	}

	// ========================================================================
	// Step 3: Update cache with fresh stats
	// ========================================================================
	s.statsCache.mu.Lock()
	// Double-check if another goroutine updated the cache while we were computing
	if s.statsCache.hasStats && time.Since(s.statsCache.timestamp) < s.statsCache.ttl {
		cached := s.statsCache.stats
		s.statsCache.mu.Unlock()
		return &cached, nil
	}
	s.statsCache.stats = stats
	s.statsCache.hasStats = true
	s.statsCache.timestamp = time.Now()
	s.statsCache.mu.Unlock()

	return &stats, nil
}
