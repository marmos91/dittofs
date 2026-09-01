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
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetServerConfig(ctx, config)
	})
}

// GetServerConfig returns the current server configuration.
func (s *BadgerMetadataStore) GetServerConfig(ctx context.Context) (metadata.MetadataServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return metadata.MetadataServerConfig{}, err
	}

	var config metadata.MetadataServerConfig
	err := s.db.View(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		var err error
		config, err = tx.GetServerConfig(ctx)
		return err
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
		tx := &badgerTransaction{store: s, txn: txn}
		var err error
		caps, err = tx.GetFilesystemCapabilities(ctx, handle)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get filesystem capabilities: %w", err)
	}
	return caps, nil
}

// SetFilesystemCapabilities updates the filesystem capabilities for this store.
//
// It delegates to the transaction path so the in-memory copy is staged and
// applied only after a successful commit: updating it here before the write
// would leave GetFilesystemMeta advertising capabilities that never reached
// the store. The interface is void, so a failure can only be logged.
func (s *BadgerMetadataStore) SetFilesystemCapabilities(capabilities metadata.FilesystemCapabilities) {
	err := s.WithTransaction(context.Background(), func(tx metadata.Transaction) error {
		return tx.(*badgerTransaction).writeCapabilities(capabilities)
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
// This one deliberately does NOT delegate to the transaction path the way the
// rest of this file does. The transaction path scans every file key so a statfs
// issued inside a transaction sees that transaction's own uncommitted writes;
// outside a transaction there is nothing uncommitted to see, so this reads the
// O(1) per-share usage bucket instead. Delegating would turn every statfs into
// a full keyspace scan.
//
// The store instance is shared by every share naming the same metadata store
// config, so usage is scoped to that share; the capacity ceilings
// (maxStorageBytes / maxFiles) are store-wide configuration.
//
// Both figures are O(1) reads of the per-share usage bucket. Only regular files
// contribute, matching the SQL backends' scoped aggregate: directories carry no
// logical bytes and the share root would otherwise inflate UsedFiles.
//
// A handle that does not decode names no share and is rejected.
func (s *BadgerMetadataStore) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, err
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
