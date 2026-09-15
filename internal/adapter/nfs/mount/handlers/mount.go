package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/auth"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	internalxdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/internal/logger"
	xdr "github.com/rasky/go-xdr/xdr2"
)

// Handler implements the Mount protocol handlers defined in RFC 1813 Appendix I.
// It provides the standard implementation for mount operations, allowing NFS clients
// to obtain file handles for exported filesystems.
type Handler struct {
	// Registry provides access to all stores and shares
	// Exported to allow injection by the NFS adapter
	Registry mountRuntime
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

// Mount handles MOUNT MNT (RFC 1813 Appendix I, Mount procedure 1).
// Returns root file handle for an exported share after access control validation.
// Delegates to Runtime for share lookup, netgroup checks, and root handle retrieval.
// Records mount in Runtime mount tracker; validates auth policy and netgroup ACL.
// Errors: MountErrNoEnt (share not found), MountErrAccess (auth/netgroup denied), MountErrServerFault.
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

	// Refuse MOUNT on a disabled share. Per-request check is
	// the last line of defense when adapters hold a stale reference; the
	// runtime Share.Enabled flag is flipped synchronously by shares.Service
	// DisableShare before restore begins. MNT3ERR_ACCES (MountErrAccess=13)
	// is the spec-prescribed refusal code.
	if !share.Enabled {
		logger.Warn("NFS MOUNT refused: share disabled",
			"share", share.Name, "client_ip", clientIP)
		return &MountResponse{MountResponseBase: MountResponseBase{Status: MountErrAccess}}, nil
	}

	// Export access-control policy: which auth flavors this export accepts, the
	// GSS protection floor, and the netgroup client allowlist. The decision lives
	// in one place so MNT and the per-operation gate on the data path cannot
	// drift apart, and so this handler stays protocol-only.
	//
	// Existing connections are grandfathered at MOUNT only in the sense that MNT
	// is not replayed; the same policy is re-applied per operation.
	if accessErr := auth.CheckExportAccess(
		ctx.Context, share, ctx.AuthFlavor, net.ParseIP(clientIP), h.Registry.CheckNetgroupAccess,
	); accessErr != nil {
		logger.Warn("Mount denied by export access policy",
			"path", req.DirPath, "client_ip", clientIP, "reason", accessErr)
		return &MountResponse{MountResponseBase: MountResponseBase{Status: MountErrAccess}}, nil
	}

	// Record the mount in the registry
	h.Registry.RecordMount(clientIP, req.DirPath, time.Now().Unix())

	// Get root handle from registry (which encodes the share and root path)
	rootHandle, err := h.Registry.GetRootHandle(req.DirPath)
	if err != nil {
		logger.Error("Mount failed: cannot get root handle", "path", req.DirPath, "client_ip", clientIP, "error", err)
		return &MountResponse{MountResponseBase: MountResponseBase{Status: MountErrServerFault}}, nil
	}

	// Report only the flavors this share will actually accept. Clients negotiate
	// sec= from this list, so advertising one the gates above then refuse sends
	// them to a mount that cannot succeed.
	authFlavors := auth.AdvertisedAuthFlavors(share, ctx.KerberosEnabled)

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
	// The path length is read off the wire before the path itself, so cap the
	// decoder at the delivered bytes rather than sizing a buffer from a length
	// the request may not honour.
	_, err := xdr.UnmarshalLimited(bytes.NewReader(data), req, uint(len(data)))
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
