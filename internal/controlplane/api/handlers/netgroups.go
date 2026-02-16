package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// NetgroupHandler handles netgroup management API endpoints.
type NetgroupHandler struct {
	store store.Store
}

// NewNetgroupHandler creates a new NetgroupHandler.
func NewNetgroupHandler(cpStore store.Store) *NetgroupHandler {
	return &NetgroupHandler{store: cpStore}
}

// CreateNetgroupRequest is the request body for POST /api/v1/netgroups.
type CreateNetgroupRequest struct {
	Name string `json:"name"`
}

// NetgroupResponse is the response body for netgroup endpoints.
type NetgroupResponse struct {
	ID        string                   `json:"id"`
	Name      string                   `json:"name"`
	Members   []NetgroupMemberResponse `json:"members,omitempty"`
	CreatedAt time.Time                `json:"created_at"`
	UpdatedAt time.Time                `json:"updated_at"`
}

// NetgroupMemberResponse is the response body for netgroup member endpoints.
type NetgroupMemberResponse struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// AddNetgroupMemberRequest is the request body for POST /api/v1/netgroups/{name}/members.
type AddNetgroupMemberRequest struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Create handles POST /api/v1/netgroups.
func (h *NetgroupHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateNetgroupRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Name == "" {
		BadRequest(w, "Netgroup name is required")
		return
	}

	netgroup := &models.Netgroup{
		Name: req.Name,
	}

	if _, err := h.store.CreateNetgroup(r.Context(), netgroup); err != nil {
		if errors.Is(err, models.ErrDuplicateNetgroup) {
			Conflict(w, "Netgroup already exists")
			return
		}
		InternalServerError(w, "Failed to create netgroup")
		return
	}

	WriteJSONCreated(w, netgroupToResponse(netgroup))
}

// List handles GET /api/v1/netgroups.
func (h *NetgroupHandler) List(w http.ResponseWriter, r *http.Request) {
	netgroups, err := h.store.ListNetgroups(r.Context())
	if err != nil {
		InternalServerError(w, "Failed to list netgroups")
		return
	}

	response := make([]NetgroupResponse, len(netgroups))
	for i, ng := range netgroups {
		response[i] = netgroupToResponse(ng)
	}

	WriteJSONOK(w, response)
}

// Get handles GET /api/v1/netgroups/{name}.
func (h *NetgroupHandler) Get(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Netgroup name is required")
		return
	}

	netgroup, err := h.store.GetNetgroup(r.Context(), name)
	if err != nil {
		if errors.Is(err, models.ErrNetgroupNotFound) {
			NotFound(w, "Netgroup not found")
			return
		}
		InternalServerError(w, "Failed to get netgroup")
		return
	}

	WriteJSONOK(w, netgroupToResponse(netgroup))
}

// Delete handles DELETE /api/v1/netgroups/{name}.
// Returns 409 Conflict if the netgroup is referenced by any shares.
func (h *NetgroupHandler) Delete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Netgroup name is required")
		return
	}

	if err := h.store.DeleteNetgroup(r.Context(), name); err != nil {
		if errors.Is(err, models.ErrNetgroupNotFound) {
			NotFound(w, "Netgroup not found")
			return
		}
		if errors.Is(err, models.ErrNetgroupInUse) {
			Conflict(w, "Netgroup is referenced by one or more shares")
			return
		}
		InternalServerError(w, "Failed to delete netgroup")
		return
	}

	WriteNoContent(w)
}

// AddMember handles POST /api/v1/netgroups/{name}/members.
func (h *NetgroupHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Netgroup name is required")
		return
	}

	var req AddNetgroupMemberRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Type == "" {
		BadRequest(w, "Member type is required")
		return
	}
	if req.Value == "" {
		BadRequest(w, "Member value is required")
		return
	}

	// Validate member type
	if !models.ValidateMemberType(req.Type) {
		BadRequest(w, "Invalid member type. Must be one of: ip, cidr, hostname")
		return
	}

	// Validate member value
	if err := models.ValidateMemberValue(req.Type, req.Value); err != nil {
		BadRequest(w, err.Error())
		return
	}

	member := &models.NetgroupMember{
		ID:    uuid.New().String(),
		Type:  req.Type,
		Value: req.Value,
	}

	if err := h.store.AddNetgroupMember(r.Context(), name, member); err != nil {
		if errors.Is(err, models.ErrNetgroupNotFound) {
			NotFound(w, "Netgroup not found")
			return
		}
		InternalServerError(w, "Failed to add netgroup member")
		return
	}

	WriteJSONCreated(w, NetgroupMemberResponse{
		ID:    member.ID,
		Type:  member.Type,
		Value: member.Value,
	})
}

// RemoveMember handles DELETE /api/v1/netgroups/{name}/members/{id}.
func (h *NetgroupHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	memberID := chi.URLParam(r, "id")

	if name == "" {
		BadRequest(w, "Netgroup name is required")
		return
	}
	if memberID == "" {
		BadRequest(w, "Member ID is required")
		return
	}

	if err := h.store.RemoveNetgroupMember(r.Context(), name, memberID); err != nil {
		if errors.Is(err, models.ErrNetgroupNotFound) {
			NotFound(w, "Netgroup not found")
			return
		}
		InternalServerError(w, "Failed to remove netgroup member")
		return
	}

	WriteNoContent(w)
}

// netgroupToResponse converts a models.Netgroup to NetgroupResponse.
func netgroupToResponse(ng *models.Netgroup) NetgroupResponse {
	members := make([]NetgroupMemberResponse, len(ng.Members))
	for i, m := range ng.Members {
		members[i] = NetgroupMemberResponse{
			ID:    m.ID,
			Type:  m.Type,
			Value: m.Value,
		}
	}
	return NetgroupResponse{
		ID:        ng.ID,
		Name:      ng.Name,
		Members:   members,
		CreatedAt: ng.CreatedAt,
		UpdatedAt: ng.UpdatedAt,
	}
}
