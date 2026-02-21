package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"
	"github.com/marmos91/dittofs/internal/controlplane/api/middleware"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// AdapterSettingsHandler handles adapter settings API endpoints.
type AdapterSettingsHandler struct {
	store   store.Store
	runtime *runtime.Runtime
}

// NewAdapterSettingsHandler creates a new AdapterSettingsHandler.
func NewAdapterSettingsHandler(cpStore store.Store, rt *runtime.Runtime) *AdapterSettingsHandler {
	return &AdapterSettingsHandler{store: cpStore, runtime: rt}
}

// --- NFS request types ---

// PatchNFSSettingsRequest uses pointer fields for partial updates (nil = keep current).
type PatchNFSSettingsRequest struct {
	MinVersion              *string   `json:"min_version,omitempty"`
	MaxVersion              *string   `json:"max_version,omitempty"`
	LeaseTime               *int      `json:"lease_time,omitempty"`
	GracePeriod             *int      `json:"grace_period,omitempty"`
	DelegationRecallTimeout *int      `json:"delegation_recall_timeout,omitempty"`
	CallbackTimeout         *int      `json:"callback_timeout,omitempty"`
	LeaseBreakTimeout       *int      `json:"lease_break_timeout,omitempty"`
	MaxConnections          *int      `json:"max_connections,omitempty"`
	MaxClients              *int      `json:"max_clients,omitempty"`
	MaxCompoundOps          *int      `json:"max_compound_ops,omitempty"`
	MaxReadSize             *int      `json:"max_read_size,omitempty"`
	MaxWriteSize            *int      `json:"max_write_size,omitempty"`
	PreferredTransferSize   *int      `json:"preferred_transfer_size,omitempty"`
	DelegationsEnabled      *bool     `json:"delegations_enabled,omitempty"`
	V4MinMinorVersion       *int      `json:"v4_min_minor_version,omitempty"`
	V4MaxMinorVersion       *int      `json:"v4_max_minor_version,omitempty"`
	BlockedOperations       *[]string `json:"blocked_operations,omitempty"`
	PortmapperEnabled       *bool     `json:"portmapper_enabled,omitempty"`
	PortmapperPort          *int      `json:"portmapper_port,omitempty"`
}

// PutNFSSettingsRequest requires all fields for full replacement.
type PutNFSSettingsRequest struct {
	MinVersion              string   `json:"min_version"`
	MaxVersion              string   `json:"max_version"`
	LeaseTime               int      `json:"lease_time"`
	GracePeriod             int      `json:"grace_period"`
	DelegationRecallTimeout int      `json:"delegation_recall_timeout"`
	CallbackTimeout         int      `json:"callback_timeout"`
	LeaseBreakTimeout       int      `json:"lease_break_timeout"`
	MaxConnections          int      `json:"max_connections"`
	MaxClients              int      `json:"max_clients"`
	MaxCompoundOps          int      `json:"max_compound_ops"`
	MaxReadSize             int      `json:"max_read_size"`
	MaxWriteSize            int      `json:"max_write_size"`
	PreferredTransferSize   int      `json:"preferred_transfer_size"`
	DelegationsEnabled      bool     `json:"delegations_enabled"`
	V4MinMinorVersion       int      `json:"v4_min_minor_version"`
	V4MaxMinorVersion       int      `json:"v4_max_minor_version"`
	BlockedOperations       []string `json:"blocked_operations"`
	PortmapperEnabled       bool     `json:"portmapper_enabled"`
	PortmapperPort          int      `json:"portmapper_port"`
}

// --- SMB request types ---

// PatchSMBSettingsRequest uses pointer fields for partial updates.
type PatchSMBSettingsRequest struct {
	MinDialect         *string   `json:"min_dialect,omitempty"`
	MaxDialect         *string   `json:"max_dialect,omitempty"`
	SessionTimeout     *int      `json:"session_timeout,omitempty"`
	OplockBreakTimeout *int      `json:"oplock_break_timeout,omitempty"`
	MaxConnections     *int      `json:"max_connections,omitempty"`
	MaxSessions        *int      `json:"max_sessions,omitempty"`
	EnableEncryption   *bool     `json:"enable_encryption,omitempty"`
	BlockedOperations  *[]string `json:"blocked_operations,omitempty"`
}

// PutSMBSettingsRequest requires all fields for full replacement.
type PutSMBSettingsRequest struct {
	MinDialect         string   `json:"min_dialect"`
	MaxDialect         string   `json:"max_dialect"`
	SessionTimeout     int      `json:"session_timeout"`
	OplockBreakTimeout int      `json:"oplock_break_timeout"`
	MaxConnections     int      `json:"max_connections"`
	MaxSessions        int      `json:"max_sessions"`
	EnableEncryption   bool     `json:"enable_encryption"`
	BlockedOperations  []string `json:"blocked_operations"`
}

// --- Response types ---

// NFSSettingsResponse is the API response for NFS adapter settings.
type NFSSettingsResponse struct {
	MinVersion              string   `json:"min_version"`
	MaxVersion              string   `json:"max_version"`
	LeaseTime               int      `json:"lease_time"`
	GracePeriod             int      `json:"grace_period"`
	DelegationRecallTimeout int      `json:"delegation_recall_timeout"`
	CallbackTimeout         int      `json:"callback_timeout"`
	LeaseBreakTimeout       int      `json:"lease_break_timeout"`
	MaxConnections          int      `json:"max_connections"`
	MaxClients              int      `json:"max_clients"`
	MaxCompoundOps          int      `json:"max_compound_ops"`
	MaxReadSize             int      `json:"max_read_size"`
	MaxWriteSize            int      `json:"max_write_size"`
	PreferredTransferSize   int      `json:"preferred_transfer_size"`
	DelegationsEnabled      bool     `json:"delegations_enabled"`
	V4MinMinorVersion       int      `json:"v4_min_minor_version"`
	V4MaxMinorVersion       int      `json:"v4_max_minor_version"`
	BlockedOperations       []string `json:"blocked_operations"`
	PortmapperEnabled       bool     `json:"portmapper_enabled"`
	PortmapperPort          int      `json:"portmapper_port"`
	Version                 int      `json:"version"`
}

// SMBSettingsResponse is the API response for SMB adapter settings.
type SMBSettingsResponse struct {
	MinDialect         string   `json:"min_dialect"`
	MaxDialect         string   `json:"max_dialect"`
	SessionTimeout     int      `json:"session_timeout"`
	OplockBreakTimeout int      `json:"oplock_break_timeout"`
	MaxConnections     int      `json:"max_connections"`
	MaxSessions        int      `json:"max_sessions"`
	EnableEncryption   bool     `json:"enable_encryption"`
	BlockedOperations  []string `json:"blocked_operations"`
	Version            int      `json:"version"`
}

// SettingRange describes the valid range for a setting field.
type SettingRange struct {
	Min    any      `json:"min,omitempty"`
	Max    any      `json:"max,omitempty"`
	Values []string `json:"values,omitempty"`
}

// SettingsDefaultsResponse contains defaults and valid ranges.
type SettingsDefaultsResponse struct {
	Defaults any                      `json:"defaults"`
	Ranges   map[string]*SettingRange `json:"ranges"`
}

// ValidationErrorResponse is an RFC 7807 problem with per-field errors.
type ValidationErrorResponse struct {
	Type   string            `json:"type"`
	Title  string            `json:"title"`
	Status int               `json:"status"`
	Detail string            `json:"detail,omitempty"`
	Errors map[string]string `json:"errors"`
}

// --- Handlers ---

// GetSettings handles GET /api/v1/adapters/{type}/settings.
func (h *AdapterSettingsHandler) GetSettings(w http.ResponseWriter, r *http.Request) {
	adapterType := chi.URLParam(r, "type")
	adapter, err := h.store.GetAdapter(r.Context(), adapterType)
	if err != nil {
		if errors.Is(err, models.ErrAdapterNotFound) {
			NotFound(w, "Adapter not found")
			return
		}
		InternalServerError(w, "Failed to get adapter")
		return
	}

	switch adapterType {
	case "nfs":
		settings, err := h.store.GetNFSAdapterSettings(r.Context(), adapter.ID)
		if err != nil {
			// Settings may not exist yet if adapter was created without them.
			// Try to create default settings on-demand.
			if errors.Is(err, models.ErrAdapterNotFound) {
				if ensureErr := h.store.EnsureAdapterSettings(r.Context()); ensureErr != nil {
					logger.Error("Failed to ensure adapter settings", "error", ensureErr)
					InternalServerError(w, "Failed to create NFS adapter settings")
					return
				}
				// Retry after ensuring settings exist
				settings, err = h.store.GetNFSAdapterSettings(r.Context(), adapter.ID)
				if err != nil {
					logger.Error("Failed to get NFS adapter settings after ensure", "adapter_id", adapter.ID, "error", err)
					InternalServerError(w, "Failed to get NFS adapter settings")
					return
				}
			} else {
				logger.Error("Failed to get NFS adapter settings", "adapter_id", adapter.ID, "error", err)
				InternalServerError(w, "Failed to get NFS adapter settings")
				return
			}
		}
		WriteJSONOK(w, nfsSettingsToResponse(settings))

	case "smb":
		settings, err := h.store.GetSMBAdapterSettings(r.Context(), adapter.ID)
		if err != nil {
			// Settings may not exist yet if adapter was created without them.
			// Try to create default settings on-demand.
			if errors.Is(err, models.ErrAdapterNotFound) {
				if ensureErr := h.store.EnsureAdapterSettings(r.Context()); ensureErr != nil {
					logger.Error("Failed to ensure adapter settings", "error", ensureErr)
					InternalServerError(w, "Failed to create SMB adapter settings")
					return
				}
				// Retry after ensuring settings exist
				settings, err = h.store.GetSMBAdapterSettings(r.Context(), adapter.ID)
				if err != nil {
					logger.Error("Failed to get SMB adapter settings after ensure", "adapter_id", adapter.ID, "error", err)
					InternalServerError(w, "Failed to get SMB adapter settings")
					return
				}
			} else {
				logger.Error("Failed to get SMB adapter settings", "adapter_id", adapter.ID, "error", err)
				InternalServerError(w, "Failed to get SMB adapter settings")
				return
			}
		}
		WriteJSONOK(w, smbSettingsToResponse(settings))

	default:
		BadRequest(w, fmt.Sprintf("Unsupported adapter type: %s", adapterType))
	}
}

// GetDefaults handles GET /api/v1/adapters/{type}/settings/defaults.
func (h *AdapterSettingsHandler) GetDefaults(w http.ResponseWriter, r *http.Request) {
	adapterType := chi.URLParam(r, "type")

	switch adapterType {
	case "nfs":
		defaults := models.NewDefaultNFSSettings("")
		ranges := models.DefaultNFSSettingsValidRange()
		resp := SettingsDefaultsResponse{
			Defaults: nfsSettingsToResponse(defaults),
			Ranges: map[string]*SettingRange{
				"min_version":               {Values: models.ValidNFSVersions},
				"max_version":               {Values: models.ValidNFSVersions},
				"lease_time":                {Min: ranges.LeaseTimeMin, Max: ranges.LeaseTimeMax},
				"grace_period":              {Min: ranges.GracePeriodMin, Max: ranges.GracePeriodMax},
				"delegation_recall_timeout": {Min: ranges.DelegationRecallTimeoutMin, Max: ranges.DelegationRecallTimeoutMax},
				"callback_timeout":          {Min: ranges.CallbackTimeoutMin, Max: ranges.CallbackTimeoutMax},
				"lease_break_timeout":       {Min: ranges.LeaseBreakTimeoutMin, Max: ranges.LeaseBreakTimeoutMax},
				"max_connections":           {Min: ranges.MaxConnectionsMin, Max: ranges.MaxConnectionsMax},
				"max_clients":               {Min: ranges.MaxClientsMin, Max: ranges.MaxClientsMax},
				"max_compound_ops":          {Min: ranges.MaxCompoundOpsMin, Max: ranges.MaxCompoundOpsMax},
				"max_read_size":             {Min: ranges.MaxReadSizeMin, Max: ranges.MaxReadSizeMax},
				"max_write_size":            {Min: ranges.MaxWriteSizeMin, Max: ranges.MaxWriteSizeMax},
				"preferred_transfer_size":   {Min: ranges.PreferredTransferSizeMin, Max: ranges.PreferredTransferSizeMax},
				"v4_min_minor_version":      {Min: 0, Max: 1},
				"v4_max_minor_version":      {Min: 0, Max: 1},
				"portmapper_port":           {Min: ranges.PortmapperPortMin, Max: ranges.PortmapperPortMax},
			},
		}
		WriteJSONOK(w, resp)

	case "smb":
		defaults := models.NewDefaultSMBSettings("")
		ranges := models.DefaultSMBSettingsValidRange()
		resp := SettingsDefaultsResponse{
			Defaults: smbSettingsToResponse(defaults),
			Ranges: map[string]*SettingRange{
				"min_dialect":          {Values: models.ValidSMBDialects},
				"max_dialect":          {Values: models.ValidSMBDialects},
				"session_timeout":      {Min: ranges.SessionTimeoutMin, Max: ranges.SessionTimeoutMax},
				"oplock_break_timeout": {Min: ranges.OplockBreakTimeoutMin, Max: ranges.OplockBreakTimeoutMax},
				"max_connections":      {Min: ranges.MaxConnectionsMin, Max: ranges.MaxConnectionsMax},
				"max_sessions":         {Min: ranges.MaxSessionsMin, Max: ranges.MaxSessionsMax},
			},
		}
		WriteJSONOK(w, resp)

	default:
		BadRequest(w, fmt.Sprintf("Unsupported adapter type: %s", adapterType))
	}
}

// PutSettings handles PUT /api/v1/adapters/{type}/settings (full replace).
func (h *AdapterSettingsHandler) PutSettings(w http.ResponseWriter, r *http.Request) {
	adapterType := chi.URLParam(r, "type")
	force := r.URL.Query().Get("force") == "true"
	dryRun := r.URL.Query().Get("dry_run") == "true"

	adapter, err := h.store.GetAdapter(r.Context(), adapterType)
	if err != nil {
		if errors.Is(err, models.ErrAdapterNotFound) {
			NotFound(w, "Adapter not found")
			return
		}
		InternalServerError(w, "Failed to get adapter")
		return
	}

	switch adapterType {
	case "nfs":
		var req PutNFSSettingsRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}

		settings, err := h.store.GetNFSAdapterSettings(r.Context(), adapter.ID)
		if err != nil {
			InternalServerError(w, "Failed to get NFS adapter settings")
			return
		}

		// Apply all fields
		settings.MinVersion = req.MinVersion
		settings.MaxVersion = req.MaxVersion
		settings.LeaseTime = req.LeaseTime
		settings.GracePeriod = req.GracePeriod
		settings.DelegationRecallTimeout = req.DelegationRecallTimeout
		settings.CallbackTimeout = req.CallbackTimeout
		settings.LeaseBreakTimeout = req.LeaseBreakTimeout
		settings.MaxConnections = req.MaxConnections
		settings.MaxClients = req.MaxClients
		settings.MaxCompoundOps = req.MaxCompoundOps
		settings.MaxReadSize = req.MaxReadSize
		settings.MaxWriteSize = req.MaxWriteSize
		settings.PreferredTransferSize = req.PreferredTransferSize
		settings.DelegationsEnabled = req.DelegationsEnabled
		settings.V4MinMinorVersion = req.V4MinMinorVersion
		settings.V4MaxMinorVersion = req.V4MaxMinorVersion
		settings.SetBlockedOperations(req.BlockedOperations)
		settings.PortmapperEnabled = req.PortmapperEnabled
		settings.PortmapperPort = req.PortmapperPort

		if !h.validateAndRespond(w, adapterType, settings, nil, force) {
			return
		}

		if dryRun {
			WriteJSONOK(w, nfsSettingsToResponse(settings))
			return
		}

		if err := h.store.UpdateNFSAdapterSettings(r.Context(), settings); err != nil {
			InternalServerError(w, "Failed to update NFS adapter settings")
			return
		}

		// Re-fetch to get updated version (incremented by store)
		updatedSettings, err := h.store.GetNFSAdapterSettings(r.Context(), adapter.ID)
		if err != nil {
			InternalServerError(w, "Failed to get updated NFS adapter settings")
			return
		}

		h.auditLog(r, "NFS adapter settings replaced")
		WriteJSONOK(w, nfsSettingsToResponse(updatedSettings))

	case "smb":
		var req PutSMBSettingsRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}

		settings, err := h.store.GetSMBAdapterSettings(r.Context(), adapter.ID)
		if err != nil {
			InternalServerError(w, "Failed to get SMB adapter settings")
			return
		}

		settings.MinDialect = req.MinDialect
		settings.MaxDialect = req.MaxDialect
		settings.SessionTimeout = req.SessionTimeout
		settings.OplockBreakTimeout = req.OplockBreakTimeout
		settings.MaxConnections = req.MaxConnections
		settings.MaxSessions = req.MaxSessions
		settings.EnableEncryption = req.EnableEncryption
		settings.SetBlockedOperations(req.BlockedOperations)

		if !h.validateAndRespond(w, adapterType, nil, settings, force) {
			return
		}

		if dryRun {
			WriteJSONOK(w, smbSettingsToResponse(settings))
			return
		}

		if err := h.store.UpdateSMBAdapterSettings(r.Context(), settings); err != nil {
			InternalServerError(w, "Failed to update SMB adapter settings")
			return
		}

		// Re-fetch to get updated version (incremented by store)
		updatedSettings, err := h.store.GetSMBAdapterSettings(r.Context(), adapter.ID)
		if err != nil {
			InternalServerError(w, "Failed to get updated SMB adapter settings")
			return
		}

		h.auditLog(r, "SMB adapter settings replaced")
		WriteJSONOK(w, smbSettingsToResponse(updatedSettings))

	default:
		BadRequest(w, fmt.Sprintf("Unsupported adapter type: %s", adapterType))
	}
}

// PatchSettings handles PATCH /api/v1/adapters/{type}/settings (partial update).
func (h *AdapterSettingsHandler) PatchSettings(w http.ResponseWriter, r *http.Request) {
	adapterType := chi.URLParam(r, "type")
	force := r.URL.Query().Get("force") == "true"
	dryRun := r.URL.Query().Get("dry_run") == "true"

	adapter, err := h.store.GetAdapter(r.Context(), adapterType)
	if err != nil {
		if errors.Is(err, models.ErrAdapterNotFound) {
			NotFound(w, "Adapter not found")
			return
		}
		InternalServerError(w, "Failed to get adapter")
		return
	}

	switch adapterType {
	case "nfs":
		var req PatchNFSSettingsRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}

		settings, err := h.store.GetNFSAdapterSettings(r.Context(), adapter.ID)
		if err != nil {
			InternalServerError(w, "Failed to get NFS adapter settings")
			return
		}

		// Apply only non-nil fields
		if req.MinVersion != nil {
			settings.MinVersion = *req.MinVersion
		}
		if req.MaxVersion != nil {
			settings.MaxVersion = *req.MaxVersion
		}
		if req.LeaseTime != nil {
			settings.LeaseTime = *req.LeaseTime
		}
		if req.GracePeriod != nil {
			settings.GracePeriod = *req.GracePeriod
		}
		if req.DelegationRecallTimeout != nil {
			settings.DelegationRecallTimeout = *req.DelegationRecallTimeout
		}
		if req.CallbackTimeout != nil {
			settings.CallbackTimeout = *req.CallbackTimeout
		}
		if req.LeaseBreakTimeout != nil {
			settings.LeaseBreakTimeout = *req.LeaseBreakTimeout
		}
		if req.MaxConnections != nil {
			settings.MaxConnections = *req.MaxConnections
		}
		if req.MaxClients != nil {
			settings.MaxClients = *req.MaxClients
		}
		if req.MaxCompoundOps != nil {
			settings.MaxCompoundOps = *req.MaxCompoundOps
		}
		if req.MaxReadSize != nil {
			settings.MaxReadSize = *req.MaxReadSize
		}
		if req.MaxWriteSize != nil {
			settings.MaxWriteSize = *req.MaxWriteSize
		}
		if req.PreferredTransferSize != nil {
			settings.PreferredTransferSize = *req.PreferredTransferSize
		}
		if req.DelegationsEnabled != nil {
			settings.DelegationsEnabled = *req.DelegationsEnabled
		}
		if req.V4MinMinorVersion != nil {
			settings.V4MinMinorVersion = *req.V4MinMinorVersion
		}
		if req.V4MaxMinorVersion != nil {
			settings.V4MaxMinorVersion = *req.V4MaxMinorVersion
		}
		if req.BlockedOperations != nil {
			settings.SetBlockedOperations(*req.BlockedOperations)
		}
		if req.PortmapperEnabled != nil {
			settings.PortmapperEnabled = *req.PortmapperEnabled
		}
		if req.PortmapperPort != nil {
			settings.PortmapperPort = *req.PortmapperPort
		}

		if !h.validateAndRespond(w, adapterType, settings, nil, force) {
			return
		}

		if dryRun {
			WriteJSONOK(w, nfsSettingsToResponse(settings))
			return
		}

		if err := h.store.UpdateNFSAdapterSettings(r.Context(), settings); err != nil {
			InternalServerError(w, "Failed to update NFS adapter settings")
			return
		}

		// Re-fetch to get updated version (incremented by store)
		updatedSettings, err := h.store.GetNFSAdapterSettings(r.Context(), adapter.ID)
		if err != nil {
			InternalServerError(w, "Failed to get updated NFS adapter settings")
			return
		}

		h.auditLog(r, "NFS adapter settings updated (patch)")
		WriteJSONOK(w, nfsSettingsToResponse(updatedSettings))

	case "smb":
		var req PatchSMBSettingsRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}

		settings, err := h.store.GetSMBAdapterSettings(r.Context(), adapter.ID)
		if err != nil {
			InternalServerError(w, "Failed to get SMB adapter settings")
			return
		}

		if req.MinDialect != nil {
			settings.MinDialect = *req.MinDialect
		}
		if req.MaxDialect != nil {
			settings.MaxDialect = *req.MaxDialect
		}
		if req.SessionTimeout != nil {
			settings.SessionTimeout = *req.SessionTimeout
		}
		if req.OplockBreakTimeout != nil {
			settings.OplockBreakTimeout = *req.OplockBreakTimeout
		}
		if req.MaxConnections != nil {
			settings.MaxConnections = *req.MaxConnections
		}
		if req.MaxSessions != nil {
			settings.MaxSessions = *req.MaxSessions
		}
		if req.EnableEncryption != nil {
			settings.EnableEncryption = *req.EnableEncryption
		}
		if req.BlockedOperations != nil {
			settings.SetBlockedOperations(*req.BlockedOperations)
		}

		if !h.validateAndRespond(w, adapterType, nil, settings, force) {
			return
		}

		if dryRun {
			WriteJSONOK(w, smbSettingsToResponse(settings))
			return
		}

		if err := h.store.UpdateSMBAdapterSettings(r.Context(), settings); err != nil {
			InternalServerError(w, "Failed to update SMB adapter settings")
			return
		}

		// Re-fetch to get updated version (incremented by store)
		updatedSettings, err := h.store.GetSMBAdapterSettings(r.Context(), adapter.ID)
		if err != nil {
			InternalServerError(w, "Failed to get updated SMB adapter settings")
			return
		}

		h.auditLog(r, "SMB adapter settings updated (patch)")
		WriteJSONOK(w, smbSettingsToResponse(updatedSettings))

	default:
		BadRequest(w, fmt.Sprintf("Unsupported adapter type: %s", adapterType))
	}
}

// ResetSettings handles POST /api/v1/adapters/{type}/settings/reset.
// Resets all settings or a specific setting (via ?setting= query param) to defaults.
func (h *AdapterSettingsHandler) ResetSettings(w http.ResponseWriter, r *http.Request) {
	adapterType := chi.URLParam(r, "type")
	settingName := r.URL.Query().Get("setting")

	adapter, err := h.store.GetAdapter(r.Context(), adapterType)
	if err != nil {
		if errors.Is(err, models.ErrAdapterNotFound) {
			NotFound(w, "Adapter not found")
			return
		}
		InternalServerError(w, "Failed to get adapter")
		return
	}

	if settingName == "" {
		// Reset all settings to defaults
		switch adapterType {
		case "nfs":
			if err := h.store.ResetNFSAdapterSettings(r.Context(), adapter.ID); err != nil {
				InternalServerError(w, "Failed to reset NFS adapter settings")
				return
			}
			settings, err := h.store.GetNFSAdapterSettings(r.Context(), adapter.ID)
			if err != nil {
				InternalServerError(w, "Failed to get NFS adapter settings after reset")
				return
			}
			h.auditLog(r, "NFS adapter settings reset to defaults")
			WriteJSONOK(w, nfsSettingsToResponse(settings))

		case "smb":
			if err := h.store.ResetSMBAdapterSettings(r.Context(), adapter.ID); err != nil {
				InternalServerError(w, "Failed to reset SMB adapter settings")
				return
			}
			settings, err := h.store.GetSMBAdapterSettings(r.Context(), adapter.ID)
			if err != nil {
				InternalServerError(w, "Failed to get SMB adapter settings after reset")
				return
			}
			h.auditLog(r, "SMB adapter settings reset to defaults")
			WriteJSONOK(w, smbSettingsToResponse(settings))

		default:
			BadRequest(w, fmt.Sprintf("Unsupported adapter type: %s", adapterType))
		}
		return
	}

	// Reset a specific setting to its default value
	switch adapterType {
	case "nfs":
		settings, err := h.store.GetNFSAdapterSettings(r.Context(), adapter.ID)
		if err != nil {
			InternalServerError(w, "Failed to get NFS adapter settings")
			return
		}
		defaults := models.NewDefaultNFSSettings(adapter.ID)
		if !resetNFSSetting(settings, defaults, settingName) {
			BadRequest(w, fmt.Sprintf("Unknown NFS setting: %s", settingName))
			return
		}
		if err := h.store.UpdateNFSAdapterSettings(r.Context(), settings); err != nil {
			InternalServerError(w, "Failed to reset NFS adapter setting")
			return
		}
		// Re-fetch to get updated version (incremented by store)
		updatedSettings, err := h.store.GetNFSAdapterSettings(r.Context(), adapter.ID)
		if err != nil {
			InternalServerError(w, "Failed to get updated NFS adapter settings")
			return
		}
		h.auditLog(r, fmt.Sprintf("NFS adapter setting '%s' reset to default", settingName))
		WriteJSONOK(w, nfsSettingsToResponse(updatedSettings))

	case "smb":
		settings, err := h.store.GetSMBAdapterSettings(r.Context(), adapter.ID)
		if err != nil {
			InternalServerError(w, "Failed to get SMB adapter settings")
			return
		}
		defaults := models.NewDefaultSMBSettings(adapter.ID)
		if !resetSMBSetting(settings, defaults, settingName) {
			BadRequest(w, fmt.Sprintf("Unknown SMB setting: %s", settingName))
			return
		}
		if err := h.store.UpdateSMBAdapterSettings(r.Context(), settings); err != nil {
			InternalServerError(w, "Failed to reset SMB adapter setting")
			return
		}
		// Re-fetch to get updated version (incremented by store)
		updatedSettings, err := h.store.GetSMBAdapterSettings(r.Context(), adapter.ID)
		if err != nil {
			InternalServerError(w, "Failed to get updated SMB adapter settings")
			return
		}
		h.auditLog(r, fmt.Sprintf("SMB adapter setting '%s' reset to default", settingName))
		WriteJSONOK(w, smbSettingsToResponse(updatedSettings))

	default:
		BadRequest(w, fmt.Sprintf("Unsupported adapter type: %s", adapterType))
	}
}

// --- Validation ---

// validateNFSSettings validates NFS adapter settings against valid ranges.
// Returns a map of field name -> error message for invalid fields.
func validateNFSSettings(s *models.NFSAdapterSettings) map[string]string {
	errs := make(map[string]string)
	ranges := models.DefaultNFSSettingsValidRange()

	// Version validation
	if !isValidNFSVersion(s.MinVersion) {
		errs["min_version"] = fmt.Sprintf("must be one of: %v", models.ValidNFSVersions)
	}
	if !isValidNFSVersion(s.MaxVersion) {
		errs["max_version"] = fmt.Sprintf("must be one of: %v", models.ValidNFSVersions)
	}

	// Range validations
	validateIntRange(errs, "lease_time", s.LeaseTime, ranges.LeaseTimeMin, ranges.LeaseTimeMax)
	validateIntRange(errs, "grace_period", s.GracePeriod, ranges.GracePeriodMin, ranges.GracePeriodMax)
	validateIntRange(errs, "delegation_recall_timeout", s.DelegationRecallTimeout, ranges.DelegationRecallTimeoutMin, ranges.DelegationRecallTimeoutMax)
	validateIntRange(errs, "callback_timeout", s.CallbackTimeout, ranges.CallbackTimeoutMin, ranges.CallbackTimeoutMax)
	validateIntRange(errs, "lease_break_timeout", s.LeaseBreakTimeout, ranges.LeaseBreakTimeoutMin, ranges.LeaseBreakTimeoutMax)
	validateIntRange(errs, "max_connections", s.MaxConnections, ranges.MaxConnectionsMin, ranges.MaxConnectionsMax)
	validateIntRange(errs, "max_clients", s.MaxClients, ranges.MaxClientsMin, ranges.MaxClientsMax)
	validateIntRange(errs, "max_compound_ops", s.MaxCompoundOps, ranges.MaxCompoundOpsMin, ranges.MaxCompoundOpsMax)
	validateIntRange(errs, "max_read_size", s.MaxReadSize, ranges.MaxReadSizeMin, ranges.MaxReadSizeMax)
	validateIntRange(errs, "max_write_size", s.MaxWriteSize, ranges.MaxWriteSizeMin, ranges.MaxWriteSizeMax)
	validateIntRange(errs, "preferred_transfer_size", s.PreferredTransferSize, ranges.PreferredTransferSizeMin, ranges.PreferredTransferSizeMax)
	validateIntRange(errs, "portmapper_port", s.PortmapperPort, ranges.PortmapperPortMin, ranges.PortmapperPortMax)
	validateIntRange(errs, "v4_min_minor_version", s.V4MinMinorVersion, 0, 1)
	validateIntRange(errs, "v4_max_minor_version", s.V4MaxMinorVersion, 0, 1)
	if s.V4MinMinorVersion > s.V4MaxMinorVersion {
		errs["v4_min_minor_version"] = "must be <= v4_max_minor_version"
	}

	// Blocked operations validation
	for _, op := range s.GetBlockedOperations() {
		if !isValidNFSOperation(op) {
			errs["blocked_operations"] = fmt.Sprintf("unknown NFS operation: %s", op)
			break
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errs
}

// validateSMBSettings validates SMB adapter settings against valid ranges.
func validateSMBSettings(s *models.SMBAdapterSettings) map[string]string {
	errs := make(map[string]string)
	ranges := models.DefaultSMBSettingsValidRange()

	if !isValidSMBDialect(s.MinDialect) {
		errs["min_dialect"] = fmt.Sprintf("must be one of: %v", models.ValidSMBDialects)
	}
	if !isValidSMBDialect(s.MaxDialect) {
		errs["max_dialect"] = fmt.Sprintf("must be one of: %v", models.ValidSMBDialects)
	}

	validateIntRange(errs, "session_timeout", s.SessionTimeout, ranges.SessionTimeoutMin, ranges.SessionTimeoutMax)
	validateIntRange(errs, "oplock_break_timeout", s.OplockBreakTimeout, ranges.OplockBreakTimeoutMin, ranges.OplockBreakTimeoutMax)
	validateIntRange(errs, "max_connections", s.MaxConnections, ranges.MaxConnectionsMin, ranges.MaxConnectionsMax)
	validateIntRange(errs, "max_sessions", s.MaxSessions, ranges.MaxSessionsMin, ranges.MaxSessionsMax)

	// Blocked operations validation
	for _, op := range s.GetBlockedOperations() {
		if !isValidSMBOperation(op) {
			errs["blocked_operations"] = fmt.Sprintf("unknown SMB operation: %s", op)
			break
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errs
}

// validateAndRespond validates settings and writes a 422 response if invalid.
// Returns true if validation passed (or force=true), false if response was written.
func (h *AdapterSettingsHandler) validateAndRespond(
	w http.ResponseWriter,
	adapterType string,
	nfsSettings *models.NFSAdapterSettings,
	smbSettings *models.SMBAdapterSettings,
	force bool,
) bool {
	var errs map[string]string
	switch adapterType {
	case "nfs":
		errs = validateNFSSettings(nfsSettings)
	case "smb":
		errs = validateSMBSettings(smbSettings)
	}

	if errs == nil {
		return true
	}

	if force {
		logger.Warn("Adapter settings validation bypassed with force flag",
			"adapter_type", adapterType,
			"errors", errs,
		)
		return true
	}

	resp := ValidationErrorResponse{
		Type:   "about:blank",
		Title:  "Validation Failed",
		Status: http.StatusUnprocessableEntity,
		Detail: "One or more settings are outside valid range",
		Errors: errs,
	}

	w.Header().Set("Content-Type", ContentTypeProblemJSON)
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(resp)
	return false
}

// --- Helpers ---

func (h *AdapterSettingsHandler) auditLog(r *http.Request, action string) {
	claims := middleware.GetClaimsFromContext(r.Context())
	username := "unknown"
	if claims != nil {
		username = claims.Username
	}
	logger.Info(action, "changed_by", username)
}

func validateIntRange(errs map[string]string, field string, value, min, max int) {
	if value < min || value > max {
		errs[field] = fmt.Sprintf("must be between %d and %d", min, max)
	}
}

func isValidNFSVersion(v string) bool {
	return slices.Contains(models.ValidNFSVersions, v)
}

func isValidSMBDialect(d string) bool {
	return slices.Contains(models.ValidSMBDialects, d)
}

// validNFSOperations lists known NFS operations that can be blocked.
var validNFSOperations = []string{
	"NULL", "GETATTR", "SETATTR", "LOOKUP", "ACCESS", "READLINK",
	"READ", "WRITE", "CREATE", "MKDIR", "SYMLINK", "MKNOD",
	"REMOVE", "RMDIR", "RENAME", "LINK", "READDIR", "READDIRPLUS",
	"FSSTAT", "FSINFO", "PATHCONF", "COMMIT",
	// NFSv4 operations
	"OPEN", "CLOSE", "LOCK", "LOCKT", "LOCKU", "DELEGRETURN",
	"PUTFH", "PUTROOTFH", "GETFH", "SAVEFH", "RESTOREFH",
	"VERIFY", "NVERIFY", "SECINFO", "SETCLIENTID", "RENEW",
}

func isValidNFSOperation(op string) bool {
	return slices.Contains(validNFSOperations, op)
}

// validSMBOperations lists known SMB operations that can be blocked.
var validSMBOperations = []string{
	"NEGOTIATE", "SESSION_SETUP", "LOGOFF", "TREE_CONNECT", "TREE_DISCONNECT",
	"CREATE", "CLOSE", "FLUSH", "READ", "WRITE", "LOCK", "IOCTL",
	"CANCEL", "ECHO", "QUERY_DIRECTORY", "CHANGE_NOTIFY",
	"QUERY_INFO", "SET_INFO", "OPLOCK_BREAK",
}

func isValidSMBOperation(op string) bool {
	return slices.Contains(validSMBOperations, op)
}

// resetNFSSetting resets a single NFS setting to its default value.
// Returns false if the setting name is unknown.
func resetNFSSetting(settings, defaults *models.NFSAdapterSettings, name string) bool {
	switch name {
	case "min_version":
		settings.MinVersion = defaults.MinVersion
	case "max_version":
		settings.MaxVersion = defaults.MaxVersion
	case "lease_time":
		settings.LeaseTime = defaults.LeaseTime
	case "grace_period":
		settings.GracePeriod = defaults.GracePeriod
	case "delegation_recall_timeout":
		settings.DelegationRecallTimeout = defaults.DelegationRecallTimeout
	case "callback_timeout":
		settings.CallbackTimeout = defaults.CallbackTimeout
	case "lease_break_timeout":
		settings.LeaseBreakTimeout = defaults.LeaseBreakTimeout
	case "max_connections":
		settings.MaxConnections = defaults.MaxConnections
	case "max_clients":
		settings.MaxClients = defaults.MaxClients
	case "max_compound_ops":
		settings.MaxCompoundOps = defaults.MaxCompoundOps
	case "max_read_size":
		settings.MaxReadSize = defaults.MaxReadSize
	case "max_write_size":
		settings.MaxWriteSize = defaults.MaxWriteSize
	case "preferred_transfer_size":
		settings.PreferredTransferSize = defaults.PreferredTransferSize
	case "delegations_enabled":
		settings.DelegationsEnabled = defaults.DelegationsEnabled
	case "v4_min_minor_version":
		settings.V4MinMinorVersion = defaults.V4MinMinorVersion
	case "v4_max_minor_version":
		settings.V4MaxMinorVersion = defaults.V4MaxMinorVersion
	case "blocked_operations":
		settings.BlockedOperations = defaults.BlockedOperations
	case "portmapper_enabled":
		settings.PortmapperEnabled = defaults.PortmapperEnabled
	case "portmapper_port":
		settings.PortmapperPort = defaults.PortmapperPort
	default:
		return false
	}
	return true
}

// resetSMBSetting resets a single SMB setting to its default value.
func resetSMBSetting(settings, defaults *models.SMBAdapterSettings, name string) bool {
	switch name {
	case "min_dialect":
		settings.MinDialect = defaults.MinDialect
	case "max_dialect":
		settings.MaxDialect = defaults.MaxDialect
	case "session_timeout":
		settings.SessionTimeout = defaults.SessionTimeout
	case "oplock_break_timeout":
		settings.OplockBreakTimeout = defaults.OplockBreakTimeout
	case "max_connections":
		settings.MaxConnections = defaults.MaxConnections
	case "max_sessions":
		settings.MaxSessions = defaults.MaxSessions
	case "enable_encryption":
		settings.EnableEncryption = defaults.EnableEncryption
	case "blocked_operations":
		settings.BlockedOperations = defaults.BlockedOperations
	default:
		return false
	}
	return true
}

// --- Response converters ---

func nfsSettingsToResponse(s *models.NFSAdapterSettings) NFSSettingsResponse {
	ops := s.GetBlockedOperations()
	if ops == nil {
		ops = []string{}
	}
	return NFSSettingsResponse{
		MinVersion:              s.MinVersion,
		MaxVersion:              s.MaxVersion,
		LeaseTime:               s.LeaseTime,
		GracePeriod:             s.GracePeriod,
		DelegationRecallTimeout: s.DelegationRecallTimeout,
		CallbackTimeout:         s.CallbackTimeout,
		LeaseBreakTimeout:       s.LeaseBreakTimeout,
		MaxConnections:          s.MaxConnections,
		MaxClients:              s.MaxClients,
		MaxCompoundOps:          s.MaxCompoundOps,
		MaxReadSize:             s.MaxReadSize,
		MaxWriteSize:            s.MaxWriteSize,
		PreferredTransferSize:   s.PreferredTransferSize,
		DelegationsEnabled:      s.DelegationsEnabled,
		V4MinMinorVersion:       s.V4MinMinorVersion,
		V4MaxMinorVersion:       s.V4MaxMinorVersion,
		BlockedOperations:       ops,
		PortmapperEnabled:       s.PortmapperEnabled,
		PortmapperPort:          s.PortmapperPort,
		Version:                 s.Version,
	}
}

func smbSettingsToResponse(s *models.SMBAdapterSettings) SMBSettingsResponse {
	ops := s.GetBlockedOperations()
	if ops == nil {
		ops = []string{}
	}
	return SMBSettingsResponse{
		MinDialect:         s.MinDialect,
		MaxDialect:         s.MaxDialect,
		SessionTimeout:     s.SessionTimeout,
		OplockBreakTimeout: s.OplockBreakTimeout,
		MaxConnections:     s.MaxConnections,
		MaxSessions:        s.MaxSessions,
		EnableEncryption:   s.EnableEncryption,
		BlockedOperations:  ops,
		Version:            s.Version,
	}
}
