package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/api/dto"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// SnapshotRuntime is the narrow Runtime surface SnapshotHandler depends on.
// Defining the interface inside the handler package keeps the unit tests
// independent of *runtime.Runtime — tests substitute a fake that records
// calls and returns canned responses (mirrors BlockGCRuntime).
type SnapshotRuntime interface {
	CreateSnapshot(ctx context.Context, share string, opts runtime.CreateSnapshotOpts) (string, error)
	WaitForSnapshot(ctx context.Context, share, snapID string) (*models.Snapshot, error)
	RestoreSnapshot(ctx context.Context, share, snapID string, opts runtime.RestoreSnapshotOpts) (string, error)
	GetSnapshot(ctx context.Context, share, snapID string) (*models.Snapshot, error)
	ListSnapshots(ctx context.Context, share string) ([]*models.Snapshot, error)
	DeleteSnapshot(ctx context.Context, share, snapID string) error
}

// SnapshotHandler serves the per-share REST surface for snapshot
// create/list/show/delete/restore. All routes inherit the parent
// /api/v1/shares group's RequireAdmin middleware.
type SnapshotHandler struct {
	runtime            SnapshotRuntime
	restoreHTTPTimeout time.Duration
	localStoreDirFn    func(share string) (string, error)
}

// NewSnapshotHandler constructs a handler bound to the given Runtime
// surface. restoreHTTPTimeout bounds each Restore request's context.
// localStoreDirFn resolves the on-disk share root so the show endpoint
// can compute manifest hash count + dump bytes lazily.
func NewSnapshotHandler(
	rt SnapshotRuntime,
	restoreHTTPTimeout time.Duration,
	localStoreDirFn func(string) (string, error),
) *SnapshotHandler {
	return &SnapshotHandler{
		runtime:            rt,
		restoreHTTPTimeout: restoreHTTPTimeout,
		localStoreDirFn:    localStoreDirFn,
	}
}

// Create handles POST /api/v1/shares/{name}/snapshots. Returns 202 with
// a Location header pointing at the per-snapshot show URL.
func (h *SnapshotHandler) Create(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}
	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "" {
		BadRequest(w, "share name is required")
		return
	}

	var req dto.CreateSnapshotRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			BadRequest(w, "invalid request body: "+err.Error())
			return
		}
	}

	opts := runtime.CreateSnapshotOpts{
		NoVerify: req.NoVerify,
		RetryOf:  req.RetryOf,
	}
	snapID, err := h.runtime.CreateSnapshot(r.Context(), name, opts)
	if err != nil {
		if mapSnapshotError(w, err) {
			return
		}
		logger.Debug("Snapshot create error", "share", name, "error", err)
		InternalServerError(w, "snapshot create failed")
		return
	}

	w.Header().Set(
		"Location",
		fmt.Sprintf("/api/v1/shares/%s/snapshots/%s",
			url.PathEscape(name), url.PathEscape(snapID)),
	)
	WriteJSONAccepted(w, dto.CreateSnapshotResponse{
		SnapshotID: snapID,
		Share:      name,
	})
}

// List handles GET /api/v1/shares/{name}/snapshots. Returns 200 with a
// JSON array of dto.Snapshot. Empty share returns [], not null.
func (h *SnapshotHandler) List(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}
	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "" {
		BadRequest(w, "share name is required")
		return
	}

	snaps, err := h.runtime.ListSnapshots(r.Context(), name)
	if err != nil {
		if mapSnapshotError(w, err) {
			return
		}
		logger.Debug("Snapshot list error", "share", name, "error", err)
		InternalServerError(w, "snapshot list failed")
		return
	}
	out := make([]dto.Snapshot, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, h.toWire(s, false))
	}
	WriteJSONOK(w, out)
}

// Get handles GET /api/v1/shares/{name}/snapshots/{id}. Returns 200 with
// a full dto.Snapshot (including manifest_count + dump_bytes pulled from
// disk on demand) or 404 if the snapshot is missing.
func (h *SnapshotHandler) Get(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}
	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "" {
		BadRequest(w, "share name is required")
		return
	}
	snapID := chi.URLParam(r, "id")
	if snapID == "" {
		BadRequest(w, "snapshot id is required")
		return
	}

	snap, err := h.runtime.GetSnapshot(r.Context(), name, snapID)
	if err != nil {
		if mapSnapshotError(w, err) {
			return
		}
		logger.Debug("Snapshot get error", "share", name, "snapshot_id", snapID, "error", err)
		InternalServerError(w, "snapshot get failed")
		return
	}
	WriteJSONOK(w, h.toWire(snap, true))
}

// Delete handles DELETE /api/v1/shares/{name}/snapshots/{id}. Returns
// 204 No Content on success.
func (h *SnapshotHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}
	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "" {
		BadRequest(w, "share name is required")
		return
	}
	snapID := chi.URLParam(r, "id")
	if snapID == "" {
		BadRequest(w, "snapshot id is required")
		return
	}

	if err := h.runtime.DeleteSnapshot(r.Context(), name, snapID); err != nil {
		if mapSnapshotError(w, err) {
			return
		}
		logger.Debug("Snapshot delete error", "share", name, "snapshot_id", snapID, "error", err)
		InternalServerError(w, "snapshot delete failed")
		return
	}
	WriteNoContent(w)
}

// Restore handles POST /api/v1/shares/{name}/snapshots/{id}/restore.
// Returns 200 with the restored snapshot id, the share, and the safety
// snapshot id surfaced directly from the runtime's first return value.
func (h *SnapshotHandler) Restore(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}
	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "" {
		BadRequest(w, "share name is required")
		return
	}
	snapID := chi.URLParam(r, "id")
	if snapID == "" {
		BadRequest(w, "snapshot id is required")
		return
	}

	var body dto.RestoreSnapshotRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			BadRequest(w, "invalid request body: "+err.Error())
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.restoreHTTPTimeout)
	defer cancel()

	opts := runtime.RestoreSnapshotOpts{AllowNonDurable: body.AllowNonDurable}
	safetyID, err := h.runtime.RestoreSnapshot(ctx, name, snapID, opts)
	if err != nil {
		if mapSnapshotError(w, err) {
			return
		}
		logger.Debug("Snapshot restore error", "share", name, "snapshot_id", snapID, "error", err)
		InternalServerError(w, "snapshot restore failed")
		return
	}
	WriteJSONOK(w, dto.RestoreSnapshotResponse{
		SnapshotID:       snapID,
		SafetySnapshotID: safetyID,
		Share:            name,
	})
}

// toWire converts a models.Snapshot into the wire DTO. When includeDisk
// is true the manifest hash count + dump byte count are read from disk;
// errors there are logged at Debug and the fields stay zero (do not 500
// the show endpoint because a snapshot's on-disk artifacts are gone).
func (h *SnapshotHandler) toWire(s *models.Snapshot, includeDisk bool) dto.Snapshot {
	out := dto.Snapshot{
		ID:            s.ID,
		Share:         s.ShareName,
		State:         s.State,
		RemoteDurable: s.RemoteDurable,
		CreatedAt:     s.CreatedAt,
		UpdatedAt:     s.UpdatedAt,
	}
	if !includeDisk || h.localStoreDirFn == nil {
		return out
	}
	localStoreDir, err := h.localStoreDirFn(s.ShareName)
	if err != nil || localStoreDir == "" {
		logger.Debug("snapshot toWire: local store dir unavailable",
			"share", s.ShareName, "snapshot_id", s.ID, "err", err)
		return out
	}
	if info, err := os.Stat(s.MetadataDumpPath(localStoreDir)); err == nil {
		out.DumpBytes = info.Size()
	} else {
		logger.Debug("snapshot toWire: stat dump",
			"share", s.ShareName, "snapshot_id", s.ID, "err", err)
	}
	if f, err := os.Open(s.ManifestPath(localStoreDir)); err == nil {
		hs, rerr := snapshot.ReadManifest(f)
		_ = f.Close()
		if rerr == nil {
			out.ManifestCount = hs.Len()
		} else {
			logger.Debug("snapshot toWire: read manifest",
				"share", s.ShareName, "snapshot_id", s.ID, "err", rerr)
		}
	} else {
		logger.Debug("snapshot toWire: open manifest",
			"share", s.ShareName, "snapshot_id", s.ID, "err", err)
	}
	return out
}

// mapSnapshotError writes the canonical HTTP response for a typed
// snapshot/restore sentinel and returns true. Returns false when err is
// nil or unmapped, letting the caller fall through to a sanitized 500.
//
// Sanitization: each branch writes a fixed operator-friendly message —
// the original err is never interpolated into the response body. Callers
// log err at Debug for postmortems.
func mapSnapshotError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, models.ErrSnapshotNotFound):
		NotFound(w, "snapshot not found")
		return true
	case errors.Is(err, shares.ErrShareNotFound):
		NotFound(w, "share not found")
		return true
	case errors.Is(err, models.ErrShareEnabled):
		Conflict(w, "share is enabled; disable before restore")
		return true
	case errors.Is(err, models.ErrSnapshotNotDurable):
		PreconditionFailed(w, "snapshot not remotely durable; pass allow_non_durable=true to force")
		return true
	case errors.Is(err, models.ErrSnapshotRetryTargetNotFound):
		NotFound(w, "retry target snapshot not found")
		return true
	case errors.Is(err, models.ErrSnapshotRetryTargetNotFailed):
		Conflict(w, "retry target is not in failed state")
		return true
	case errors.Is(err, models.ErrSnapshotStateConflict):
		Conflict(w, "snapshot is not in a state that allows this operation")
		return true
	case errors.Is(err, models.ErrSnapshotDrainTimeout):
		GatewayTimeout(w, "upload drain timed out")
		return true
	case errors.Is(err, models.ErrSnapshotMetadataDumpMissing):
		InternalServerError(w, "snapshot artifacts missing")
		return true
	case errors.Is(err, models.ErrMetadataStoreNotResetable):
		InternalServerError(w, "backend does not support reset")
		return true
	case errors.Is(err, models.ErrSnapshotBackupFailed),
		errors.Is(err, models.ErrSnapshotVerifyFailed),
		errors.Is(err, models.ErrRestoreSafetySnapFailed),
		errors.Is(err, models.ErrRestoreAborted),
		errors.Is(err, models.ErrRestoreVerifyFailed):
		InternalServerError(w, "snapshot operation failed")
		return true
	}
	return false
}
