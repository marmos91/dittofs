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
// Since UMNTALL removes all mounts for a specific client, only the client address is needed,
// along with cancellation handling.
type UmountAllContext struct {
	// Context carries cancellation signals and deadlines
	// The UmntAll handler checks this context to handle client disconnection
	// Note: We prioritize completing cleanup once started to maintain
	// mount tracking consistency
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
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Extract the client IP address from the network context
//  3. Query the repository for all mounts from this client (for logging)
//  4. Check for cancellation before the destructive operation
//  5. Remove all mount records for this client across all exports
//  6. Log the operation with count of removed mounts
//  7. Always return success (RFC 1813 specifies void return)
//
// Context cancellation:
//   - Check at the beginning to respect client disconnection
//   - Check before RemoveAllMounts to avoid starting destructive operation
//   - Once RemoveAllMounts starts, let it complete to ensure consistency
//   - If cancelled during RemoveAllMounts, we still return success per RFC
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
//   - GetMountsByClient is optional (for logging) - failure is non-fatal
//
// Parameters:
//   - ctx: Context with cancellation and client information
//   - repository: The metadata repository that tracks active mounts
//   - req: Empty request struct (UMNTALL takes no parameters)
//
// Returns:
//   - *UmountAllResponse: Empty response (void)
//   - error: Returns error only if context was cancelled before cleanup;
//     otherwise always returns nil per RFC 1813
//
// RFC 1813 Appendix I: UMNTALL Procedure
//
// Example:
//
//	handler := &DefaultMountHandler{}
//	req := &UmountAllRequest{}
//	ctx := &UmountAllContext{
//	    Context: context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	}
//	resp, err := handler.UmntAll(ctx, repository, req)
//	if err != nil {
//	    if errors.Is(err, context.Canceled) {
//	        // Client disconnected before unmount-all could complete
//	    }
//	}
//	// Response is always success unless cancelled early
func (h *DefaultMountHandler) UmntAll(
	ctx *UmountAllContext,
	repository metadata.Repository,
	req *UmountAllRequest,
) (*UmountAllResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	select {
	case <-ctx.Context.Done():
		logger.Debug("Unmount-all request cancelled before processing: client=%s error=%v",
			ctx.ClientAddr, ctx.Context.Err())
		return &UmountAllResponse{}, ctx.Context.Err()
	default:
	}

	// Extract client IP from address (remove port)
	clientIP, _, err := net.SplitHostPort(ctx.ClientAddr)
	if err != nil {
		// If parsing fails, use the whole address (might be IP only)
		clientIP = ctx.ClientAddr
	}

	logger.Info("Unmount-all request: client=%s", clientIP)

	// Get all mounts for this client to count them before removal
	// This is optional (for logging purposes) - if it fails, we continue anyway
	// The repository should respect context cancellation internally
	mounts, err := repository.GetMountsByClient(ctx.Context, clientIP)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("Unmount-all cancelled while getting mount list: client=%s error=%v",
				clientIP, ctx.Context.Err())
			return &UmountAllResponse{}, ctx.Context.Err()
		}

		// Log the error but continue - we'll try to remove anyway
		logger.Warn("Failed to get mounts for client: client=%s error=%v", clientIP, err)
		mounts = []metadata.MountEntry{} // Empty list
	}

	mountCount := len(mounts)
	if mountCount == 0 {
		logger.Info("Unmount-all: client=%s had no active mounts", clientIP)
		return &UmountAllResponse{}, nil
	}

	// Check for cancellation before the destructive RemoveAllMounts operation
	// This is important because once we start removing, we want to complete
	// the operation to avoid leaving partial/inconsistent mount records
	select {
	case <-ctx.Context.Done():
		logger.Debug("Unmount-all cancelled before removing mounts: client=%s count=%d error=%v",
			clientIP, mountCount, ctx.Context.Err())
		return &UmountAllResponse{}, ctx.Context.Err()
	default:
	}

	// Remove all mounts for this client
	// We don't check for cancellation after this point because:
	// 1. We want to complete the cleanup to maintain consistency
	// 2. The operation should be relatively fast (batch delete)
	// 3. Partial removal would leave inconsistent mount tracking
	// 4. The client has already decided to unmount everything
	//
	// The repository should respect context internally if needed
	err = repository.RemoveAllMounts(ctx.Context, clientIP)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			// Context was cancelled during RemoveAllMounts
			// We still return success per RFC 1813 (UMNTALL always succeeds)
			// but log that some mount records may not have been removed
			logger.Warn("Unmount-all cancelled during mount record removal: client=%s count=%d error=%v (mount records may be partially removed)",
				clientIP, mountCount, ctx.Context.Err())
		} else {
			// Log the error but still return success per RFC 1813
			// The mount tracking is informational - the client has already unmounted
			logger.Warn("Failed to remove mount records: client=%s count=%d error=%v",
				clientIP, mountCount, err)
		}
	} else {
		logger.Info("Unmount-all successful: client=%s removed=%d mounts",
			clientIP, mountCount)
	}

	// UMNTALL always returns void/success per RFC 1813
	// Even if RemoveAllMounts failed or was cancelled, we return success
	// because the client-side unmount has already occurred
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
