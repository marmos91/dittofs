package handlers

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// ShareNFSConfigHandlerStore is the composite store interface required by
// ShareNFSConfigHandler: share lookup, per-share adapter config persistence,
// and netgroup name/ID resolution.
type ShareNFSConfigHandlerStore interface {
	store.ShareStore
	store.NetgroupStore
}

// ShareNFSConfigHandler serves the per-share NFS adapter config endpoints
// (GET/PATCH /api/v1/shares/{name}/adapters/nfs/config). It decouples
// protocol-specific export settings (squash, netgroup association, auth flavor)
// from the generic Share resource, persisting them in share_adapter_configs and
// pushing the live netgroup association into the runtime.
type ShareNFSConfigHandler struct {
	store   ShareNFSConfigHandlerStore
	runtime *runtime.Runtime
}

// NewShareNFSConfigHandler creates a new ShareNFSConfigHandler.
func NewShareNFSConfigHandler(s ShareNFSConfigHandlerStore, rt *runtime.Runtime) *ShareNFSConfigHandler {
	return &ShareNFSConfigHandler{store: s, runtime: rt}
}

// ShareNFSConfigResponse is the response body for the per-share NFS config
// endpoints. Netgroup is exposed by name (empty string = no association),
// keeping IDs internal to the storage layer.
type ShareNFSConfigResponse struct {
	Squash             string  `json:"squash"`
	AnonymousUID       *uint32 `json:"anonymous_uid,omitempty"`
	AnonymousGID       *uint32 `json:"anonymous_gid,omitempty"`
	AllowAuthSys       bool    `json:"allow_auth_sys"`
	RequireKerberos    bool    `json:"require_kerberos"`
	MinKerberosLevel   string  `json:"min_kerberos_level"`
	Netgroup           string  `json:"netgroup"`
	DisableReaddirplus bool    `json:"disable_readdirplus"`
}

// PatchShareNFSConfigRequest is the request body for PATCH. All fields are
// pointers so the caller can update only the fields it specifies. Netgroup is a
// pointer-to-string: a non-nil empty string clears the association, a non-nil
// name sets it, nil leaves it unchanged.
type PatchShareNFSConfigRequest struct {
	Squash             *string `json:"squash,omitempty"`
	AnonymousUID       *uint32 `json:"anonymous_uid,omitempty"`
	AnonymousGID       *uint32 `json:"anonymous_gid,omitempty"`
	AllowAuthSys       *bool   `json:"allow_auth_sys,omitempty"`
	RequireKerberos    *bool   `json:"require_kerberos,omitempty"`
	MinKerberosLevel   *string `json:"min_kerberos_level,omitempty"`
	Netgroup           *string `json:"netgroup,omitempty"`
	DisableReaddirplus *bool   `json:"disable_readdirplus,omitempty"`
}

// Get handles GET /api/v1/shares/{name}/adapters/nfs/config.
func (h *ShareNFSConfigHandler) Get(w http.ResponseWriter, r *http.Request) {
	share, ok := h.lookupShare(w, r)
	if !ok {
		return
	}

	opts, ok := h.loadOptions(w, r, share.ID)
	if !ok {
		return
	}

	resp, ok := h.optionsToResponse(w, r, opts)
	if !ok {
		return
	}
	WriteJSONOK(w, resp)
}

// Patch handles PATCH /api/v1/shares/{name}/adapters/nfs/config.
func (h *ShareNFSConfigHandler) Patch(w http.ResponseWriter, r *http.Request) {
	share, ok := h.lookupShare(w, r)
	if !ok {
		return
	}

	var req PatchShareNFSConfigRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	opts, ok := h.loadOptions(w, r, share.ID)
	if !ok {
		return
	}

	// Resolve netgroup name -> ID before mutating, so an unknown name fails the
	// request without persisting a partial update.
	netgroupName := ""
	if req.Netgroup != nil {
		netgroupName = *req.Netgroup
		if netgroupName == "" {
			opts.NetgroupID = nil
		} else {
			ng, err := h.store.GetNetgroup(r.Context(), netgroupName)
			if err != nil {
				if errors.Is(err, models.ErrNetgroupNotFound) {
					BadRequest(w, "Netgroup not found: "+netgroupName)
					return
				}
				InternalServerError(w, "Failed to resolve netgroup")
				return
			}
			id := ng.ID
			opts.NetgroupID = &id
		}
	}

	if req.Squash != nil {
		opts.Squash = *req.Squash
	}
	if req.AnonymousUID != nil {
		opts.AnonymousUID = req.AnonymousUID
	}
	if req.AnonymousGID != nil {
		opts.AnonymousGID = req.AnonymousGID
	}
	if req.AllowAuthSys != nil {
		opts.AllowAuthSys = *req.AllowAuthSys
	}
	if req.RequireKerberos != nil {
		opts.RequireKerberos = *req.RequireKerberos
	}
	if req.MinKerberosLevel != nil {
		opts.MinKerberosLevel = *req.MinKerberosLevel
	}
	if req.DisableReaddirplus != nil {
		opts.DisableReaddirplus = *req.DisableReaddirplus
	}

	cfg := &models.ShareAdapterConfig{ShareID: share.ID, AdapterType: "nfs"}
	if err := cfg.SetConfig(opts); err != nil {
		InternalServerError(w, "Failed to encode NFS config")
		return
	}
	if err := h.store.SetShareAdapterConfig(r.Context(), cfg); err != nil {
		InternalServerError(w, "Failed to persist NFS config")
		return
	}

	// Push the netgroup association into the running share so it takes effect
	// immediately (CheckNetgroupAccess reads NetgroupName from the runtime
	// registry). Other NFS export fields apply on adapter restart.
	if req.Netgroup != nil && h.runtime != nil {
		if err := h.runtime.SetShareNetgroup(share.Name, netgroupName); err != nil {
			logger.Warn("NFS config persisted but failed to update runtime netgroup",
				"share", share.Name, "netgroup", netgroupName, "error", err)
		}
	}

	resp, ok := h.optionsToResponse(w, r, opts)
	if !ok {
		return
	}
	WriteJSONOK(w, resp)
}

// lookupShare resolves the {name} path param to a stored share, writing a
// 404/500 problem and returning ok=false on failure.
func (h *ShareNFSConfigHandler) lookupShare(w http.ResponseWriter, r *http.Request) (*models.Share, bool) {
	name := normalizeShareName(chi.URLParam(r, "name"))
	share, err := h.store.GetShare(r.Context(), name)
	if err != nil {
		if errors.Is(err, models.ErrShareNotFound) {
			NotFound(w, "Share not found")
			return nil, false
		}
		InternalServerError(w, "Failed to get share")
		return nil, false
	}
	return share, true
}

// loadOptions returns the share's persisted NFS export options, falling back to
// defaults when no config row exists yet.
func (h *ShareNFSConfigHandler) loadOptions(w http.ResponseWriter, r *http.Request, shareID string) (models.NFSExportOptions, bool) {
	opts := models.DefaultNFSExportOptions()
	cfg, err := h.store.GetShareAdapterConfig(r.Context(), shareID, "nfs")
	if err != nil {
		InternalServerError(w, "Failed to get NFS config")
		return opts, false
	}
	if cfg != nil {
		if err := cfg.ParseConfig(&opts); err != nil {
			InternalServerError(w, "Failed to parse NFS config")
			return opts, false
		}
	}
	return opts, true
}

// optionsToResponse resolves the stored netgroup ID back to a name for the
// response payload.
func (h *ShareNFSConfigHandler) optionsToResponse(w http.ResponseWriter, r *http.Request, opts models.NFSExportOptions) (ShareNFSConfigResponse, bool) {
	resp := ShareNFSConfigResponse{
		Squash:             opts.Squash,
		AnonymousUID:       opts.AnonymousUID,
		AnonymousGID:       opts.AnonymousGID,
		AllowAuthSys:       opts.AllowAuthSys,
		RequireKerberos:    opts.RequireKerberos,
		MinKerberosLevel:   opts.MinKerberosLevel,
		DisableReaddirplus: opts.DisableReaddirplus,
	}
	if opts.NetgroupID != nil && *opts.NetgroupID != "" {
		ng, err := h.store.GetNetgroupByID(r.Context(), *opts.NetgroupID)
		if err != nil {
			if errors.Is(err, models.ErrNetgroupNotFound) {
				// Dangling reference (netgroup deleted out from under the
				// config) — report empty rather than failing the read.
				logger.Warn("Share NFS config references unknown netgroup",
					"netgroup_id", *opts.NetgroupID)
				return resp, true
			}
			InternalServerError(w, "Failed to resolve netgroup")
			return resp, false
		}
		resp.Netgroup = ng.Name
	}
	return resp, true
}
