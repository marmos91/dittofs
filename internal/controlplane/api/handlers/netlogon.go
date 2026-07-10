package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/marmos91/dittofs/internal/auth/netlogon"
)

// NetlogonHandler serves the admin-only NETLOGON machine-account API: it reports
// live machine-account / secure-channel state and forces a machine-password
// rotation on the running server (#1631). It is backed by the netlogon.Controller
// the wiring layer registers with the Runtime, so it never reaches into the SMB
// adapter or the process-local authenticator.
type NetlogonHandler struct {
	ctrl *netlogon.Controller
}

// NewNetlogonHandler constructs a NetlogonHandler. Returns nil when ctrl is nil
// (NTLM pass-through not configured), so the router can skip mounting the routes
// and the API cleanly 404s instead of exposing a dead surface.
func NewNetlogonHandler(ctrl *netlogon.Controller) *NetlogonHandler {
	if ctrl == nil {
		return nil
	}
	return &NetlogonHandler{ctrl: ctrl}
}

// netlogonStatusDTO is the wire shape of `netlogon status`. Times are RFC3339
// strings, omitted when zero (never rotated / rotation disabled), so a client
// renders "-" rather than the Go zero time.
type netlogonStatusDTO struct {
	Enabled          bool     `json:"enabled"`
	Provider         string   `json:"provider"`
	AccountName      string   `json:"account_name,omitempty"`
	Realm            string   `json:"realm,omitempty"`
	NetBIOSDomain    string   `json:"netbios_domain,omitempty"`
	DCAddresses      []string `json:"dc_addresses,omitempty"`
	Joined           bool     `json:"joined"`
	ChannelConnected bool     `json:"channel_connected"`
	RotationEnabled  bool     `json:"rotation_enabled"`
	RotationInterval string   `json:"rotation_interval,omitempty"`
	LastRotation     string   `json:"last_rotation,omitempty"`
	NextRotation     string   `json:"next_rotation,omitempty"`
}

// netlogonRotateResultDTO is the wire shape of a successful `netlogon rotate`: it
// echoes the post-rotation status so the operator sees the new state in one call.
type netlogonRotateResultDTO struct {
	OK      bool              `json:"ok"`
	Message string            `json:"message"`
	Status  netlogonStatusDTO `json:"status"`
}

// Status reports live machine-account and secure-channel state. It performs no
// DC I/O (in particular it never provisions the online-join account).
func (h *NetlogonHandler) Status(w http.ResponseWriter, r *http.Request) {
	WriteJSONOK(w, netlogonStatusToDTO(h.ctrl.Status(r.Context())))
}

// Rotate forces a correct (PasswordSet2 → switch → persist) machine-password
// rotation now. It applies only to the online-join provider; for the offline
// provider it returns 409 Conflict with a clear message.
func (h *NetlogonHandler) Rotate(w http.ResponseWriter, r *http.Request) {
	if err := h.ctrl.Rotate(r.Context()); err != nil {
		if errors.Is(err, netlogon.ErrRotateNotOnlineJoin) {
			// Expected, caller-actionable condition: not a server fault.
			Conflict(w, err.Error())
			return
		}
		// Unexpected: the DC rejected the set, or persistence failed.
		InternalServerError(w, fmt.Sprintf("machine-password rotation failed: %v", err))
		return
	}
	WriteJSONOK(w, netlogonRotateResultDTO{
		OK:      true,
		Message: "machine-account password rotated",
		Status:  netlogonStatusToDTO(h.ctrl.Status(r.Context())),
	})
}

// netlogonStatusToDTO maps the domain Status onto the wire DTO, formatting times
// as RFC3339 (empty when zero) and the interval as a Go duration string.
func netlogonStatusToDTO(s netlogon.Status) netlogonStatusDTO {
	dto := netlogonStatusDTO{
		Enabled:          true,
		Provider:         string(s.Provider),
		AccountName:      s.AccountName,
		Realm:            s.Realm,
		NetBIOSDomain:    s.NetBIOSDomain,
		DCAddresses:      s.DCAddresses,
		Joined:           s.Joined,
		ChannelConnected: s.ChannelConnected,
		RotationEnabled:  s.RotationEnabled,
	}
	if s.RotationInterval > 0 {
		dto.RotationInterval = s.RotationInterval.String()
	}
	if !s.LastRotation.IsZero() {
		dto.LastRotation = s.LastRotation.UTC().Format(time.RFC3339)
	}
	if !s.NextRotation.IsZero() {
		dto.NextRotation = s.NextRotation.UTC().Format(time.RFC3339)
	}
	return dto
}
