package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// QuotaHandlerStore is the minimal interface required by QuotaHandler: just the
// per-identity quota CRUD methods. Mirrors how ShareHandlerStore composes only
// the sub-interfaces it needs.
type QuotaHandlerStore interface {
	store.QuotaStore
}

// QuotaHandler handles per-identity (user/group/default-user) quota API
// endpoints. It needs the runtime to push hot-updates into the live metadata
// service after a DB write/delete and to read live usage for responses.
type QuotaHandler struct {
	store   QuotaHandlerStore
	runtime *runtime.Runtime
}

// NewQuotaHandler creates a new QuotaHandler.
func NewQuotaHandler(s QuotaHandlerStore, rt *runtime.Runtime) *QuotaHandler {
	return &QuotaHandler{store: s, runtime: rt}
}

// UpsertQuotaRequest is the request body for PUT
// /api/v1/shares/{name}/quotas/{scope}[/{id}]. Byte ceilings are
// human-readable (e.g. "10GiB"); file/grace fields are plain integers. A full
// replace is performed: omitted byte fields parse to 0 ("no limit").
type UpsertQuotaRequest struct {
	LimitBytes   string `json:"limit_bytes,omitempty"`
	SoftBytes    string `json:"soft_bytes,omitempty"`
	LimitFiles   int64  `json:"limit_files,omitempty"`
	SoftFiles    int64  `json:"soft_files,omitempty"`
	GraceSeconds int64  `json:"grace_seconds,omitempty"`
}

// QuotaResponse is the response body for quota endpoints. Byte ceilings are
// rendered human-readable; live usage (used_bytes / used_files) is read from the
// metadata store backing the share.
type QuotaResponse struct {
	ShareName  string  `json:"share_name"`
	Scope      string  `json:"scope"`
	IdentityID *uint32 `json:"identity_id,omitempty"`
	// LimitBytes / SoftBytes are human-readable, omitempty when unlimited/none.
	LimitBytes     string     `json:"limit_bytes,omitempty"`
	SoftBytes      string     `json:"soft_bytes,omitempty"`
	LimitFiles     int64      `json:"limit_files"`
	SoftFiles      int64      `json:"soft_files"`
	GraceSeconds   int64      `json:"grace_seconds"`
	GraceStartedAt *time.Time `json:"grace_started_at,omitempty"`
	// UsedBytes / UsedFiles are the live per-identity usage. No omitempty: 0 is
	// meaningful (no usage yet) and consumers render it explicitly.
	UsedBytes int64 `json:"used_bytes"`
	UsedFiles int64 `json:"used_files"`
}

// isValidQuotaScope reports whether scope is one of the supported quota scopes.
func isValidQuotaScope(scope string) bool {
	switch scope {
	case models.QuotaScopeUser, models.QuotaScopeGroup, models.QuotaScopeDefaultUser:
		return true
	default:
		return false
	}
}

// parseIdentityID resolves the {id} route param into the *uint32 identity used
// by the store. For the default-user scope identityID is always nil (and any id
// segment is ignored). For user/group scopes an id is required and must parse as
// a uint32. Returns ok=false (after writing a problem response) on invalid
// input.
func parseIdentityID(w http.ResponseWriter, scope, idParam string) (identityID *uint32, ok bool) {
	if scope == models.QuotaScopeDefaultUser {
		return nil, true
	}
	if idParam == "" {
		BadRequest(w, "identity id is required for scope "+scope)
		return nil, false
	}
	parsed, err := strconv.ParseUint(idParam, 10, 32)
	if err != nil {
		BadRequest(w, "Invalid identity id: "+idParam)
		return nil, false
	}
	v := uint32(parsed)
	return &v, true
}

// parseQuotaTarget resolves the {name}, {scope} and {id} route params shared by
// the Get/Set/Delete handlers, validating each in turn. It writes a problem
// response and returns ok=false on any invalid input, following the same
// convention as parseIdentityID.
func parseQuotaTarget(w http.ResponseWriter, r *http.Request) (shareName, scope string, identityID *uint32, ok bool) {
	shareName = normalizeShareName(chi.URLParam(r, "name"))
	if shareName == "/" {
		BadRequest(w, "Share name is required")
		return "", "", nil, false
	}
	scope = chi.URLParam(r, "scope")
	if !isValidQuotaScope(scope) {
		BadRequest(w, "Invalid scope: "+scope+" (want user|group|default-user)")
		return "", "", nil, false
	}
	identityID, ok = parseIdentityID(w, scope, chi.URLParam(r, "id"))
	if !ok {
		return "", "", nil, false
	}
	return shareName, scope, identityID, true
}

// List handles GET /api/v1/shares/{name}/quotas.
func (h *QuotaHandler) List(w http.ResponseWriter, r *http.Request) {
	shareName := normalizeShareName(chi.URLParam(r, "name"))
	if shareName == "/" {
		BadRequest(w, "Share name is required")
		return
	}

	quotas, err := h.store.ListQuotas(r.Context(), shareName)
	if err != nil {
		InternalServerError(w, "Failed to list quotas")
		return
	}

	response := make([]QuotaResponse, len(quotas))
	for i, q := range quotas {
		response[i] = h.quotaToResponse(q)
	}
	WriteJSONOK(w, response)
}

// Get handles GET /api/v1/shares/{name}/quotas/{scope}[/{id}].
func (h *QuotaHandler) Get(w http.ResponseWriter, r *http.Request) {
	shareName, scope, identityID, ok := parseQuotaTarget(w, r)
	if !ok {
		return
	}

	quota, err := h.store.GetQuota(r.Context(), shareName, scope, identityID)
	if err != nil {
		if errors.Is(err, models.ErrQuotaNotFound) {
			NotFound(w, "Quota not found")
			return
		}
		InternalServerError(w, "Failed to get quota")
		return
	}
	WriteJSONOK(w, h.quotaToResponse(quota))
}

// Set handles PUT /api/v1/shares/{name}/quotas/{scope}[/{id}] (create/update).
func (h *QuotaHandler) Set(w http.ResponseWriter, r *http.Request) {
	shareName, scope, identityID, ok := parseQuotaTarget(w, r)
	if !ok {
		return
	}

	var req UpsertQuotaRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	var limitBytes, softBytes int64
	if req.LimitBytes != "" {
		bs, err := bytesize.ParseByteSize(req.LimitBytes)
		if err != nil {
			BadRequest(w, "Invalid limit_bytes: "+err.Error())
			return
		}
		// ParseByteSize returns a uint64-backed value; reject sizes that would
		// overflow the int64 limit column (and be treated as "no limit").
		if bs.Int64() < 0 {
			BadRequest(w, "limit_bytes too large")
			return
		}
		limitBytes = bs.Int64()
	}
	if req.SoftBytes != "" {
		bs, err := bytesize.ParseByteSize(req.SoftBytes)
		if err != nil {
			BadRequest(w, "Invalid soft_bytes: "+err.Error())
			return
		}
		if bs.Int64() < 0 {
			BadRequest(w, "soft_bytes too large")
			return
		}
		softBytes = bs.Int64()
	}
	if req.LimitFiles < 0 || req.SoftFiles < 0 || req.GraceSeconds < 0 {
		BadRequest(w, "limit_files, soft_files and grace_seconds must be >= 0")
		return
	}
	// Enforce the soft <= hard invariant when both dimensions are set.
	if limitBytes > 0 && softBytes > 0 && softBytes > limitBytes {
		BadRequest(w, "soft_bytes must not exceed limit_bytes")
		return
	}
	if req.LimitFiles > 0 && req.SoftFiles > 0 && req.SoftFiles > req.LimitFiles {
		BadRequest(w, "soft_files must not exceed limit_files")
		return
	}

	// Preserve an existing grace timer across an update so a soft-threshold
	// timer already running is not reset by a limit edit.
	var graceStartedAt *time.Time
	if existing, err := h.store.GetQuota(r.Context(), shareName, scope, identityID); err == nil {
		graceStartedAt = existing.GraceStartedAt
	}

	now := time.Now()
	quota := &models.Quota{
		ID:             uuid.New().String(),
		ShareName:      shareName,
		Scope:          scope,
		IdentityID:     identityID,
		LimitBytes:     limitBytes,
		SoftBytes:      softBytes,
		LimitFiles:     req.LimitFiles,
		SoftFiles:      req.SoftFiles,
		GraceSeconds:   req.GraceSeconds,
		GraceStartedAt: graceStartedAt,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if err := h.store.UpsertQuota(r.Context(), quota); err != nil {
		if errors.Is(err, models.ErrDuplicateQuota) {
			Conflict(w, "Quota already exists")
			return
		}
		InternalServerError(w, "Failed to set quota")
		return
	}

	// Hot-update the live metadata service so enforcement takes effect
	// immediately, mirroring how shares.go calls UpdateShareQuota.
	if h.runtime != nil {
		h.runtime.UpdateIdentityQuota(quota)
	}

	WriteJSONOK(w, h.quotaToResponse(quota))
}

// Delete handles DELETE /api/v1/shares/{name}/quotas/{scope}[/{id}].
func (h *QuotaHandler) Delete(w http.ResponseWriter, r *http.Request) {
	shareName, scope, identityID, ok := parseQuotaTarget(w, r)
	if !ok {
		return
	}

	if err := h.store.DeleteQuota(r.Context(), shareName, scope, identityID); err != nil {
		if errors.Is(err, models.ErrQuotaNotFound) {
			NotFound(w, "Quota not found")
			return
		}
		InternalServerError(w, "Failed to delete quota")
		return
	}

	// Remove from the live metadata service so enforcement stops immediately.
	if h.runtime != nil {
		h.runtime.RemoveIdentityQuota(shareName, scope, identityID)
	}

	WriteNoContent(w)
}

// quotaToResponse converts a models.Quota into a QuotaResponse, formatting byte
// ceilings human-readable and populating live per-identity usage from the
// runtime (degrading to 0/0 when no runtime is wired or the share is not
// loaded).
func (h *QuotaHandler) quotaToResponse(q *models.Quota) QuotaResponse {
	var limitBytesStr, softBytesStr string
	if q.LimitBytes > 0 {
		limitBytesStr = bytesize.ByteSize(q.LimitBytes).String()
	}
	if q.SoftBytes > 0 {
		softBytesStr = bytesize.ByteSize(q.SoftBytes).String()
	}
	resp := QuotaResponse{
		ShareName:      q.ShareName,
		Scope:          q.Scope,
		IdentityID:     q.IdentityID,
		LimitBytes:     limitBytesStr,
		SoftBytes:      softBytesStr,
		LimitFiles:     q.LimitFiles,
		SoftFiles:      q.SoftFiles,
		GraceSeconds:   q.GraceSeconds,
		GraceStartedAt: q.GraceStartedAt,
	}
	if h.runtime != nil {
		resp.UsedBytes, resp.UsedFiles = h.runtime.GetIdentityQuotaUsage(q.ShareName, q.Scope, q.IdentityID)
	}
	return resp
}
