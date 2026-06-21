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
	// LocalMemMax is retained for wire/JSON compatibility but is always 0:
	// the FSStore no longer tracks a configurable dirty-buffer memory budget
	// (the former MaxMemory knob was never enforced and was removed). The real
	// append-log pressure budget is AppendLogLimitBytes below.
	LocalMemMax int64 `json:"local_mem_max"`

	// AppendLogLimitBytes is the effective append-log pressure budget
	// (resolved max_log_bytes: per-store > global > deduced). AppendWrite
	// blocks once the on-disk append log exceeds this ceiling and ultimately
	// returns ErrPressureTimeout if the rollup cannot drain. This is the real
	// write-pressure ceiling that replaced the inert MaxMemory knob.
	AppendLogLimitBytes int64 `json:"append_log_limit_bytes"`

	ReadBufferEntries int   `json:"read_buffer_entries"`
	ReadBufferUsed    int64 `json:"read_buffer_used"`
	ReadBufferMax     int64 `json:"read_buffer_max"`

	HasRemote      bool `json:"has_remote"`
	PendingSyncs   int  `json:"pending_syncs"`
	PendingUploads int  `json:"pending_uploads"`
	CompletedSyncs int  `json:"completed_syncs"`
	FailedSyncs    int  `json:"failed_syncs"`

	// UnsyncedBytes is the running on-disk size of CAS chunks present locally
	// but not yet mirrored to the remote (the #1136 backpressure signal). It is
	// the headline data-at-risk gauge: bytes that would be lost if local
	// storage were lost before the syncer drains them.
	UnsyncedBytes int64 `json:"unsynced_bytes"`

	RemoteHealthy       bool    `json:"remote_healthy"`
	EvictionSuspended   bool    `json:"eviction_suspended"`
	OutageDurationSecs  float64 `json:"outage_duration_seconds"`
	OfflineReadsBlocked int64   `json:"offline_reads_blocked"`

	// LocalDurable / RemoteDurable expose the effective per-store durability
	// (#1274): the type default (fs/s3 → true, memory → false) unless the
	// operator overrode it via config["durable"]. They drive the honest
	// CLOSE/COMMIT contract — a payload is committed iff
	// LocalDurable || (Finalized && RemoteDurable). RemoteDurable is always
	// false when HasRemote is false. Surfaced so operators can confirm whether
	// CLOSE/COMMIT acks are crash-safe for a given share.
	LocalDurable  bool `json:"local_durable"`
	RemoteDurable bool `json:"remote_durable"`
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

// GetStats returns comprehensive block store statistics, including the
// per-block-state counts. Those counts require a per-file walk of the file
// block store (DB I/O), so callers that do not need them should use
// GetStatsLite.
func (bs *Store) GetStats() BlockStoreStats { return bs.getStats(true) }

// GetStatsLite returns the same statistics as GetStats but skips the
// per-block-state counts (BlocksDirty/Local/Remote/Total) and the per-file
// file-block-store walk that computes them. It is O(1)-ish and safe to call on
// a hot path such as a metrics scrape.
func (bs *Store) GetStatsLite() BlockStoreStats { return bs.getStats(false) }

// getStats builds the statistics snapshot. When withBlockCounts is true it also
// walks the file block store to fill the per-block-state counts.
func (bs *Store) getStats(withBlockCounts bool) BlockStoreStats {
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

	cacheStats := bs.loadCache().Stats()

	pending, completed, failed := bs.syncer.Queue().Stats()
	_, uploads, _ := bs.syncer.Queue().PendingByType()

	remoteHealthy := bs.syncer.IsRemoteHealthy()
	outageDuration := bs.syncer.RemoteOutageDuration()

	stats := BlockStoreStats{
		FileCount:           len(files),
		LocalDiskUsed:       localStats.DiskUsed,
		LocalDiskMax:        localStats.MaxDisk,
		LocalMemUsed:        localStats.MemUsed,
		LocalMemMax:         0, // retained for compatibility; FSStore no longer tracks a mem budget
		AppendLogLimitBytes: localStats.MaxLogBytes,

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
		UnsyncedBytes:       bs.syncer.UnsyncedBytes(),
		LocalDurable:        bs.LocalDurable(),
		RemoteDurable:       bs.RemoteDurable(),
	}

	if withBlockCounts {
		bs.populateBlockCounts(&stats, files)
	}

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
				// A CAS-pending block with a non-zero hash is locally present in the
				// CAS store; a zero hash means the block is truly dirty/in-flight
				// (rollup not yet complete). LocalPath and BlockStoreKey are legacy
				// signals irrelevant on the CAS path.
				if !b.Hash.IsZero() {
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
