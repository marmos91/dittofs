package engine

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local"
)

// BlockStoreStats holds comprehensive block store statistics for a Store.
type BlockStoreStats struct {
	FileCount    int `json:"file_count"`
	BlocksDirty  int `json:"blocks_dirty"`
	BlocksLocal  int `json:"blocks_local"`
	BlocksRemote int `json:"blocks_remote"`
	BlocksTotal  int `json:"blocks_total"`

	LocalDiskUsed int64 `json:"local_disk_used"`
	LocalDiskMax  int64 `json:"local_disk_max"`
	LocalMemUsed  int64 `json:"local_mem_used"`
	LocalMemMax   int64 `json:"local_mem_max"`

	ReadBufferEntries int   `json:"read_buffer_entries"`
	ReadBufferUsed    int64 `json:"read_buffer_used"`
	ReadBufferMax     int64 `json:"read_buffer_max"`

	HasRemote      bool `json:"has_remote"`
	PendingSyncs   int  `json:"pending_syncs"`
	PendingUploads int  `json:"pending_uploads"`
	CompletedSyncs int  `json:"completed_syncs"`
	FailedSyncs    int  `json:"failed_syncs"`

	RemoteHealthy       bool    `json:"remote_healthy"`
	EvictionSuspended   bool    `json:"eviction_suspended"`
	OutageDurationSecs  float64 `json:"outage_duration_seconds"`
	OfflineReadsBlocked int64   `json:"offline_reads_blocked"`
}

// Stats returns storage statistics from the local store.
func (bs *Store) Stats() (*block.Stats, error) {
	if err := bs.enter(); err != nil {
		return nil, err
	}
	defer bs.closeMu.RUnlock()
	localStats := bs.local.Stats()
	files := bs.local.ListFiles()
	used := uint64(localStats.DiskUsed)
	total := uint64(localStats.MaxDisk)
	avail := uint64(0)
	if total > used {
		avail = total - used
	}
	count := uint64(len(files))
	avg := uint64(0)
	if count > 0 {
		avg = used / count
	}
	return &block.Stats{
		UsedSize:      used,
		ContentCount:  count,
		TotalSize:     total,
		AvailableSize: avail,
		AverageSize:   avg,
	}, nil
}

// GetStats returns comprehensive block store statistics.
func (bs *Store) GetStats() BlockStoreStats {
	// Pin against Close teardown. This method has no error return, so a
	// closed store reports empty stats rather than racing the
	// local/syncer/cache teardown Close performs under closeMu.Lock.
	bs.closeMu.RLock()
	defer bs.closeMu.RUnlock()
	if bs.closed {
		return BlockStoreStats{}
	}

	localStats := bs.local.Stats()
	files := bs.local.ListFiles()

	cacheStats := bs.cache.Stats()

	pending, completed, failed := bs.syncer.Queue().Stats()
	_, uploads, _ := bs.syncer.Queue().PendingByType()

	remoteHealthy := bs.syncer.IsRemoteHealthy()
	outageDuration := bs.syncer.RemoteOutageDuration()

	stats := BlockStoreStats{
		FileCount:           len(files),
		LocalDiskUsed:       localStats.DiskUsed,
		LocalDiskMax:        localStats.MaxDisk,
		LocalMemUsed:        localStats.MemUsed,
		LocalMemMax:         localStats.MaxMemory,
		ReadBufferEntries:   cacheStats.Entries,
		ReadBufferUsed:      cacheStats.CurBytes,
		ReadBufferMax:       cacheStats.MaxBytes,
		HasRemote:           bs.remote != nil,
		PendingSyncs:        pending,
		PendingUploads:      uploads,
		CompletedSyncs:      completed,
		FailedSyncs:         failed,
		RemoteHealthy:       remoteHealthy,
		EvictionSuspended:   bs.remote != nil && !remoteHealthy,
		OutageDurationSecs:  outageDuration.Seconds(),
		OfflineReadsBlocked: bs.syncer.OfflineReadsBlocked(),
	}

	bs.populateBlockCounts(&stats, files)

	return stats
}

// populateBlockCounts fills block count fields from the metadata store.
func (bs *Store) populateBlockCounts(stats *BlockStoreStats, files []string) {
	if bs.fileBlockStore == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, payloadID := range files {
		blocks, err := bs.fileBlockStore.ListFileBlocks(ctx, payloadID)
		if err != nil {
			continue
		}
		for _, b := range blocks {
			stats.BlocksTotal++
			switch b.State {
			case block.BlockStatePending:
				// Pending now covers both legacy Dirty (no LocalPath / no key)
				// and Local (complete, awaiting sync). Distinguish by data
				// state to keep the existing introspection counters meaningful.
				if b.LocalPath != "" || b.BlockStoreKey != "" {
					stats.BlocksLocal++
				} else {
					stats.BlocksDirty++
				}
			case block.BlockStateSyncing:
				stats.BlocksLocal++
			case block.BlockStateRemote:
				stats.BlocksRemote++
			}
		}
	}
}

// LocalStats returns a snapshot of local store statistics. A closed store
// reports empty stats rather than racing the local teardown Close performs
// under closeMu.Lock (no error return to surface ErrStoreClosed).
func (bs *Store) LocalStats() local.Stats {
	bs.closeMu.RLock()
	defer bs.closeMu.RUnlock()
	if bs.closed {
		return local.Stats{}
	}
	return bs.local.Stats()
}
