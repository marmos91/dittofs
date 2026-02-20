package handlers

import (
	"github.com/marmos91/dittofs/internal/protocol/portmap/xdr"
)

// Set handles the portmap SET procedure (procedure 1).
//
// SET registers a mapping of (prog, vers, prot) -> port in the registry.
// The argument is an XDR-encoded Mapping struct.
// Returns an XDR boolean: true on success, false on failure.
func (h *Handler) Set(data []byte) ([]byte, error) {
	m, err := xdr.DecodeMapping(data)
	if err != nil {
		return xdr.EncodeBoolResponse(false), err
	}

	result := h.Registry.Set(m)
	return xdr.EncodeBoolResponse(result), nil
}
