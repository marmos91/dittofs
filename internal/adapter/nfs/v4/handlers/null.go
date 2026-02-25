package handlers

// HandleNull implements the NFSv4 NULL procedure (RFC 7530, procedure 0).
// No-op ping/health check verifying the NFSv4 service is running and reachable.
// No delegation; returns immediately with empty response (no store access).
// No side effects; separate RPC procedure (not a COMPOUND operation).
// Errors: none (NULL always succeeds).
func (h *Handler) HandleNull(data []byte) ([]byte, error) {
	return []byte{}, nil
}
