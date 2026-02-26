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

// GroupHandler handles group management API endpoints.
type GroupHandler struct {
	store store.GroupStore
}

// NewGroupHandler creates a new GroupHandler.
func NewGroupHandler(s store.GroupStore) *GroupHandler {
	return &GroupHandler{store: s}
}

// CreateGroupRequest is the request body for POST /api/v1/groups.
type CreateGroupRequest struct {
	Name        string `json:"name"`
	GID         uint32 `json:"gid,omitempty"`
	Description string `json:"description,omitempty"`
}

// UpdateGroupRequest is the request body for PUT /api/v1/groups/{name}.
type UpdateGroupRequest struct {
	Description *string `json:"description,omitempty"`
	GID         *uint32 `json:"gid,omitempty"`
}

// GroupResponse is the response body for group endpoints.
type GroupResponse struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	GID         *uint32   `json:"gid,omitempty"`
	Description string    `json:"description,omitempty"`
	Members     []string  `json:"members,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Create handles POST /api/v1/groups.
// Creates a new group (admin only).
func (h *GroupHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateGroupRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Name == "" {
		BadRequest(w, "Group name is required")
		return
	}

	group := &models.Group{
		ID:          uuid.New().String(),
		Name:        req.Name,
		Description: req.Description,
		CreatedAt:   time.Now(),
	}

	if req.GID != 0 {
		group.GID = &req.GID
	}

	if _, err := h.store.CreateGroup(r.Context(), group); err != nil {
		if errors.Is(err, models.ErrDuplicateGroup) {
			Conflict(w, "Group already exists")
			return
		}
		InternalServerError(w, "Failed to create group")
		return
	}

	WriteJSONCreated(w, groupToResponse(group))
}

// List handles GET /api/v1/groups.
// Lists all groups (admin only).
func (h *GroupHandler) List(w http.ResponseWriter, r *http.Request) {
	groups, err := h.store.ListGroups(r.Context())
	if err != nil {
		InternalServerError(w, "Failed to list groups")
		return
	}

	response := make([]GroupResponse, len(groups))
	for i, g := range groups {
		response[i] = groupToResponse(g)
	}

	WriteJSONOK(w, response)
}

// Get handles GET /api/v1/groups/{name}.
// Gets a group by name (admin only).
func (h *GroupHandler) Get(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Group name is required")
		return
	}

	group, err := h.store.GetGroup(r.Context(), name)
	if err != nil {
		if errors.Is(err, models.ErrGroupNotFound) {
			NotFound(w, "Group not found")
			return
		}
		InternalServerError(w, "Failed to get group")
		return
	}

	WriteJSONOK(w, groupToResponse(group))
}

// Update handles PUT /api/v1/groups/{name}.
// Updates a group (admin only).
func (h *GroupHandler) Update(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Group name is required")
		return
	}

	var req UpdateGroupRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// Fetch existing group
	group, err := h.store.GetGroup(r.Context(), name)
	if err != nil {
		if errors.Is(err, models.ErrGroupNotFound) {
			NotFound(w, "Group not found")
			return
		}
		InternalServerError(w, "Failed to get group")
		return
	}

	// Apply updates
	if req.Description != nil {
		group.Description = *req.Description
	}
	if req.GID != nil {
		group.GID = req.GID
	}

	if err := h.store.UpdateGroup(r.Context(), group); err != nil {
		InternalServerError(w, "Failed to update group")
		return
	}

	WriteJSONOK(w, groupToResponse(group))
}

// Delete handles DELETE /api/v1/groups/{name}.
// Deletes a group (admin only).
// System groups (admins, users) cannot be deleted.
func (h *GroupHandler) Delete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Group name is required")
		return
	}

	// Protect system groups from deletion
	if name == "admins" || name == "users" {
		Forbidden(w, "Cannot delete system group")
		return
	}

	if err := h.store.DeleteGroup(r.Context(), name); err != nil {
		if errors.Is(err, models.ErrGroupNotFound) {
			NotFound(w, "Group not found")
			return
		}
		InternalServerError(w, "Failed to delete group")
		return
	}

	WriteNoContent(w)
}

// AddMember handles POST /api/v1/groups/{name}/members.
// Adds a user to a group (admin only).
func (h *GroupHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	groupName := chi.URLParam(r, "name")
	if groupName == "" {
		BadRequest(w, "Group name is required")
		return
	}

	var req struct {
		Username string `json:"username"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Username == "" {
		BadRequest(w, "Username is required")
		return
	}

	if err := h.store.AddUserToGroup(r.Context(), req.Username, groupName); err != nil {
		if errors.Is(err, models.ErrUserNotFound) {
			NotFound(w, "User not found")
			return
		}
		if errors.Is(err, models.ErrGroupNotFound) {
			NotFound(w, "Group not found")
			return
		}
		InternalServerError(w, "Failed to add user to group")
		return
	}

	WriteNoContent(w)
}

// RemoveMember handles DELETE /api/v1/groups/{name}/members/{username}.
// Removes a user from a group (admin only).
func (h *GroupHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	groupName := chi.URLParam(r, "name")
	username := chi.URLParam(r, "username")

	if groupName == "" {
		BadRequest(w, "Group name is required")
		return
	}
	if username == "" {
		BadRequest(w, "Username is required")
		return
	}

	if err := h.store.RemoveUserFromGroup(r.Context(), username, groupName); err != nil {
		if errors.Is(err, models.ErrUserNotFound) {
			NotFound(w, "User not found")
			return
		}
		InternalServerError(w, "Failed to remove user from group")
		return
	}

	WriteNoContent(w)
}

// ListMembers handles GET /api/v1/groups/{name}/members.
// Lists all members of a group (admin only).
func (h *GroupHandler) ListMembers(w http.ResponseWriter, r *http.Request) {
	groupName := chi.URLParam(r, "name")
	if groupName == "" {
		BadRequest(w, "Group name is required")
		return
	}

	members, err := h.store.GetGroupMembers(r.Context(), groupName)
	if err != nil {
		if errors.Is(err, models.ErrGroupNotFound) {
			NotFound(w, "Group not found")
			return
		}
		InternalServerError(w, "Failed to list group members")
		return
	}

	response := make([]GroupMemberResponse, len(members))
	for i, u := range members {
		response[i] = controlplaneUserToMemberResponse(u)
	}

	WriteJSONOK(w, response)
}

// GroupMemberResponse is the response body for group member endpoints.
type GroupMemberResponse struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	Role        string `json:"role"`
}

// groupToResponse converts a models.Group to GroupResponse.
func groupToResponse(g *models.Group) GroupResponse {
	resp := GroupResponse{
		ID:          g.ID,
		Name:        g.Name,
		Description: g.Description,
		CreatedAt:   g.CreatedAt,
	}
	// Copy GID pointer
	if g.GID != nil {
		gid := *g.GID
		resp.GID = &gid
	}
	// Populate members from Users
	if len(g.Users) > 0 {
		resp.Members = make([]string, len(g.Users))
		for i, u := range g.Users {
			resp.Members[i] = u.Username
		}
	}
	return resp
}

// controlplaneUserToMemberResponse converts a models.User to GroupMemberResponse.
func controlplaneUserToMemberResponse(u *models.User) GroupMemberResponse {
	return GroupMemberResponse{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Email:       u.Email,
		Role:        u.Role,
	}
}
