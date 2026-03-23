package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// NfsClientHandler handles NFS-specific client management API endpoints
// (sessions, state inspection). The unified ClientHandler handles cross-protocol
// client listing and disconnect.
type NfsClientHandler struct {
	sm *state.StateManager
}

// NewNfsClientHandler creates a handler for NFS-specific client endpoints.
// Returns nil if sm is nil (no NFS adapter configured).
func NewNfsClientHandler(sm *state.StateManager) *NfsClientHandler {
	if sm == nil {
		return nil
	}
	return &NfsClientHandler{sm: sm}
}

// NewNfsClientHandlerFromProvider creates an NfsClientHandler from an untyped provider.
// Used by the router in pkg/ which cannot import the state package directly.
func NewNfsClientHandlerFromProvider(provider any) *NfsClientHandler {
	if provider == nil {
		return nil
	}
	sm, ok := provider.(*state.StateManager)
	if !ok {
		return nil
	}
	return NewNfsClientHandler(sm)
}

// ListSessions handles GET /api/v1/adapters/nfs/clients/{id}/sessions.
func (h *NfsClientHandler) ListSessions(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	clientID, err := strconv.ParseUint(idStr, 16, 64)
	if err != nil {
		BadRequest(w, "invalid client ID format, expected hex")
		return
	}

	sessions := h.sm.ListSessionsForClient(clientID)
	result := make([]SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		var backSlots uint32
		hasBackChan := s.BackChannelSlots != nil
		if hasBackChan {
			backSlots = s.BackChannelAttrs.MaxRequests
		}

		info := SessionInfo{
			SessionID:   s.SessionID.String(),
			ClientID:    fmt.Sprintf("%016x", s.ClientID),
			CreatedAt:   s.CreatedAt,
			ForeSlots:   s.ForeChannelAttrs.MaxRequests,
			BackSlots:   backSlots,
			Flags:       s.Flags,
			BackChannel: hasBackChan,
		}

		// Enrich with connection binding information
		bindings := h.sm.GetConnectionBindings(s.SessionID)
		if len(bindings) > 0 {
			connInfos := make([]ConnectionInfo, 0, len(bindings))
			summary := ConnectionSummary{Total: len(bindings)}
			for _, b := range bindings {
				connInfos = append(connInfos, ConnectionInfo{
					ConnectionID: b.ConnectionID,
					Direction:    b.Direction.String(),
					ConnType:     b.ConnType.String(),
					BoundAt:      b.BoundAt.Format(time.RFC3339),
					LastActivity: b.LastActivity.Format(time.RFC3339),
					Draining:     b.Draining,
				})
				switch b.Direction {
				case state.ConnDirFore:
					summary.Fore++
				case state.ConnDirBack:
					summary.Back++
				case state.ConnDirBoth:
					summary.Both++
				}
			}
			info.Connections = connInfos
			info.ConnectionSummary = &summary
		}

		result = append(result, info)
	}

	WriteJSONOK(w, result)
}

// ForceDestroySession handles DELETE /api/v1/adapters/nfs/clients/{id}/sessions/{sid}.
func (h *NfsClientHandler) ForceDestroySession(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	clientID, err := strconv.ParseUint(idStr, 16, 64)
	if err != nil {
		BadRequest(w, "invalid client ID format, expected hex")
		return
	}

	sidStr := chi.URLParam(r, "sid")

	// Parse hex session ID (32 hex chars = 16 bytes)
	sidBytes, err := parseSessionID(sidStr)
	if err != nil {
		BadRequest(w, "invalid session ID format, expected 32 hex characters")
		return
	}

	// Verify the session belongs to the specified client
	session := h.sm.GetSession(sidBytes)
	if session == nil {
		NotFound(w, "session not found")
		return
	}
	if session.ClientID != clientID {
		NotFound(w, "session not found for this client")
		return
	}

	if err := h.sm.ForceDestroySession(sidBytes); err != nil {
		NotFound(w, "session not found")
		return
	}

	WriteNoContent(w)
}

// ConnectionInfo represents a single bound connection in the session detail response.
type ConnectionInfo struct {
	ConnectionID uint64 `json:"connection_id"`
	Direction    string `json:"direction"`     // "fore", "back", "both"
	ConnType     string `json:"conn_type"`     // "TCP", "RDMA"
	BoundAt      string `json:"bound_at"`      // RFC3339
	LastActivity string `json:"last_activity"` // RFC3339
	Draining     bool   `json:"draining"`
}

// ConnectionSummary provides a per-direction breakdown of bound connections.
type ConnectionSummary struct {
	Fore  int `json:"fore"`
	Back  int `json:"back"`
	Both  int `json:"both"`
	Total int `json:"total"`
}

// SessionInfo is the response type for session list endpoints.
type SessionInfo struct {
	SessionID         string             `json:"session_id"`
	ClientID          string             `json:"client_id"`
	CreatedAt         time.Time          `json:"created_at"`
	ForeSlots         uint32             `json:"fore_slots"`
	BackSlots         uint32             `json:"back_slots"`
	Flags             uint32             `json:"flags"`
	BackChannel       bool               `json:"back_channel"`
	Connections       []ConnectionInfo   `json:"connections,omitempty"`
	ConnectionSummary *ConnectionSummary `json:"connection_summary,omitempty"`
}

// parseSessionID parses a 32-character hex string into a SessionId4.
func parseSessionID(hexStr string) (types.SessionId4, error) {
	var sid types.SessionId4
	if len(hexStr) != 32 {
		return sid, fmt.Errorf("expected 32 hex chars, got %d", len(hexStr))
	}
	for i := range 16 {
		b, err := strconv.ParseUint(hexStr[i*2:i*2+2], 16, 8)
		if err != nil {
			return sid, fmt.Errorf("invalid hex at position %d: %w", i*2, err)
		}
		sid[i] = byte(b)
	}
	return sid, nil
}

// ServerIdentityFromProvider extracts server identity info from an untyped provider.
// Returns nil if provider is nil or not a *state.StateManager.
func ServerIdentityFromProvider(provider any) map[string]any {
	if provider == nil {
		return nil
	}
	sm, ok := provider.(*state.StateManager)
	if !ok {
		return nil
	}
	si := sm.ServerInfo()
	if si == nil {
		return nil
	}
	return serverIdentityToMap(si)
}

// serverIdentityToMap converts a ServerIdentity to a map for health endpoint JSON.
func serverIdentityToMap(si *state.ServerIdentity) map[string]any {
	return map[string]any{
		"server_owner": map[string]any{
			"major_id": string(si.ServerOwner.MajorID),
			"minor_id": fmt.Sprintf("%08x", uint32(si.ServerOwner.MinorID)),
		},
		"server_impl": map[string]any{
			"name":   si.ImplID.Name,
			"domain": si.ImplID.Domain,
		},
		"server_scope": string(si.ServerScope),
	}
}
