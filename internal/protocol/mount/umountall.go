package mount

import (
	"context"
	"net"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
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
	// Empty struct - UMNTALL returns void (no response data)
}

// UmountAllContext contains the context information needed to process an unmount-all request.
// Since UMNTALL removes all mounts for a specific client, only the client address is needed.
type UmountAllContext struct {
	Context context.Context
	// ClientAddr is the network address of the client making the request
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	// The IP portion is used to identify all mounts belonging to this client
	ClientAddr string
}

// UmntAll handles the UMOUNTALL (UMNTALL) procedure, which allows clients
// to remove all of their mount entries in a single operation.
//
// The unmount-all process:
//  1. Extract the client IP address from the network context
//  2. Query the repository for all mounts from this client
//  3. Remove all mount records for this client across all exports
//  4. Log the operation with count of removed mounts
//  5. Always return success (RFC 1813 specifies void return)
//
// Use cases for UMNTALL:
//   - Client is shutting down and wants to clean up all mounts
//   - Client encountered an error and wants to reset mount state
//   - Administrative cleanup of stale mount records
//   - Client reboot/restart scenarios
//
// Important notes:
//   - UMNTALL always succeeds, even if no mounts exist
//   - The actual unmounting happens on the client side
//   - Server-side tracking is informational only
//   - No error status codes are defined for UMNTALL in the protocol
//   - This is more efficient than calling UMNT for each mount individually
//
// Parameters:
//   - repository: The metadata repository that tracks active mounts
//   - req: Empty request struct (UMNTALL takes no parameters)
//   - ctx: Context information including client address
//
// Returns:
//   - *UmountAllResponse: Empty response (void)
//   - error: Always returns nil; unmount-all cannot fail per RFC 1813
//
// RFC 1813 Appendix I: UMNTALL Procedure
//
// Example:
//
//	handler := &DefaultMountHandler{}
//	req := &UmountAllRequest{}
//	ctx := &UmountAllContext{ClientAddr: "192.168.1.100:1234"}
//	resp, err := handler.UmntAll(repository, req, ctx)
//	// Response is always success
func (h *DefaultMountHandler) UmntAll(repository metadata.Repository, req *UmountAllRequest, ctx *UmountAllContext) (*UmountAllResponse, error) {
	// Extract client IP from address (remove port)
	clientIP, _, err := net.SplitHostPort(ctx.ClientAddr)
	if err != nil {
		// If parsing fails, use the whole address (might be IP only)
		clientIP = ctx.ClientAddr
	}

	logger.Info("Unmount-all request: client=%s", clientIP)

	// Get all mounts for this client to count them before removal
	mounts, err := repository.GetMountsByClient(clientIP)
	if err != nil {
		// Log the error but continue - we'll try to remove anyway
		logger.Warn("Failed to get mounts for client: client=%s error=%v", clientIP, err)
		mounts = []metadata.MountEntry{} // Empty list
	}

	mountCount := len(mounts)
	if mountCount == 0 {
		logger.Info("Unmount-all: client=%s had no active mounts", clientIP)
		return &UmountAllResponse{}, nil
	}

	// Remove all mounts for this client
	err = repository.RemoveAllMounts(clientIP)
	if err != nil {
		// Log the error but still return success per RFC 1813
		// The mount tracking is informational - the client has already unmounted
		logger.Warn("Failed to remove mount records: client=%s count=%d error=%v",
			clientIP, mountCount, err)
	} else {
		logger.Info("Unmount-all successful: client=%s removed=%d mounts",
			clientIP, mountCount)
	}

	// UMNTALL always returns void/success
	return &UmountAllResponse{}, nil
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
//
// Example:
//
//	data := []byte{} // Empty - UMNTALL has no parameters
//	req, err := DecodeUmountAllRequest(data)
//	if err != nil {
//	    // handle error
//	}
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
//
// Example:
//
//	resp := &UmountAllResponse{}
//	data, err := resp.Encode()
//	// data will be []byte{}, err will be nil
func (resp *UmountAllResponse) Encode() ([]byte, error) {
	// UMNTALL returns void (no data)
	return []byte{}, nil
}
