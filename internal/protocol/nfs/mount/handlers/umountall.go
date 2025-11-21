package handlers

import (
	"net"

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

// UmntAll handles the UMOUNTALL (UMNTALL) procedure, which allows clients
// to remove all of their mount entries in a single operation.
//
// The unmount-all process:
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Extract the client IP address from the network context
//  3. Query the repository for all mounts from this client (for logging)
//  4. Check for cancellation before the destructive operation
//  5. Remove all mount records for this client across all shares
//  6. Log the operation with count of removed mounts
//  7. Always return success (RFC 1813 specifies void return)
//
// Context cancellation:
//   - Check at the beginning to respect client disconnection
//   - Check before removing mounts to avoid starting destructive operation
//   - Once removal starts, let it complete to ensure consistency
//   - If cancelled during removal, we still return success per RFC
//
// Use cases for UMNTALL:
//   - Client is shutting down and wants to clean up all mounts
//   - Client encountered an error and wants to reset mount state
//   - Administrative cleanup of stale mount records
//   - Client reboot/restart scenarios
//
// Important notes:
//   - UMNTALL always succeeds per RFC 1813, even if no mounts exist
//   - The actual unmounting happens on the client side
//   - Server-side tracking is informational only
//   - No error status codes are defined for UMNTALL in the protocol
//   - This is more efficient than calling UMNT for each mount individually
//
// Parameters:
//   - ctx: Context with cancellation and client information
//   - repository: The metadata repository that tracks active shares
//   - req: Empty request struct (UMNTALL takes no parameters)
//
// Returns:
//   - *UmountAllResponse: Empty response (void)
//   - error: Returns error only if context was cancelled before cleanup;
//     otherwise always returns nil per RFC 1813
//
// RFC 1813 Appendix I: UMNTALL Procedure
func (h *Handler) UmntAll(
	ctx *MountHandlerContext,
	req *UmountAllRequest,
) (*UmountAllResponse, error) {
	// Check for cancellation before starting any work
	select {
	case <-ctx.Context.Done():
		logger.Debug("Unmount-all request cancelled before processing: client=%s error=%v",
			ctx.ClientAddr, ctx.Context.Err())
		return &UmountAllResponse{MountResponseBase: MountResponseBase{Status: MountOK}}, ctx.Context.Err()
	default:
	}

	// Extract client IP from address (remove port)
	clientIP, _, err := net.SplitHostPort(ctx.ClientAddr)
	if err != nil {
		// If parsing fails, use the whole address (might be IP only)
		clientIP = ctx.ClientAddr
	}

	logger.Info("Unmount-all request: client=%s", clientIP)

	// Remove all mount records from the registry
	count := h.Registry.RemoveAllMounts()

	logger.Info("Unmount-all successful: client=%s removed=%d mount(s)", clientIP, count)

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
