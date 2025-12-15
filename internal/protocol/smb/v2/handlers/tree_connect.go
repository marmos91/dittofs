package handlers

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
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

	// For Phase 1, check against mock shares
	if !h.MockShareExists(shareName) {
		logger.Debug("Share not found", "shareName", shareName)
		return NewErrorResult(types.StatusBadNetworkName), nil
	}

	// Create tree connection
	treeID := h.GenerateTreeID()
	tree := &TreeConnection{
		TreeID:    treeID,
		SessionID: ctx.SessionID,
		ShareName: shareName,
		ShareType: types.SMB2ShareTypeDisk,
		CreatedAt: time.Now(),
	}
	h.StoreTree(tree)

	ctx.TreeID = treeID
	ctx.ShareName = shareName

	// Build response (16 bytes)
	resp := make([]byte, 16)
	binary.LittleEndian.PutUint16(resp[0:2], 16)               // StructureSize
	resp[2] = types.SMB2ShareTypeDisk                          // ShareType
	resp[3] = 0                                                 // Reserved
	binary.LittleEndian.PutUint32(resp[4:8], 0)                // ShareFlags
	binary.LittleEndian.PutUint32(resp[8:12], 0)               // Capabilities
	binary.LittleEndian.PutUint32(resp[12:16], 0x001F01FF)     // MaximalAccess (full access)

	return NewResult(types.StatusSuccess, resp), nil
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

// encodeUTF16LE encodes a string to UTF-16LE byte slice
func encodeUTF16LE(s string) []byte {
	u16s := utf16.Encode([]rune(s))
	b := make([]byte, len(u16s)*2)
	for i, r := range u16s {
		binary.LittleEndian.PutUint16(b[i*2:], r)
	}
	return b
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
