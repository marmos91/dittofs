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
// Supports a ?protocol=nfs|smb query filter.
func (h *ClientHandler) List(w http.ResponseWriter, r *http.Request) {
	registry := h.rt.Clients()

	if protocol := r.URL.Query().Get("protocol"); protocol != "" {
		WriteJSONOK(w, registry.ListByProtocol(protocol))
		return
	}
	WriteJSONOK(w, registry.List())
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
