package handlers

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
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
	MountResponseBase // Embeds Status and GetStatus()
}

// Umnt handles MOUNT UMNT (RFC 1813 Appendix I, Mount procedure 3).
// Removes a client's mount record for a previously mounted filesystem.
// Delegates to Runtime.RemoveMount to clear mount tracking state.
// Removes mount session record (not the share itself); idempotent.
// Errors: none (UMNT always succeeds per RFC 1813, returns void).
func (h *Handler) Umnt(
	ctx *MountHandlerContext,
	req *UmountRequest,
) (*UmountResponse, error) {
	// Check for cancellation before starting
	// This is the only cancellation check for UMNT since:
	// 1. The operation is very fast (simple database delete)
	// 2. We want to complete cleanup once started to maintain consistency
	// 3. Per RFC 1813, UMNT always succeeds and should be quick
	if ctx.isContextCancelled() {
		logger.Debug("Unmount request cancelled before processing", "path", req.DirPath, "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return &UmountResponse{MountResponseBase: MountResponseBase{Status: MountOK}}, ctx.Context.Err()
	}

	// Extract client IP from address (remove port)
	clientIP := extractClientIP(ctx.ClientAddr)

	logger.Info("Unmount request", "path", req.DirPath, "client_ip", clientIP)

	// Remove the mount record from the registry
	// Note: We remove the mount SESSION, NOT the share itself! The share persists.
	// UMNT always succeeds per RFC 1813, even if no mount record exists
	removed := h.Registry.RemoveMount(clientIP)
	if removed {
		logger.Info("Unmount successful", "path", req.DirPath, "client_ip", clientIP)
	} else {
		logger.Debug("Unmount acknowledged (no active mount)", "path", req.DirPath, "client_ip", clientIP)
	}

	// UMNT always returns void/success per RFC 1813
	// Even if RemoveMount failed or was cancelled, we return success
	// because the client-side unmount has already occurred
	return &UmountResponse{MountResponseBase: MountResponseBase{Status: MountOK}}, nil
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
