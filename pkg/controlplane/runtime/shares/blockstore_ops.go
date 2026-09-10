package shares

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// GetBlockStoreForHandle decodes a file handle and resolves the per-share
// BlockStore. The handle is decoded once for its share name, then the store is
// read from the lock-free blockStoreCache — the read/write hot path never
// touches s.mu.
func (s *Service) GetBlockStoreForHandle(_ context.Context, handle metadata.FileHandle) (*engine.Store, error) {
	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, fmt.Errorf("failed to decode file handle: %w", err)
	}
	return s.GetBlockStoreForShare(shareName)
}

// GetBlockStoreForShare returns the BlockStore for a named share. The common
// case is served from the lock-free blockStoreCache; a miss falls through to
// the locked registry, which produces the exact not-found / no-store errors.
// Because the cache is written under s.mu alongside the registry, a miss after
// RemoveShare always resolves to a not-found error — never a stale or closed
// store.
func (s *Service) GetBlockStoreForShare(name string) (*engine.Store, error) {
	if v, ok := s.blockStoreCache.Load(name); ok {
		if bs, ok := v.(*engine.Store); ok {
			return bs, nil
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	share, exists := s.registry[name]
	if !exists {
		return nil, fmt.Errorf("share %q not found", name)
	}
	if share.BlockStore == nil {
		return nil, fmt.Errorf("share %q has no block store configured", name)
	}
	return share.BlockStore, nil
}

// RemoteStoreEntry describes a distinct remote block store that is referenced
// by one or more shares. Surface used by production block-GC enumeration
// (Runtime.RunBlockGC): we want each underlying remote store scanned exactly
// once per run, not once per share.
type RemoteStoreEntry struct {
	// Store is the underlying remote store (NOT the nonClosingRemote wrapper).
	Store remote.RemoteStore
	// ConfigID is the remote block-store config UUID this entry represents.
	// Empty string indicates a test-only binding (SetShareRemoteForTest).
	ConfigID string
	// Shares are the registered share names that reference this remote.
	Shares []string
}

// DistinctRemoteStores returns every distinct underlying remote.RemoteStore
// referenced by at least one registered share. Shares that reference the same
// remote-store config (ref-counted via remoteStores) are grouped into a
// single entry — deduped by ConfigID, NOT by the per-share nonClosingRemote
// wrapper pointer. Local-only shares (no remote) contribute nothing.
//
// Returned entries have a non-nil Store and a non-empty Shares slice. Order
// is unspecified (map iteration).
func (s *Service) DistinctRemoteStores() []RemoteStoreEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Bucket share names by the configID they reference. configID == "" means
	// "local-only share" — skipped entirely.
	sharesByConfigID := make(map[string][]string, len(s.remoteStores))
	for name, sh := range s.registry {
		if sh.remoteConfigID == "" {
			continue
		}
		sharesByConfigID[sh.remoteConfigID] = append(sharesByConfigID[sh.remoteConfigID], name)
	}

	out := make([]RemoteStoreEntry, 0, len(sharesByConfigID))
	for cid, shareNames := range sharesByConfigID {
		sr, ok := s.remoteStores[cid]
		if !ok || sr == nil || sr.store == nil {
			// Orphaned configID → skip. DistinctRemoteStores is a read-only
			// surface; we don't try to self-heal bookkeeping here.
			continue
		}
		out = append(out, RemoteStoreEntry{
			Store:    sr.store,
			ConfigID: cid,
			Shares:   shareNames,
		})
	}
	return out
}

// DrainAllBlockStores drains all pending uploads across all per-share BlockStores.
func (s *Service) DrainAllBlockStores(ctx context.Context) error {
	s.mu.RLock()
	blockStores := make([]*engine.Store, 0, len(s.registry))
	for _, share := range s.registry {
		if share.BlockStore != nil {
			blockStores = append(blockStores, share.BlockStore)
		}
	}
	s.mu.RUnlock()

	var errs []error
	for _, bs := range blockStores {
		if err := bs.DrainAllUploads(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// UploadProgress returns a monotonic count of mirror attempts that have
// concluded (completed + failed) summed across every share's block store.
//
// It is a liveness signal for the drain-uploads idle watchdog: while a drain
// is genuinely making progress this value keeps rising; a value that stays
// flat across an idle window means the drain has stalled (e.g. the remote went
// unreachable). Counting failed attempts as well as completed ones means a
// retrying upload still registers as activity, so transient errors don't trip
// the watchdog.
func (s *Service) UploadProgress() int64 {
	s.mu.RLock()
	blockStores := make([]*engine.Store, 0, len(s.registry))
	for _, share := range s.registry {
		if share.BlockStore != nil {
			blockStores = append(blockStores, share.BlockStore)
		}
	}
	s.mu.RUnlock()

	var total int64
	for _, bs := range blockStores {
		completed, failed := bs.SyncCounts()
		total += int64(completed) + int64(failed)
	}
	return total
}

// UnsyncedBytes returns the local bytes not yet mirrored to a remote, summed
// across every share's block store.
//
// The drain-uploads watchdog reads it to tell a stalled drain from one whose
// uploads are simply finished: with nothing unsynced left, no upload attempt
// CAN conclude, so a flat UploadProgress is the expected reading rather than
// evidence the remote has wedged. Uses the lite stats — the per-block-state
// walk the full snapshot performs is a whole-manifest scan, far too expensive
// for a supervisor.
func (s *Service) UnsyncedBytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var total int64
	for _, share := range s.registry {
		if share.BlockStore != nil {
			total += share.BlockStore.GetStatsLite().UnsyncedBytes
		}
	}
	return total
}

// ShareBlockStoreStats holds block store statistics for a single share.
type ShareBlockStoreStats struct {
	ShareName string                 `json:"share_name"`
	Stats     engine.BlockStoreStats `json:"stats"`
}

// BlockStoreStatsResponse holds aggregated and per-share block store statistics.
type BlockStoreStatsResponse struct {
	Totals   engine.BlockStoreStats `json:"totals"`
	PerShare []ShareBlockStoreStats `json:"per_share,omitempty"`
}

// EvictOptions controls which block store tiers to evict.
type EvictOptions struct {
	ReadBufferOnly bool `json:"read_buffer_only"`
	LocalOnly      bool `json:"local_only"`
}

// EvictResult holds the result of a block store eviction operation.
type EvictResult struct {
	ReadBufferEntriesCleared int   `json:"read_buffer_entries_cleared"`
	LocalFilesEvicted        int   `json:"local_files_evicted"`
	BytesFreed               int64 `json:"bytes_freed"`
}

// MetricsBlockStats returns per-share block-store stats for observability. It
// uses the lite stats path (no per-file block-count DB walk) so a metrics
// scrape never pays for the per-block-state counts, which are not exported.
func (s *Service) MetricsBlockStats() []ShareBlockStoreStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []ShareBlockStoreStats
	for name, share := range s.registry {
		if share.BlockStore == nil {
			continue
		}
		out = append(out, ShareBlockStoreStats{ShareName: name, Stats: share.BlockStore.GetStatsLite()})
	}
	return out
}

// GetBlockStoreStats returns block store statistics, optionally filtered by share name.
// If shareName is empty, returns aggregated stats across all shares with per-share breakdown.
func (s *Service) GetBlockStoreStats(shareName string) (*BlockStoreStatsResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if shareName != "" {
		share, exists := s.registry[shareName]
		if !exists {
			return nil, fmt.Errorf("share %q not found", shareName)
		}
		if share.BlockStore == nil {
			return nil, fmt.Errorf("share %q has no block store configured", shareName)
		}
		stats := share.BlockStore.GetStats()
		return &BlockStoreStatsResponse{
			Totals:   stats,
			PerShare: []ShareBlockStoreStats{{ShareName: shareName, Stats: stats}},
		}, nil
	}

	var totals engine.BlockStoreStats
	var perShare []ShareBlockStoreStats

	for name, share := range s.registry {
		if share.BlockStore == nil {
			continue
		}
		stats := share.BlockStore.GetStats()
		perShare = append(perShare, ShareBlockStoreStats{ShareName: name, Stats: stats})
		addBlockStoreStats(&totals, stats)
	}

	return &BlockStoreStatsResponse{
		Totals:   totals,
		PerShare: perShare,
	}, nil
}

// addBlockStoreStats accumulates src into dst (field-by-field summation).
func addBlockStoreStats(dst *engine.BlockStoreStats, src engine.BlockStoreStats) {
	dst.FileCount += src.FileCount
	dst.BlocksDirty += src.BlocksDirty
	dst.BlocksLocal += src.BlocksLocal
	dst.BlocksRemote += src.BlocksRemote
	dst.BlocksTotal += src.BlocksTotal
	dst.BlocksCached += src.BlocksCached
	dst.LocalDiskUsed += src.LocalDiskUsed
	dst.LocalDiskMax += src.LocalDiskMax
	dst.LocalMemUsed += src.LocalMemUsed
	dst.LocalMemMax += src.LocalMemMax
	dst.AppendLogLimitBytes += src.AppendLogLimitBytes
	dst.ReadBufferEntries += src.ReadBufferEntries
	dst.ReadBufferUsed += src.ReadBufferUsed
	dst.ReadBufferMax += src.ReadBufferMax
	dst.PendingSyncs += src.PendingSyncs
	dst.PendingUploads += src.PendingUploads
	dst.CompletedSyncs += src.CompletedSyncs
	dst.FailedSyncs += src.FailedSyncs
	dst.UnsyncedBytes += src.UnsyncedBytes
	dst.OfflineReadsBlocked += src.OfflineReadsBlocked
	if src.HasRemote {
		dst.HasRemote = true
	}
}

// EvictBlockStore evicts block store data for the given share (or all shares if shareName is empty).
// Returns an error if trying to evict local blocks without a remote store (safety check).
func (s *Service) EvictBlockStore(ctx context.Context, shareName string, opts EvictOptions) (*EvictResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var targets []*Share
	if shareName != "" {
		share, exists := s.registry[shareName]
		if !exists {
			return nil, fmt.Errorf("share %q not found", shareName)
		}
		if share.BlockStore == nil {
			return nil, fmt.Errorf("share %q has no block store configured", shareName)
		}
		targets = []*Share{share}
	} else {
		for _, share := range s.registry {
			if share.BlockStore != nil {
				targets = append(targets, share)
			}
		}
	}

	var result EvictResult

	for _, share := range targets {
		bs := share.BlockStore

		if !opts.ReadBufferOnly && !bs.HasRemoteStore() {
			return nil, fmt.Errorf("cannot evict local blocks for share %q: no remote store configured (data would be lost)", share.Name)
		}

		if !opts.LocalOnly {
			result.ReadBufferEntriesCleared += bs.DestroyCache()
		}

		if !opts.ReadBufferOnly {
			beforeDisk := bs.LocalStats().DiskUsed

			files := bs.ListFiles()
			for _, payloadID := range files {
				_ = bs.EvictLocal(ctx, payloadID)
				result.LocalFilesEvicted++
			}

			// EvictLocal only clears per-file append-log/memory state, and
			// ListFiles goes empty after rollup — so post-rollup the resident
			// bytes live in sealed log blobs it never touches. Drain them now
			// (synced-only; safe because the no-remote refusal above guarantees
			// a remote copy exists) so reads fall back to the remote.
			drained, err := bs.DrainLocalSynced(ctx)
			if err != nil {
				return nil, fmt.Errorf("drain local synced blocks for share %q: %w", share.Name, err)
			}

			// Report the larger of the observed DiskUsed delta (captures the
			// EvictLocal loop plus the drain) and the drain's own freed count.
			// The raw delta can go negative if a concurrent write grows DiskUsed
			// mid-eviction; max with the non-negative drained count keeps
			// BytesFreed honest and never negative.
			result.BytesFreed += max(beforeDisk-bs.LocalStats().DiskUsed, drained)
		}
	}

	return &result, nil
}

// StartWarm starts (or returns the already-running) async warm job for
// shareName. The job proactively materializes every remote block of the share
// onto the local tier (engine.Store.WarmAll) on a DETACHED context so it
// outlives the request that triggered it; RemoveShare cancels it. Returns an
// error if the share is unknown or has no block store. The returned WarmJob is
// a snapshot taken under the registry lock.
func (s *Service) StartWarm(_ context.Context, shareName string) (*WarmJob, error) {
	s.mu.RLock()
	share, exists := s.registry[shareName]
	if !exists {
		s.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrShareNotFound, shareName)
	}
	if share.BlockStore == nil {
		s.mu.RUnlock()
		return nil, fmt.Errorf("share %q has no block store configured", shareName)
	}
	bs := share.BlockStore
	s.mu.RUnlock()

	// Capture the share's currently-stored byte count up front (O(1) lite
	// stats, no per-file DB walk). The registry uses it to warn when a warm
	// run enumerates zero blocks on a non-empty share (#1374).
	usedBytes := bs.GetStatsLite().LocalDiskUsed

	job := s.warmJobs.start(shareName, usedBytes, func(ctx context.Context, progress func(done, total int64)) (warmAllResult, error) {
		res, err := bs.WarmAll(ctx, progress)
		return warmAllResult{
			BlocksFetched: res.BlocksFetched,
			BytesFetched:  res.BytesFetched,
		}, err
	})
	return job, nil
}

// GetWarm returns a snapshot of the warm job by ID, or (nil, false) if unknown.
func (s *Service) GetWarm(jobID string) (*WarmJob, bool) {
	return s.warmJobs.get(jobID)
}
