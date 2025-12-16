package handlers

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/identity"
)

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
	if adjustedOffset < 8 {
		adjustedOffset = 8 // Path starts after the 8-byte fixed structure
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

	// Check if share exists in registry (or fall back to mock shares)
	share, shareErr := h.Registry.GetShare(shareName)
	if shareErr != nil {
		// Fall back to mock shares for Phase 1 compatibility
		if !h.MockShareExists(shareName) {
			logger.Debug("Share not found", "shareName", shareName)
			return NewErrorResult(types.StatusBadNetworkName), nil
		}
		// Mock share - allow with full access
		share = nil
	}

	// Get session and check permissions
	sess, _ := h.SessionManager.GetSession(ctx.SessionID)
	permission := identity.PermissionReadWrite // Default for guests/mock

	if share != nil && sess != nil {
		userStore := h.Registry.GetUserStore()

		if sess.User != nil && userStore != nil {
			// Authenticated user - resolve their permission
			defaultPerm := identity.ParseSharePermission(share.DefaultPermission)
			permission = userStore.ResolveSharePermission(sess.User, shareName, defaultPerm)

			if permission == identity.PermissionNone {
				logger.Debug("Share access denied", "shareName", shareName, "user", sess.User.Username)
				return NewErrorResult(types.StatusAccessDenied), nil
			}

			logger.Debug("User permission resolved for tree connect",
				"shareName", shareName,
				"user", sess.User.Username,
				"permission", permission)
		} else if sess.IsGuest {
			// Guest session - check if guest access is allowed
			if !share.AllowGuest {
				logger.Debug("Share access denied (guest not allowed)", "shareName", shareName)
				return NewErrorResult(types.StatusAccessDenied), nil
			}

			// Use default permission for guests
			permission = identity.ParseSharePermission(share.DefaultPermission)
			if permission == identity.PermissionNone {
				logger.Debug("Share access denied (no guest permission)", "shareName", shareName)
				return NewErrorResult(types.StatusAccessDenied), nil
			}

			logger.Debug("Guest permission for tree connect",
				"shareName", shareName,
				"permission", permission)
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

	// Calculate MaximalAccess based on permission
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

// decodeUTF16LE decodes a UTF-16LE byte slice to a string
func decodeUTF16LE(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	u16s := make([]uint16, len(b)/2)
	for i := range u16s {
		u16s[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	// Remove null terminator if present
	for len(u16s) > 0 && u16s[len(u16s)-1] == 0 {
		u16s = u16s[:len(u16s)-1]
	}
	return string(utf16.Decode(u16s))
}

// parseSharePath parses \\server\share to /share or just share
func parseSharePath(path string) string {
	// Remove leading backslashes
	path = strings.TrimPrefix(path, "\\\\")

	// Split by backslash
	parts := strings.SplitN(path, "\\", 2)
	if len(parts) < 2 {
		// No server part, return as-is
		return "/" + strings.TrimPrefix(path, "/")
	}

	// Return the share part
	return "/" + parts[1]
}
