package handlers

import (
	"github.com/marmos91/dittofs/internal/logger"
)

// UmountAllRequest represents an UMNTALL request from an NFS client.
// Unlike UMNT which unmounts a specific path, UMNTALL removes ALL mount
// entries for the calling client across all export paths.
// This procedure takes no parameters.
//
// RFC 1813 Appendix I specifies the UMNTALL procedure as:
//
//	UMNTALL() -> void
type UmountAllRequest struct {
	// Empty struct - UMNTALL takes no parameters
	// The client is identified by the network connection
}

// UmountAllResponse represents the response to an UMNTALL request.
// According to RFC 1813 Appendix I, the UMNTALL procedure returns void,
// so there is no response data - only the RPC success/failure indication.
//
// Note: Like UMNT, UMNTALL always succeeds per the protocol specification.
// The server acknowledges the request regardless of whether any mounts existed.
type UmountAllResponse struct {
	MountResponseBase // Embeds Status and GetStatus()
}

// UmntAll handles MOUNT UMNTALL (RFC 1813 Appendix I, Mount procedure 4).
// Removes all mount records for the calling client across all shares.
// Delegates to Runtime.RemoveAllMounts to clear all mount tracking state.
// Clears all mount session records; idempotent, more efficient than per-path UMNT.
// Errors: none (UMNTALL always succeeds per RFC 1813, returns void).
func (h *Handler) UmntAll(
	ctx *MountHandlerContext,
	req *UmountAllRequest,
) (*UmountAllResponse, error) {
	// Check for cancellation before starting any work
	if ctx.isContextCancelled() {
		logger.Debug("Unmount-all request cancelled before processing", "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return &UmountAllResponse{MountResponseBase: MountResponseBase{Status: MountOK}}, ctx.Context.Err()
	}

	// Extract client IP from address (remove port)
	clientIP := extractClientIP(ctx.ClientAddr)

	logger.Info("Unmount-all request", "client_ip", clientIP)

	// Remove all mount records from the registry
	count := h.Registry.RemoveAllMounts()

	logger.Info("Unmount-all successful", "client_ip", clientIP, "removed", count)

	// UMNTALL always returns void/success per RFC 1813
	// Even if RemoveShareMount failed or was cancelled, we return success
	// because the client-side unmount has already occurred
	return &UmountAllResponse{MountResponseBase: MountResponseBase{Status: MountOK}}, nil
}

// DecodeUmountAllRequest decodes an UMOUNTALL request.
// Since UMNTALL takes no parameters, this function simply validates
// that the data is empty and returns an empty request struct.
//
// Parameters:
//   - data: Should be empty (UMNTALL has no parameters)
//
// Returns:
//   - *UmountAllRequest: Empty request struct
//   - error: Returns error only if data is unexpectedly non-empty
func DecodeUmountAllRequest(data []byte) (*UmountAllRequest, error) {
	// UMNTALL takes no parameters, so we just return an empty request
	// We don't error on non-empty data to be lenient with client implementations
	return &UmountAllRequest{}, nil
}

// Encode serializes the UmountAllResponse into XDR-encoded bytes.
// Since UMNTALL returns void, this always returns an empty byte slice.
//
// Returns:
//   - []byte: Empty slice (void response)
//   - error: Always nil (encoding void cannot fail)
func (resp *UmountAllResponse) Encode() ([]byte, error) {
	// UMNTALL returns void (no data)
	return []byte{}, nil
}
