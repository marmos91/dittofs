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
	// BlocksCached is the subset of BlocksLocal that is physically present on
	// local disk but whose metadata row still reads BlockStateRemote — i.e.
	// blocks populated by the read-through cache after a remote fetch. It lets
	// operators see how much of the local tier is read-cached vs write-side
	// (#1362).
	BlocksCached int `json:"blocks_cached"`

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

	cacheStats := bs.loadCache().Stats()

	// Source sync stats from the live carve path (pending-carve set +
	// completed/failed counters), NOT the vestigial SyncQueue, whose Stats()
	// always read zero because it has no production upload callers (#1266).
	// The dead-queue numbers made operators believe nothing synced even when
	// uploads provably succeeded (#1405 diagnosis).
	//
	// In local-only mode (no remote) these counters are meaningless: addPendingHash
	// still records rolled-up chunks but nothing ever carves them, so a nonzero
	// PendingUploads would falsely imply an upload backlog. Report zeros when there
	// is no remote — there is nothing to sync.
	var pendingUploads, completed, failed int
	if bs.remote != nil {
		pendingUploads = bs.syncer.PendingCount()
		completed, failed = bs.syncer.SyncCounts()
	}

	remoteHealthy := bs.syncer.IsRemoteHealthy()
	outageDuration := bs.syncer.RemoteOutageDuration()

	stats := BlockStoreStats{
		// FileCount is the cheap local-only count here; the full-stats path
		// (withBlockCounts) overwrites it with the authoritative distinct-payload
		// count from the metadata so it reflects rolled-up files too (#1374).
		FileCount:           len(bs.local.ListFiles()),
		LocalDiskUsed:       localStats.DiskUsed,
		LocalDiskMax:        localStats.MaxDisk,
		LocalMemUsed:        localStats.MemUsed,
		LocalMemMax:         0, // retained for compatibility; FSStore no longer tracks a mem budget
		AppendLogLimitBytes: localStats.MaxLogBytes,

		ReadBufferEntries:   cacheStats.Entries,
		ReadBufferUsed:      cacheStats.CurBytes,
		ReadBufferMax:       cacheStats.MaxBytes,
		HasRemote:           bs.remote != nil,
		PendingSyncs:        pendingUploads,
		PendingUploads:      pendingUploads,
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
		bs.populateBlockCounts(&stats)
	}

	return stats
}

// populateBlockCounts fills block count fields and FileCount from the
// authoritative metadata. It enumerates payloads via
// fileChunkStore.EnumeratePayloads (which survives rollup) rather than the
// local store's ListFiles, so a rolled-up share whose append logs are gone
// still reports its blocks and files instead of zero (#1374).
func (bs *Store) populateBlockCounts(stats *BlockStoreStats) {
	if bs.fileChunkStore == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var payloadCount int
	if err := bs.fileChunkStore.EnumeratePayloads(ctx, func(payloadID string) error {
		payloadCount++
		blocks, err := bs.fileChunkStore.ListFileChunks(ctx, payloadID)
		if err != nil {
			return nil // skip this payload, keep going
		}
		bs.classifyBlocks(ctx, stats, blocks)
		return nil
	}); err != nil {
		return
	}
	// Authoritative distinct-payload count (reflects rolled-up files too).
	stats.FileCount = payloadCount
}

// classifyBlocks tallies a payload's FileChunk rows into the per-state stats
// counters. Split out of populateBlockCounts so the EnumeratePayloads callback
// stays readable; the classification logic is unchanged.
func (bs *Store) classifyBlocks(ctx context.Context, stats *BlockStoreStats, blocks []*block.FileChunk) {
	for _, b := range blocks {
		stats.BlocksTotal++
		// Classify by PHYSICAL presence in the local CAS store, not by sync
		// state. A block fetched from remote keeps a BlockStateRemote row
		// even though its CAS chunk now sits on local disk; classifying by
		// state alone reported it as remote, so a fully read-cached share
		// showed "Blocks Local: 0" while du showed the cache was full
		// (#1362). A zero hash means the block is genuinely dirty/in-flight
		// (rollup not yet complete) and cannot be present.
		if !b.Hash.IsZero() {
			if present, herr := bs.local.Has(ctx, b.Hash); herr == nil && present {
				stats.BlocksLocal++
				if b.State == block.BlockStateRemote {
					// On disk yet the row says remote: read-through-cached.
					stats.BlocksCached++
				}
				continue
			}
		}
		// Not physically present (or zero hash): fall back to sync state.
		switch b.State {
		case block.BlockStatePending:
			if b.Hash.IsZero() {
				stats.BlocksDirty++
			} else {
				// Rollup-committed but absent from disk (evicted): the
				// remote copy is authoritative; a read will refetch it.
				stats.BlocksRemote++
			}
		case block.BlockStateSyncing:
			stats.BlocksRemote++
		case block.BlockStateRemote:
			stats.BlocksRemote++
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
