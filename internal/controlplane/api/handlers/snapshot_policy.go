package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/api/dto"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/schedule"
)

// SnapshotPolicyRuntime is the narrow Runtime surface SnapshotPolicyHandler
// depends on. Defined here so unit tests can substitute a fake.
type SnapshotPolicyRuntime interface {
	GetSnapshotPolicy(ctx context.Context, share string) (*models.SnapshotPolicy, error)
	ListSnapshotPolicies(ctx context.Context) ([]*models.SnapshotPolicy, error)
	UpsertSnapshotPolicy(ctx context.Context, policy *models.SnapshotPolicy) error
	DeleteSnapshotPolicy(ctx context.Context, share string) error
	RunSnapshotPolicyNow(ctx context.Context, share string) (string, error)
}

// SnapshotPolicyHandler serves the per-share snapshot policy REST surface
// (upsert/get/delete/run) plus the cross-share list. All routes inherit the
// parent admin-only middleware.
type SnapshotPolicyHandler struct {
	runtime SnapshotPolicyRuntime
}

// NewSnapshotPolicyHandler constructs a handler bound to the given Runtime.
func NewSnapshotPolicyHandler(rt SnapshotPolicyRuntime) *SnapshotPolicyHandler {
	return &SnapshotPolicyHandler{runtime: rt}
}

func (h *SnapshotPolicyHandler) resolveShare(w http.ResponseWriter, r *http.Request) string {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return ""
	}
	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "" {
		BadRequest(w, "share name is required")
		return ""
	}
	return name
}

// Upsert handles PUT /api/v1/shares/{name}/snapshot-policy.
func (h *SnapshotPolicyHandler) Upsert(w http.ResponseWriter, r *http.Request) {
	name := h.resolveShare(w, r)
	if name == "" {
		return
	}

	var req dto.UpsertSnapshotPolicyRequest
	if !decodeBody(w, r, &req) {
		return
	}

	interval, err := schedule.ParseInterval(req.Interval)
	if err != nil {
		BadRequest(w, "invalid interval: "+err.Error())
		return
	}
	ttl, err := parseOptionalDuration(req.TTL)
	if err != nil {
		BadRequest(w, "invalid ttl: "+err.Error())
		return
	}
	if req.KeepLast < 0 {
		BadRequest(w, "keep_last must be >= 0")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	policy := &models.SnapshotPolicy{
		ShareName:  name,
		Enabled:    enabled,
		Interval:   interval,
		KeepLast:   req.KeepLast,
		TTL:        ttl,
		NamePrefix: req.NamePrefix,
	}
	if err := h.runtime.UpsertSnapshotPolicy(r.Context(), policy); err != nil {
		h.handleErr(w, "snapshot policy upsert", []any{"share", name}, err)
		return
	}

	// Re-read so the response reflects the persisted row (ID, timestamps,
	// preserved LastRunAt).
	saved, err := h.runtime.GetSnapshotPolicy(r.Context(), name)
	if err != nil {
		h.handleErr(w, "snapshot policy upsert reload", []any{"share", name}, err)
		return
	}
	WriteJSONOK(w, policyToWire(saved))
}

// Get handles GET /api/v1/shares/{name}/snapshot-policy.
func (h *SnapshotPolicyHandler) Get(w http.ResponseWriter, r *http.Request) {
	name := h.resolveShare(w, r)
	if name == "" {
		return
	}
	policy, err := h.runtime.GetSnapshotPolicy(r.Context(), name)
	if err != nil {
		h.handleErr(w, "snapshot policy get", []any{"share", name}, err)
		return
	}
	WriteJSONOK(w, policyToWire(policy))
}

// Delete handles DELETE /api/v1/shares/{name}/snapshot-policy.
func (h *SnapshotPolicyHandler) Delete(w http.ResponseWriter, r *http.Request) {
	name := h.resolveShare(w, r)
	if name == "" {
		return
	}
	if err := h.runtime.DeleteSnapshotPolicy(r.Context(), name); err != nil {
		h.handleErr(w, "snapshot policy delete", []any{"share", name}, err)
		return
	}
	WriteNoContent(w)
}

// Run handles POST /api/v1/shares/{name}/snapshot-policy/run. Returns 202 with
// the new snapshot id and a Location header pointing at its show URL.
func (h *SnapshotPolicyHandler) Run(w http.ResponseWriter, r *http.Request) {
	name := h.resolveShare(w, r)
	if name == "" {
		return
	}
	snapID, err := h.runtime.RunSnapshotPolicyNow(r.Context(), name)
	if err != nil {
		h.handleErr(w, "snapshot policy run", []any{"share", name}, err)
		return
	}
	w.Header().Set(
		"Location",
		fmt.Sprintf("/api/v1/shares/%s/snapshots/%s", url.PathEscape(name), url.PathEscape(snapID)),
	)
	WriteJSONAccepted(w, dto.CreateSnapshotResponse{SnapshotID: snapID, Share: name})
}

// List handles GET /api/v1/snapshot-policies. Returns 200 with a JSON array
// (empty, not null) of all policies across shares.
func (h *SnapshotPolicyHandler) List(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}
	policies, err := h.runtime.ListSnapshotPolicies(r.Context())
	if err != nil {
		h.handleErr(w, "snapshot policy list", nil, err)
		return
	}
	out := make([]dto.SnapshotPolicy, 0, len(policies))
	for _, p := range policies {
		out = append(out, policyToWire(p))
	}
	WriteJSONOK(w, out)
}

func (h *SnapshotPolicyHandler) handleErr(w http.ResponseWriter, op string, fields []any, err error) {
	if mapSnapshotPolicyError(w, err) {
		return
	}
	logger.Error(op+" error", append(fields, "error", err)...)
	InternalServerError(w, op+" failed")
}

// mapSnapshotPolicyError maps policy-specific sentinels, then delegates to
// mapSnapshotError for the shared share/state sentinels.
func mapSnapshotPolicyError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, models.ErrSnapshotPolicyNotFound) {
		NotFound(w, "snapshot policy not found")
		return true
	}
	return mapSnapshotError(w, err)
}

// parseOptionalDuration parses a Go duration. An empty string or "0" means no
// bound (0). Negative durations are rejected.
func parseOptionalDuration(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("must be >= 0")
	}
	return d, nil
}

// policyToWire converts a models.SnapshotPolicy into the wire DTO.
func policyToWire(p *models.SnapshotPolicy) dto.SnapshotPolicy {
	out := dto.SnapshotPolicy{
		Share:      p.ShareName,
		Enabled:    p.Enabled,
		Interval:   p.Interval.String(),
		KeepLast:   p.KeepLast,
		NamePrefix: p.NamePrefix,
		LastRunAt:  p.LastRunAt,
		CreatedAt:  p.CreatedAt,
		UpdatedAt:  p.UpdatedAt,
	}
	if p.TTL > 0 {
		out.TTL = p.TTL.String()
	}
	return out
}
