package apiclient

import "fmt"

// CacheStats holds cache statistics returned by the server.
// Mirrors pkg/blockstore/engine.CacheStats.
type CacheStats struct {
	FileCount      int   `json:"file_count"`
	BlocksDirty    int   `json:"blocks_dirty"`
	BlocksLocal    int   `json:"blocks_local"`
	BlocksRemote   int   `json:"blocks_remote"`
	BlocksTotal    int   `json:"blocks_total"`
	LocalDiskUsed  int64 `json:"local_disk_used"`
	LocalDiskMax   int64 `json:"local_disk_max"`
	LocalMemUsed   int64 `json:"local_mem_used"`
	LocalMemMax    int64 `json:"local_mem_max"`
	L1Entries      int   `json:"l1_entries"`
	L1CurBytes     int64 `json:"l1_cur_bytes"`
	L1MaxBytes     int64 `json:"l1_max_bytes"`
	HasRemote      bool  `json:"has_remote"`
	PendingSyncs   int   `json:"pending_syncs"`
	PendingUploads int   `json:"pending_uploads"`
	CompletedSyncs int   `json:"completed_syncs"`
	FailedSyncs    int   `json:"failed_syncs"`
}

// ShareCacheStats holds cache statistics for a single share.
type ShareCacheStats struct {
	ShareName string     `json:"share_name"`
	Stats     CacheStats `json:"stats"`
}

// CacheStatsResponse holds aggregated and per-share cache statistics.
type CacheStatsResponse struct {
	Totals   CacheStats        `json:"totals"`
	PerShare []ShareCacheStats `json:"per_share,omitempty"`
}

// CacheEvictRequest is the request body for cache eviction.
type CacheEvictRequest struct {
	L1Only    bool `json:"l1_only,omitempty"`
	LocalOnly bool `json:"local_only,omitempty"`
}

// CacheEvictResult holds the result of a cache eviction operation.
type CacheEvictResult struct {
	L1EntriesCleared   int   `json:"l1_entries_cleared"`
	LocalBlocksEvicted int   `json:"local_blocks_evicted"`
	BytesFreed         int64 `json:"bytes_freed"`
}

// CacheStatsAll returns aggregated cache statistics across all shares.
func (c *Client) CacheStatsAll() (*CacheStatsResponse, error) {
	return getResource[CacheStatsResponse](c, "/api/v1/cache/stats")
}

// CacheStatsForShare returns cache statistics for a specific share.
func (c *Client) CacheStatsForShare(shareName string) (*CacheStatsResponse, error) {
	return getResource[CacheStatsResponse](c, fmt.Sprintf("/api/v1/shares/%s/cache/stats", shareName))
}

// CacheEvict evicts cache data across all shares.
func (c *Client) CacheEvict(req *CacheEvictRequest) (*CacheEvictResult, error) {
	return createResource[CacheEvictResult](c, "/api/v1/cache/evict", req)
}

// CacheEvictForShare evicts cache data for a specific share.
func (c *Client) CacheEvictForShare(shareName string, req *CacheEvictRequest) (*CacheEvictResult, error) {
	return createResource[CacheEvictResult](c, fmt.Sprintf("/api/v1/shares/%s/cache/evict", shareName), req)
}
