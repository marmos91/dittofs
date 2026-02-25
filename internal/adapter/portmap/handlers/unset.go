package handlers

import (
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/portmap/xdr"
)

// Unset handles the portmap UNSET procedure (procedure 2).
//
// UNSET removes a mapping for (prog, vers, prot). Per RFC 1057,
// only the prog, vers, and prot fields are used; the port field is ignored.
// Returns an XDR boolean: true if the mapping existed and was removed, false otherwise.
//
// Per standard portmapper security practices, UNSET is restricted to
// localhost clients only. Remote clients receive false (rejected).
func (h *Handler) Unset(data []byte, clientAddr string) ([]byte, error) {
	if !IsLocalhost(clientAddr) {
		logger.Warn("Portmap UNSET rejected: non-localhost client", "client", clientAddr)
		return xdr.EncodeBoolResponse(false), nil
	}

	m, err := xdr.DecodeMapping(data)
	if err != nil {
		return xdr.EncodeBoolResponse(false), err
	}

	result := h.Registry.Unset(m.Prog, m.Vers, m.Prot)
	return xdr.EncodeBoolResponse(result), nil
}
