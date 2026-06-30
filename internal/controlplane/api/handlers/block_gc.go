package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// BlockGCRuntime is the narrow Runtime surface needed by BlockStoreGCHandler.
// Defining the interface here (rather than depending on *runtime.Runtime
// directly) keeps the handler unit-testable: tests substitute a fake that
// records calls and returns canned responses, mirroring the
// testBlockStoreHandler pattern in blockstore_test.go.
type BlockGCRuntime interface {
	// StartBlockGC launches (or returns the already-running) async GC job. When
	// reconcile is true the run reaps stranded file_blocks rows across all
	// shares before sweeping both tiers; otherwise it runs the share-scoped
	// sweep. The run executes on a context detached from the request, so a
	// request/client timeout cannot abort the (potentially multi-minute) mark
	// phase (#1433).
	StartBlockGC(shareName string, dryRun, reconcile bool, gracePeriod *time.Duration) (*runtime.GCJob, error)

	// GetGCJobStatus returns a GC job by ID, or false if unknown (never started
	// or evicted from the retained-terminal window).
	GetGCJobStatus(jobID string) (*runtime.GCJob, bool)

	// GCStateDirForShare returns the per-share gc-state directory the GC
	// engine writes `last-run.json` into. Empty when the share's local
	// store has no persistent root (in-memory backend).
	GCStateDirForShare(shareName string) (string, error)
}

// BlockStoreGCHandler exposes on-demand GC + last-run-summary endpoints.
type BlockStoreGCHandler struct {
	runtime BlockGCRuntime
}

// NewBlockStoreGCHandler constructs a handler bound to the given Runtime
// surface. Pass a nil-safe value: the handler refuses requests when
// runtime is nil so the server can still boot in degraded modes.
func NewBlockStoreGCHandler(rt BlockGCRuntime) *BlockStoreGCHandler {
	return &BlockStoreGCHandler{runtime: rt}
}

// BlockStoreGCRequest is the JSON body for POST /api/v1/shares/{name}/blockstore/gc.
// The dry_run flag flows through to engine.Options.DryRun: mark + sweep
// enumeration runs, but no DELETEs are issued, and the candidate set is
// captured in GCStats.DryRunCandidates (capped at engine.Options.DryRunSampleSize,
// default 1000).
type BlockStoreGCRequest struct {
	DryRun bool `json:"dry_run,omitempty"`
	// Reconcile runs the migration pass first: reap stranded file_blocks rows
	// (leaked by the pre-fix delete path) across all shares, then sweep both
	// tiers. Reclaims historical leaks a plain GC cannot (#1433).
	Reconcile bool `json:"reconcile,omitempty"`
	// GracePeriodSeconds, when non-nil, overrides the server-configured sweep
	// grace for this run only. Zero is valid and meaningful: reap every
	// eligible orphan with no age guard (bypassing the config's 5-minute
	// floor). Honoured only on the share-scoped path — combining it with
	// reconcile is rejected with 400.
	GracePeriodSeconds *int64 `json:"grace_period_seconds,omitempty"`
}

// GCJobStatusResponse is the JSON status body for an async GC job, returned by
// both RunGC (202) and GCJobStatus (200). Mirrors runtime.GCJob's wire shape;
// Stats is populated once the job reaches a terminal state.
type GCJobStatusResponse struct {
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

func gcJobToResponse(j *runtime.GCJob) GCJobStatusResponse {
	resp := GCJobStatusResponse{
		ID:             j.ID,
		State:          j.State,
		Share:          j.Share,
		Reconcile:      j.Reconcile,
		DryRun:         j.DryRun,
		HashesMarked:   j.HashesMarked,
		ObjectsScanned: j.ObjectsScanned,
		ObjectsSwept:   j.ObjectsSwept,
		BytesFreed:     j.BytesFreed,
		Stats:          j.Stats,
		Error:          j.Err,
	}
	if !j.StartedAt.IsZero() {
		resp.StartedAt = j.StartedAt.UTC().Format(time.RFC3339)
	}
	if !j.FinishedAt.IsZero() {
		resp.FinishedAt = j.FinishedAt.UTC().Format(time.RFC3339)
	}
	return resp
}

// RunGC handles POST /api/v1/shares/{name}/blockstore/gc.
//
// Body: BlockStoreGCRequest (optional; missing body equals {dry_run:false}).
// Behavior: invokes Runtime.RunBlockGCForShare with the URL share name
// and the body's dry_run flag. Cross-share aggregation means the GC
// scans every share whose remote-store config matches; the {name} path
// parameter scopes the last-run.json persistence target, not the
// mark/sweep set itself.
//
// Status codes:
//   - 200 OK with BlockStoreGCResponse on success
//   - 400 Bad Request when {name} is empty or the body decode fails
//   - 404 Not Found when {name} is not a registered share
//   - 500 Internal Server Error on unexpected runtime errors
func (h *BlockStoreGCHandler) RunGC(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "/" {
		BadRequest(w, "share name is required")
		return
	}

	var req BlockStoreGCRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			BadRequest(w, "invalid request body: "+err.Error())
			return
		}
	}

	var gracePeriod *time.Duration
	if req.GracePeriodSeconds != nil {
		if *req.GracePeriodSeconds < 0 {
			BadRequest(w, "grace_period_seconds must not be negative")
			return
		}
		if req.Reconcile {
			BadRequest(w, "grace_period_seconds cannot be combined with reconcile")
			return
		}
		// Guard the seconds→Duration multiply: without this an absurdly large
		// value overflows int64 nanoseconds and wraps negative, which the engine
		// then clamps to 0 (GracePeriodSet) — silently turning a "very long
		// grace" request into the most aggressive possible reap.
		if *req.GracePeriodSeconds > int64(math.MaxInt64/int64(time.Second)) {
			BadRequest(w, "grace_period_seconds is too large")
			return
		}
		d := time.Duration(*req.GracePeriodSeconds) * time.Second
		gracePeriod = &d
	}

	// Kick off the run asynchronously and return immediately: the mark phase can
	// take minutes on a large or snapshot-heavy deployment, far longer than the
	// request-middleware/client timeout, so a synchronous run was aborted
	// mid-mark with "context deadline exceeded" and never reclaimed anything
	// (#1433). The job runs on a detached context; clients poll
	// GET .../blockstore/gc/{job_id}. Reconcile is server-wide (reaps stranded
	// rows across all shares) and ignores the per-share scoping internally.
	job, err := h.runtime.StartBlockGC(name, req.DryRun, req.Reconcile, gracePeriod)
	if err != nil {
		if errors.Is(err, shares.ErrShareNotFound) {
			NotFound(w, "share not found: "+name)
			return
		}
		logger.Debug("Block store GC start error", "share", name, "error", err)
		InternalServerError(w, "GC failed to start")
		return
	}

	WriteJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"status": gcJobToResponse(job),
	})
}

// GCJobStatus handles GET /api/v1/shares/{name}/blockstore/gc/{job_id}. It
// returns the current status of an async GC job. The {name} segment is accepted
// for route symmetry with the start endpoint; the job id alone identifies the
// job.
func (h *BlockStoreGCHandler) GCJobStatus(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	jobID := chi.URLParam(r, "job_id")
	if jobID == "" {
		BadRequest(w, "job id required")
		return
	}

	job, ok := h.runtime.GetGCJobStatus(jobID)
	if !ok {
		NotFound(w, "GC job not found")
		return
	}

	WriteJSONOK(w, gcJobToResponse(job))
}

// GCStatus handles GET /api/v1/shares/{name}/blockstore/gc-status.
//
// Reads `<gcStateRoot>/last-run.json` for the share and returns the
// parsed engine.GCRunSummary. Returns 404 when no run has
// completed yet (file does not exist), letting operators distinguish
// "GC has never run for this share" from "GC ran and reported errors".
//
// Status codes:
//   - 200 OK with engine.GCRunSummary
//   - 400 Bad Request when {name} is empty
//   - 404 Not Found when the share is unknown OR when no last-run.json exists
//   - 500 Internal Server Error on filesystem or parse failures
func (h *BlockStoreGCHandler) GCStatus(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "/" {
		BadRequest(w, "share name is required")
		return
	}

	gcRoot, err := h.runtime.GCStateDirForShare(name)
	if err != nil {
		if errors.Is(err, shares.ErrShareNotFound) {
			NotFound(w, "share not found: "+name)
			return
		}
		logger.Debug("Block store GC status lookup error", "share", name, "error", err)
		// IN-4-02: strip underlying err string from the response — same
		// rationale as IN-2-01 in RunGC. Details are logged at Debug.
		InternalServerError(w, "GC status lookup failed")
		return
	}
	if gcRoot == "" {
		// In-memory local store: no persistent run summary is written.
		NotFound(w, "no GC run recorded for share "+name+" (local store has no persistent root)")
		return
	}

	summaryPath := filepath.Join(gcRoot, "last-run.json")
	data, err := os.ReadFile(summaryPath) //nolint:gosec // path is server-controlled (joined to gc-state root)
	if err != nil {
		if os.IsNotExist(err) {
			NotFound(w, "no GC run recorded for share "+name)
			return
		}
		logger.Debug("Block store GC status read error", "share", name, "path", summaryPath, "error", err)
		// IN-4-02: don't leak filesystem path / underlying error.
		InternalServerError(w, "GC status read failed")
		return
	}

	var summary engine.GCRunSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		logger.Debug("Block store GC status parse error", "share", name, "path", summaryPath, "error", err)
		// IN-4-02: don't leak parse-error internals (file path embedded
		// via fmt.Errorf wrap previously surfaced to caller).
		InternalServerError(w, "GC status parse failed")
		return
	}

	WriteJSONOK(w, summary)
}
