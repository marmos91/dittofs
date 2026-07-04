package apiclient

import (
	"fmt"
	"net/url"

	"github.com/marmos91/dittofs/pkg/block/engine"
)

// BlockStoreStats is the wire-shape for block store statistics returned by
// the server. It is a type alias of engine.BlockStoreStats so the client and
// server share a single canonical definition (identical fields, identical
// json tags). The alias preserves the JSON wire shape and lets callers pass
// values across the apiclient/engine boundary without conversion.
type BlockStoreStats = engine.BlockStoreStats

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

// WarmJobStatus is the wire-shape for an async share-warm job, returned by
// GetShareWarm and embedded in StartShareWarm's response.
type WarmJobStatus struct {
	ID          string `json:"id"`
	Share       string `json:"share"`
	State       string `json:"state"`
	BlocksTotal int64  `json:"blocks_total"`
	BlocksDone  int64  `json:"blocks_done"`
	BytesDone   int64  `json:"bytes_done"`
	StartedAt   string `json:"started_at,omitempty"`
	FinishedAt  string `json:"finished_at,omitempty"`
	Error       string `json:"error,omitempty"`
	Warning     string `json:"warning,omitempty"`
}

// warmStartResponse is the 202 body from POST .../blockstore/warm.
type warmStartResponse struct {
	JobID  string        `json:"job_id"`
	Status WarmJobStatus `json:"status"`
}

// StartShareWarm starts (or returns the already-running) async warm job that
// materializes the named share's blocks onto its local tier. Returns the job
// id to poll via GetShareWarm.
func (c *Client) StartShareWarm(name string) (string, error) {
	resp, err := createResource[warmStartResponse](
		c,
		fmt.Sprintf("/api/v1/shares/%s/blockstore/warm", url.PathEscape(normalizeShareNameForAPI(name))),
		struct{}{},
	)
	if err != nil {
		return "", err
	}
	return resp.JobID, nil
}

// GetShareWarm returns the current status of warm job jobID for the named
// share.
func (c *Client) GetShareWarm(name, jobID string) (*WarmJobStatus, error) {
	return getResource[WarmJobStatus](
		c,
		fmt.Sprintf("/api/v1/shares/%s/blockstore/warm/%s",
			url.PathEscape(normalizeShareNameForAPI(name)), url.PathEscape(jobID)),
	)
}

// BlockStoreGCOptions is the request body for
// POST /api/v1/shares/{name}/blockstore/gc. DryRun maps to
// engine.Options.DryRun: mark + sweep enumeration runs but no DELETEs
// are issued; candidate keys are returned in the job's
// GCJobStatus.Stats.DryRunCandidates (capped at the engine dry-run
// sample size, default 1000).
type BlockStoreGCOptions struct {
	DryRun bool `json:"dry_run,omitempty"`
	// Reconcile runs the migration pass: reap stranded file_blocks rows
	// (leaked by the pre-fix delete path) across all shares, then sweep both
	// tiers — reclaiming historical leaks a plain GC cannot (#1433).
	Reconcile bool `json:"reconcile,omitempty"`
	// GracePeriodSeconds, when non-nil, overrides the server-configured sweep
	// grace for this run only. Zero is valid: reap every eligible orphan with
	// no age guard, bypassing the config's 5-minute floor. The server rejects
	// combining it with Reconcile.
	GracePeriodSeconds *int64 `json:"grace_period_seconds,omitempty"`
}

// GCJobStatus is the wire-shape for an async block-store GC job, returned by
// GetBlockStoreGCJob and embedded in StartBlockStoreGC's response. Stats is
// populated once State is terminal ("done"/"failed"); the live counters track
// the in-flight run.
type GCJobStatus struct {
	ID             string          `json:"id"`
	State          string          `json:"state"`
	Share          string          `json:"share"`
	Reconcile      bool            `json:"reconcile"`
	DryRun         bool            `json:"dry_run"`
	HashesMarked   int64           `json:"hashes_marked"`
	ObjectsScanned int64           `json:"objects_scanned"`
	ObjectsSwept   int64           `json:"objects_swept"`
	BytesFreed     int64           `json:"bytes_freed"`
	StartedAt      string          `json:"started_at,omitempty"`
	FinishedAt     string          `json:"finished_at,omitempty"`
	Stats          *engine.GCStats `json:"stats,omitempty"`
	Error          string          `json:"error,omitempty"`
}

// gcStartResponse is the 202 body from POST .../blockstore/gc.
type gcStartResponse struct {
	JobID  string      `json:"job_id"`
	Status GCJobStatus `json:"status"`
}

// StartBlockStoreGC kicks off (or returns the already-running) async GC run for
// the named share and returns the job id to poll via GetBlockStoreGCJob. The
// run scans every share whose remote-store config matches the named share's
// remote, but `last-run.json` is persisted under the named share's gc-state
// directory. The server runs it on a detached context, so this call returns
// immediately rather than blocking for the (possibly multi-minute) run (#1433).
// opts may be nil (treated as a non-dry-run request).
//
// Note: the route is mounted at /api/v1/shares/{name}/blockstore/gc because the
// /store/block/{kind} sub-router would shadow a {name} segment at the same
// level — chi v5 cannot disambiguate two differently-named wildcards on one path
// segment. The per-share /shares/{name}/blockstore/... pattern matches the
// existing stats and evict endpoints.
func (c *Client) StartBlockStoreGC(shareName string, opts *BlockStoreGCOptions) (string, error) {
	if opts == nil {
		opts = &BlockStoreGCOptions{}
	}
	resp, err := createResource[gcStartResponse](
		c,
		fmt.Sprintf("/api/v1/shares/%s/blockstore/gc", url.PathEscape(normalizeShareNameForAPI(shareName))),
		opts,
	)
	if err != nil {
		return "", err
	}
	return resp.JobID, nil
}

// GetBlockStoreGCJob returns the current status of GC job jobID for the named
// share.
func (c *Client) GetBlockStoreGCJob(shareName, jobID string) (*GCJobStatus, error) {
	return getResource[GCJobStatus](
		c,
		fmt.Sprintf("/api/v1/shares/%s/blockstore/gc/%s",
			url.PathEscape(normalizeShareNameForAPI(shareName)), url.PathEscape(jobID)),
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

// BlockStoreAuditResult is the response body for
// POST /api/v1/shares/{name}/audit/refcounts. Wraps the
// engine.AuditRefcountsResult value (CAS manifest-consistency audit).
// Mirrors the server-side handlers.BlockStoreAuditResponse shape.
type BlockStoreAuditResult struct {
	Result *engine.AuditRefcountsResult `json:"result"`
}

// BlockStoreAuditRefcounts triggers the on-demand CAS manifest-consistency
// audit for the named share. Server walks the share's metadata store and
// verifies every manifest reference (FileAttr.Blocks) has a backing
// FileChunk row; a non-zero delta (DanglingRefs) indicates a file
// references a chunk the store has no record of. The audit persists
// last-inv02.json under the share's audit-state directory; this client
// method returns the same summary in the response body for direct
// consumption by `dfsctl blockstore audit-refcounts`.
//
// Mirrors BlockStoreGC's URL/error pattern (per-share path, JSON body,
// JWT auth via the underlying transport).
func (c *Client) BlockStoreAuditRefcounts(shareName string) (*BlockStoreAuditResult, error) {
	return createResource[BlockStoreAuditResult](
		c,
		fmt.Sprintf("/api/v1/shares/%s/audit/refcounts", url.PathEscape(normalizeShareNameForAPI(shareName))),
		struct{}{},
	)
}
