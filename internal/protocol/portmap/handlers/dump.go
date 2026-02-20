package handlers

import (
	"github.com/marmos91/dittofs/internal/protocol/portmap/xdr"
)

// Dump handles the portmap DUMP procedure (procedure 4).
//
// DUMP returns all registered mappings as an XDR optional-data linked list.
// It takes no arguments.
func (h *Handler) Dump() []byte {
	mappings := h.Registry.Dump()
	return xdr.EncodeDumpResponse(mappings)
}
