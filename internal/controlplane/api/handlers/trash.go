package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/trash"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// trashService is the narrow trash.Service surface TrashHandler depends on.
// Defining it in the handler package keeps the unit tests independent of a
// full *runtime.Runtime — tests substitute a fake that records calls and
// returns canned responses (mirrors SnapshotRuntime / BlockGCRuntime). The
// real *trash.Service satisfies it directly.
type trashService interface {
	List(ctx *metadata.AuthContext, shareName string) ([]trash.Entry, error)
	Restore(ctx *metadata.AuthContext, shareName, binPath, dest string) error
	Empty(ctx *metadata.AuthContext, shareName string, force bool) (int, error)
	Status(ctx *metadata.AuthContext, shareName string) (*trash.Status, error)
}

// TrashHandler serves the per-share REST recycle-bin surface:
// list / restore / empty / status. All routes inherit the parent
// /api/v1/shares group's RequireAdmin middleware.
//
// The /shares group is admin-gated, so every endpoint here — Empty in
// particular — is admin-only by construction. This is the API-side
// enforcement point for the share's RestrictToAdmin policy: destructive and
// administrative trash operations live behind the admin REST surface, while
// end-user restore of one's own deletions happens over the mount (the
// metadata layer's recycle/restore path), not here.
type TrashHandler struct {
	rt *runtime.Runtime
	// svc, when set, overrides the runtime-resolved service. Tests inject a
	// fake here; production leaves it nil and resolves rt.Trash() lazily so a
	// nil/partially-built runtime (e.g. the router lifecycle test) does not
	// panic at construction time.
	svc trashService
}

// NewTrashHandler constructs a handler bound to the Runtime's recycle-bin
// service. The service is resolved lazily (rt.Trash()) on first request, not
// at construction, so the router can be built before the runtime is fully
// initialized.
func NewTrashHandler(rt *runtime.Runtime) *TrashHandler {
	return &TrashHandler{rt: rt}
}

// service resolves the trash service: the injected test override if present,
// otherwise rt.Trash(). Returns nil when neither is available.
func (h *TrashHandler) service() trashService {
	if h.svc != nil {
		return h.svc
	}
	if h.rt == nil {
		return nil
	}
	return h.rt.Trash()
}

// trashAuthContext derives the AuthContext threaded into the trash service.
//
// These endpoints sit behind RequireAdmin (JWT bearer auth at the REST
// boundary), not behind a mount's UNIX/Kerberos principal. There is no
// filesystem principal to thread, so operations run as the system identity —
// the same convention the runtime's own trash integration test and reaper use
// (metadata.NewSystemAuthContext). Per-user authorization for trash is the
// admin gate on this route group; finer-grained restore lives over the mount.
func trashAuthContext(r *http.Request) *metadata.AuthContext {
	return metadata.NewSystemAuthContext(r.Context())
}

// restoreTrashRequest is the POST /restore body.
type restoreTrashRequest struct {
	// BinPath identifies the recycled root to restore (its path under #recycle).
	BinPath string `json:"bin_path"`
	// To is the optional share-relative restore destination; empty restores to
	// the entry's recorded original path.
	To string `json:"to"`
}

// emptyTrashRequest is the POST /empty body. force is advisory at the service
// layer; the admin gate is the real authorization on this route.
type emptyTrashRequest struct {
	Force bool `json:"force"`
}

// emptyTrashResponse reports how many recycled roots were purged.
type emptyTrashResponse struct {
	Removed int `json:"removed"`
}

// resolveShare extracts and validates the share name from the URL, returning
// it alongside the resolved trash service. On any precondition failure it
// writes an error response and returns "" / nil; the caller must then bail.
func (h *TrashHandler) resolveShare(w http.ResponseWriter, r *http.Request) (string, trashService) {
	svc := h.service()
	if svc == nil {
		InternalServerError(w, "trash service not initialized")
		return "", nil
	}
	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "" {
		BadRequest(w, "share name is required")
		return "", nil
	}
	return name, svc
}

// List handles GET /api/v1/shares/{name}/trash. Returns 200 with a JSON array
// of trash.Entry (empty array, not null, when the bin is empty).
func (h *TrashHandler) List(w http.ResponseWriter, r *http.Request) {
	name, svc := h.resolveShare(w, r)
	if name == "" {
		return
	}
	entries, err := svc.List(trashAuthContext(r), name)
	if err != nil {
		h.handleErr(w, "trash list", name, err)
		return
	}
	if entries == nil {
		entries = []trash.Entry{}
	}
	WriteJSONOK(w, entries)
}

// Restore handles POST /api/v1/shares/{name}/trash/restore. Returns 204 No
// Content on success, 409 Conflict when the destination is occupied, and 404
// when the share or entry is unknown.
func (h *TrashHandler) Restore(w http.ResponseWriter, r *http.Request) {
	name, svc := h.resolveShare(w, r)
	if name == "" {
		return
	}
	var req restoreTrashRequest
	if !decodeTrashBody(w, r, &req) {
		return
	}
	if req.BinPath == "" {
		BadRequest(w, "bin_path is required")
		return
	}
	if err := svc.Restore(trashAuthContext(r), name, req.BinPath, req.To); err != nil {
		h.handleErr(w, "trash restore", name, err)
		return
	}
	WriteNoContent(w)
}

// Empty handles POST /api/v1/shares/{name}/trash/empty. Admin-only by virtue
// of the parent /shares group's RequireAdmin middleware. Returns 200 with the
// number of recycled roots removed.
func (h *TrashHandler) Empty(w http.ResponseWriter, r *http.Request) {
	name, svc := h.resolveShare(w, r)
	if name == "" {
		return
	}
	var req emptyTrashRequest
	if !decodeTrashBody(w, r, &req) {
		return
	}
	removed, err := svc.Empty(trashAuthContext(r), name, req.Force)
	if err != nil {
		h.handleErr(w, "trash empty", name, err)
		return
	}
	WriteJSONOK(w, emptyTrashResponse{Removed: removed})
}

// Status handles GET /api/v1/shares/{name}/trash/status. Returns 200 with the
// bin roll-up (enabled, item_count, total_bytes, oldest).
func (h *TrashHandler) Status(w http.ResponseWriter, r *http.Request) {
	name, svc := h.resolveShare(w, r)
	if name == "" {
		return
	}
	status, err := svc.Status(trashAuthContext(r), name)
	if err != nil {
		h.handleErr(w, "trash status", name, err)
		return
	}
	WriteJSONOK(w, status)
}

// handleErr maps a trash service error to the canonical HTTP response:
// unknown share / entry -> 404, restore-destination conflict -> 409, anything
// else -> sanitized 500 (the original err is logged at Error, never returned
// in the body).
func (h *TrashHandler) handleErr(w http.ResponseWriter, op, share string, err error) {
	switch {
	case metadata.IsNotFoundError(err):
		NotFound(w, "share or recycled entry not found")
		return
	case isTrashConflict(err):
		Conflict(w, "restore destination already exists")
		return
	}
	logger.Error(op+" error", "share", share, "error", err)
	InternalServerError(w, op+" failed")
}

// isTrashConflict reports whether err is a StoreError carrying the
// AlreadyExists code (restore onto an occupied destination). The metadata
// package surfaces "destination exists" as a coded StoreError, not a sentinel.
func isTrashConflict(err error) bool {
	var se *metadata.StoreError
	return errors.As(err, &se) && se.Code == metadata.ErrAlreadyExists
}

// decodeTrashBody decodes an optional JSON request body. Returns false (with a
// 400 already written) on a malformed body; an empty/EOF body is treated as
// the zero value and returns true.
func decodeTrashBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		BadRequest(w, "invalid request body: "+err.Error())
		return false
	}
	return true
}
