package handlers

import "github.com/marmos91/dittofs/internal/adapter/portmap/xdr"

// Getport handles the portmap GETPORT procedure (procedure 3).
//
// GETPORT looks up the port for a given (prog, vers, prot) tuple.
// Only the prog, vers, and prot fields of the mapping argument are used.
// Returns the port number as a uint32, or 0 if not registered (per RFC 1057).
func (h *Handler) Getport(data []byte) ([]byte, error) {
	m, err := xdr.DecodeMapping(data)
	if err != nil {
		return xdr.EncodeGetportResponse(0), err
	}

	port := h.Registry.Getport(m.Prog, m.Vers, m.Prot)
	return xdr.EncodeGetportResponse(port), nil
}
