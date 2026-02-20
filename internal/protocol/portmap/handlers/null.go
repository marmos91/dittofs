package handlers

// Null handles the portmap NULL procedure (procedure 0).
//
// The NULL procedure takes no arguments and returns void.
// It is used for connection testing (ping) per RFC 1057.
func (h *Handler) Null() []byte {
	return []byte{}
}
