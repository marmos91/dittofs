package handlers

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/identity"
)

// treeConnectFixedSize is the size of the TREE_CONNECT request fixed structure [MS-SMB2] 2.2.9
// StructureSize(2) + Reserved/Flags(2) + PathOffset(2) + PathLength(2) = 8 bytes
const treeConnectFixedSize = 8

// ipcMaximalAccess defines the access rights for the IPC$ virtual share.
// [MS-SMB2] Section 2.2.10 - MaximalAccess is a bitmask of allowed operations.
// Value 0x1F grants the following SMB2 access rights for named pipe operations:
//   - FILE_READ_DATA   (0x01): Read data from the pipe
//   - FILE_WRITE_DATA  (0x02): Write data to the pipe
//   - FILE_APPEND_DATA (0x04): Append data to the pipe
//   - FILE_READ_EA     (0x08): Read extended attributes
//   - FILE_WRITE_EA    (0x10): Write extended attributes
//
// This is the minimum access required for RPC operations over named pipes.
const ipcMaximalAccess = 0x1F

// TreeConnect handles SMB2 TREE_CONNECT command [MS-SMB2] 2.2.9, 2.2.10
func (h *Handler) TreeConnect(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 9 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse request
	// structureSize := binary.LittleEndian.Uint16(body[0:2]) // Always 9
	// flags := binary.LittleEndian.Uint16(body[2:4]) // Reserved for SMB 3.x
	pathOffset := binary.LittleEndian.Uint16(body[4:6])
	pathLength := binary.LittleEndian.Uint16(body[6:8])

	// Path offset is relative to the start of the SMB2 header (64 bytes)
	// Since we receive body after the header, subtract 64 to get body offset
	adjustedOffset := int(pathOffset) - 64
	if adjustedOffset < treeConnectFixedSize {
		adjustedOffset = treeConnectFixedSize // Path starts after the fixed structure
	}

	// Extract path from body
	var sharePath string
	if pathLength > 0 && len(body) >= adjustedOffset+int(pathLength) {
		pathBytes := body[adjustedOffset : adjustedOffset+int(pathLength)]
		sharePath = decodeUTF16LE(pathBytes)
	}

	// Parse share path: \\server\share -> /share
	shareName := parseSharePath(sharePath)

	logger.Debug("TREE_CONNECT request",
		"pathOffset", pathOffset,
		"pathLength", pathLength,
		"adjustedOffset", adjustedOffset,
		"rawPath", sharePath,
		"parsedShareName", shareName,
		"bodyLen", len(body),
		"bodyHex", fmt.Sprintf("%x", body))

	// Handle IPC$ virtual share for named pipe operations (RPC, share enumeration)
	// IPC$ is always available and doesn't require registry configuration
	if strings.EqualFold(shareName, "/ipc$") {
		return h.handleIPCShare(ctx)
	}

	// Check if share exists in registry
	share, shareErr := h.Registry.GetShare(shareName)
	if shareErr != nil {
		logger.Debug("Share not found", "shareName", shareName)
		return NewErrorResult(types.StatusBadNetworkName), nil
	}

	// Get session and resolve permissions
	sess, _ := h.SessionManager.GetSession(ctx.SessionID)
	permission := identity.PermissionReadWrite // Default for unauthenticated
	defaultPerm := identity.ParseSharePermission(share.DefaultPermission)
	userStore := h.Registry.GetUserStore()

	// Resolve permission based on session type
	var user string
	if sess != nil && sess.User != nil && userStore != nil {
		// Authenticated user - resolve their permission
		permission = userStore.ResolveSharePermission(sess.User, shareName, defaultPerm)
		user = sess.User.Username
	} else if sess != nil && sess.IsGuest {
		// Guest session - use default permission
		permission = defaultPerm
		user = "guest"
	}

	// Check for access denied
	if permission == identity.PermissionNone {
		logger.Debug("Share access denied", "shareName", shareName, "user", user)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	logger.Debug("Permission resolved for tree connect",
		"shareName", shareName,
		"user", user,
		"permission", permission)

	// Apply share-level read_only override
	// If share is configured as read_only, cap permission to Read
	if share.ReadOnly && permission != identity.PermissionNone {
		if permission == identity.PermissionReadWrite || permission == identity.PermissionAdmin {
			logger.Debug("Share is read-only, capping permission to read",
				"shareName", shareName, "originalPermission", permission)
			permission = identity.PermissionRead
		}
	}

	// Create tree connection with permission
	treeID := h.GenerateTreeID()
	tree := &TreeConnection{
		TreeID:     treeID,
		SessionID:  ctx.SessionID,
		ShareName:  shareName,
		ShareType:  types.SMB2ShareTypeDisk,
		CreatedAt:  time.Now(),
		Permission: permission,
	}
	h.StoreTree(tree)

	ctx.TreeID = treeID
	ctx.ShareName = shareName

	// Calculate MaximalAccess based on effective permission
	maximalAccess := calculateMaximalAccess(permission)

	// Build response (16 bytes)
	resp := make([]byte, 16)
	binary.LittleEndian.PutUint16(resp[0:2], 16)              // StructureSize
	resp[2] = types.SMB2ShareTypeDisk                         // ShareType
	resp[3] = 0                                               // Reserved
	binary.LittleEndian.PutUint32(resp[4:8], 0)               // ShareFlags
	binary.LittleEndian.PutUint32(resp[8:12], 0)              // Capabilities
	binary.LittleEndian.PutUint32(resp[12:16], maximalAccess) // MaximalAccess

	return NewResult(types.StatusSuccess, resp), nil
}

// calculateMaximalAccess returns the SMB2 MaximalAccess mask based on share permission.
// [MS-SMB2] Section 2.2.10 - MaximalAccess is a bit mask of allowed operations.
func calculateMaximalAccess(perm identity.SharePermission) uint32 {
	// SMB2 Access Mask values
	const (
		// Standard rights
		fileReadData        = 0x00000001
		fileWriteData       = 0x00000002
		fileAppendData      = 0x00000004
		fileReadEA          = 0x00000008
		fileWriteEA         = 0x00000010
		fileExecute         = 0x00000020
		fileDeleteChild     = 0x00000040
		fileReadAttributes  = 0x00000080
		fileWriteAttributes = 0x00000100
		delete_             = 0x00010000
		readControl         = 0x00020000
		writeDAC            = 0x00040000
		writeOwner          = 0x00080000
		synchronize         = 0x00100000

		// Generic read access
		genericRead = fileReadData | fileReadEA | fileReadAttributes | readControl | synchronize

		// Generic write access
		genericWrite = fileWriteData | fileAppendData | fileWriteEA | fileWriteAttributes | synchronize

		// Full access
		fullAccess = 0x001F01FF
	)

	switch perm {
	case identity.PermissionAdmin:
		// Full access including delete and ownership
		return fullAccess
	case identity.PermissionReadWrite:
		// Read and write access
		return genericRead | genericWrite | delete_ | fileDeleteChild
	case identity.PermissionRead:
		// Read-only access
		return genericRead
	default:
		// No access (shouldn't reach here, access denied earlier)
		return 0
	}
}

// Note: decodeUTF16LE and encodeUTF16LE are defined in encoding.go

// handleIPCShare handles TREE_CONNECT to the virtual IPC$ share.
// IPC$ is used for inter-process communication including:
// - Share enumeration via SRVSVC RPC
// - Remote registry access
// - Named pipe operations
// [MS-SMB2] Section 2.2.10 specifies ShareType 0x02 for pipe shares.
func (h *Handler) handleIPCShare(ctx *SMBHandlerContext) (*HandlerResult, error) {
	logger.Debug("TREE_CONNECT to virtual IPC$ share", "sessionID", ctx.SessionID)

	// Verify that a valid session exists before granting IPC$ access.
	// While IPC$ is a well-known share that should be accessible to authenticated clients,
	// we still require a valid session to have been established first.
	sess, found := h.SessionManager.GetSession(ctx.SessionID)
	if !found || sess == nil {
		logger.Debug("IPC$ access denied: no valid session", "sessionID", ctx.SessionID)
		return NewErrorResult(types.StatusUserSessionDeleted), nil
	}

	// Create tree connection for IPC$ with PIPE share type
	treeID := h.GenerateTreeID()
	tree := &TreeConnection{
		TreeID:     treeID,
		SessionID:  ctx.SessionID,
		ShareName:  "/ipc$",
		ShareType:  types.SMB2ShareTypePipe, // Named pipe share
		CreatedAt:  time.Now(),
		Permission: identity.PermissionReadWrite,
	}
	h.StoreTree(tree)

	ctx.TreeID = treeID
	ctx.ShareName = "/ipc$"

	// Build response with PIPE share type
	// [MS-SMB2] Section 2.2.10 TREE_CONNECT Response
	resp := make([]byte, 16)
	binary.LittleEndian.PutUint16(resp[0:2], 16)                 // StructureSize
	resp[2] = types.SMB2ShareTypePipe                            // ShareType: Named pipe
	resp[3] = 0                                                  // Reserved
	binary.LittleEndian.PutUint32(resp[4:8], 0)                  // ShareFlags: none
	binary.LittleEndian.PutUint32(resp[8:12], 0)                 // Capabilities: none
	binary.LittleEndian.PutUint32(resp[12:16], ipcMaximalAccess) // MaximalAccess: basic read/write for IPC

	return NewResult(types.StatusSuccess, resp), nil
}

// parseSharePath parses \\server\share to /share or just share
// The share name is normalized to lowercase for case-insensitive matching.
func parseSharePath(path string) string {
	// Remove leading backslashes
	path = strings.TrimPrefix(path, "\\\\")

	// Split by backslash
	parts := strings.SplitN(path, "\\", 2)
	if len(parts) < 2 {
		// No server part, return as-is with lowercase normalization
		return "/" + strings.ToLower(strings.TrimPrefix(path, "/"))
	}

	// Return the share part, normalized to lowercase
	// Windows clients often send share names in uppercase (e.g., /EXPORT)
	// but our shares are typically configured in lowercase (e.g., /export)
	return "/" + strings.ToLower(parts[1])
}
