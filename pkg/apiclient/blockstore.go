package apiclient

import (
	"fmt"
	"net/url"

	"github.com/marmos91/dittofs/pkg/blockstore/engine"
)

// BlockStoreStats holds block store statistics returned by the server.
// Mirrors pkg/blockstore/engine.BlockStoreStats.
type BlockStoreStats struct {
	FileCount         int   `json:"file_count"`
	BlocksDirty       int   `json:"blocks_dirty"`
	BlocksLocal       int   `json:"blocks_local"`
	BlocksRemote      int   `json:"blocks_remote"`
	BlocksTotal       int   `json:"blocks_total"`
	LocalDiskUsed     int64 `json:"local_disk_used"`
	LocalDiskMax      int64 `json:"local_disk_max"`
	LocalMemUsed      int64 `json:"local_mem_used"`
	LocalMemMax       int64 `json:"local_mem_max"`
	ReadBufferEntries int   `json:"read_buffer_entries"`
	ReadBufferUsed    int64 `json:"read_buffer_used"`
	ReadBufferMax     int64 `json:"read_buffer_max"`
	HasRemote         bool  `json:"has_remote"`
	PendingSyncs      int   `json:"pending_syncs"`
	PendingUploads    int   `json:"pending_uploads"`
	CompletedSyncs    int   `json:"completed_syncs"`
	FailedSyncs       int   `json:"failed_syncs"`

	RemoteHealthy       bool    `json:"remote_healthy"`
	EvictionSuspended   bool    `json:"eviction_suspended"`
	OutageDurationSecs  float64 `json:"outage_duration_seconds"`
	OfflineReadsBlocked int64   `json:"offline_reads_blocked"`
}

// ShareBlockStoreStats holds block store statistics for a single share.
type ShareBlockStoreStats struct {
	ShareName string          `json:"share_name"`
	Stats     BlockStoreStats `json:"stats"`
}

// BlockStoreStatsResponse holds aggregated and per-share block store statistics.
type BlockStoreStatsResponse struct {
	Totals   BlockStoreStats        `json:"totals"`
	PerShare []ShareBlockStoreStats `json:"per_share,omitempty"`
}

// BlockStoreEvictOptions is the request body for block store eviction.
type BlockStoreEvictOptions struct {
	ReadBufferOnly bool `json:"read_buffer_only,omitempty"`
	LocalOnly      bool `json:"local_only,omitempty"`
}

// BlockStoreEvictResult holds the result of a block store eviction operation.
type BlockStoreEvictResult struct {
	ReadBufferEntriesCleared int   `json:"read_buffer_entries_cleared"`
	LocalFilesEvicted        int   `json:"local_files_evicted"`
	BytesFreed               int64 `json:"bytes_freed"`
}

// BlockStoreStatsAll returns aggregated block store statistics across all shares.
func (c *Client) BlockStoreStatsAll() (*BlockStoreStatsResponse, error) {
	return getResource[BlockStoreStatsResponse](c, "/api/v1/blockstore/stats")
}

// BlockStoreStatsForShare returns block store statistics for a specific share.
func (c *Client) BlockStoreStatsForShare(shareName string) (*BlockStoreStatsResponse, error) {
	return getResource[BlockStoreStatsResponse](c, fmt.Sprintf("/api/v1/shares/%s/blockstore/stats", url.PathEscape(normalizeShareNameForAPI(shareName))))
}

// BlockStoreEvict evicts block store data across all shares.
func (c *Client) BlockStoreEvict(req *BlockStoreEvictOptions) (*BlockStoreEvictResult, error) {
	return createResource[BlockStoreEvictResult](c, "/api/v1/blockstore/evict", req)
}

// BlockStoreEvictForShare evicts block store data for a specific share.
func (c *Client) BlockStoreEvictForShare(shareName string, req *BlockStoreEvictOptions) (*BlockStoreEvictResult, error) {
	return createResource[BlockStoreEvictResult](c, fmt.Sprintf("/api/v1/shares/%s/blockstore/evict", url.PathEscape(normalizeShareNameForAPI(shareName))), req)
}

// BlockStoreGCOptions is the request body for
// POST /api/v1/shares/{name}/blockstore/gc. DryRun maps to
// engine.Options.DryRun: mark + sweep enumeration runs but no DELETEs
// are issued; candidate keys are returned in
// BlockStoreGCResult.Stats.DryRunCandidates (capped at the engine
// dry-run sample size, default 1000 — Phase 11 D-09).
type BlockStoreGCOptions struct {
	DryRun bool `json:"dry_run,omitempty"`
}

// BlockStoreGCResult is the response body for
// POST /api/v1/shares/{name}/blockstore/gc. Stats wraps the *engine.GCStats
// summed across every distinct remote scanned during the run (D-03
// cross-share aggregation). Mirrors the server-side
// handlers.BlockStoreGCResponse shape.
type BlockStoreGCResult struct {
	Stats *engine.GCStats `json:"stats"`
}

// BlockStoreGC triggers an on-demand GC run for the named share. The
// run scans every share whose remote-store config matches the named
// share's remote (D-03), but `last-run.json` is persisted under the
// named share's gc-state directory. opts may be nil (treated as a
// non-dry-run request).
//
// Note: the route is mounted at /api/v1/shares/{name}/blockstore/gc
// (not /api/v1/store/block/{name}/gc as initially scoped) because the
// /store/block/{kind} sub-router would shadow a {name} segment at the
// same level — chi v5 cannot disambiguate two differently-named wildcards
// on the same path segment. The per-share /shares/{name}/blockstore/...
// pattern matches the existing stats and evict endpoints.
func (c *Client) BlockStoreGC(shareName string, opts *BlockStoreGCOptions) (*BlockStoreGCResult, error) {
	if opts == nil {
		opts = &BlockStoreGCOptions{}
	}
	return createResource[BlockStoreGCResult](
		c,
		fmt.Sprintf("/api/v1/shares/%s/blockstore/gc", url.PathEscape(normalizeShareNameForAPI(shareName))),
		opts,
	)
}

// BlockStoreGCStatus reads the last-run summary for the named share's
// GC engine. Returns an APIError with IsNotFound() == true when no run
// has been recorded yet (no `last-run.json` exists for the share).
func (c *Client) BlockStoreGCStatus(shareName string) (*engine.GCRunSummary, error) {
	return getResource[engine.GCRunSummary](
		c,
		fmt.Sprintf("/api/v1/shares/%s/blockstore/gc-status", url.PathEscape(normalizeShareNameForAPI(shareName))),
	)
}
