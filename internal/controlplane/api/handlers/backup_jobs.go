package handlers

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/storebackups"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// ListJobs handles GET /api/v1/store/metadata/{name}/backup-jobs.
// Query parameters: ?status=<BackupStatus>&kind=<BackupJobKind>&repo=<name>&limit=<int>.
// D-42: limit defaults to 50 and caps at 200 in the store layer.
func (h *BackupHandler) ListJobs(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w) {
		return
	}
	storeName := chi.URLParam(r, "name")
	storeCfg, err := h.store.GetMetadataStore(r.Context(), storeName)
	if err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Metadata store not found")
			return
		}
		InternalServerError(w, "Failed to get metadata store")
		return
	}

	q := r.URL.Query()
	status, ok := parseBackupStatus(q.Get("status"), true)
	if !ok {
		BadRequest(w, "invalid status filter")
		return
	}
	kind, ok := parseBackupJobKind(q.Get("kind"), true)
	if !ok {
		BadRequest(w, "invalid kind filter")
		return
	}
	limit, ok := parseLimit(q.Get("limit"))
	if !ok {
		BadRequest(w, "invalid limit")
		return
	}

	// Resolve ?repo= to a repo ID scoped to this store (D-42 — per-store).
	var repoID string
	if repoName := q.Get("repo"); repoName != "" {
		repos, err := h.store.ListReposByTarget(r.Context(), "metadata", storeCfg.ID)
		if err != nil {
			InternalServerError(w, "Failed to list backup repos")
			return
		}
		for _, rp := range repos {
			if rp.Name == repoName {
				repoID = rp.ID
				break
			}
		}
		if repoID == "" {
			NotFound(w, "Backup repo not found")
			return
		}
	}

	jobs, err := h.store.ListBackupJobsFiltered(r.Context(), store.BackupJobFilter{
		RepoID: repoID,
		Status: status,
		Kind:   kind,
		Limit:  limit,
	})
	if err != nil {
		InternalServerError(w, "Failed to list backup jobs")
		return
	}
	WriteJSONOK(w, jobsToResponses(jobs))
}

// GetJob handles GET /api/v1/store/metadata/{name}/backup-jobs/{id}. Polling
// endpoint for CLI `--wait` flows.
func (h *BackupHandler) GetJob(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w) {
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		BadRequest(w, "Job id is required")
		return
	}
	job, err := h.store.GetBackupJob(r.Context(), id)
	if err != nil {
		if errors.Is(err, models.ErrBackupJobNotFound) {
			NotFound(w, "Backup job not found")
			return
		}
		InternalServerError(w, "Failed to get backup job")
		return
	}
	WriteJSONOK(w, jobToResponse(job))
}

// CancelJob handles POST /api/v1/store/metadata/{name}/backup-jobs/{id}/cancel.
//
// D-43/D-44/D-45 semantics:
//   - Unknown job ID (DB miss) → 404.
//   - Terminal job (succeeded / failed / interrupted) → 200 OK + current job
//     (idempotent: no cancel dispatched; the row is already final).
//   - Running / pending → dispatch CancelBackupJob; return 202 Accepted + the
//     (possibly re-read) current job. svc returning ErrBackupJobNotFound
//     after a DB-present pre-check is a race (job became terminal between
//     the read and the registry lookup) — treat as 200 OK idempotent.
func (h *BackupHandler) CancelJob(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w) {
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		BadRequest(w, "Job id is required")
		return
	}

	job, err := h.store.GetBackupJob(r.Context(), id)
	if err != nil {
		if errors.Is(err, models.ErrBackupJobNotFound) {
			NotFound(w, "Backup job not found")
			return
		}
		InternalServerError(w, "Failed to get backup job")
		return
	}

	if isTerminalJobStatus(job.Status) {
		WriteJSONOK(w, jobToResponse(job))
		return
	}

	if err := h.svc.CancelBackupJob(r.Context(), id); err != nil {
		if errors.Is(err, storebackups.ErrBackupJobNotFound) {
			// Race: DB row exists but run-ctx is gone — treat as
			// idempotent success per D-45. Re-read the row so the
			// caller sees the latest terminal state.
			if fresh, rerr := h.store.GetBackupJob(r.Context(), id); rerr == nil {
				WriteJSONOK(w, jobToResponse(fresh))
				return
			}
			WriteJSONOK(w, jobToResponse(job))
			return
		}
		logger.Error("Cancel backup job failed", "job_id", id, "error", err)
		WriteClassifiedBackupError(w, err)
		return
	}

	// Best-effort re-read so the response reflects the latest state.
	if fresh, rerr := h.store.GetBackupJob(r.Context(), id); rerr == nil {
		WriteJSON(w, http.StatusAccepted, jobToResponse(fresh))
		return
	}
	WriteJSON(w, http.StatusAccepted, jobToResponse(job))
}

func isTerminalJobStatus(s models.BackupStatus) bool {
	switch s {
	case models.BackupStatusSucceeded, models.BackupStatusFailed, models.BackupStatusInterrupted:
		return true
	}
	return false
}
