package badger

import (
	"context"
	"fmt"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Server Configuration
// ============================================================================

// SetServerConfig sets the server-wide configuration.
func (s *BadgerMetadataStore) SetServerConfig(ctx context.Context, config metadata.MetadataServerConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
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
func (s *BadgerMetadataStore) GetServerConfig(ctx context.Context) (metadata.MetadataServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return metadata.MetadataServerConfig{}, err
	}

	var config metadata.MetadataServerConfig

	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyServerConfig())
		if err == badgerdb.ErrKeyNotFound {
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

// ============================================================================
// Filesystem Capabilities
// ============================================================================

// GetFilesystemCapabilities returns static filesystem capabilities and limits.
func (s *BadgerMetadataStore) GetFilesystemCapabilities(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var caps *metadata.FilesystemCapabilities

	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyFilesystemCapabilities())
		if err == badgerdb.ErrKeyNotFound {
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

// SetFilesystemCapabilities updates the filesystem capabilities for this store.
func (s *BadgerMetadataStore) SetFilesystemCapabilities(capabilities metadata.FilesystemCapabilities) {
	s.capabilities = capabilities

	_ = s.db.Update(func(txn *badgerdb.Txn) error {
		data, err := encodeFilesystemCapabilities(&capabilities)
		if err != nil {
			return err
		}
		return txn.Set(keyFilesystemCapabilities(), data)
	})
}

// ============================================================================
// Filesystem Statistics
// ============================================================================

// GetFilesystemStatistics returns dynamic filesystem statistics.
func (s *BadgerMetadataStore) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Check cache first
	s.statsCache.mu.RLock()
	if s.statsCache.hasStats && time.Since(s.statsCache.timestamp) < s.statsCache.ttl {
		cached := s.statsCache.stats
		s.statsCache.mu.RUnlock()
		return &cached, nil
	}
	s.statsCache.mu.RUnlock()

	// Cache miss - compute stats
	var stats metadata.FilesystemStatistics
	var fileCount uint64
	var usedSize uint64

	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = []byte(prefixFile)
		opts.PrefetchValues = true

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			if fileCount%100 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}

			item := it.Item()
			err := item.Value(func(val []byte) error {
				file, err := decodeFile(val)
				if err != nil {
					return err
				}
				fileCount++
				usedSize += file.Size
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

	stats.UsedFiles = fileCount
	stats.UsedBytes = usedSize

	if s.maxStorageBytes > 0 {
		stats.TotalBytes = s.maxStorageBytes
		if usedSize < s.maxStorageBytes {
			stats.AvailableBytes = s.maxStorageBytes - usedSize
		} else {
			stats.AvailableBytes = 0
		}
	} else {
		const reportedSize = 1024 * 1024 * 1024 * 1024 // 1TB
		stats.TotalBytes = reportedSize
		stats.AvailableBytes = reportedSize - usedSize
	}

	if s.maxFiles > 0 {
		stats.TotalFiles = s.maxFiles
		if fileCount < s.maxFiles {
			stats.AvailableFiles = s.maxFiles - fileCount
		} else {
			stats.AvailableFiles = 0
		}
	} else {
		const reportedFiles = 1000000
		stats.TotalFiles = reportedFiles
		stats.AvailableFiles = reportedFiles - fileCount
	}

	// Update cache
	s.statsCache.mu.Lock()
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
