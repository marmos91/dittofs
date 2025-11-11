package mount

import (
	"bytes"
	"context"
	"fmt"
	"net"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
	xdr "github.com/rasky/go-xdr/xdr2"
)

// UmountRequest represents an UMNT (unmount) request from an NFS client.
// The client indicates they are done using a previously mounted filesystem.
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Appendix I specifies the UMNT procedure as:
//
//	UMNT(dirpath) -> void
type UmountRequest struct {
	// DirPath is the absolute path on the server that the client wants to unmount.
	// This should match a path that was previously mounted via the MOUNT procedure.
	// Example: "/export" or "/data/shared"
	DirPath string
}

// UmountResponse represents the response to an UMNT request.
// According to RFC 1813 Appendix I, the UMNT procedure returns void,
// so there is no response data - only the RPC success/failure indication.
//
// Note: The NFS protocol does not define error conditions for UMNT.
// The server always acknowledges the unmount request, even if:
//   - The path was never mounted
//   - The mount was already removed
//   - The client never had permission to mount
//
// This is because unmounting is primarily a client-side operation.
// The server's mount tracking is informational and used by the DUMP procedure.
type UmountResponse struct {
	// Empty struct - UMNT returns void (no response data)
}

// UmountContext contains the context information needed to process an unmount request.
// This includes client identification for removing the correct mount record and
// cancellation handling.
type UmountContext struct {
	// Context carries cancellation signals and deadlines
	// The Umnt handler checks this context to handle client disconnection
	// Note: Even if cancelled, we may complete the unmount to maintain
	// mount tracking consistency
	Context context.Context

	// ClientAddr is the network address of the client making the request
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	ClientAddr string
}

// Umnt handles the UMOUNT (UMNT) procedure, which allows clients to indicate
// they are done using a previously mounted filesystem.
//
// The unmount process:
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Extract the client IP address from the network context
//  3. Remove the mount record from the repository's tracking
//  4. Log the unmount operation for audit purposes
//  5. Always return success (RFC 1813 specifies void return)
//
// Context cancellation:
//   - Single check at the beginning to respect client disconnection
//   - No check after RemoveMount to ensure cleanup completes
//   - If cancelled early, returns immediately with error
//   - If RemoveMount is in progress when cancelled, we let it complete
//     to maintain mount tracking consistency
//
// Important notes:
//   - UMNT always succeeds per RFC 1813, even if the mount doesn't exist
//   - The actual unmounting happens on the client side
//   - Server-side tracking is informational only (used by DUMP)
//   - No error status codes are defined for UMNT in the protocol
//   - RemoveMount failure is logged but doesn't fail the UMNT request
//
// The mount tracking serves several purposes:
//   - Provides data for the DUMP procedure (list active mounts)
//   - Enables audit logging of mount/unmount operations
//   - Allows monitoring of active NFS clients
//   - Can be used for graceful server shutdown (notify mounted clients)
//
// Parameters:
//   - ctx: Context with cancellation and client information
//   - repository: The metadata repository that tracks active mounts
//   - req: The unmount request containing the directory path to unmount
//
// Returns:
//   - *UmountResponse: Empty response (void)
//   - error: Returns error only if context was cancelled before processing;
//     otherwise always returns nil per RFC 1813
//
// RFC 1813 Appendix I: UMNT Procedure
//
// Example:
//
//	handler := &DefaultMountHandler{}
//	req := &UmountRequest{DirPath: "/export"}
//	ctx := &UmountContext{
//	    Context: context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	}
//	resp, err := handler.Umnt(ctx, repository, req)
//	if err != nil {
//	    if errors.Is(err, context.Canceled) {
//	        // Client disconnected before unmount could be processed
//	    }
//	}
//	// Response is always success unless cancelled early
func (h *DefaultMountHandler) Umnt(
	ctx *UmountContext,
	repository metadata.Repository,
	req *UmountRequest,
) (*UmountResponse, error) {
	// Check for cancellation before starting
	// This is the only cancellation check for UMNT since:
	// 1. The operation is very fast (simple database delete)
	// 2. We want to complete cleanup once started to maintain consistency
	// 3. Per RFC 1813, UMNT always succeeds and should be quick
	select {
	case <-ctx.Context.Done():
		logger.Debug("Unmount request cancelled before processing: path=%s client=%s error=%v",
			req.DirPath, ctx.ClientAddr, ctx.Context.Err())
		return &UmountResponse{}, ctx.Context.Err()
	default:
	}

	// Extract client IP from address (remove port)
	clientIP, _, err := net.SplitHostPort(ctx.ClientAddr)
	if err != nil {
		// If parsing fails, use the whole address (might be IP only)
		clientIP = ctx.ClientAddr
	}

	logger.Info("Unmount request: path=%s client=%s", req.DirPath, clientIP)

	// Remove the mount record from tracking
	// Note: We don't check for errors because UMNT always succeeds per RFC 1813
	// Even if the mount doesn't exist, we acknowledge the unmount
	//
	// We don't check for cancellation here because:
	// 1. RemoveMount should be very fast (typically < 1ms)
	// 2. We want to complete the cleanup to maintain tracking consistency
	// 3. The client has already decided to unmount, so we should honor that
	// 4. Leaving stale mount records would pollute the DUMP output
	//
	// The repository should respect context internally if needed
	err = repository.RemoveMount(ctx.Context, req.DirPath, clientIP)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			// Context was cancelled during RemoveMount
			// We still return success per RFC 1813 (UMNT always succeeds)
			// but log that the mount record may not have been removed
			logger.Warn("Unmount cancelled during mount record removal: path=%s client=%s error=%v (mount record may be stale)",
				req.DirPath, clientIP, ctx.Context.Err())
		} else {
			// Log the error but still return success
			// The mount tracking is informational - the client has already unmounted
			logger.Warn("Failed to remove mount record: path=%s client=%s error=%v",
				req.DirPath, clientIP, err)
		}
	} else {
		logger.Info("Unmount successful: path=%s client=%s", req.DirPath, clientIP)
	}

	// UMNT always returns void/success per RFC 1813
	// Even if RemoveMount failed or was cancelled, we return success
	// because the client-side unmount has already occurred
	return &UmountResponse{}, nil
}

// DecodeUmountRequest decodes an UMOUNT request from XDR-encoded bytes.
// It uses the XDR unmarshaling library to parse the incoming data according
// to the Mount protocol specification.
//
// Parameters:
//   - data: XDR-encoded bytes containing the unmount request
//
// Returns:
//   - *UmountRequest: The decoded unmount request containing the directory path
//   - error: Any error encountered during decoding
//
// Example:
//
//	data := []byte{...} // XDR-encoded unmount request
//	req, err := DecodeUmountRequest(data)
//	if err != nil {
//	    // handle error
//	}
//	fmt.Println("Unmount path:", req.DirPath)
func DecodeUmountRequest(data []byte) (*UmountRequest, error) {
	req := &UmountRequest{}
	_, err := xdr.Unmarshal(bytes.NewReader(data), req)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal umount request: %w", err)
	}

	// Validate the path
	if err := ValidateExportPath(req.DirPath); err != nil {
		return nil, fmt.Errorf("invalid export path: %w", err)
	}

	return req, nil
}

// Encode serializes the UmountResponse into XDR-encoded bytes.
// Since UMNT returns void, this always returns an empty byte slice.
//
// Returns:
//   - []byte: Empty slice (void response)
//   - error: Always nil (encoding void cannot fail)
//
// Example:
//
//	resp := &UmountResponse{}
//	data, err := resp.Encode()
//	// data will be []byte{}, err will be nil
func (resp *UmountResponse) Encode() ([]byte, error) {
	// UMNT returns void (no data)
	return []byte{}, nil
}
