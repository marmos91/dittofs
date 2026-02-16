package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc/gss"
	internalxdr "github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	xdr "github.com/rasky/go-xdr/xdr2"
)

// Handler implements the Mount protocol handlers defined in RFC 1813 Appendix I.
// It provides the standard implementation for mount operations, allowing NFS clients
// to obtain file handles for exported filesystems.
type Handler struct {
	// Registry provides access to all stores and shares
	// Exported to allow injection by the NFS adapter
	Registry *runtime.Runtime
}

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
	MountResponseBase // Embeds Status and GetStatus()

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
func (h *Handler) Mount(
	ctx *MountHandlerContext,
	req *MountRequest,
) (*MountResponse, error) {
	// Check for cancellation before starting any work
	if ctx.isContextCancelled() {
		logger.Debug("Mount request cancelled before processing", "path", req.DirPath, "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return &MountResponse{MountResponseBase: MountResponseBase{Status: MountErrServerFault}}, ctx.Context.Err()
	}

	// Extract client IP from address (remove port)
	clientIP := extractClientIP(ctx.ClientAddr)

	// Log authentication info
	if ctx.AuthFlavor == rpc.AuthUnix && ctx.UID != nil && ctx.GID != nil {
		logger.Info("Mount request", "path", req.DirPath, "client_ip", clientIP, "auth", "UNIX", "uid", *ctx.UID, "gid", *ctx.GID)
	} else {
		authMethod := authFlavorName(ctx.AuthFlavor)
		logger.Info("Mount request", "path", req.DirPath, "client_ip", clientIP, "auth", authMethod)
	}

	// Check for cancellation before the potentially expensive access control check
	select {
	case <-ctx.Context.Done():
		logger.Debug("Mount request cancelled before access check", "path", req.DirPath, "client_ip", clientIP, "error", ctx.Context.Err())
		return &MountResponse{MountResponseBase: MountResponseBase{Status: MountErrServerFault}}, ctx.Context.Err()
	default:
	}

	// Check if share exists in registry
	if !h.Registry.ShareExists(req.DirPath) {
		logger.Warn("Mount denied", "path", req.DirPath, "client_ip", clientIP, "reason", "share not found")
		return &MountResponse{MountResponseBase: MountResponseBase{Status: MountErrNoEnt}}, nil
	}

	// Get share to check read-only status and security policy
	share, err := h.Registry.GetShare(req.DirPath)
	if err != nil {
		logger.Error("Mount access check failed", "path", req.DirPath, "client_ip", clientIP, "error", err)
		return &MountResponse{MountResponseBase: MountResponseBase{Status: MountErrServerFault}}, nil
	}

	// Security policy enforcement: check auth flavor against share policy.
	// Per locked decision: existing connections are grandfathered; this check
	// applies to NEW mount requests only.
	if !share.AllowAuthSys && ctx.AuthFlavor == rpc.AuthUnix {
		logger.Warn("Mount denied: AUTH_SYS not allowed on share",
			"path", req.DirPath, "client_ip", clientIP)
		return &MountResponse{MountResponseBase: MountResponseBase{Status: MountErrAccess}}, nil
	}
	if share.RequireKerberos && ctx.AuthFlavor != rpc.AuthRPCSECGSS {
		logger.Warn("Mount denied: Kerberos required but client uses non-GSS auth",
			"path", req.DirPath, "client_ip", clientIP, "auth_flavor", ctx.AuthFlavor)
		return &MountResponse{MountResponseBase: MountResponseBase{Status: MountErrAccess}}, nil
	}

	// Netgroup IP access check: reject mount if client IP is not in allowed netgroup.
	// Per locked decision: empty allowlist = allow all.
	// Fail-closed: if we cannot parse the client IP, deny access.
	clientNetIP := net.ParseIP(clientIP)
	if clientNetIP == nil {
		logger.Warn("Mount denied: unable to parse client IP for netgroup check",
			"path", req.DirPath, "client_ip", clientIP)
		return &MountResponse{MountResponseBase: MountResponseBase{Status: MountErrAccess}}, nil
	}
	{
		allowed, netErr := h.Registry.CheckNetgroupAccess(ctx.Context, req.DirPath, clientNetIP)
		if netErr != nil {
			logger.Warn("Mount netgroup access check error",
				"path", req.DirPath, "client_ip", clientIP, "error", netErr)
			// On error, deny access (fail-closed)
			return &MountResponse{MountResponseBase: MountResponseBase{Status: MountErrAccess}}, nil
		}
		if !allowed {
			logger.Warn("Mount denied: client IP not in allowed netgroup",
				"path", req.DirPath, "client_ip", clientIP)
			return &MountResponse{MountResponseBase: MountResponseBase{Status: MountErrAccess}}, nil
		}
	}

	// Record the mount in the registry
	h.Registry.RecordMount(clientIP, req.DirPath, time.Now().Unix())

	// Get root handle from registry (which encodes the share and root path)
	rootHandle, err := h.Registry.GetRootHandle(req.DirPath)
	if err != nil {
		logger.Error("Mount failed: cannot get root handle", "path", req.DirPath, "client_ip", clientIP, "error", err)
		return &MountResponse{MountResponseBase: MountResponseBase{Status: MountErrServerFault}}, nil
	}

	// Return supported authentication flavors
	// AUTH_UNIX (1) is always supported
	// When Kerberos is enabled, add Kerberos pseudoflavors per RFC 2623:
	//   - 390003: krb5 (authentication only)
	//   - 390004: krb5i (integrity protection)
	//   - 390005: krb5p (privacy/encryption)
	// These are the pseudoflavors that Linux NFS clients expect when mounting
	// with sec=krb5, sec=krb5i, or sec=krb5p options.
	authFlavors := []int32{1} // AUTH_UNIX
	if ctx.KerberosEnabled {
		// Kerberos pseudoflavors per RFC 2623 Section 2.1
		authFlavors = append(authFlavors,
			int32(gss.PseudoFlavorKrb5),  // krb5 - authentication only
			int32(gss.PseudoFlavorKrb5i), // krb5i - integrity
			int32(gss.PseudoFlavorKrb5p), // krb5p - privacy
		)
	}

	logger.Info("Mount successful", "path", req.DirPath, "client_ip", clientIP, "handle_len", len(rootHandle), "auth_flavors", authFlavors, "readonly", share.ReadOnly)

	return &MountResponse{
		MountResponseBase: MountResponseBase{Status: MountOK},
		FileHandle:        rootHandle,
		AuthFlavors:       authFlavors,
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
	// XDR opaque format: length followed by data with padding
	if err := internalxdr.WriteXDROpaque(&buf, resp.FileHandle); err != nil {
		return nil, fmt.Errorf("write file handle: %w", err)
	}

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
	case rpc.AuthRPCSECGSS:
		return "RPCSEC_GSS"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", flavor)
	}
}
