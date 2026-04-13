package handlers

import (
	"context"
	"errors"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// resolveIdentityMapping checks the identity mapping store for a principal-to-username
// mapping. This enables cross-protocol identity consistency: the same Kerberos or NTLM
// principal resolves to the same control plane user regardless of whether the client
// connects via NFS or SMB.
//
// Returns (mapped username, true) if a mapping was found, or (fallbackUsername, false)
// if no mapping exists. Callers should use the found flag to distinguish "no mapping"
// from "mapping exists but resolved user may not exist in UserStore" — the latter
// should be treated as a hard auth failure.
//
// Security note: for NTLM principals ("DOMAIN\user"), this also tries the bare
// username as a fallback key. A mapping keyed by bare username "alice" will match
// any domain principal "*/alice". Operators should use fully-qualified principals
// (e.g., "CORP\alice") for precise matching.
func (h *Handler) resolveIdentityMapping(ctx context.Context, principal, fallbackUsername string) (string, bool) {
	ims := h.Registry.GetIdentityMappingStore()
	if ims == nil {
		return fallbackUsername, false
	}

	// Try the full principal first (e.g., "DOMAIN\user" or "user@REALM").
	mapping, err := ims.GetIdentityMapping(ctx, "kerberos", principal)
	if err == nil {
		logger.Debug("Identity mapping resolved",
			"principal", principal, "username", mapping.Username)
		return mapping.Username, true
	}
	if !errors.Is(err, models.ErrMappingNotFound) {
		logger.Error("Identity mapping store error", "principal", principal, "error", err)
		return fallbackUsername, false
	}

	// For NTLM principals ("DOMAIN\user"), also try bare username as fallback.
	if idx := strings.IndexByte(principal, '\\'); idx >= 0 {
		bare := principal[idx+1:]
		mapping, err := ims.GetIdentityMapping(ctx, "kerberos", bare)
		if err == nil {
			logger.Debug("Identity mapping resolved via bare username",
				"principal", principal, "bare", bare, "username", mapping.Username)
			return mapping.Username, true
		}
		if !errors.Is(err, models.ErrMappingNotFound) {
			logger.Error("Identity mapping store error", "principal", bare, "error", err)
			return fallbackUsername, false
		}
	}

	return fallbackUsername, false
}

// formatNTLMPrincipal constructs an NTLM principal string from domain and username.
// Returns "DOMAIN\username" if domain is non-empty, otherwise just "username".
func formatNTLMPrincipal(domain, username string) string {
	if domain != "" {
		return domain + `\` + username
	}
	return username
}
