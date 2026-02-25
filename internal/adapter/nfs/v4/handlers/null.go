package handlers

// HandleNull implements the NFSv4 NULL procedure (procedure 0).
//
// Per RFC 7530, the NULL procedure is a standard RPC ping/keepalive.
// It is NOT a COMPOUND operation -- it's a separate RPC procedure.
// The request and response bodies are both empty.
//
// Returns an empty reply body (same behavior as NFSv3 NULL).
func (h *Handler) HandleNull(data []byte) ([]byte, error) {
	return []byte{}, nil
}
