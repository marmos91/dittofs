package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	"github.com/marmos91/dittofs/pkg/metadata"
	xdr "github.com/rasky/go-xdr/xdr2"
)

// DefaultMountHandler implements the Mount protocol handlers defined in RFC 1813 Appendix I.
// It provides the standard implementation for mount operations, allowing NFS clients
// to obtain file handles for exported filesystems.
type DefaultMountHandler struct{}

// MountRequest represents a MOUNT (MNT) request from an NFS client.
// The client sends the path of the directory they wish to mount.
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Appendix I specifies the MOUNT procedure as:
//
//	MNT(dirpath) -> fhstatus3
type MountRequest struct {
	// DirPath is the absolute path on the server that the client wants to mount.
	// This must match a share name configured in the server's repository.
	// Example: "/export" or "/data/shared"
	DirPath string
}

// MountResponse represents the response to a MOUNT (MNT) request.
// It contains the status of the mount operation and, if successful,
// the file handle and supported authentication methods.
//
// The response is encoded in XDR format before being sent back to the client.
type MountResponse struct {
	// Status indicates the result of the mount operation.
	// Common values:
	//   - MountOK (0): Success
	//   - MountErrNoEnt (2): Export path not found
	//   - MountErrAccess (13): Permission denied
	//   - MountErrServerFault (10006): Internal server error
	Status uint32

	// FileHandle is the opaque file handle for the root of the mounted filesystem.
	// This handle is used in subsequent NFS operations to identify the filesystem.
	// Only present when Status == MountOK.
	// The handle format is server-specific; clients treat it as opaque data.
	FileHandle []byte

	// AuthFlavors is a list of authentication flavors supported by the server
	// for this mount. Only present when Status == MountOK.
	// Common values:
	//   - 0: AUTH_NULL (no authentication)
	//   - 1: AUTH_UNIX (Unix-style authentication)
	AuthFlavors []int32
}

// MountContext contains the context information needed to process a mount request.
// This includes client identification, authentication details, and cancellation handling.
type MountContext struct {
	// Context carries cancellation signals and deadlines
	// The Mount handler checks this context to abort operations if the client
	// disconnects or the request times out
	Context context.Context

	// ClientAddr is the network address of the client making the request
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	ClientAddr string

	// AuthFlavor is the authentication method used by the client
	// 0 = AUTH_NULL, 1 = AUTH_UNIX, etc.
	AuthFlavor uint32

	// UnixAuth contains Unix authentication credentials if AuthFlavor == AUTH_UNIX
	// This includes UID, GID, machine name, etc.
	UnixAuth *rpc.UnixAuth
}

// Mount handles the MOUNT (MNT) procedure, which is the primary operation
// used by NFS clients to obtain a file handle for an exported filesystem.
//
// The mount process follows these steps:
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Extract client IP address from the network context
//  3. Extract and log authentication credentials
//  4. Perform access control checks via the repository (most expensive operation)
//  5. Validate authentication requirements
//  6. Retrieve the root file handle for the share
//  7. Record the mount in the repository for tracking
//  8. Return the handle with appropriate authentication flavors
//
// Context cancellation:
//   - The handler respects context cancellation at key I/O and computation points
//   - If the client disconnects or the request times out, the operation aborts
//   - Returns MountErrServerFault status with context.Canceled error for cancellation
//   - Cancellation is checked before and after expensive operations
//
// Security considerations:
//   - Validates client IP against share access control lists
//   - Enforces authentication requirements per share configuration
//   - Tracks active mounts for audit and the DUMP procedure
//   - Returns detailed error codes for troubleshooting
//
// Parameters:
//   - ctx: Context information including cancellation, client address, and auth flavor
//   - repository: The metadata repository containing share configurations
//   - req: The mount request containing the directory path to mount
//
// Returns:
//   - *MountResponse: The mount response with status and file handle (if successful)
//   - error: Returns error for context cancellation or internal server failures;
//     protocol-level errors are indicated via the response Status field
//
// RFC 1813 Appendix I: MOUNT Procedure
func (h *DefaultMountHandler) Mount(
	ctx *MountContext,
	repository metadata.MetadataStore,
	req *MountRequest,
) (*MountResponse, error) {
	// Check for cancellation before starting any work
	select {
	case <-ctx.Context.Done():
		logger.Debug("Mount request cancelled before processing: path=%s client=%s error=%v",
			req.DirPath, ctx.ClientAddr, ctx.Context.Err())
		return &MountResponse{Status: MountErrServerFault}, ctx.Context.Err()
	default:
	}

	// Extract client IP from address (remove port)
	clientIP, _, err := net.SplitHostPort(ctx.ClientAddr)
	if err != nil {
		// If parsing fails, use the whole address (might be IP only)
		clientIP = ctx.ClientAddr
	}

	// Build identity from authentication credentials
	var identity *metadata.Identity
	var authMethod string

	if ctx.AuthFlavor == rpc.AuthUnix && ctx.UnixAuth != nil {
		authMethod = "unix"
		identity = &metadata.Identity{
			UID:      &ctx.UnixAuth.UID,
			GID:      &ctx.UnixAuth.GID,
			GIDs:     ctx.UnixAuth.GIDs,
			Username: ctx.UnixAuth.MachineName, // NFS doesn't have usernames, use machine name
		}

		logger.Info("Mount request: path=%s client=%s auth=UNIX uid=%d gid=%d machine=%s",
			req.DirPath, clientIP, ctx.UnixAuth.UID, ctx.UnixAuth.GID, ctx.UnixAuth.MachineName)
	} else {
		authMethod = authFlavorName(ctx.AuthFlavor)
		identity = &metadata.Identity{
			Username: "anonymous",
		}
		logger.Info("Mount request: path=%s client=%s auth=%s",
			req.DirPath, clientIP, authMethod)
	}

	// Check for cancellation before the potentially expensive access control check
	select {
	case <-ctx.Context.Done():
		logger.Debug("Mount request cancelled before access check: path=%s client=%s error=%v",
			req.DirPath, clientIP, ctx.Context.Err())
		return &MountResponse{Status: MountErrServerFault}, ctx.Context.Err()
	default:
	}

	// Check share access control
	// This validates IP-based access control, authentication, and applies identity mapping
	accessDecision, authCtx, err := repository.CheckShareAccess(
		ctx.Context,
		req.DirPath, // Share name (NFS uses path as share name)
		clientIP,
		authMethod,
		identity,
	)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("Mount request cancelled during access check: path=%s client=%s error=%v",
				req.DirPath, clientIP, ctx.Context.Err())
			return &MountResponse{Status: MountErrServerFault}, ctx.Context.Err()
		}

		// Map StoreError to mount status codes
		if storeErr, ok := err.(*metadata.StoreError); ok {
			logger.Warn("Mount denied: path=%s client=%s reason=%s",
				req.DirPath, clientIP, storeErr.Message)

			status := mapStoreErrorToMountStatus(storeErr.Code)
			return &MountResponse{Status: status}, nil
		}

		logger.Error("Mount access check failed: path=%s client=%s error=%v",
			req.DirPath, clientIP, err)
		return &MountResponse{Status: MountErrServerFault}, nil
	}

	if !accessDecision.Allowed {
		logger.Warn("Mount access denied: path=%s client=%s reason=%s",
			req.DirPath, clientIP, accessDecision.Reason)
		return &MountResponse{Status: MountErrAccess}, nil
	}

	// Log identity mapping if credentials were changed
	if identity.UID != nil && authCtx.Identity.UID != nil && *identity.UID != *authCtx.Identity.UID {
		logger.Info("Mount: credentials mapped for path=%s client=%s original_uid=%d effective_uid=%d original_gid=%d effective_gid=%d",
			req.DirPath, clientIP,
			*identity.UID,
			*authCtx.Identity.UID,
			func() uint32 {
				if identity.GID != nil {
					return *identity.GID
				}
				return 0
			}(),
			func() uint32 {
				if authCtx.Identity.GID != nil {
					return *authCtx.Identity.GID
				}
				return 0
			}(),
		)
	}

	// Check for cancellation before retrieving the root handle
	select {
	case <-ctx.Context.Done():
		logger.Debug("Mount request cancelled before root handle retrieval: path=%s client=%s error=%v",
			req.DirPath, clientIP, ctx.Context.Err())
		return &MountResponse{Status: MountErrServerFault}, ctx.Context.Err()
	default:
	}

	// Get the root file handle for the share
	rootHandle, err := repository.GetShareRoot(ctx.Context, req.DirPath)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("Mount request cancelled during root handle retrieval: path=%s client=%s error=%v",
				req.DirPath, clientIP, ctx.Context.Err())
			return &MountResponse{Status: MountErrServerFault}, ctx.Context.Err()
		}

		// Map StoreError to mount status codes
		if storeErr, ok := err.(*metadata.StoreError); ok {
			logger.Error("Failed to get root handle: path=%s client=%s error=%s",
				req.DirPath, clientIP, storeErr.Message)
			status := mapStoreErrorToMountStatus(storeErr.Code)
			return &MountResponse{Status: status}, nil
		}

		logger.Error("Failed to get root handle: path=%s client=%s error=%v",
			req.DirPath, clientIP, err)
		return &MountResponse{Status: MountErrServerFault}, nil
	}

	// Record the mount session for tracking (needed by DUMP procedure)
	// Note: We don't fail the mount if session recording fails - it's informational only
	if err := repository.RecordShareMount(ctx.Context, req.DirPath, clientIP); err != nil {
		logger.Warn("Failed to record mount session: path=%s client=%s error=%v",
			req.DirPath, clientIP, err)
	}

	// Convert allowed auth methods to int32 array for response
	authFlavors := make([]int32, len(accessDecision.AllowedAuthMethods))
	for i, method := range accessDecision.AllowedAuthMethods {
		authFlavors[i] = int32(authMethodToFlavor(method))
	}

	// If no specific auth methods configured, return the one used for mount
	if len(authFlavors) == 0 {
		authFlavors = []int32{int32(ctx.AuthFlavor)}
	}

	logger.Info("Mount successful: path=%s client=%s handle_len=%d auth_flavors=%v readonly=%v",
		req.DirPath, clientIP, len(rootHandle), authFlavors, accessDecision.ReadOnly)

	return &MountResponse{
		Status:      MountOK,
		FileHandle:  rootHandle,
		AuthFlavors: authFlavors,
	}, nil
}

// DecodeMountRequest decodes a MOUNT request from XDR-encoded bytes.
// It uses the XDR unmarshaling library to parse the incoming data according
// to the Mount protocol specification.
//
// Parameters:
//   - data: XDR-encoded bytes containing the mount request
//
// Returns:
//   - *MountRequest: The decoded mount request containing the directory path
//   - error: Any error encountered during decoding
func DecodeMountRequest(data []byte) (*MountRequest, error) {
	req := &MountRequest{}
	_, err := xdr.Unmarshal(bytes.NewReader(data), req)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal mount request: %w", err)
	}

	// Validate the path
	if err := ValidateExportPath(req.DirPath); err != nil {
		return nil, fmt.Errorf("invalid export path: %w", err)
	}

	return req, nil
}

// Encode serializes the MountResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Appendix I specifications:
//  1. Status code (4 bytes)
//  2. If status == MountOK:
//     a. File handle length (4 bytes)
//     b. File handle data (variable length)
//     c. Padding to 4-byte boundary
//     d. Auth flavors count (4 bytes)
//     e. Auth flavor values (4 bytes each)
//
// XDR encoding requires all data to be aligned to 4-byte boundaries,
// so padding bytes are added after variable-length fields.
func (resp *MountResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// If status is not OK, we're done - only status is returned for errors
	if resp.Status != MountOK {
		return buf.Bytes(), nil
	}

	// Write file handle (opaque data)
	// XDR opaque format: length followed by data
	handleLen := uint32(len(resp.FileHandle))
	if err := binary.Write(&buf, binary.BigEndian, handleLen); err != nil {
		return nil, fmt.Errorf("write handle length: %w", err)
	}

	buf.Write(resp.FileHandle)

	// Add padding to 4-byte boundary (XDR alignment requirement)
	padding := (4 - (handleLen % 4)) % 4
	buf.Write(make([]byte, padding))

	// Write auth flavors array
	// XDR array format: count followed by elements
	authCount := uint32(len(resp.AuthFlavors))
	if err := binary.Write(&buf, binary.BigEndian, authCount); err != nil {
		return nil, fmt.Errorf("write auth count: %w", err)
	}

	for _, flavor := range resp.AuthFlavors {
		if err := binary.Write(&buf, binary.BigEndian, flavor); err != nil {
			return nil, fmt.Errorf("write auth flavor: %w", err)
		}
	}

	return buf.Bytes(), nil
}

// mapStoreErrorToMountStatus converts repository store errors to Mount protocol status codes
func mapStoreErrorToMountStatus(code metadata.ErrorCode) uint32 {
	switch code {
	case metadata.ErrNotFound:
		return MountErrNoEnt
	case metadata.ErrAccessDenied:
		return MountErrAccess
	case metadata.ErrAuthRequired:
		return MountErrAccess
	case metadata.ErrPermissionDenied:
		return MountErrAccess
	default:
		return MountErrServerFault
	}
}

// authFlavorName returns a human-readable name for an auth flavor
func authFlavorName(flavor uint32) string {
	switch flavor {
	case rpc.AuthNull:
		return "NULL"
	case rpc.AuthUnix:
		return "UNIX"
	case rpc.AuthShort:
		return "SHORT"
	case rpc.AuthDES:
		return "DES"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", flavor)
	}
}

// authMethodToFlavor converts string auth method to RPC auth flavor
func authMethodToFlavor(method string) uint32 {
	switch method {
	case "anonymous", "null":
		return rpc.AuthNull
	case "unix":
		return rpc.AuthUnix
	case "short":
		return rpc.AuthShort
	case "des":
		return rpc.AuthDES
	default:
		return rpc.AuthNull
	}
}
