package smb

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/adapter/smb/v2/handlers"
	"github.com/marmos91/dittofs/internal/logger"
)

// ProcessCompoundRequest processes all commands in a compound request sequentially.
// Related operations share FileID from the previous response.
// compoundData contains the remaining commands after the first one.
//
// Parameters:
//   - ctx: context for cancellation
//   - firstHeader: parsed header of the first command
//   - firstBody: body bytes of the first command
//   - compoundData: remaining compound bytes after the first command
//   - connInfo: connection metadata for handler dispatch
func ProcessCompoundRequest(ctx context.Context, firstHeader *header.SMB2Header, firstBody []byte, compoundData []byte, connInfo *ConnInfo) {
	// Track the last FileID for related operations
	var lastFileID [16]byte
	lastSessionID := firstHeader.SessionID
	lastTreeID := firstHeader.TreeID

	// Process first command
	logger.Debug("Processing compound request - first command",
		"command", firstHeader.Command.String(),
		"messageId", firstHeader.MessageID)

	result, fileID := ProcessRequestWithFileID(ctx, firstHeader, firstBody, connInfo)
	if fileID != [16]byte{} {
		lastFileID = fileID
	}
	if err := SendResponse(firstHeader, &handlers.SMBHandlerContext{SessionID: lastSessionID, TreeID: lastTreeID}, result, connInfo); err != nil {
		logger.Debug("Error sending compound response", "error", err)
	}

	// Process remaining commands from compound data
	remaining := compoundData
	for len(remaining) >= header.HeaderSize {
		// Keep a reference to the current command's start for signature verification.
		// Per MS-SMB2 3.2.4.1.4, each compound command is signed over its own bytes.
		currentCommandData := remaining

		hdr, body, nextRemaining, err := ParseCompoundCommand(remaining)
		if err != nil {
			logger.Debug("Error parsing compound command", "error", err)
			break
		}
		remaining = nextRemaining

		// Verify signature for this compound sub-command
		if err := VerifyCompoundCommandSignature(currentCommandData, hdr, connInfo); err != nil {
			logger.Warn("Compound command signature verification failed", "error", err)
			if sendErr := SendErrorResponse(hdr, types.StatusAccessDenied, connInfo); sendErr != nil {
				logger.Debug("Failed to send error response for signature failure", "error", sendErr)
			}
			break
		}

		// Handle related operations - inherit IDs from previous command
		if hdr.IsRelated() {
			if hdr.SessionID == 0 {
				hdr.SessionID = lastSessionID
			}
			if hdr.TreeID == 0 {
				hdr.TreeID = lastTreeID
			}
		}

		logger.Debug("Processing compound request - command",
			"command", hdr.Command.String(),
			"messageId", hdr.MessageID,
			"isRelated", hdr.IsRelated(),
			"usingFileID", lastFileID != [16]byte{})

		// Process with the inherited FileID for related operations
		var result *HandlerResult
		if hdr.IsRelated() && lastFileID != [16]byte{} {
			result = ProcessRequestWithInheritedFileID(ctx, hdr, body, lastFileID, connInfo)
		} else {
			var fileID [16]byte
			result, fileID = ProcessRequestWithFileID(ctx, hdr, body, connInfo)
			if fileID != [16]byte{} {
				lastFileID = fileID
			}
		}

		// Update tracking
		lastSessionID = hdr.SessionID
		lastTreeID = hdr.TreeID

		if err := SendResponse(hdr, &handlers.SMBHandlerContext{SessionID: lastSessionID, TreeID: lastTreeID}, result, connInfo); err != nil {
			logger.Debug("Error sending compound response", "error", err)
		}
	}
}

// ParseCompoundCommand parses the next command from compound data.
// Returns header, body, remaining data, and error.
func ParseCompoundCommand(data []byte) (*header.SMB2Header, []byte, []byte, error) {
	if len(data) < header.HeaderSize {
		return nil, nil, nil, fmt.Errorf("compound data too small: %d bytes", len(data))
	}

	// Parse SMB2 header
	hdr, err := header.Parse(data[:header.HeaderSize])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse compound SMB2 header: %w", err)
	}

	// Extract body for this command
	var body []byte
	var remaining []byte
	if hdr.NextCommand > 0 {
		bodyEnd := int(hdr.NextCommand)
		if bodyEnd > len(data) {
			bodyEnd = len(data)
		}
		body = data[header.HeaderSize:bodyEnd]
		if int(hdr.NextCommand) < len(data) {
			remaining = data[hdr.NextCommand:]
		}
	} else {
		body = data[header.HeaderSize:]
	}

	logger.Debug("SMB2 compound request",
		"command", hdr.Command.String(),
		"messageId", hdr.MessageID,
		"sessionId", fmt.Sprintf("0x%x", hdr.SessionID),
		"treeId", hdr.TreeID,
		"nextCommand", hdr.NextCommand,
		"flags", fmt.Sprintf("0x%x", hdr.Flags),
		"isRelated", hdr.IsRelated())

	return hdr, body, remaining, nil
}

// VerifyCompoundCommandSignature verifies the signature of a compound sub-command.
// Per MS-SMB2 3.2.4.1.4, each command in a compound is signed individually.
// The signature covers only this command's bytes (from its header to NextCommand or end).
func VerifyCompoundCommandSignature(data []byte, hdr *header.SMB2Header, connInfo *ConnInfo) error {
	if hdr.SessionID == 0 || hdr.Command == types.SMB2Negotiate || hdr.Command == types.SMB2SessionSetup {
		return nil
	}

	sess, ok := connInfo.Handler.GetSession(hdr.SessionID)
	if !ok {
		return nil
	}

	isSigned := hdr.Flags.IsSigned()
	if sess.Signing != nil && sess.Signing.SigningRequired && !isSigned {
		return fmt.Errorf("STATUS_ACCESS_DENIED: compound message not signed")
	}

	if isSigned && sess.ShouldVerify() {
		verifyBytes := data
		if hdr.NextCommand > 0 && int(hdr.NextCommand) <= len(data) {
			verifyBytes = data[:hdr.NextCommand]
		}

		if !sess.VerifyMessage(verifyBytes) {
			logger.Warn("SMB2 compound command signature verification failed",
				"command", hdr.Command.String(),
				"sessionID", hdr.SessionID,
				"verifyLen", len(verifyBytes))
			return fmt.Errorf("STATUS_ACCESS_DENIED: compound signature verification failed")
		}
		logger.Debug("Verified compound command signature",
			"command", hdr.Command.String(),
			"sessionID", hdr.SessionID)
	}
	return nil
}

// InjectFileID injects a FileID into the appropriate position in the request body.
// Offsets are per [MS-SMB2] specification for each command.
func InjectFileID(command types.Command, body []byte, fileID [16]byte) []byte {
	var offset int
	switch command {
	case types.SMB2Close, types.SMB2QueryDirectory:
		offset = 8 // [MS-SMB2] 2.2.15, 2.2.33
	case types.SMB2Read, types.SMB2Write, types.SMB2SetInfo:
		offset = 16 // [MS-SMB2] 2.2.19, 2.2.21, 2.2.39
	case types.SMB2QueryInfo:
		offset = 24 // [MS-SMB2] 2.2.37
	default:
		return body
	}

	requiredLen := offset + 16
	if len(body) < requiredLen {
		logger.Debug("Body too small for FileID injection",
			"command", command.String(),
			"need", requiredLen,
			"have", len(body))
		return body
	}

	newBody := make([]byte, len(body))
	copy(newBody, body)
	copy(newBody[offset:offset+16], fileID[:])

	logger.Debug("Injected FileID",
		"command", command.String(),
		"offset", offset)

	return newBody
}
