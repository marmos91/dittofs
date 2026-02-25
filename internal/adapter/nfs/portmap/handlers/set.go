package handlers

import (
	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/xdr"
	"github.com/marmos91/dittofs/internal/logger"
)

// Set handles the portmap SET procedure (procedure 1).
//
// SET registers a mapping of (prog, vers, prot) -> port in the registry.
// The argument is an XDR-encoded Mapping struct.
// Returns an XDR boolean: true on success, false on failure.
//
// Per standard portmapper security practices, SET is restricted to
// localhost clients only. Remote clients receive false (rejected).
func (h *Handler) Set(data []byte, clientAddr string) ([]byte, error) {
	if !IsLocalhost(clientAddr) {
		logger.Warn("Portmap SET rejected: non-localhost client", "client", clientAddr)
		return xdr.EncodeBoolResponse(false), nil
	}

	m, err := xdr.DecodeMapping(data)
	if err != nil {
		return xdr.EncodeBoolResponse(false), err
	}

	result := h.Registry.Set(m)
	return xdr.EncodeBoolResponse(result), nil
}
