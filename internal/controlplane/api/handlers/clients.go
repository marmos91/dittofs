package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// ClientHandler handles unified client management API endpoints.
// It uses the ClientRegistry for cross-protocol client listing and
// Runtime.DisconnectClient for protocol-specific teardown.
type ClientHandler struct {
	rt *runtime.Runtime
}

// NewClientHandler creates a handler for unified client endpoints.
// Returns nil if rt is nil.
func NewClientHandler(rt *runtime.Runtime) *ClientHandler {
	if rt == nil {
		return nil
	}
	return &ClientHandler{rt: rt}
}

// List handles GET /api/v1/clients.
// Supports ?protocol=nfs|smb and ?share=/export query filters.
func (h *ClientHandler) List(w http.ResponseWriter, r *http.Request) {
	protocol := r.URL.Query().Get("protocol")
	share := r.URL.Query().Get("share")

	registry := h.rt.Clients()

	switch {
	case protocol != "" && share != "":
		// Filter by protocol first, then by share in-handler.
		byProto := registry.ListByProtocol(protocol)
		filtered := make([]*runtime.ClientRecord, 0, len(byProto))
		for _, rec := range byProto {
			for _, s := range rec.Shares {
				if s == share {
					filtered = append(filtered, rec)
					break
				}
			}
		}
		WriteJSONOK(w, filtered)
	case protocol != "":
		WriteJSONOK(w, registry.ListByProtocol(protocol))
	case share != "":
		WriteJSONOK(w, registry.ListByShare(share))
	default:
		WriteJSONOK(w, registry.List())
	}
}

// Disconnect handles DELETE /api/v1/clients/{id}.
// Performs protocol-specific teardown (closes TCP, triggers NFS state revocation /
// SMB session cleanup) before deregistering the client.
func (h *ClientHandler) Disconnect(w http.ResponseWriter, r *http.Request) {
	clientID := chi.URLParam(r, "id")

	record := h.rt.DisconnectClient(clientID)
	if record == nil {
		NotFound(w, "client not found")
		return
	}

	WriteNoContent(w)
}
