package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/storebackups"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// BackupHandlerStore is the narrow composite the backup handlers need:
// the per-store metadata config lookups (to resolve {name} → target_id) and
// the BackupStore CRUD surface.
type BackupHandlerStore interface {
	store.MetadataStoreConfigStore
	store.BackupStore
}

// BackupService is the narrow interface over storebackups.Service that the
// backup handlers call. Defined here (not in the runtime package) so tests
// can swap in fakes without wiring a full runtime.
//
// Satisfied by *storebackups.Service.
type BackupService interface {
	RunBackup(ctx context.Context, repoID string) (*models.BackupRecord, *models.BackupJob, error)
	RunRestore(ctx context.Context, repoID string, recordID *string) (*models.BackupJob, error)
	RunRestoreDryRun(ctx context.Context, repoID string, recordID *string) (*storebackups.DryRunResult, error)
	CancelBackupJob(ctx context.Context, jobID string) error
	RegisterRepo(ctx context.Context, repoID string) error
	UnregisterRepo(ctx context.Context, repoID string) error
	UpdateRepo(ctx context.Context, repoID string) error
	ValidateSchedule(expr string) error
}

// BackupDestinationDeleter is the narrow interface over a destination
// driver's Delete method used by DeleteRepo(?purge_archives=true). Kept
// local so tests can stub it without importing pkg/backup/destination.
type BackupDestinationDeleter interface {
	Delete(ctx context.Context, id string) error
	Close() error
}

// BackupDestinationFactory returns a BackupDestinationDeleter for a repo.
// Production wiring passes a closure over destination.DestinationFactoryFromRepo;
// tests pass a stub.
type BackupDestinationFactory func(ctx context.Context, repo *models.BackupRepo) (BackupDestinationDeleter, error)

// BackupHandler handles backup records, backup jobs, backup repo CRUD, and
// restore endpoints for a single metadata store. Methods live across
// backups.go (records + trigger + restore), backup_jobs.go (jobs), and
// backup_repos.go (repo CRUD) for file-size hygiene.
type BackupHandler struct {
	store       BackupHandlerStore
	svc         BackupService
	destFactory BackupDestinationFactory
}

// NewBackupHandler constructs a BackupHandler. destFactory may be nil for
// tests that don't exercise DELETE ?purge_archives=true. svc may be nil
// when the server starts without a runtime (tests, degraded mode) — every
// handler method checks h.requireService and returns 503 if so.
func NewBackupHandler(s BackupHandlerStore, svc BackupService, destFactory BackupDestinationFactory) *BackupHandler {
	return &BackupHandler{store: s, svc: svc, destFactory: destFactory}
}

// requireService gates handler entry when the backup subsystem is not
// wired (h.svc == nil). Writes 503 ServiceUnavailable and returns false
// so the caller short-circuits. Registering routes unconditionally and
// returning 503 here keeps clients able to distinguish "endpoint missing"
// (404) from "endpoint present, backend down" (503).
func (h *BackupHandler) requireService(w http.ResponseWriter) bool {
	if h.svc == nil {
		ServiceUnavailable(w, "Backup subsystem is not initialized on this server.")
		return false
	}
	return true
}

// -----------------------------------------------------------------------------
// Request/response types — shared with apiclient.
// -----------------------------------------------------------------------------

// TriggerBackupRequest is the body for POST /backups.
type TriggerBackupRequest struct {
	// Repo is the repo NAME (not ID). Required when the store has >1 repo
	// attached (D-24); optional otherwise.
	Repo string `json:"repo,omitempty"`
}

// TriggerBackupResponse is returned by POST /backups. Carries both the
// persisted BackupRecord (for `backup show`) and the BackupJob (for `--wait`
// polling). Returned with HTTP 202 Accepted (async-job semantics per REQ API-05).
type TriggerBackupResponse struct {
	Record *BackupRecordResponse `json:"record"`
	Job    *BackupJobResponse    `json:"job"`
}

// RestoreRequest is the body for POST /restore and POST /restore/dry-run.
type RestoreRequest struct {
	// FromBackupID must be a 26-char ULID when set (D-40). Empty = latest-succeeded.
	FromBackupID string `json:"from_backup_id,omitempty"`
}

// RestoreDryRunResponse mirrors storebackups.DryRunResult (D-31).
type RestoreDryRunResponse struct {
	Record        *BackupRecordResponse `json:"record"`
	ManifestValid bool                  `json:"manifest_valid"`
	EnabledShares []string              `json:"enabled_shares"`
}

// PatchBackupRecordRequest is the body for PATCH /backups/{id}. Nil Pinned
// means "unchanged" (D-23).
type PatchBackupRecordRequest struct {
	Pinned *bool `json:"pinned,omitempty"`
}

// BackupRecordResponse is the wire shape for a BackupRecord row.
type BackupRecordResponse struct {
	ID           string    `json:"id"`
	RepoID       string    `json:"repo_id"`
	CreatedAt    time.Time `json:"created_at"`
	SizeBytes    int64     `json:"size_bytes"`
	Status       string    `json:"status"`
	Pinned       bool      `json:"pinned"`
	ManifestPath string    `json:"manifest_path"`
	SHA256       string    `json:"sha256"`
	StoreID      string    `json:"store_id"`
	Error        string    `json:"error,omitempty"`
}

// BackupJobResponse is the wire shape for a BackupJob row.
type BackupJobResponse struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind"`
	RepoID         string     `json:"repo_id"`
	BackupRecordID *string    `json:"backup_record_id,omitempty"`
	Status         string     `json:"status"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	Error          string     `json:"error,omitempty"`
	Progress       int        `json:"progress"`
}

// BackupRepoRequest is the body for POST /repos and PATCH /repos/{repo}.
// Pointer fields signal partial-update semantics (D-19) on PATCH.
type BackupRepoRequest struct {
	Name              string         `json:"name"`
	Kind              string         `json:"kind"`
	Config            map[string]any `json:"config,omitempty"`
	Schedule          *string        `json:"schedule,omitempty"`
	KeepCount         *int           `json:"keep_count,omitempty"`
	KeepAgeDays       *int           `json:"keep_age_days,omitempty"`
	EncryptionEnabled *bool          `json:"encryption_enabled,omitempty"`
	EncryptionKeyRef  *string        `json:"encryption_key_ref,omitempty"`
}

// BackupRepoResponse is the wire shape for a BackupRepo row. Config is
// returned as-is; callers must NOT persist secret_access_key or similar
// credential fields via the Config map (admin-only route, D-32).
type BackupRepoResponse struct {
	ID                string         `json:"id"`
	TargetID          string         `json:"target_id"`
	TargetKind        string         `json:"target_kind"`
	Name              string         `json:"name"`
	Kind              string         `json:"kind"`
	Schedule          *string        `json:"schedule,omitempty"`
	KeepCount         *int           `json:"keep_count,omitempty"`
	KeepAgeDays       *int           `json:"keep_age_days,omitempty"`
	EncryptionEnabled bool           `json:"encryption_enabled"`
	EncryptionKeyRef  string         `json:"encryption_key_ref,omitempty"`
	Config            map[string]any `json:"config,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

// -----------------------------------------------------------------------------
// Backup records + trigger + restore + dry-run handlers
// -----------------------------------------------------------------------------

// TriggerBackup handles POST /api/v1/store/metadata/{name}/backups.
// Returns 202 Accepted + TriggerBackupResponse{Record, Job} on success.
// Conflict → 409 with running_job_id (D-13).
func (h *BackupHandler) TriggerBackup(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w) {
		return
	}
	storeName := chi.URLParam(r, "name")
	if storeName == "" {
		BadRequest(w, "Store name is required")
		return
	}

	var req TriggerBackupRequest
	// Empty body is permitted — decodeJSONBody would 400 on empty, so do a best-effort decode.
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			BadRequest(w, "Invalid request body")
			return
		}
	}

	storeCfg, err := h.store.GetMetadataStore(r.Context(), storeName)
	if err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Metadata store not found")
			return
		}
		InternalServerError(w, "Failed to get metadata store")
		return
	}

	repos, err := h.store.ListReposByTarget(r.Context(), "metadata", storeCfg.ID)
	if err != nil {
		InternalServerError(w, "Failed to list backup repos")
		return
	}
	if len(repos) == 0 {
		BadRequest(w, "No backup repos attached to this store")
		return
	}

	repo, err := h.selectRepo(repos, req.Repo)
	if err != nil {
		BadRequest(w, err.Error())
		return
	}

	rec, job, err := h.svc.RunBackup(r.Context(), repo.ID)
	if err != nil {
		h.writeBackupError(w, r.Context(), repo.ID, err)
		return
	}

	WriteJSON(w, http.StatusAccepted, TriggerBackupResponse{
		Record: recordToResponse(rec),
		Job:    jobToResponse(job),
	})
}

// selectRepo implements D-24: with one repo, empty req.Repo matches it; with
// multiple repos, req.Repo must match a repo Name exactly.
func (h *BackupHandler) selectRepo(repos []*models.BackupRepo, name string) (*models.BackupRepo, error) {
	if name == "" {
		if len(repos) == 1 {
			return repos[0], nil
		}
		names := make([]string, 0, len(repos))
		for _, r := range repos {
			names = append(names, r.Name)
		}
		return nil, fmt.Errorf("multiple repos attached — specify one of: %v", names)
	}
	for _, r := range repos {
		if r.Name == name {
			return r, nil
		}
	}
	return nil, fmt.Errorf("repo %q not attached to this store", name)
}

// writeBackupError maps storebackups sentinels to HTTP + writes the body. On
// ErrBackupAlreadyRunning we look up the running job ID and emit the typed
// problem (D-13).
func (h *BackupHandler) writeBackupError(w http.ResponseWriter, ctx context.Context, repoID string, err error) {
	switch {
	case errors.Is(err, storebackups.ErrBackupAlreadyRunning):
		// Resolve the running job ID best-effort via the store.
		runningID := ""
		if jobs, jerr := h.store.ListBackupJobsFiltered(ctx, store.BackupJobFilter{
			RepoID: repoID,
			Status: models.BackupStatusRunning,
			Limit:  1,
		}); jerr == nil && len(jobs) > 0 {
			runningID = jobs[0].ID
		}
		WriteBackupAlreadyRunningProblem(w, runningID)
	case errors.Is(err, storebackups.ErrRepoNotFound),
		errors.Is(err, models.ErrBackupRepoNotFound):
		NotFound(w, "Backup repo not found")
	case errors.Is(err, storebackups.ErrInvalidTargetKind):
		BadRequest(w, err.Error())
	default:
		logger.Error("Backup run failed", "repo_id", repoID, "error", err)
		InternalServerError(w, "Backup run failed")
	}
}

// ListRecords handles GET /api/v1/store/metadata/{name}/backups.
// Query parameters: ?repo=<name> and ?status=<BackupStatus>.
func (h *BackupHandler) ListRecords(w http.ResponseWriter, r *http.Request) {
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

	repoName := r.URL.Query().Get("repo")
	statusParam := r.URL.Query().Get("status")
	statusFilter, ok := parseBackupStatus(statusParam, true)
	if !ok {
		BadRequest(w, "invalid status filter")
		return
	}

	// If a repo name is provided, resolve to a specific repo and filter by ID.
	// If not and there's exactly one repo, use that; otherwise union across all repos.
	repos, err := h.store.ListReposByTarget(r.Context(), "metadata", storeCfg.ID)
	if err != nil {
		InternalServerError(w, "Failed to list backup repos")
		return
	}
	if repoName != "" {
		var found *models.BackupRepo
		for _, rp := range repos {
			if rp.Name == repoName {
				found = rp
				break
			}
		}
		if found == nil {
			NotFound(w, fmt.Sprintf("Backup repo %q not found", repoName))
			return
		}
		recs, err := h.store.ListBackupRecords(r.Context(), found.ID, statusFilter)
		if err != nil {
			InternalServerError(w, "Failed to list backup records")
			return
		}
		WriteJSONOK(w, recordsToResponses(recs))
		return
	}

	var all []*models.BackupRecord
	for _, rp := range repos {
		recs, err := h.store.ListBackupRecords(r.Context(), rp.ID, statusFilter)
		if err != nil {
			InternalServerError(w, "Failed to list backup records")
			return
		}
		all = append(all, recs...)
	}
	WriteJSONOK(w, recordsToResponses(all))
}

// ShowRecord handles GET /api/v1/store/metadata/{name}/backups/{id}.
func (h *BackupHandler) ShowRecord(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w) {
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		BadRequest(w, "Record id is required")
		return
	}
	rec, err := h.store.GetBackupRecord(r.Context(), id)
	if err != nil {
		if errors.Is(err, models.ErrBackupRecordNotFound) {
			NotFound(w, "Backup record not found")
			return
		}
		InternalServerError(w, "Failed to get backup record")
		return
	}
	WriteJSONOK(w, recordToResponse(rec))
}

// PatchRecord handles PATCH /api/v1/store/metadata/{name}/backups/{id}.
// Body: {"pinned": bool}. Only the pinned field is patchable today (D-23).
func (h *BackupHandler) PatchRecord(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w) {
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		BadRequest(w, "Record id is required")
		return
	}

	var req PatchBackupRecordRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Pinned == nil {
		BadRequest(w, "no patch fields provided")
		return
	}

	if err := h.store.UpdateBackupRecordPinned(r.Context(), id, *req.Pinned); err != nil {
		if errors.Is(err, models.ErrBackupRecordNotFound) {
			NotFound(w, "Backup record not found")
			return
		}
		InternalServerError(w, "Failed to update backup record")
		return
	}
	rec, err := h.store.GetBackupRecord(r.Context(), id)
	if err != nil {
		InternalServerError(w, "Failed to reload backup record")
		return
	}
	WriteJSONOK(w, recordToResponse(rec))
}

// Restore handles POST /api/v1/store/metadata/{name}/restore.
// Returns 202 Accepted + BackupJobResponse on success.
func (h *BackupHandler) Restore(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w) {
		return
	}
	storeName := chi.URLParam(r, "name")
	if storeName == "" {
		BadRequest(w, "Store name is required")
		return
	}

	var req RestoreRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			BadRequest(w, "Invalid request body")
			return
		}
	}

	recordIDPtr, ok := validateOptionalULID(w, req.FromBackupID)
	if !ok {
		return
	}

	repo, ok := h.resolveSingleRepoFromStore(w, r, storeName)
	if !ok {
		return
	}

	job, err := h.svc.RunRestore(r.Context(), repo.ID, recordIDPtr)
	if err != nil {
		h.writeRestoreError(w, err)
		return
	}

	WriteJSON(w, http.StatusAccepted, jobToResponse(job))
}

// RestoreDryRun handles POST /api/v1/store/metadata/{name}/restore/dry-run.
// Returns 200 OK + RestoreDryRunResponse. SKIPS the shares-enabled gate (D-31)
// — the enabled_shares list is reported, never enforced.
func (h *BackupHandler) RestoreDryRun(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w) {
		return
	}
	storeName := chi.URLParam(r, "name")
	if storeName == "" {
		BadRequest(w, "Store name is required")
		return
	}

	var req RestoreRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			BadRequest(w, "Invalid request body")
			return
		}
	}

	recordIDPtr, ok := validateOptionalULID(w, req.FromBackupID)
	if !ok {
		return
	}

	repo, ok := h.resolveSingleRepoFromStore(w, r, storeName)
	if !ok {
		return
	}

	result, err := h.svc.RunRestoreDryRun(r.Context(), repo.ID, recordIDPtr)
	if err != nil {
		h.writeRestoreError(w, err)
		return
	}

	WriteJSONOK(w, RestoreDryRunResponse{
		Record:        recordToResponse(result.Record),
		ManifestValid: result.ManifestValid,
		EnabledShares: result.EnabledShares,
	})
}

// resolveSingleRepoFromStore resolves the single repo attached to the named
// metadata store. Writes a problem response and returns ok=false on error or
// on multi-repo ambiguity (restore does not yet support repo selection).
func (h *BackupHandler) resolveSingleRepoFromStore(w http.ResponseWriter, r *http.Request, storeName string) (*models.BackupRepo, bool) {
	storeCfg, err := h.store.GetMetadataStore(r.Context(), storeName)
	if err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Metadata store not found")
			return nil, false
		}
		InternalServerError(w, "Failed to get metadata store")
		return nil, false
	}
	repos, err := h.store.ListReposByTarget(r.Context(), "metadata", storeCfg.ID)
	if err != nil {
		InternalServerError(w, "Failed to list backup repos")
		return nil, false
	}
	if len(repos) == 0 {
		BadRequest(w, "No backup repos attached to this store")
		return nil, false
	}
	if len(repos) > 1 {
		BadRequest(w, "Multiple backup repos attached; restore requires a single-repo store")
		return nil, false
	}
	return repos[0], true
}

// writeRestoreError maps restore-path sentinels to HTTP + writes the body.
func (h *BackupHandler) writeRestoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storebackups.ErrRestorePreconditionFailed):
		WriteRestorePreconditionFailedProblem(w, extractEnabledShares(err))
	case errors.Is(err, storebackups.ErrNoRestoreCandidate),
		errors.Is(err, storebackups.ErrRecordNotRestorable),
		errors.Is(err, storebackups.ErrBackupAlreadyRunning):
		Conflict(w, err.Error())
	case errors.Is(err, storebackups.ErrStoreIDMismatch),
		errors.Is(err, storebackups.ErrStoreKindMismatch),
		errors.Is(err, storebackups.ErrRecordRepoMismatch),
		errors.Is(err, storebackups.ErrManifestVersionUnsupported):
		BadRequest(w, err.Error())
	case errors.Is(err, storebackups.ErrRepoNotFound),
		errors.Is(err, models.ErrBackupRepoNotFound),
		errors.Is(err, models.ErrBackupRecordNotFound):
		NotFound(w, err.Error())
	default:
		logger.Error("Restore run failed", "error", err)
		InternalServerError(w, "Restore run failed")
	}
}

// extractEnabledShares pulls the enabled_shares slice out of a wrapped
// ErrRestorePreconditionFailed error. The runtime formats the error as
// `fmt.Errorf("%w: store %q has %d enabled share(s): %v", …, enabled)` so
// we round-trip via a typed error where possible and fall back to nil.
func extractEnabledShares(err error) []string {
	type enabledSharesCarrier interface{ EnabledShares() []string }
	var carrier enabledSharesCarrier
	if errors.As(err, &carrier) {
		return carrier.EnabledShares()
	}
	return nil
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// validateOptionalULID enforces D-40: if provided, from_backup_id must be a
// 26-character ULID. Writes a BadRequest and returns ok=false on invalid
// input; returns a nil pointer for empty input.
func validateOptionalULID(w http.ResponseWriter, id string) (*string, bool) {
	if id == "" {
		return nil, true
	}
	if len(id) != 26 {
		BadRequest(w, "from_backup_id must be a 26-character ULID")
		return nil, false
	}
	s := id
	return &s, true
}

// parseBackupStatus validates a BackupStatus query parameter. Empty string is
// always acceptable (no filter). Returns the enum value + ok=true on success.
func parseBackupStatus(s string, allowEmpty bool) (models.BackupStatus, bool) {
	if s == "" {
		if allowEmpty {
			return "", true
		}
		return "", false
	}
	switch models.BackupStatus(s) {
	case models.BackupStatusPending,
		models.BackupStatusRunning,
		models.BackupStatusSucceeded,
		models.BackupStatusFailed,
		models.BackupStatusInterrupted:
		return models.BackupStatus(s), true
	default:
		return "", false
	}
}

// parseBackupJobKind validates a BackupJobKind query parameter.
func parseBackupJobKind(s string, allowEmpty bool) (models.BackupJobKind, bool) {
	if s == "" {
		if allowEmpty {
			return "", true
		}
		return "", false
	}
	switch models.BackupJobKind(s) {
	case models.BackupJobKindBackup, models.BackupJobKindRestore:
		return models.BackupJobKind(s), true
	default:
		return "", false
	}
}

// parseLimit parses a non-negative integer limit from a query string; returns
// 0 on empty (store layer applies the 50/200 default/cap). Returns -1 and
// ok=false on invalid (non-numeric) input.
func parseLimit(s string) (int, bool) {
	if s == "" {
		return 0, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return -1, false
	}
	return n, true
}

// -----------------------------------------------------------------------------
// Model → response conversions
// -----------------------------------------------------------------------------

func recordToResponse(r *models.BackupRecord) *BackupRecordResponse {
	if r == nil {
		return nil
	}
	return &BackupRecordResponse{
		ID:           r.ID,
		RepoID:       r.RepoID,
		CreatedAt:    r.CreatedAt,
		SizeBytes:    r.SizeBytes,
		Status:       string(r.Status),
		Pinned:       r.Pinned,
		ManifestPath: r.ManifestPath,
		SHA256:       r.SHA256,
		StoreID:      r.StoreID,
		Error:        r.Error,
	}
}

func recordsToResponses(list []*models.BackupRecord) []*BackupRecordResponse {
	out := make([]*BackupRecordResponse, 0, len(list))
	for _, r := range list {
		out = append(out, recordToResponse(r))
	}
	return out
}

func jobToResponse(j *models.BackupJob) *BackupJobResponse {
	if j == nil {
		return nil
	}
	return &BackupJobResponse{
		ID:             j.ID,
		Kind:           string(j.Kind),
		RepoID:         j.RepoID,
		BackupRecordID: j.BackupRecordID,
		Status:         string(j.Status),
		StartedAt:      j.StartedAt,
		FinishedAt:     j.FinishedAt,
		Error:          j.Error,
		Progress:       j.Progress,
	}
}

func jobsToResponses(list []*models.BackupJob) []*BackupJobResponse {
	out := make([]*BackupJobResponse, 0, len(list))
	for _, j := range list {
		out = append(out, jobToResponse(j))
	}
	return out
}

func repoToResponse(r *models.BackupRepo) *BackupRepoResponse {
	if r == nil {
		return nil
	}
	cfg, _ := r.GetConfig()
	return &BackupRepoResponse{
		ID:                r.ID,
		TargetID:          r.TargetID,
		TargetKind:        r.TargetKind,
		Name:              r.Name,
		Kind:              string(r.Kind),
		Schedule:          r.Schedule,
		KeepCount:         r.KeepCount,
		KeepAgeDays:       r.KeepAgeDays,
		EncryptionEnabled: r.EncryptionEnabled,
		EncryptionKeyRef:  r.EncryptionKeyRef,
		Config:            cfg,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
}

func reposToResponses(list []*models.BackupRepo) []*BackupRepoResponse {
	out := make([]*BackupRepoResponse, 0, len(list))
	for _, r := range list {
		out = append(out, repoToResponse(r))
	}
	return out
}
