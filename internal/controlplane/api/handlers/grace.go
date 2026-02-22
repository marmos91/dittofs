package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/state"
)

// GraceHandler handles grace period management API endpoints.
type GraceHandler struct {
	sm *state.StateManager
}

// NewGraceHandler creates a handler for grace period endpoints.
// Returns nil if sm is nil (no NFS adapter configured).
func NewGraceHandler(sm *state.StateManager) *GraceHandler {
	if sm == nil {
		return nil
	}
	return &GraceHandler{sm: sm}
}

// NewGraceHandlerFromProvider creates a GraceHandler from an untyped provider.
// Used by the router in pkg/ which cannot import the state package directly.
func NewGraceHandlerFromProvider(provider any) *GraceHandler {
	if provider == nil {
		return nil
	}
	sm, ok := provider.(*state.StateManager)
	if !ok {
		return nil
	}
	return NewGraceHandler(sm)
}

// GraceStatusResponse is the JSON response for GET /api/v1/grace.
type GraceStatusResponse struct {
	Active           bool    `json:"active"`
	RemainingSeconds float64 `json:"remaining_seconds"`
	TotalDuration    string  `json:"total_duration,omitempty"`
	ExpectedClients  int     `json:"expected_clients"`
	ReclaimedClients int     `json:"reclaimed_clients"`
	StartedAt        string  `json:"started_at,omitempty"`
	Message          string  `json:"message"`
}

// Status handles GET /api/v1/grace - grace period status (unauthenticated).
func (h *GraceHandler) Status(w http.ResponseWriter, r *http.Request) {
	info := h.sm.GraceStatus()

	resp := GraceStatusResponse{
		Active:           info.Active,
		RemainingSeconds: info.RemainingSeconds,
		ExpectedClients:  info.ExpectedClients,
		ReclaimedClients: info.ReclaimedClients,
	}

	if info.Active {
		resp.TotalDuration = info.TotalDuration.String()
		resp.StartedAt = info.StartedAt.UTC().Format(time.RFC3339)
		remaining := int(info.RemainingSeconds)
		resp.Message = fmt.Sprintf("Grace period active: %ds remaining (%d/%d clients reclaimed)",
			remaining, info.ReclaimedClients, info.ExpectedClients)
	} else {
		resp.Message = "No active grace period"
	}

	WriteJSONOK(w, resp)
}

// ForceEnd handles POST /api/v1/grace/end - force-end grace period (admin only).
func (h *GraceHandler) ForceEnd(w http.ResponseWriter, r *http.Request) {
	info := h.sm.GraceStatus()
	if !info.Active {
		WriteJSONOK(w, map[string]string{"message": "No active grace period"})
		return
	}

	h.sm.ForceEndGrace()
	WriteJSONOK(w, map[string]string{"message": "Grace period ended"})
}
