package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// CreateRepo handles POST /api/v1/store/metadata/{name}/repos. Persists the
// BackupRepo row then calls svc.RegisterRepo so the scheduler picks it up
// (D-22).
func (h *BackupHandler) CreateRepo(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w) {
		return
	}
	storeName := chi.URLParam(r, "name")
	storeCfg, ok := h.resolveMetadataStore(w, r, storeName)
	if !ok {
		return
	}

	var req BackupRepoRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == "" {
		BadRequest(w, "repo name is required")
		return
	}
	if req.Kind == "" {
		BadRequest(w, "repo kind is required")
		return
	}

	// D-18 — strict schedule validation before persist.
	if req.Schedule != nil && *req.Schedule != "" {
		if err := h.svc.ValidateSchedule(*req.Schedule); err != nil {
			BadRequest(w, err.Error())
			return
		}
	}

	repo := &models.BackupRepo{
		TargetID:   storeCfg.ID,
		TargetKind: "metadata",
		Name:       req.Name,
		Kind:       models.BackupRepoKind(req.Kind),
		Schedule:   req.Schedule,
	}
	if req.Config != nil {
		if err := repo.SetConfig(req.Config); err != nil {
			BadRequest(w, "invalid config: "+err.Error())
			return
		}
	}
	if req.KeepCount != nil {
		repo.KeepCount = req.KeepCount
	}
	if req.KeepAgeDays != nil {
		repo.KeepAgeDays = req.KeepAgeDays
	}
	if req.EncryptionEnabled != nil {
		repo.EncryptionEnabled = *req.EncryptionEnabled
	}
	if req.EncryptionKeyRef != nil {
		repo.EncryptionKeyRef = *req.EncryptionKeyRef
	}

	id, err := h.store.CreateBackupRepo(r.Context(), repo)
	if err != nil {
		if errors.Is(err, models.ErrDuplicateBackupRepo) {
			Conflict(w, "Backup repo already exists")
			return
		}
		logger.Error("Create backup repo failed", "name", req.Name, "error", err)
		InternalServerError(w, "Failed to create backup repo")
		return
	}
	repo.ID = id

	// Ask the scheduler to pick up the schedule (no-op when empty).
	if err := h.svc.RegisterRepo(r.Context(), id); err != nil {
		// Do not fail the create — the row is persisted. Log + warn.
		logger.Warn("RegisterRepo failed after create", "repo_id", id, "error", err)
	}

	WriteJSONCreated(w, repoToResponse(repo))
}

// ListRepos handles GET /api/v1/store/metadata/{name}/repos.
func (h *BackupHandler) ListRepos(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w) {
		return
	}
	storeName := chi.URLParam(r, "name")
	storeCfg, ok := h.resolveMetadataStore(w, r, storeName)
	if !ok {
		return
	}
	repos, err := h.store.ListReposByTarget(r.Context(), "metadata", storeCfg.ID)
	if err != nil {
		InternalServerError(w, "Failed to list backup repos")
		return
	}
	WriteJSONOK(w, reposToResponses(repos))
}

// GetRepo handles GET /api/v1/store/metadata/{name}/repos/{repo}.
func (h *BackupHandler) GetRepo(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w) {
		return
	}
	storeName := chi.URLParam(r, "name")
	repoName := chi.URLParam(r, "repo")
	if repoName == "" {
		BadRequest(w, "Repo name is required")
		return
	}
	storeCfg, ok := h.resolveMetadataStore(w, r, storeName)
	if !ok {
		return
	}
	repo, err := h.store.GetBackupRepo(r.Context(), storeCfg.ID, repoName)
	if err != nil {
		if errors.Is(err, models.ErrBackupRepoNotFound) {
			NotFound(w, "Backup repo not found")
			return
		}
		InternalServerError(w, "Failed to get backup repo")
		return
	}
	WriteJSONOK(w, repoToResponse(repo))
}

// PatchRepo handles PATCH /api/v1/store/metadata/{name}/repos/{repo}.
// D-19 partial update: only non-nil fields in the request mutate the row;
// schedule is validated before persist; svc.UpdateRepo is invoked to
// trigger Unregister+Register (D-22).
func (h *BackupHandler) PatchRepo(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w) {
		return
	}
	storeName := chi.URLParam(r, "name")
	repoName := chi.URLParam(r, "repo")
	storeCfg, ok := h.resolveMetadataStore(w, r, storeName)
	if !ok {
		return
	}

	repo, err := h.store.GetBackupRepo(r.Context(), storeCfg.ID, repoName)
	if err != nil {
		if errors.Is(err, models.ErrBackupRepoNotFound) {
			NotFound(w, "Backup repo not found")
			return
		}
		InternalServerError(w, "Failed to get backup repo")
		return
	}

	var req BackupRepoRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Name != "" {
		repo.Name = req.Name
	}
	if req.Kind != "" {
		repo.Kind = models.BackupRepoKind(req.Kind)
	}
	if req.Config != nil {
		if err := repo.SetConfig(req.Config); err != nil {
			BadRequest(w, "invalid config: "+err.Error())
			return
		}
	}
	if req.Schedule != nil {
		if *req.Schedule != "" {
			if err := h.svc.ValidateSchedule(*req.Schedule); err != nil {
				BadRequest(w, err.Error())
				return
			}
		}
		repo.Schedule = req.Schedule
	}
	if req.KeepCount != nil {
		repo.KeepCount = req.KeepCount
	}
	if req.KeepAgeDays != nil {
		repo.KeepAgeDays = req.KeepAgeDays
	}
	if req.EncryptionEnabled != nil {
		repo.EncryptionEnabled = *req.EncryptionEnabled
	}
	if req.EncryptionKeyRef != nil {
		repo.EncryptionKeyRef = *req.EncryptionKeyRef
	}

	if err := h.store.UpdateBackupRepo(r.Context(), repo); err != nil {
		if errors.Is(err, models.ErrBackupRepoNotFound) {
			NotFound(w, "Backup repo not found")
			return
		}
		InternalServerError(w, "Failed to update backup repo")
		return
	}
	if err := h.svc.UpdateRepo(r.Context(), repo.ID); err != nil {
		logger.Warn("UpdateRepo (scheduler) failed after PATCH", "repo_id", repo.ID, "error", err)
	}
	WriteJSONOK(w, repoToResponse(repo))
}

// DeleteRepo handles DELETE /api/v1/store/metadata/{name}/repos/{repo}.
// Default: 204 + UnregisterRepo + delete row. With ?purge_archives=true (D-21),
// iterates Destination.Delete across every record before removing the row.
// Partial failures are reported via a problem body carrying failed_record_ids.
func (h *BackupHandler) DeleteRepo(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w) {
		return
	}
	storeName := chi.URLParam(r, "name")
	repoName := chi.URLParam(r, "repo")
	storeCfg, ok := h.resolveMetadataStore(w, r, storeName)
	if !ok {
		return
	}
	repo, err := h.store.GetBackupRepo(r.Context(), storeCfg.ID, repoName)
	if err != nil {
		if errors.Is(err, models.ErrBackupRepoNotFound) {
			NotFound(w, "Backup repo not found")
			return
		}
		InternalServerError(w, "Failed to get backup repo")
		return
	}

	purge := r.URL.Query().Get("purge_archives") == "true"
	if purge {
		failed, perr := h.purgeRepoArchives(r.Context(), repo)
		if perr != nil && len(failed) == 0 {
			// Build destination or listing failed before any delete.
			InternalServerError(w, "Failed to purge archives: "+perr.Error())
			return
		}
		if len(failed) > 0 {
			// Partial failure: the row is NOT deleted (D-21 — operators can retry).
			body := &BackupRepoPurgeProblem{
				Problem: Problem{
					Type:   "about:blank",
					Title:  "Partial purge failure",
					Status: http.StatusOK,
					Detail: fmt.Sprintf("%d record(s) failed to delete; repo row preserved", len(failed)),
				},
				FailedRecordIDs: failed,
			}
			writeProblemJSON(w, http.StatusOK, body)
			return
		}
	}

	if err := h.svc.UnregisterRepo(r.Context(), repo.ID); err != nil {
		logger.Warn("UnregisterRepo failed", "repo_id", repo.ID, "error", err)
	}

	if err := h.store.DeleteBackupRepo(r.Context(), repo.ID); err != nil {
		if errors.Is(err, models.ErrBackupRepoNotFound) {
			NotFound(w, "Backup repo not found")
			return
		}
		if errors.Is(err, models.ErrBackupRepoInUse) {
			Conflict(w, "Backup repo is in use")
			return
		}
		logger.Error("Delete backup repo failed", "repo_id", repo.ID, "error", err)
		InternalServerError(w, "Failed to delete backup repo")
		return
	}

	WriteNoContent(w)
}

// BackupRepoPurgeProblem extends Problem with the list of record IDs that
// failed to purge. Emitted with 200 OK on partial-failure so the repo row
// is preserved for retry (D-21).
type BackupRepoPurgeProblem struct {
	Problem
	FailedRecordIDs []string `json:"failed_record_ids"`
}

// purgeRepoArchives iterates records for the repo and Destination.Delete's
// each archive. Returns the list of record IDs that failed. Requires a
// configured destFactory; otherwise returns an error.
func (h *BackupHandler) purgeRepoArchives(ctx context.Context, repo *models.BackupRepo) ([]string, error) {
	if h.destFactory == nil {
		return nil, fmt.Errorf("destination factory not configured")
	}
	// Use all records (not just succeeded) — failed/interrupted may have
	// partial manifests to clean up.
	recs, err := h.store.ListBackupRecords(ctx, repo.ID, "")
	if err != nil {
		return nil, fmt.Errorf("list records: %w", err)
	}
	if len(recs) == 0 {
		return nil, nil
	}
	dst, err := h.destFactory(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("build destination: %w", err)
	}
	defer func() {
		if cerr := dst.Close(); cerr != nil {
			logger.Warn("Destination close error", "repo_id", repo.ID, "error", cerr)
		}
	}()

	var failed []string
	for _, rec := range recs {
		if err := dst.Delete(ctx, rec.ID); err != nil {
			logger.Warn("purge_archives Delete failed",
				"repo_id", repo.ID, "record_id", rec.ID, "error", err)
			failed = append(failed, rec.ID)
		}
	}
	return failed, nil
}

// resolveMetadataStore writes a problem + returns ok=false on lookup error,
// else returns the store config.
func (h *BackupHandler) resolveMetadataStore(w http.ResponseWriter, r *http.Request, name string) (*models.MetadataStoreConfig, bool) {
	if name == "" {
		BadRequest(w, "Store name is required")
		return nil, false
	}
	cfg, err := h.store.GetMetadataStore(r.Context(), name)
	if err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Metadata store not found")
			return nil, false
		}
		InternalServerError(w, "Failed to get metadata store")
		return nil, false
	}
	return cfg, true
}
