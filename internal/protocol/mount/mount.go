package mount

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
	"github.com/marmos91/dittofs/internal/protocol/rpc"
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
	// This must match an exported path configured in the server's repository.
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
// This includes client identification and authentication details.
type MountContext struct {
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
//  1. Extract client IP address from the network context
//  2. Perform access control checks via the repository
//  3. Validate authentication requirements
//  4. Retrieve the root file handle for the export
//  5. Record the mount in the repository for tracking
//  6. Return the handle with appropriate authentication flavors
//
// Security considerations:
//   - Validates client IP against export access control lists
//   - Enforces authentication requirements per export configuration
//   - Tracks active mounts for audit and the DUMP procedure
//   - Returns detailed error codes for troubleshooting
//
// Parameters:
//   - repository: The metadata repository containing export configurations
//   - req: The mount request containing the directory path to mount
//   - ctx: Context information including client address and auth flavor
//
// Returns:
//   - *MountResponse: The mount response with status and file handle (if successful)
//   - error: Returns error only for internal server failures; protocol-level
//     errors are indicated via the response Status field
//
// RFC 1813 Appendix I: MOUNT Procedure
//
// Example:
//
//	handler := &DefaultMountHandler{}
//	req := &MountRequest{DirPath: "/export"}
//	ctx := &MountContext{
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 0, // AUTH_NULL
//	}
//	resp, err := handler.Mount(repository, req, ctx)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == MountOK {
//	    // Use resp.FileHandle for NFS operations
//	}
func (h *DefaultMountHandler) Mount(repository metadata.Repository, req *MountRequest, ctx *MountContext) (*MountResponse, error) {
	// Extract client IP from address (remove port)
	clientIP, _, err := net.SplitHostPort(ctx.ClientAddr)
	if err != nil {
		// If parsing fails, use the whole address (might be IP only)
		clientIP = ctx.ClientAddr
	}

	// Extract credentials from context
	var uid, gid *uint32
	var gids []uint32

	if ctx.AuthFlavor == rpc.AuthUnix && ctx.UnixAuth != nil {
		uid = &ctx.UnixAuth.UID
		gid = &ctx.UnixAuth.GID
		gids = ctx.UnixAuth.GIDs

		logger.Info("Mount request: path=%s client=%s auth=UNIX uid=%d gid=%d machine=%s",
			req.DirPath, clientIP, *uid, *gid, ctx.UnixAuth.MachineName)
	} else {
		logger.Info("Mount request: path=%s client=%s auth=%s",
			req.DirPath, clientIP, authFlavorName(ctx.AuthFlavor))
	}

	// Check export access and apply squashing
	// The repository will apply AllSquash/RootSquash and return effective credentials
	accessDecision, authCtx, err := repository.CheckExportAccess(
		req.DirPath,
		clientIP,
		ctx.AuthFlavor,
		uid,
		gid,
		gids,
	)
	if err != nil {
		if exportErr, ok := err.(*metadata.ExportError); ok {
			logger.Warn("Mount denied: path=%s client=%s reason=%s",
				req.DirPath, clientIP, exportErr.Message)

			status := mapExportErrorToMountStatus(exportErr.Code)
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

	// Log squashing information if credentials were modified
	if authCtx.UID != nil && uid != nil && *authCtx.UID != *uid {
		logger.Info("Mount: credentials squashed for path=%s client=%s original_uid=%d effective_uid=%d",
			req.DirPath, clientIP, *uid, *authCtx.UID)
	}

	handleBytes, err := repository.GetRootHandle(req.DirPath)
	if err != nil {
		logger.Error("Failed to get root handle: path=%s error=%v", req.DirPath, err)
		return &MountResponse{Status: MountErrServerFault}, nil
	}

	// Record mount with ORIGINAL credentials (before squashing)
	// This preserves audit trail of who actually mounted
	var recordUID, recordGID *uint32
	machineName := ""
	if ctx.UnixAuth != nil {
		recordUID = &ctx.UnixAuth.UID
		recordGID = &ctx.UnixAuth.GID
		machineName = ctx.UnixAuth.MachineName
	}

	if err := repository.RecordMount(req.DirPath, clientIP, ctx.AuthFlavor, machineName, recordUID, recordGID); err != nil {
		logger.Warn("Failed to record mount: path=%s client=%s error=%v",
			req.DirPath, clientIP, err)
	}

	authFlavors := make([]int32, len(accessDecision.AllowedAuth))
	for i, flavor := range accessDecision.AllowedAuth {
		authFlavors[i] = int32(flavor)
	}

	logger.Info("Mount successful: path=%s client=%s handle_len=%d auth_flavors=%v readonly=%v",
		req.DirPath, clientIP, len(handleBytes), authFlavors, accessDecision.ReadOnly)

	return &MountResponse{
		Status:      MountOK,
		FileHandle:  handleBytes,
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
//
// Returns:
//   - []byte: The XDR-encoded response ready to send to the client
//   - error: Any error encountered during encoding
//
// Example:
//
//	resp := &MountResponse{
//	    Status:      MountOK,
//	    FileHandle:  []byte{0x01, 0x02, 0x03, 0x04},
//	    AuthFlavors: []int32{0},
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // handle error
//	}
//	// Send 'data' to client
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

// converts repository export errors to Mount protocol status codes
func mapExportErrorToMountStatus(code metadata.ExportErrorCode) uint32 {
	switch code {
	case metadata.ExportErrNotFound:
		return MountErrNoEnt
	case metadata.ExportErrAccessDenied:
		return MountErrAccess
	case metadata.ExportErrAuthRequired:
		return MountErrAccess
	case metadata.ExportErrServerFault:
		return MountErrServerFault
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
