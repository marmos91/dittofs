package badger

import (
	"context"
	"fmt"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/internal/logger"
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
			c := s.loadCapabilities()
			caps = &c
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
	s.storeCapabilities(capabilities)

	err := s.db.Update(func(txn *badgerdb.Txn) error {
		data, err := encodeFilesystemCapabilities(&capabilities)
		if err != nil {
			return err
		}
		return txn.Set(keyFilesystemCapabilities(), data)
	})
	if err != nil {
		logger.Error("badger: failed to persist filesystem capabilities", "error", err)
	}
}

// loadCapabilities returns a copy of the in-memory capabilities under a read lock.
func (s *BadgerMetadataStore) loadCapabilities() metadata.FilesystemCapabilities {
	s.capsMu.RLock()
	defer s.capsMu.RUnlock()
	return s.capabilities
}

// storeCapabilities replaces the in-memory capabilities under a write lock.
func (s *BadgerMetadataStore) storeCapabilities(capabilities metadata.FilesystemCapabilities) {
	s.capsMu.Lock()
	defer s.capsMu.Unlock()
	s.capabilities = capabilities
}

// ============================================================================
// Filesystem Statistics
// ============================================================================

// GetFilesystemStatistics returns dynamic filesystem statistics for the share
// the handle belongs to.
//
// The store instance is shared by every share naming the same metadata store
// config, so usage is scoped to that share; the capacity ceilings
// (maxStorageBytes / maxFiles) are store-wide configuration.
//
// Both figures are O(1) reads of the per-share usage bucket. Only regular files
// contribute, matching the SQL backends' scoped aggregate: directories carry no
// logical bytes and the share root would otherwise inflate UsedFiles.
func (s *BadgerMetadataStore) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		// An undecodable handle names no share; its usage buckets are empty,
		// the same answer a share with no files gives.
		shareName = ""
	}

	s.quotaMu.Lock()
	usage := s.quota.Share(shareName)
	s.quotaMu.Unlock()

	usedSize := uint64(max(usage.Bytes, 0))
	fileCount := uint64(max(usage.Files, 0))

	stats := buildShareStatistics(usedSize, fileCount, s.maxStorageBytes, s.maxFiles)
	return &stats, nil
}

// buildShareStatistics assembles the reported statistics from a share's usage
// and the store-wide capacity ceilings, substituting "effectively unlimited"
// sentinels for an unconfigured ceiling. Shared by the pool and transaction
// paths so both report the same shape.
func buildShareStatistics(usedSize, fileCount, maxStorageBytes, maxFiles uint64) metadata.FilesystemStatistics {
	const (
		reportedSize  = uint64(1) << 50 // 1 PiB (unlimited sentinel)
		reportedFiles = uint64(1000000)
	)
	if maxStorageBytes == 0 {
		maxStorageBytes = reportedSize
	}
	if maxFiles == 0 {
		maxFiles = reportedFiles
	}

	stats := metadata.FilesystemStatistics{
		UsedBytes:  usedSize,
		UsedFiles:  fileCount,
		TotalBytes: maxStorageBytes,
		TotalFiles: maxFiles,
	}
	if maxStorageBytes > usedSize {
		stats.AvailableBytes = maxStorageBytes - usedSize
	}
	if maxFiles > fileCount {
		stats.AvailableFiles = maxFiles - fileCount
	}
	return stats
}
